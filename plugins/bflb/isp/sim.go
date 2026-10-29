package isp

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"hash/crc32"
	"sync"
	"time"
)

// Sim is a SYNTHETIC BL702 in ISP mode: boot ROM, eflash_loader helper and a
// NOR flash with erase-before-write semantics. It models the behaviours the
// client depends on (handshake only in handshake state, non-0x55 bytes
// ignored there, 0x55 bytes in command mode parsed as a frame, the helper
// returning to handshake state when the port is reopened). It is a model,
// not a validation of the protocol on silicon.
type Sim struct {
	mu sync.Mutex

	Flash []byte
	MAC   [8]byte
	JEDEC [3]byte
	// SignRequired makes the ROM report a signed-image requirement.
	SignRequired bool
	// ErasePending is the number of PD answers before an erase completes.
	ErasePending int
	// Resets counts helper reset commands.
	Resets int
	// USBPacketLoss models the helper losing host writes of 64 bytes or
	// more over USB (true by default: the simulator is the USB device).
	USBPacketLoss bool
	// Lost counts host writes the helper dropped.
	Lost int

	helper   bool // the helper is running
	command  bool // in command mode (else waiting for a handshake)
	rx       []byte
	out      bytes.Buffer
	header   []byte
	segs     int
	segLeft  int
	loaded   int
	checked  bool
	hbnMagic bool
	entry    uint32
	// syncTail swallows the rest of the 0x55 burst that ended a handshake.
	syncTail bool
}

// NewSim returns a chip with a 1 MiB Macronix flash, erased.
func NewSim() *Sim {
	s := &Sim{Flash: bytes.Repeat([]byte{0xff}, 1<<20), JEDEC: [3]byte{0xc2, 0x20, 0x14}, ErasePending: 2, USBPacketLoss: true}
	copy(s.MAC[:], []byte{0x00, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66})
	return s
}

// HelperRunning reports whether the helper is running.
func (s *Sim) HelperRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.helper
}

// Reopen models closing and reopening the USB CDC port: the chip returns to
// handshake state (the helper stays resident).
func (s *Sim) Reopen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.command, s.rx = false, nil
	s.out.Reset()
}

// PowerCycle models a replug with BOOT held: ROM, nothing loaded.
func (s *Sim) PowerCycle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.helper, s.command, s.rx = false, false, nil
	s.out.Reset()
	s.header, s.segs, s.segLeft, s.loaded, s.checked, s.hbnMagic, s.entry = nil, 0, 0, 0, false, false, 0
}

// SetReadTimeout implements Link.
func (s *Sim) SetReadTimeout(time.Duration) error { return nil }

// ResetInput implements Link.
func (s *Sim) ResetInput() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.Reset()
	return nil
}

// Read implements Link; with nothing to send it times out at once.
func (s *Sim) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out.Len() == 0 {
		s.mu.Unlock()
		time.Sleep(time.Millisecond) // a real read timeout; avoids spinning
		s.mu.Lock()
		return 0, nil
	}
	return s.out.Read(p)
}

// Write implements Link.
func (s *Sim) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.helper && s.USBPacketLoss && len(p) >= 64 {
		s.Lost++
		return len(p), nil
	}
	for _, b := range p {
		if !s.command {
			if b == 0x55 {
				s.command = true
				s.out.WriteString("OK")
				s.syncTail = true
			}
			continue
		}
		if s.syncTail && b == 0x55 {
			continue // the rest of the same handshake burst
		}
		s.syncTail = false
		s.rx = append(s.rx, b)
		s.step()
	}
	s.syncTail = false
	return len(p), nil
}

func (s *Sim) ok() { s.out.WriteString("OK") }
func (s *Sim) fail(code uint16) {
	s.out.WriteString("FL")
	_ = binary.Write(&s.out, binary.LittleEndian, code)
}
func (s *Sim) okData(d []byte) {
	s.ok()
	_ = binary.Write(&s.out, binary.LittleEndian, uint16(len(d)))
	s.out.Write(d)
}

// step executes the frame in rx once it is complete.
func (s *Sim) step() {
	if len(s.rx) < 4 {
		return
	}
	n := int(binary.LittleEndian.Uint16(s.rx[2:]))
	if len(s.rx) < 4+n {
		return
	}
	cmd, ck, data := s.rx[0], s.rx[1], append([]byte(nil), s.rx[4:4+n]...)
	s.rx = s.rx[4+n:]
	if s.helper {
		sum := n&0xff + n>>8
		for _, b := range data {
			sum += int(b)
		}
		if byte(sum) != ck {
			s.fail(0x0103)
			return
		}
		s.helperCmd(cmd, data)
		return
	}
	s.romCmd(cmd, data)
}

func (s *Sim) romCmd(cmd byte, d []byte) {
	switch cmd {
	case cmdGetBootInfo:
		bi := make([]byte, 24)
		binary.LittleEndian.PutUint32(bi, 0x00000001)
		if s.SignRequired {
			bi[4] = 1
		}
		copy(bi[16:], []byte{0x66, 0x55, 0x44, 0x33, 0x22, 0x11, 0x00, 0x00})
		s.okData(bi)
	case cmdLoadBootHeader:
		if len(d) != bootHeaderLen {
			s.fail(0x0201)
			return
		}
		if string(d[:4]) != "BFNP" {
			s.fail(0x0203)
			return
		}
		s.header, s.segs, s.loaded, s.checked = d, int(binary.LittleEndian.Uint32(d[segCountOff:])), 0, false
		s.ok()
	case cmdLoadSegHeader:
		if s.header == nil {
			s.fail(0x0202)
			return
		}
		if len(d) != segHeaderLen || crc32.ChecksumIEEE(d[:12]) != binary.LittleEndian.Uint32(d[12:]) {
			s.fail(0x0210)
			return
		}
		s.segLeft = int(binary.LittleEndian.Uint32(d[4:]))
		s.okData(d)
	case cmdLoadSegData:
		if len(d) > SegChunk || len(d) > s.segLeft {
			s.fail(0x0212)
			return
		}
		s.segLeft -= len(d)
		if s.segLeft == 0 {
			s.loaded++
		}
		s.ok()
	case cmdCheckImage:
		if s.header == nil || s.loaded != s.segs || s.segLeft != 0 {
			s.fail(0x0216)
			return
		}
		s.checked = true
		s.ok()
	case cmdMemWrite:
		if len(d) != 8 {
			s.fail(0x0501)
			return
		}
		addr, val := binary.LittleEndian.Uint32(d), binary.LittleEndian.Uint32(d[4:])
		s.ok()
		switch {
		case addr == 0x4000f100:
			s.hbnMagic = val == 0x4e424845
		case addr == 0x4000f104:
			s.entry = val
		case addr == 0x40000018 && val == 2:
			if s.checked && s.hbnMagic && s.entry == HelperEntry {
				s.helper, s.command, s.rx = true, false, nil
			}
		}
	default:
		s.fail(0x0101)
	}
}

func (s *Sim) helperCmd(cmd byte, d []byte) {
	u32 := func(i int) uint32 { return binary.LittleEndian.Uint32(d[i:]) }
	size := uint32(len(s.Flash))
	switch cmd {
	case cmdGetBootInfo:
		bi := bytes.Repeat([]byte{0xff}, 4)
		s.okData(append(bi, make([]byte, 20)...))
	case cmdReadMAC:
		out := append([]byte(nil), s.MAC[:]...)
		s.okData(binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out)))
	case cmdReadJEDEC:
		s.okData(append(s.JEDEC[:], 0x80))
	case cmdRead:
		if len(d) != 8 || u32(4) > IOChunk || u32(0)+u32(4) > size {
			s.fail(0x0504)
			return
		}
		s.okData(s.Flash[u32(0) : u32(0)+u32(4)])
	case cmdErase:
		if len(d) != 8 || u32(0) > u32(4) || u32(4) >= size {
			s.fail(0x0002)
			return
		}
		for a := u32(0) &^ (Sector - 1); a <= u32(4); a += Sector {
			copy(s.Flash[a:a+Sector], bytes.Repeat([]byte{0xff}, Sector))
		}
		for i := 0; i < s.ErasePending; i++ {
			s.out.WriteString("PD")
		}
		s.ok()
	case cmdWrite:
		if len(d) < 5 || len(d)-4 > IOChunk || u32(0)+uint32(len(d)-4) > size {
			s.fail(0x0004)
			return
		}
		a := u32(0)
		for i, b := range d[4:] {
			s.Flash[a+uint32(i)] &= b // NOR: programming only clears bits
		}
		s.ok()
	case cmdWriteCheck:
		s.ok()
	case cmdReadSHA:
		if len(d) != 8 || u32(0)+u32(4) > size {
			s.fail(0x0504)
			return
		}
		h := sha256.Sum256(s.Flash[u32(0) : u32(0)+u32(4)])
		s.okData(h[:])
	case cmdReset:
		s.ok()
		s.Resets++
		s.helper, s.command, s.rx = false, false, nil
	default:
		s.fail(0x0101)
	}
}

// SimHelperImage is a SYNTHETIC RAM helper image the Sim accepts (it holds
// no code); real chips need the vendor eflash_loader.
func SimHelperImage() []byte {
	h := make([]byte, bootHeaderLen)
	copy(h, "BFNP")
	binary.LittleEndian.PutUint32(h[segCountOff:], 1)
	seg := make([]byte, segHeaderLen)
	binary.LittleEndian.PutUint32(seg, HelperEntry)
	binary.LittleEndian.PutUint32(seg[4:], 4096)
	binary.LittleEndian.PutUint32(seg[12:], crc32.ChecksumIEEE(seg[:12]))
	return append(append(h, seg...), make([]byte, 4096)...)
}
