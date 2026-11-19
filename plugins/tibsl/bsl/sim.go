package bsl

import (
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"sync"
	"time"
)

// SimCCFG is the CCFG of the real CC2652P dongle this plugin was checked on
// (Z-Stack image): backdoor on DIO15 active low, CPU DAP enabled, TI FA off.
const SimCCFG = "000080011000c0fffdff5800ffffbbf1ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffc50ffec5ffffffff00ffffff00c5c5ff000000ff00000000ffffffffffffffffffffffffffffffff"

// SimBackdoorPin is the DIO the simulated board wires to DTR (Sonoff style).
const SimBackdoorPin = 15

type simMode int

const (
	simApp simMode = iota
	simReset
	simBSLWaitSync
	simBSL
)

// Sim is an in-process SYNTHETIC ROM bootloader with a CC2652P-like (or
// CC2538-like) memory map. With Wired set, DTR/RTS drive the bootloader pin
// and RESET like a Sonoff ZBDongle-P: RESET is low only while (DTR=0, RTS=1),
// DIO15 only while (DTR=1, RTS=0). Without it the board was put into the
// bootloader by hand.
type Sim struct {
	mu sync.Mutex

	Family    Family
	Flash     []byte
	FlashBase uint32
	Sector    int
	FCFG      []byte
	Regs      map[uint32]uint32
	ChipIDVal uint32
	Wired     bool
	// Inverted swaps the roles: DTR drives RESET, RTS the bootloader pin.
	Inverted bool

	// Counters for tests.
	Commands   map[byte]int
	Erased     []uint32
	BankErases int
	Resets     int
	Boots      []string
	// FlipReadAt flips a bit in the reply of the n-th MEMORY_READ (1-based; 0 off).
	FlipReadAt int

	mode     simMode
	dtr, rts bool
	syncs    int
	in       []byte
	out      []byte
	hostAck  bool
	status   byte
	dlAddr   uint32
	dlLeft   int
	reads    int
	ieee2538 [8]byte
}

// PowerOn models a power-up with the current lines (the ROM boot decision).
func (s *Sim) PowerOn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boot(s.Wired && s.bslLow())
}

// NewSim returns a simulated CC2652P (352 KiB, IEEE 00:12:4B:00:20:CD:1D:7D)
// whose flash holds a small valid image; it starts in the bootloader.
func NewSim() *Sim {
	s := &Sim{Family: FamilyCC26xx, FlashBase: 0, Sector: 8192, Flash: make([]byte, 0x2C*8192), FCFG: make([]byte, FCFG1Size),
		ChipIDVal: 0x3202F000, Commands: map[byte]int{}, mode: simBSLWaitSync}
	for i := range s.Flash {
		s.Flash[i] = 0xFF
	}
	for i := range s.FCFG {
		s.FCFG[i] = 0xFF
	}
	for i := 0; i < 0x6000; i++ {
		s.Flash[i] = byte(i*7 + i>>8)
	}
	cfg, _ := hex.DecodeString(SimCCFG)
	copy(s.Flash[len(s.Flash)-CCFGLen:], cfg)
	put := func(addr, v uint32) { binary.LittleEndian.PutUint32(s.FCFG[addr-FCFG1Base:], v) }
	put(RegICEPickDeviceID, 0x3BB4102F)
	put(RegUserID, 0x3202F000)
	put(RegMAC154, 0x20CD1D7D)
	put(RegMAC154+4, 0x00124B00)
	s.Regs = map[uint32]uint32{RegFlashSize: 0x2C, 0x40082250: 3}
	return s
}

// NewSim2538 returns a simulated CC2538 (512 KiB at 0x200000).
func NewSim2538() *Sim {
	s := &Sim{Family: FamilyCC2538, FlashBase: CC2538FlashBase, Sector: CC2538Page, Flash: make([]byte, 512<<10),
		ChipIDVal: 0x0000B964, Commands: map[byte]int{}, mode: simBSLWaitSync}
	for i := range s.Flash {
		s.Flash[i] = 0xFF
	}
	for i := 0; i < 0x3000; i++ {
		s.Flash[i] = byte(i*3 + 1)
	}
	cca := s.Flash[len(s.Flash)-CC2538CCALen:]
	binary.LittleEndian.PutUint32(cca[0:], 0xF3FFFFFF) // enabled, active low, PA3
	binary.LittleEndian.PutUint32(cca[4:], 0)          // image valid
	binary.LittleEndian.PutUint32(cca[8:], CC2538FlashBase)
	s.Regs = map[uint32]uint32{CC2538DieCfg0: 4 << 4}
	// Info page IEEE words as the ROM reports them (MSB first per word).
	s.FCFG = nil
	s.ieee2538 = [8]byte{0x00, 0x12, 0x4B, 0x00, 0x01, 0x02, 0x03, 0x04}
	return s
}

// SetLines implements the modem lines of the simulated board.
func (s *Sim) SetLines(dtr, rts bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dtr, s.rts = dtr, rts
	if !s.Wired {
		return nil
	}
	if s.resetLow() {
		s.mode = simReset
		return nil
	}
	if s.mode == simReset {
		s.boot(s.bslLow())
	}
	return nil
}

func (s *Sim) resetLow() bool {
	if s.Inverted {
		return s.dtr && !s.rts
	}
	return !s.dtr && s.rts
}

func (s *Sim) bslLow() bool {
	if s.Inverted {
		return s.rts && !s.dtr
	}
	return s.dtr && !s.rts
}

// InBootloader reports whether the simulated chip runs its ROM bootloader.
func (s *Sim) InBootloader() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode == simBSL || s.mode == simBSLWaitSync
}

// Reopen models a new port session: nothing changes on the chip.
func (s *Sim) Reopen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = nil
}

func (s *Sim) config() []byte {
	if s.Family == FamilyCC2538 {
		return s.Flash[len(s.Flash)-CC2538CCALen:]
	}
	return s.Flash[len(s.Flash)-CCFGLen:]
}

// boot models the ROM boot decision after reset.
func (s *Sim) boot(pinLow bool) {
	cfg := s.config()
	bd := backdoor(s.Family, cfg)
	validOff := CCFGImageValid
	if s.Family == FamilyCC2538 {
		validOff = cc2538ImageValid
	}
	valid := binary.LittleEndian.Uint32(cfg[validOff:]) == 0
	pin := SimBackdoorPin
	if s.Family == FamilyCC2538 {
		pin = 3
	}
	asserted := pinLow != bd.ActiveHigh
	s.syncs, s.in, s.hostAck, s.dlLeft = 0, nil, false, 0
	switch {
	case !valid:
		s.mode = simBSLWaitSync
		s.Boots = append(s.Boots, "bsl-no-image")
	case bd.BootloaderEnabled && bd.BackdoorEnabled && bd.Pin == pin && asserted:
		s.mode = simBSLWaitSync
		s.Boots = append(s.Boots, "bsl-backdoor")
	default:
		s.mode = simApp
		s.Boots = append(s.Boots, "app")
	}
}

// SetReadTimeout implements Link.
func (s *Sim) SetReadTimeout(time.Duration) error { return nil }

// ResetInput implements Link.
func (s *Sim) ResetInput() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = nil
	return nil
}

// Read implements Link; it returns at once when nothing is pending.
func (s *Sim) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := copy(p, s.out)
	s.out = s.out[n:]
	if n == 0 {
		s.mu.Unlock()
		time.Sleep(time.Millisecond)
		s.mu.Lock()
	}
	return n, nil
}

// Write implements Link.
func (s *Sim) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range p {
		s.feed(b)
	}
	return len(p), nil
}

func (s *Sim) feed(b byte) {
	switch s.mode {
	case simApp, simReset:
		return
	case simBSLWaitSync:
		if b == syncByte {
			s.syncs++
			if s.syncs == 2 {
				s.mode = simBSL
				s.out = append(s.out, 0x00, ackByte)
			}
		} else {
			s.syncs = 0
		}
		return
	}
	if s.hostAck {
		if b == 0x00 {
			return
		}
		s.hostAck = false
		return
	}
	if len(s.in) == 0 && b == 0x00 {
		return
	}
	s.in = append(s.in, b)
	if len(s.in) < 2 || len(s.in) < int(s.in[0]) {
		return
	}
	pkt := s.in
	s.in = nil
	if pkt[0] < 3 {
		s.out = append(s.out, 0x00, nackByte)
		return
	}
	var sum byte
	for _, x := range pkt[2:] {
		sum += x
	}
	if sum != pkt[1] {
		s.out = append(s.out, 0x00, nackByte)
		return
	}
	s.out = append(s.out, 0x00, ackByte)
	s.exec(pkt[2], pkt[3:])
}

func (s *Sim) reply(data []byte) {
	var sum byte
	for _, x := range data {
		sum += x
	}
	s.out = append(s.out, byte(len(data)+2), sum)
	s.out = append(s.out, data...)
	s.hostAck = true
}

func (s *Sim) inFlash(addr uint32, n int) bool {
	return addr >= s.FlashBase && uint64(addr)+uint64(n) <= uint64(s.FlashBase)+uint64(len(s.Flash))
}

// mem returns the bytes at [addr, addr+n) or false.
func (s *Sim) mem(addr uint32, n int) ([]byte, bool) {
	if s.inFlash(addr, n) {
		o := addr - s.FlashBase
		return append([]byte(nil), s.Flash[o:o+uint32(n)]...), true
	}
	if s.FCFG != nil && addr >= FCFG1Base && uint64(addr)+uint64(n) <= FCFG1Base+uint64(len(s.FCFG)) {
		o := addr - FCFG1Base
		return append([]byte(nil), s.FCFG[o:o+uint32(n)]...), true
	}
	if s.Family == FamilyCC2538 && addr >= CC2538IEEE && addr+uint32(n) <= CC2538IEEE+8 {
		// The info page holds each word so that the ROM's MSB-first answer
		// reads 00 12 4B 00 | 01 02 03 04.
		raw := make([]byte, 8)
		copy(raw[0:4], reverse(s.ieee2538[0:4]))
		copy(raw[4:8], reverse(s.ieee2538[4:8]))
		o := addr - CC2538IEEE
		return raw[o : o+uint32(n)], true
	}
	if addr%4 == 0 && n%4 == 0 {
		out := make([]byte, 0, n)
		for a := addr; a < addr+uint32(n); a += 4 {
			v, ok := s.Regs[a]
			if !ok {
				return nil, false
			}
			out = binary.LittleEndian.AppendUint32(out, v)
		}
		return out, true
	}
	return nil, false
}

func (s *Sim) protected(sector int) bool {
	if s.Family != FamilyCC26xx {
		return false
	}
	cfg := s.config()
	return sector < 128 && cfg[CCFGProt+sector/8]&(1<<(sector%8)) == 0
}

func (s *Sim) exec(cmd byte, d []byte) {
	s.Commands[cmd]++
	u32 := func(i int) uint32 { return binary.BigEndian.Uint32(d[i:]) }
	set := func(st byte) {
		if cmd != CmdGetStatus {
			s.status = st
		}
	}
	switch {
	case cmd == CmdPing && len(d) == 0:
		set(StatusSuccess)
	case cmd == CmdGetStatus && len(d) == 0:
		s.reply([]byte{s.status})
	case cmd == CmdGetChipID && len(d) == 0:
		s.reply(binary.BigEndian.AppendUint32(nil, s.ChipIDVal))
		set(StatusSuccess)
	case cmd == CmdReset && len(d) == 0:
		s.Resets++
		s.boot(s.Wired && s.bslLow())
	case cmd == CmdMemoryRead && s.Family == FamilyCC26xx && len(d) == 6:
		addr, typ, cnt := u32(0), d[4], int(d[5])
		n := cnt
		if typ == 1 {
			n = 4 * cnt
		}
		if typ > 1 || cnt == 0 || (typ == 1 && (cnt > MaxReadWords || addr%4 != 0)) || (typ == 0 && cnt > 253) {
			set(StatusInvalidCmd)
			return
		}
		b, ok := s.mem(addr, n)
		if !ok {
			set(StatusInvalidAdr)
			return
		}
		s.reads++
		if s.FlipReadAt > 0 && s.reads == s.FlipReadAt {
			b[0] ^= 1
		}
		s.reply(b)
		set(StatusSuccess)
	case cmd == CmdMemoryRead && s.Family == FamilyCC2538 && len(d) == 5:
		b, ok := s.mem(u32(0), 4)
		if !ok || d[4] != 4 {
			set(StatusInvalidAdr)
			return
		}
		s.reads++
		if s.FlipReadAt > 0 && s.reads == s.FlipReadAt {
			b[0] ^= 1
		}
		s.reply(reverse(b))
		set(StatusSuccess)
	case cmd == CmdCRC32 && ((s.Family == FamilyCC26xx && len(d) == 12) || (s.Family == FamilyCC2538 && len(d) == 8)):
		addr, n := u32(0), int(u32(4))
		b, ok := s.mem(addr, n)
		if !ok {
			set(StatusInvalidAdr)
			return
		}
		s.reply(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(b)))
		set(StatusSuccess)
	case cmd == CmdSectorErase && s.Family == FamilyCC26xx && len(d) == 4:
		addr := u32(0)
		if !s.inFlash(addr, 1) {
			set(StatusInvalidAdr)
			return
		}
		sec := int(addr) / s.Sector
		if s.protected(sec) {
			set(StatusFlashFail)
			return
		}
		fill(s.Flash[sec*s.Sector:(sec+1)*s.Sector], 0xFF)
		s.Erased = append(s.Erased, uint32(sec*s.Sector))
		set(StatusSuccess)
	case cmd == CmdSectorErase && s.Family == FamilyCC2538 && len(d) == 8:
		addr, n := u32(0), int(u32(4))
		if !s.inFlash(addr, n) || n == 0 {
			set(StatusInvalidAdr)
			return
		}
		o := int(addr-s.FlashBase) &^ (s.Sector - 1)
		e := (int(addr-s.FlashBase) + n + s.Sector - 1) &^ (s.Sector - 1)
		fill(s.Flash[o:e], 0xFF)
		for a := o; a < e; a += s.Sector {
			s.Erased = append(s.Erased, s.FlashBase+uint32(a))
		}
		set(StatusSuccess)
	case cmd == CmdBankErase && s.Family == FamilyCC26xx && len(d) == 0:
		if binary.LittleEndian.Uint32(s.config()[CCFGEraseConf:])&1 == 0 {
			set(StatusInvalidCmd)
			return
		}
		fill(s.Flash, 0xFF)
		s.BankErases++
		set(StatusSuccess)
	case cmd == CmdDownload && len(d) == 8:
		addr, n := u32(0), int(u32(4))
		if !s.inFlash(addr, n) || n%4 != 0 || addr%4 != 0 {
			set(StatusInvalidAdr)
			s.dlLeft = 0
			return
		}
		s.dlAddr, s.dlLeft = addr, n
		set(StatusSuccess)
	case cmd == CmdSendData && len(d) > 0:
		if len(d) > s.dlLeft {
			set(StatusInvalidCmd)
			return
		}
		o := s.dlAddr - s.FlashBase
		for i, x := range d {
			if s.Flash[int(o)+i]&x != x {
				set(StatusFlashFail)
				return
			}
			s.Flash[int(o)+i] &= x
		}
		s.dlAddr += uint32(len(d))
		s.dlLeft -= len(d)
		set(StatusSuccess)
	default:
		set(StatusInvalidCmd)
	}
}

func fill(b []byte, v byte) {
	for i := range b {
		b[i] = v
	}
}
