package sws

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Debug registers of the TLSR825x core, in SWire address space. These are the
// addresses the reference tool uses; they are not guessed.
const (
	RegCPUCtl  = 0x602 // CPU control byte
	RegStep    = 0x613 // write 0x80 to execute one instruction (control must be 0x05/0x06)
	RegBkpt    = 0x610 // four dwords: {status (bit31 = stopped), 3 x addr|0x01000000}
	RegPC      = 0x6bc // program counter, dword
	RegCPURegs = 0x680 // 32 dwords: r0..r15, psr, shadow set, m64 at 0x6fc
	RegReboot  = 0x6f  // write 0x20 for a soft MCU reboot
	RegChipID  = 0x7d  // rev, id (2 bytes)

	cpuStop    = 0x05 // holds the core at the reset vector: PC and registers read 0
	cpuStall   = 0x06
	cpuGo      = 0x08
	cpuGoBkpt  = 0x84
	stepAction = 0x80
)

// Registers is a snapshot of the CPU register block.
type Registers struct {
	R      [16]uint32 // r0..r15 (r11 fp, r12 ip, r13 sp, r14 lr, r15 pc)
	PSR    uint32
	Shadow [14]uint32 // dwords 18..31 of the block, reported raw
	Raw    [32]uint32
}

// PC reads the program counter. A target that is asleep does not answer SWire
// reads; that comes back as a programmer error.
func (p *Programmer) PC() (uint32, error) {
	d, err := p.SWRead(RegPC, 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(d), nil
}

// CPUControl reads the CPU control byte.
func (p *Programmer) CPUControl() (byte, error) {
	d, err := p.SWRead(RegCPUCtl, 1)
	if err != nil {
		return 0, err
	}
	return d[0], nil
}

// Stop captures the CPU (control 0x05) and verifies the control byte.
func (p *Programmer) Stop() error { return p.capture(cpuStop) }

// Stall captures the CPU with the stall mode (control 0x06).
func (p *Programmer) Stall() error { return p.capture(cpuStall) }

// captureAttempts bounds the capture writes. Writing the control byte is
// idempotent, and the reference tool likewise keeps writing it until the
// readback shows the CPU captured.
const captureAttempts = 4

func (p *Programmer) capture(mode byte) error {
	var c byte
	for attempt := 0; attempt < captureAttempts; attempt++ {
		if attempt > 0 {
			p.stats.CaptureRetries++
			time.Sleep(time.Duration(attempt) * 5 * time.Millisecond)
		}
		if err := p.SWWrite(RegCPUCtl, []byte{mode}); err != nil {
			if retryable(err) && attempt+1 < captureAttempts {
				continue
			}
			return err
		}
		var err error
		if c, err = p.CPUControl(); err != nil {
			return err
		}
		if c == mode {
			return nil
		}
	}
	return fmt.Errorf("CPU capture failed after %d attempts: [0x%03x] = 0x%02x, want 0x%02x", captureAttempts, RegCPUCtl, c, mode)
}

// Go lets a captured CPU run (control 0x08).
func (p *Programmer) Go() error { return p.SWWrite(RegCPUCtl, []byte{cpuGo}) }

// StepOne executes one instruction. The CPU must have been captured first.
func (p *Programmer) StepOne() error { return p.SWWrite(RegStep, []byte{stepAction}) }

// SoftReboot reboots the MCU through its reset register. The caller should
// Activate again afterwards.
func (p *Programmer) SoftReboot() error { return p.SWWrite(RegReboot, []byte{0x20}) }

// BreakpointSlots is the number of usable hardware breakpoint comparators:
// dwords 1..3 of the breakpoint block. Dword 0 is a status word.
const BreakpointSlots = 3

// bkptSettle is how long the core needs after the breakpoint block is
// rewritten before the comparators are reliable (the reference tool waits the
// same).
const bkptSettle = 2 * time.Millisecond

// bkptStopped is the status-word bit that is set while the core is stopped in
// debug: stalled (0x06) or stopped by a comparator. Go clears it; it is also
// clear while the core is held in reset (0x05). Rewriting the block does not
// clear it. (Observed on TLSR8258 rev 2.)
const bkptStopped = 1 << 31

// bkptArmed marks an armed comparator dword: addr | bkptArmed.
const bkptArmed = 0x01000000

// Breakpoints is the hardware breakpoint block as read from the core.
type Breakpoints struct {
	// Stopped is the status bit: the core is stalled or stopped by a
	// comparator. After a comparator stop the control byte still reads 0x08
	// (go), so this bit is the only evidence of that stop.
	Stopped bool
	Status  uint32
	Armed   [BreakpointSlots]bool
	Addr    [BreakpointSlots]uint32
	Raw     [BreakpointSlots + 1]uint32
}

// Active lists the armed comparator addresses.
func (b Breakpoints) Active() []uint32 {
	var out []uint32
	for i, on := range b.Armed {
		if on {
			out = append(out, b.Addr[i])
		}
	}
	return out
}

// ReadBreakpoints reads the breakpoint block.
func (p *Programmer) ReadBreakpoints() (Breakpoints, error) {
	d, err := p.SWRead(RegBkpt, 4*(BreakpointSlots+1))
	if err != nil {
		return Breakpoints{}, err
	}
	var b Breakpoints
	for i := range b.Raw {
		b.Raw[i] = binary.LittleEndian.Uint32(d[4*i:])
	}
	b.Status = b.Raw[0]
	b.Stopped = b.Status&bkptStopped != 0
	for i := 0; i < BreakpointSlots; i++ {
		v := b.Raw[i+1]
		b.Armed[i] = v&0xff000000 == bkptArmed
		b.Addr[i] = v & 0xffffff
	}
	return b, nil
}

// WriteBreakpoints rewrites the whole breakpoint block with up to
// BreakpointSlots armed addresses. It does not change the CPU control byte or
// the stopped bit. Comparators are live as soon as they are written: a core
// running with plain go (0x08) stops at them too.
func (p *Programmer) WriteBreakpoints(addrs []uint32) error {
	if len(addrs) > BreakpointSlots {
		return fmt.Errorf("%d breakpoints: the core has %d comparators", len(addrs), BreakpointSlots)
	}
	var b [4 * (BreakpointSlots + 1)]byte
	for i, a := range addrs {
		binary.LittleEndian.PutUint32(b[4*(i+1):], a&0xffffff|bkptArmed)
	}
	if err := p.SWWrite(RegBkpt, b[:]); err != nil {
		return err
	}
	time.Sleep(bkptSettle)
	return nil
}

// GoBreakpoints lets the CPU run with the comparators enabled (control 0x84).
func (p *Programmer) GoBreakpoints() error { return p.SWWrite(RegCPUCtl, []byte{cpuGoBkpt}) }

// SetBreakpoint arms a single hardware breakpoint at addr (replacing the
// others) and lets the CPU run (control 0x84). Use WaitHit to wait for it.
func (p *Programmer) SetBreakpoint(addr uint32) error {
	if err := p.WriteBreakpoints([]uint32{addr}); err != nil {
		return err
	}
	return p.GoBreakpoints()
}

// ClearBreakpoint disarms every comparator without moving the CPU state.
func (p *Programmer) ClearBreakpoint() error { return p.WriteBreakpoints(nil) }

// WaitHit polls the stopped bit of a running core until a comparator stopped
// it or timeout expires, and returns the PC at that moment.
func (p *Programmer) WaitHit(timeout time.Duration) (uint32, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		b, err := p.ReadBreakpoints()
		if err != nil {
			return 0, false, err
		}
		if b.Stopped {
			pc, err := p.PC()
			return pc, true, err
		}
		if time.Now().After(deadline) {
			pc, err := p.PC()
			return pc, false, err
		}
		time.Sleep(time.Millisecond)
	}
}

// ReadRegisters reads the CPU register block. The CPU should be captured, or
// the values are an arbitrary instant of a running core.
func (p *Programmer) ReadRegisters() (Registers, error) {
	d, err := p.SWRead(RegCPURegs, 128)
	if err != nil {
		return Registers{}, err
	}
	var r Registers
	for i := range r.Raw {
		r.Raw[i] = binary.LittleEndian.Uint32(d[4*i:])
	}
	copy(r.R[:], r.Raw[:16])
	r.PSR = r.Raw[16]
	copy(r.Shadow[:], r.Raw[18:])
	return r, nil
}

// ErrNotCaptured is returned by guarded operations that need a halted CPU.
var ErrNotCaptured = errors.New("the CPU is running; halt it first")
