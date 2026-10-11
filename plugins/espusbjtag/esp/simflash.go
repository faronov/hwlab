package esp

import (
	"encoding/binary"
	"sync"

	"github.com/faronov/hwlab/internal/riscv/rvsim"
)

// SimFlash is a SYNTHETIC SPI1 controller plus NOR flash for tests: it
// implements the user commands and the hardware program/erase commands the
// driver uses, with NOR semantics (programming only clears bits).
type SimFlash struct {
	mu   sync.Mutex
	Mem  []byte
	ID   [3]byte
	regs [0x100 / 4]uint32
	sr   byte
	busy int // status polls that still read WIP

	// Fault injection.
	CorruptProgram bool // program flips bit 0 of the first byte of each chunk
	Protected      bool // write-enable never sets the latch
	StuckBusy      bool // WIP never clears
	Erases         int
	Programs       int
	Reads          int
}

// NewSimFlash returns an erased flash of size bytes with JEDEC ef4016-style id
// derived from the size.
func NewSimFlash(size int) *SimFlash {
	f := &SimFlash{Mem: make([]byte, size)}
	for i := range f.Mem {
		f.Mem[i] = 0xFF
	}
	cap := byte(0)
	for 1<<cap < size {
		cap++
	}
	f.ID = [3]byte{0xEF, 0x40, cap}
	return f
}

func (f *SimFlash) sfdp() []byte {
	b := make([]byte, 0x40)
	copy(b, "SFDP")
	b[4], b[5], b[6], b[7] = 6, 1, 0, 0xFF
	b[8], b[9], b[10], b[11] = 0x00, 6, 1, 9
	b[12], b[13], b[14], b[15] = 0x30, 0, 0, 0xFF
	binary.LittleEndian.PutUint32(b[0x34:], uint32(len(f.Mem)*8-1))
	return b
}

// Device returns the register window to attach to an rvsim target.
func (f *SimFlash) Device() rvsim.Device {
	return rvsim.Device{Base: SPI1Base, Size: 0x100, Read: f.read, Write: f.write}
}

func (f *SimFlash) read(off uint32) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.regs[off/4%uint32(len(f.regs))]
}

func (f *SimFlash) write(off, v uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := off / 4 % uint32(len(f.regs))
	if i == spiCmd/4 {
		f.regs[i] = 0
		f.exec(v)
		return
	}
	f.regs[i] = v
}

func (f *SimFlash) status() byte {
	s := f.sr
	if f.StuckBusy {
		return s | 1
	}
	if f.busy > 0 {
		f.busy--
		s |= 1
	}
	return s
}

func (f *SimFlash) w(i int) []byte {
	var b []byte
	for k := 0; k < spiBufWords; k++ {
		b = append(b, byte(f.regs[spiW0/4+k]), byte(f.regs[spiW0/4+k]>>8), byte(f.regs[spiW0/4+k]>>16), byte(f.regs[spiW0/4+k]>>24))
	}
	return b[:i]
}

func (f *SimFlash) putW(b []byte) {
	for k := 0; k < spiBufWords; k++ {
		var v uint32
		for j := 0; j < 4 && k*4+j < len(b); j++ {
			v |= uint32(b[k*4+j]) << (8 * j)
		}
		if k*4 < len(b) {
			f.regs[spiW0/4+k] = v
		}
	}
}

func (f *SimFlash) exec(cmd uint32) {
	addr := f.regs[spiAddr/4]
	switch {
	case cmd&cmdUSR != 0:
		user, user2 := f.regs[spiUser/4], f.regs[0x20/4]
		op := byte(user2)
		n := int(f.regs[spiMISOLen/4]+1) / 8
		switch op {
		case 0x9F:
			f.putW(f.ID[:])
		case 0x05:
			f.putW([]byte{f.status()})
		case 0x06:
			if !f.Protected {
				f.sr |= 2
			}
		case 0x0B:
			a := int(addr & 0xFFFFFF)
			out := make([]byte, n)
			for i := range out {
				out[i] = 0xFF
				if a+i < len(f.Mem) {
					out[i] = f.Mem[a+i]
				}
			}
			f.Reads++
			f.putW(out)
		case 0x02:
			f.program(cmd, addr, f.regs[spiMOSILen/4])
		case 0x20, 0xD8, 0xC7:
			f.erase(cmd, op, addr)
		case 0x5A:
			s := f.sfdp()
			a := int(addr & 0xFFFFFF)
			out := make([]byte, n)
			for i := range out {
				if a+i < len(s) {
					out[i] = s[a+i]
				} else {
					out[i] = 0xFF
				}
			}
			f.putW(out)
		}
		_ = user
	}
}

// program models a user-mode page program. Like the real controller it only
// acts when the PE (program/erase) flag accompanies the user command.
func (f *SimFlash) program(cmd, addr, mosiLen uint32) {
	n := int(mosiLen+1) / 8
	a := int(addr & 0xFFFFFF)
	if cmd&cmdPE == 0 || f.sr&2 == 0 || n == 0 || n > MaxChunk || a%PageSize+n > PageSize || a+n > len(f.Mem) {
		return
	}
	data := f.w(n)
	for i, v := range data {
		f.Mem[a+i] &= v
	}
	if f.CorruptProgram {
		f.Mem[a] ^= 1
	}
	f.sr &^= 2
	f.busy = 1
	f.Programs++
}

func (f *SimFlash) erase(cmd uint32, op byte, addr uint32) {
	if cmd&cmdPE == 0 || f.sr&2 == 0 {
		return
	}
	size, a := SectorSize, int(addr&0xFFFFFF)
	switch op {
	case 0xD8:
		size = BlockSize
	case 0xC7:
		size, a = len(f.Mem), 0
	}
	a &^= size - 1
	for i := a; i < a+size && i < len(f.Mem); i++ {
		f.Mem[i] = 0xFF
	}
	f.sr &^= 2
	f.busy = 2
	f.Erases++
}
