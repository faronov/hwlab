package rom

import (
	"bytes"
	"crypto/md5" //nolint:gosec // protocol digest
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Sim is an in-process SYNTHETIC ESP chip behind a serial line: the reset
// wiring (classic auto-reset circuit or USB-Serial/JTAG), the ROM download
// mode and the flasher stub, with a flash array and the registers the
// plugin reads. It implements Link and models the protocol details the
// client depends on: SLIP, 4 vs 2 status bytes, ROM/stub error codes, the
// stub's deferred write errors, READ_FLASH acknowledgements and digests,
// and baud changes. Fault injection fields exercise retries and refusals.
type Sim struct {
	mu sync.Mutex

	Chip   *Chip
	Wiring Entry
	Flash  []byte
	JEDEC  uint32
	// Regs answers READ_REG; WRITE_REG updates it.
	Regs map[uint32]uint32
	// StrapNeed is how long the strap must stay low after EN is released
	// (classic wiring) for the ROM to see it.
	StrapNeed time.Duration
	// AppLog is printed by the application after a reset into it.
	AppLog string

	// Fault injection.
	IgnoreSyncs       int
	DropReadPacket    int // drop the n-th READ_FLASH packet sent (1-based, once)
	CorruptReadPacket int // flip a byte of the n-th READ_FLASH packet (once)
	ChangeFlashAfter  int // after n complete READ_FLASH runs, flip a flash byte (once)
	FailWriteAt       int // a FLASH_DATA write covering this offset fails (-1 off)
	NoOHAI            bool
	// FLASH_DATA faults by the n-th FLASH_DATA frame received (1-based,
	// once): dropped on the way in, its answer dropped, or corrupted.
	DropFlashData       int
	DropFlashDataAnswer int
	CorruptFlashData    int
	// Reenumerate makes the link fail after a USJ reset until Reopen.
	Reenumerate bool

	// Observations.
	Boots    []string
	Commands map[byte]int
	Erased   []uint32
	Writes   int
	LineLog  [][2]bool

	mode      string // "off", "reset", "app", "rom", "stub"
	dtr, rts  bool
	usjFlag   bool
	pendingAt time.Time
	pending   bool
	baud      int
	hostBaud  int
	gone      bool
	readTO    time.Duration
	out       []outSeg
	dec       SlipDecoder
	attached  bool
	limit     int
	ram       map[uint32]byte
	mem       struct{ addr, left uint32 }
	fl        struct {
		addr, left, nextErase uint32
		on                    bool
	}
	rd struct {
		on              bool
		addr, left, pkt uint32
		sent            int
		h               []byte
	}
	readRuns   int
	pktCount   int
	fdCount    int
	pendingErr uint16
}

// outSeg is output sent at one baud rate.
type outSeg struct {
	b    []byte
	baud int
}

// emit queues bytes the chip sends at its current rate.
func (s *Sim) emit(b []byte) {
	if n := len(s.out); n > 0 && s.out[n-1].baud == s.baud {
		s.out[n-1].b = append(s.out[n-1].b, b...)
		return
	}
	s.out = append(s.out, outSeg{append([]byte(nil), b...), s.baud})
}

// NewSim returns a powered-off SYNTHETIC chip of type ch with a flash of
// size bytes holding a recognizable pattern.
func NewSim(ch *Chip, wiring Entry, size int) *Sim {
	s := &Sim{Chip: ch, Wiring: wiring, Flash: make([]byte, size), Regs: map[uint32]uint32{}, Commands: map[byte]int{},
		mode: "off", baud: 115200, hostBaud: 115200, FailWriteAt: -1, ram: map[uint32]byte{}}
	for i := range s.Flash {
		s.Flash[i] = byte(i*7 + i>>12)
	}
	capBits := 0
	for 1<<capBits < size {
		capBits++
	}
	s.JEDEC = 0xC8 | 0x40<<8 | uint32(capBits)<<16
	s.AppLog = "I (29) boot: ESP-IDF v5.1 2nd stage bootloader\r\nI (300) app: SYNTHETIC application running\r\n"
	if ch.Magic != nil {
		s.Regs[ChipDetectAddr] = ch.Magic[0]
	} else {
		s.Regs[ChipDetectAddr] = 0x97E30068
	}
	switch ch {
	case ESP32:
		b := ch.EfuseBase
		s.Regs[b+0x04] = 0xC3123456              // MAC low word
		s.Regs[b+0x08] = 0x5A<<16 | 0x24DC       // MAC high + CRC
		s.Regs[b+0x0C] = 7<<9 | 1<<15            // package D0WDR2-V3, rev1
		s.Regs[b+0x14] = 1 << 20                 // rev2
		s.Regs[0x3FF6607C] = 0x80000000 | 0x1234 // rev3 bit
	default:
		if ch.EfuseBase != 0 {
			s.Regs[ch.EfuseBase+0x44] = 0xBD641ECE
			s.Regs[ch.EfuseBase+0x48] = 0xFFFE<<16 | 0x744D
		}
	}
	return s
}

// PowerOn starts the application (as after plugging the board in).
func (s *Sim) PowerOn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boot(false)
}

// HoldBoot puts the chip in download mode as a person holding BOOT at reset.
func (s *Sim) HoldBoot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boot(true)
}

// Mode is "off", "reset", "app", "rom" or "stub".
func (s *Sim) Mode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settle(false)
	return s.mode
}

// Baud is the chip's current UART rate.
func (s *Sim) Baud() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baud
}

func (s *Sim) boot(download bool) {
	s.pending, s.attached, s.limit, s.pendingErr = false, false, min(len(s.Flash), 4<<20), 0
	s.baud, s.dec, s.rd.on, s.fl.on = 115200, SlipDecoder{}, false, false
	if download {
		s.mode = "rom"
		s.Boots = append(s.Boots, "download")
		if s.Wiring == EntryUSJ {
			s.emit([]byte("ESP-ROM:sim\r\nrst:0x15 (USB_UART_CHIP_RESET),boot:0x0 (DOWNLOAD(USB/UART0))\r\nwaiting for download\r\n"))
		} else {
			s.emit([]byte("rst:0x1 (POWERON_RESET),boot:0x3 (DOWNLOAD_BOOT(UART0/UART1/SDIO_REI_REO_V2))\r\nwaiting for download\r\n"))
		}
		return
	}
	s.mode = "app"
	s.Boots = append(s.Boots, "app")
	s.emit([]byte(s.AppLog))
}

// SetLines is the host's DTR/RTS (one atomic change).
func (s *Sim) SetLines(dtr, rts bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return false, errors.New("sim: device gone")
	}
	s.settle(false)
	s.LineLog = append(s.LineLog, [2]bool{dtr, rts})
	pd, pr := s.dtr, s.rts
	s.dtr, s.rts = dtr, rts
	switch s.Wiring {
	case EntryClassic:
		enLow, wasLow := rts && !dtr, pr && !pd
		strapLow := dtr && !rts
		if s.pending && !strapLow {
			s.settle(true)
		}
		switch {
		case enLow && !wasLow:
			s.mode, s.pending = "reset", false
		case !enLow && wasLow:
			if strapLow {
				s.pending, s.pendingAt = true, time.Now()
			} else {
				s.boot(false)
			}
		}
	case EntryUSJ:
		switch {
		case !rts && !dtr:
			s.usjFlag = false
		case !rts && dtr:
			s.usjFlag = true
		case rts && !dtr && (pd || !pr):
			s.boot(s.usjFlag)
			if s.Reenumerate {
				s.gone = true
			}
		}
	}
	return true, nil
}

// settle decides a pending classic boot: download when the strap stayed low
// for StrapNeed after EN was released. released reports the strap went high now.
func (s *Sim) settle(released bool) {
	if !s.pending {
		return
	}
	held := time.Since(s.pendingAt)
	if held >= s.StrapNeed {
		s.boot(true)
		return
	}
	if released {
		s.boot(false)
	}
}

// SetBaud is the host side's line rate.
func (s *Sim) SetBaud(b int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostBaud = b
	return nil
}

// Reopen makes a re-enumerated port usable again.
func (s *Sim) Reopen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gone = false
	s.dtr, s.rts = false, false
}

// SetReadTimeout implements Link.
func (s *Sim) SetReadTimeout(d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readTO = d
	return nil
}

// ResetInput implements Link.
func (s *Sim) ResetInput() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = nil
	return nil
}

// Read implements Link.
func (s *Sim) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.gone {
		s.mu.Unlock()
		return 0, errors.New("sim: device gone")
	}
	s.settle(false)
	if len(s.out) == 0 {
		d := min(s.readTO, 2*time.Millisecond)
		s.mu.Unlock()
		time.Sleep(d)
		return 0, nil
	}
	defer s.mu.Unlock()
	seg := &s.out[0]
	n := copy(p, seg.b)
	if seg.baud != s.hostBaud {
		for i := 0; i < n; i++ {
			p[i] = 0x00 // a rate mismatch reads garbage
		}
	}
	seg.b = seg.b[n:]
	if len(seg.b) == 0 {
		s.out = s.out[1:]
	}
	return n, nil
}

// Write implements Link.
func (s *Sim) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return 0, errors.New("sim: device gone")
	}
	s.settle(false)
	if (s.mode != "rom" && s.mode != "stub") || s.hostBaud != s.baud {
		return len(p), nil
	}
	for _, f := range s.dec.Feed(p) {
		s.frame(f)
	}
	return len(p), nil
}

func (s *Sim) stub() bool { return s.mode == "stub" }

func (s *Sim) reply(op byte, val uint32, data []byte, code uint16) {
	var st []byte
	if s.stub() {
		st = []byte{byte(code >> 8), byte(code)}
	} else {
		st = []byte{0, 0, 0, 0}
		if code != 0 {
			st[0], st[1] = 1, byte(code)
		}
	}
	f := make([]byte, 8, 8+len(data)+len(st))
	f[0], f[1] = 0x01, op
	binary.LittleEndian.PutUint16(f[2:], uint16(len(data)+len(st)))
	binary.LittleEndian.PutUint32(f[4:], val)
	f = append(append(f, data...), st...)
	s.emit(SlipEncode(f))
}

// Error codes the sim uses (ROM error byte / stub 16-bit code).
const (
	romInvalid  = 0x05
	romFailed   = 0x06
	romChecksum = 0x07
	romReadErr  = 0x09
	stubBadLen  = 0xC000
	stubBadSum  = 0xC100
	stubBadCmd  = 0xC300
	stubSPIFail = 0xC400
	stubNotIn   = 0xC600
	stubNotImpl = 0xFF00
)

func (s *Sim) fail(op byte, rom byte, stub uint16) {
	if s.stub() {
		s.reply(op, 0, nil, stub)
	} else {
		s.reply(op, 0, nil, uint16(rom))
	}
}

func (s *Sim) frame(f []byte) {
	if s.rd.on {
		s.readAck(f)
		return
	}
	if len(f) < 8 || f[0] != 0 {
		return
	}
	op := f[1]
	size := int(binary.LittleEndian.Uint16(f[2:]))
	chk := binary.LittleEndian.Uint32(f[4:])
	data := f[8:]
	if len(data) != size {
		s.fail(op, romInvalid, stubBadLen)
		return
	}
	s.Commands[op]++
	if s.stub() && s.pendingErr != 0 && op != CmdSync {
		code := s.pendingErr
		s.pendingErr = 0
		s.reply(op, 0, nil, code)
		return
	}
	w := func(i int) uint32 { return binary.LittleEndian.Uint32(data[4*i:]) }
	switch op {
	case CmdSync:
		if len(data) != 36 || !bytes.Equal(data, syncPayload()) {
			s.fail(op, romInvalid, stubBadLen)
			return
		}
		if s.IgnoreSyncs > 0 {
			s.IgnoreSyncs--
			return
		}
		val := uint32(0x20120707)
		if s.stub() {
			val = 0
		}
		for i := 0; i < 8; i++ {
			s.reply(op, val, nil, 0)
		}
	case CmdReadReg:
		if len(data) != 4 {
			s.fail(op, romInvalid, stubBadLen)
			return
		}
		s.reply(op, s.Regs[w(0)], nil, 0)
	case CmdWriteReg:
		if len(data) == 0 || len(data)%16 != 0 {
			s.fail(op, romInvalid, stubBadLen)
			return
		}
		for i := 0; i < len(data)/16; i++ {
			a, v, m := w(4*i), w(4*i+1), w(4*i+2)
			s.Regs[a] = s.Regs[a]&^m | v&m
			s.spiCommand(a)
		}
		s.reply(op, 0, nil, 0)
	case CmdSPIAttach:
		if (s.stub() && len(data) != 4) || (!s.stub() && len(data) != 8) {
			s.fail(op, romInvalid, stubBadLen)
			return
		}
		s.attached = true
		s.reply(op, 0, nil, 0)
	case CmdSPISetParams:
		if len(data) != 24 {
			s.fail(op, romInvalid, stubBadLen)
			return
		}
		s.limit = min(int(w(1)), len(s.Flash))
		s.reply(op, 0, nil, 0)
	case CmdChangeBaud:
		if len(data) != 8 || (!s.stub() && w(1) != 0) {
			s.fail(op, romInvalid, stubBadLen)
			return
		}
		s.reply(op, 0, nil, 0)
		s.baud = int(w(0))
	case CmdGetSecurityInfo:
		if s.Chip == ESP32 {
			s.fail(op, romInvalid, stubNotImpl)
			return
		}
		b := make([]byte, 20)
		binary.LittleEndian.PutUint32(b[12:], s.Chip.SecChipID)
		binary.LittleEndian.PutUint32(b[16:], 1)
		if s.Chip.SecChipID == 0 {
			b = b[:12]
		}
		s.reply(op, 0, b, 0)
	case CmdMemBegin:
		if len(data) != 16 {
			s.fail(op, romInvalid, stubBadLen)
			return
		}
		s.mem.left, s.mem.addr = w(0), w(3)
		s.reply(op, 0, nil, 0)
	case CmdMemData:
		if len(data) < 16 || int(w(0)) != len(data)-16 || w(0) > s.mem.left {
			s.fail(op, romInvalid, stubBadLen)
			return
		}
		if Checksum(data[16:]) != chk {
			s.fail(op, romChecksum, stubBadSum)
			return
		}
		for i, b := range data[16:] {
			s.ram[s.mem.addr+uint32(i)] = b
		}
		s.mem.addr += w(0)
		s.mem.left -= w(0)
		s.reply(op, 0, nil, 0)
	case CmdMemEnd:
		if len(data) != 8 {
			s.fail(op, romInvalid, stubBadLen)
			return
		}
		s.reply(op, 0, nil, 0)
		if w(0) == 0 && !s.stub() && s.stubLoaded(w(1)) && !s.NoOHAI {
			s.mode = "stub"
			s.emit(SlipEncode(ohai))
		}
	case CmdSPIFlashMD5:
		if len(data) != 16 {
			s.fail(op, romInvalid, stubBadLen)
			return
		}
		a, n := int(w(0)), int(w(1))
		if !s.attached || a+n > s.limit {
			s.fail(op, romReadErr, stubSPIFail)
			return
		}
		sum := md5.Sum(s.Flash[a : a+n]) //nolint:gosec
		if s.stub() {
			s.reply(op, 0, sum[:], 0)
		} else {
			s.reply(op, 0, []byte(hex.EncodeToString(sum[:])), 0)
		}
	default:
		if !s.stub() {
			s.fail(op, romInvalid, 0)
			return
		}
		s.stubOnly(op, data, chk, w)
	}
}

func (s *Sim) stubLoaded(entry uint32) bool {
	st, err := StubFor(s.Chip.Key)
	if err != nil || st.Entry != entry {
		return false
	}
	for _, seg := range []struct {
		a uint32
		b []byte
	}{{st.TextStart, st.Text}, {st.DataStart, st.Data}} {
		for i, b := range seg.b {
			if s.ram[seg.a+uint32(i)] != b {
				return false
			}
		}
	}
	return true
}

// spiCommand runs an SPI1 user command when CMD.USR is set: JEDEC id only.
func (s *Sim) spiCommand(addr uint32) {
	r := s.Chip.SPI
	if r.Base == 0 || addr != r.Base || s.Regs[addr]&spiCmdUSR == 0 {
		return
	}
	s.Regs[addr] &^= spiCmdUSR
	if s.Regs[r.Base+r.User2]&0xFFFF == 0x9F && s.Regs[r.Base+r.User]&spiUserMISO != 0 {
		if s.attached {
			s.Regs[r.Base+r.W0] = s.JEDEC
		} else {
			s.Regs[r.Base+r.W0] = 0xFFFFFF
		}
	}
}

func (s *Sim) eraseTo(end uint32) {
	for s.fl.nextErase < end {
		s.eraseSector(s.fl.nextErase)
		s.fl.nextErase += SectorSize
	}
}

func (s *Sim) eraseSector(a uint32) {
	for i := a; i < a+SectorSize && int(i) < len(s.Flash); i++ {
		s.Flash[i] = 0xFF
	}
	s.Erased = append(s.Erased, a)
}

func (s *Sim) stubOnly(op byte, data []byte, chk uint32, w func(int) uint32) {
	switch op {
	case CmdFlashBegin:
		if len(data) != 16 && len(data) != 20 {
			s.fail(op, 0, stubBadLen)
			return
		}
		size, addr := w(0), w(3)
		if int(addr)+int(size) > s.limit {
			s.fail(op, 0, stubSPIFail)
			return
		}
		s.fl.on, s.fl.left, s.fl.addr, s.fl.nextErase = true, size, addr, addr/SectorSize*SectorSize
		s.reply(op, 0, nil, 0)
	case CmdFlashData:
		s.fdCount++
		switch s.fdCount {
		case s.DropFlashData:
			s.DropFlashData = 0
			return
		case s.CorruptFlashData:
			s.CorruptFlashData = 0
			s.fail(op, 0, stubBadSum)
			return
		}
		if !s.fl.on {
			s.fail(op, 0, stubNotIn)
			return
		}
		if len(data) < 16 || int(w(0)) != len(data)-16 {
			s.fail(op, 0, 0xC900)
			return
		}
		if Checksum(data[16:]) != chk {
			s.fail(op, 0, stubBadSum)
			return
		}
		if s.fdCount == s.DropFlashDataAnswer {
			s.DropFlashDataAnswer = 0
		} else {
			s.reply(op, 0, nil, 0)
		}
		n := min(uint32(len(data)-16), s.fl.left)
		end := s.fl.addr + n
		s.eraseTo((end + SectorSize - 1) / SectorSize * SectorSize)
		if s.FailWriteAt >= 0 && uint32(s.FailWriteAt) >= s.fl.addr && uint32(s.FailWriteAt) < end {
			s.pendingErr = stubSPIFail
			return
		}
		copy(s.Flash[s.fl.addr:end], data[16:16+n])
		s.fl.addr, s.fl.left = end, s.fl.left-n
		s.Writes++
	case CmdEraseRegion:
		if len(data) != 8 || w(0)%SectorSize != 0 || w(1)%SectorSize != 0 || int(w(0)+w(1)) > s.limit {
			s.fail(op, 0, stubBadLen)
			return
		}
		for a := w(0); a < w(0)+w(1); a += SectorSize {
			s.eraseSector(a)
		}
		s.reply(op, 0, nil, 0)
	case CmdEraseFlash:
		for a := 0; a < len(s.Flash); a += SectorSize {
			s.eraseSector(uint32(a))
		}
		s.reply(op, 0, nil, 0)
	case CmdReadFlash:
		if len(data) != 16 || w(2) > 4096 || w(2) == 0 {
			s.fail(op, 0, stubBadLen)
			return
		}
		s.reply(op, 0, nil, 0)
		if int(w(0)+w(1)) > s.limit {
			s.pendingErr = stubSPIFail // reported on the next command; nothing is streamed
			return
		}
		s.rd.on, s.rd.addr, s.rd.left, s.rd.pkt, s.rd.sent, s.rd.h = true, w(0), w(1), w(2), 0, nil
		s.sendPacket()
	default:
		s.fail(op, 0, stubBadCmd)
	}
}

func (s *Sim) sendPacket() {
	if s.rd.left == 0 {
		sum := md5.Sum(s.rd.h) //nolint:gosec
		s.emit(SlipEncode(sum[:]))
		s.rd.on = false
		s.readRuns++
		if s.ChangeFlashAfter > 0 && s.readRuns == s.ChangeFlashAfter {
			s.Flash[len(s.Flash)/2] ^= 0x01
		}
		return
	}
	n := min(s.rd.left, s.rd.pkt)
	p := append([]byte(nil), s.Flash[s.rd.addr:s.rd.addr+n]...)
	s.rd.h = append(s.rd.h, p...)
	s.rd.addr += n
	s.rd.left -= n
	s.rd.sent += int(n)
	s.pktCount++
	switch s.pktCount {
	case s.DropReadPacket:
		s.DropReadPacket = 0
		return
	case s.CorruptReadPacket:
		s.CorruptReadPacket = 0
		p[0] ^= 0xFF
	}
	s.emit(SlipEncode(p))
}

func (s *Sim) readAck(f []byte) {
	if len(f) != 4 {
		// The stub leaves the loop and sends the digest of what it read.
		sum := md5.Sum(s.rd.h) //nolint:gosec
		s.emit(SlipEncode(sum[:]))
		s.rd.on = false
		return
	}
	if int(binary.LittleEndian.Uint32(f)) == s.rd.sent {
		s.sendPacket()
	}
}

// String describes the sim for test failures.
func (s *Sim) String() string {
	return fmt.Sprintf("sim %s %s mode=%s baud=%d boots=%v", s.Chip.Name, s.Wiring, s.mode, s.baud, s.Boots)
}
