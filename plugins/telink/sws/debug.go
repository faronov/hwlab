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
	RegStep    = 0x613 // write 0x80 to execute one instruction (CPU stalled)
	RegBkpt    = 0x610 // four dwords: {0, addr|0x01000000, 0, 0}
	RegPC      = 0x6bc // program counter, dword
	RegCPURegs = 0x680 // 32 dwords: r0..r15, psr, shadow set, m64 at 0x6fc
	RegReboot  = 0x6f  // write 0x20 for a soft MCU reboot
	RegChipID  = 0x7d  // rev, id (2 bytes)

	cpuStop    = 0x05
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

// SetBreakpoint arms the single hardware breakpoint at addr and lets the CPU
// run (control 0x84). Use WaitPC to wait for it.
func (p *Programmer) SetBreakpoint(addr uint32) error {
	var b [16]byte
	binary.LittleEndian.PutUint32(b[4:], addr&0xffffff|0x01000000)
	if err := p.SWWrite(RegBkpt, b[:]); err != nil {
		return err
	}
	return p.SWWrite(RegCPUCtl, []byte{cpuGoBkpt})
}

// ClearBreakpoint disarms the breakpoint without moving the CPU state.
func (p *Programmer) ClearBreakpoint() error {
	return p.SWWrite(RegBkpt, make([]byte, 16))
}

// WaitPC polls the program counter until it equals want or timeout expires.
func (p *Programmer) WaitPC(want uint32, timeout time.Duration) (uint32, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		pc, err := p.PC()
		if err != nil {
			return 0, false, err
		}
		if pc == want&0xffffff {
			return pc, true, nil
		}
		if time.Now().After(deadline) {
			return pc, false, nil
		}
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
