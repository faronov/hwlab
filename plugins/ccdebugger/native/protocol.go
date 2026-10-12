// Package native drives a TI CC-Debugger directly over USB, without cc-tool
// or libusb, and speaks the CC2530 debug interface through it.
//
// It is a Go port of the BSD-3-Clause, hardware-tested reference implementation
// in the cc2530-zigbee project (tools/cc_debugger.py). The target-side debug
// commands come from TI's SWRU191 (CC253x debug interface); the adapter-side
// USB framing is what that reference observed on a real CC-Debugger. Nothing
// here is derived from the GPL cc-tool sources.
//
// The safety model of the reference is preserved: an [Access] policy plus
// independent permissions for CPU control, target reset, memory access,
// memory write, breakpoints and DMA enable. Every state-changing or
// target-touching operation is refused unless its permission was granted at
// construction time, and a failed exchange faults the session (no automatic
// retry, reconnect or reset).
package native

import (
	"errors"
	"fmt"
	"time"
)

// USB identity of the CC-Debugger and its bulk endpoints.
const (
	VID         = 0x0451
	PID         = 0x16A2
	EndpointOut = 0x04
	EndpointIn  = 0x84
)

// CC2530 debug commands (SWRU191F).
const (
	cmdWriteConfig   = 0x1D
	cmdReadConfig    = 0x24
	cmdGetPC         = 0x28
	cmdReadStatus    = 0x34
	cmdSetHWBreak    = 0x3F
	cmdHalt          = 0x44
	cmdResume        = 0x4C
	cmdDebugInstr    = 0x54
	cmdStepInstr     = 0x5C
	cmdGetBank       = 0x64
	targetCC2530     = 0x2530
	resetStatus      = 0x22 // oscillator stable + CPU halted, as after a debug reset
	resetConfig      = 0x26 // soft power mode, DMA paused, timer suspend
	dmaEnabledConfig = 0x22
)

// readableCoreSFRs are the only SFRs ReadSFR accepts; peripheral registers can
// have read side effects.
var readableCoreSFRs = map[uint8]bool{
	0x81: true, 0x82: true, 0x83: true, 0x84: true, 0x85: true, 0x92: true,
	0x93: true, 0x9F: true, 0xD0: true, 0xE0: true, 0xF0: true,
}

// Status is the CC2530 debug status byte.
type Status uint8

// Debug status bits.
const (
	StackOverflow    Status = 0x01
	OscillatorStable Status = 0x02
	DebugLocked      Status = 0x04
	HaltStatus       Status = 0x08
	PMActive         Status = 0x10
	CPUHalted        Status = 0x20
	PconIdle         Status = 0x40
	ChipEraseBusy    Status = 0x80
)

// Decode returns the named flags of the status byte.
func (s Status) Decode() map[string]any {
	return map[string]any{
		"raw": float64(s), "stack_overflow": s&StackOverflow != 0, "oscillator_stable": s&OscillatorStable != 0,
		"debug_locked": s&DebugLocked != 0, "halt_status": s&HaltStatus != 0, "pm_active": s&PMActive != 0,
		"cpu_halted": s&CPUHalted != 0, "pcon_idle": s&PconIdle != 0, "chip_erase_busy": s&ChipEraseBusy != 0,
	}
}

// DecodeConfig names the debug configuration byte.
func DecodeConfig(v uint8) map[string]any {
	return map[string]any{
		"raw": float64(v), "soft_power_mode": v&0x20 != 0, "timers_off": v&0x08 != 0,
		"dma_pause": v&0x04 != 0, "timer_suspend": v&0x02 != 0, "reserved_bits": float64(v & 0xD1),
	}
}

// Error classes. Wrap checks with errors.Is.
var (
	// ErrPolicy: refused by the access/permission model before any USB traffic.
	ErrPolicy = errors.New("cc-debugger policy")
	// ErrTransport: a USB or protocol failure; the session is faulted.
	ErrTransport = errors.New("cc-debugger transport")
	// ErrInvalid: a bad argument.
	ErrInvalid = errors.New("cc-debugger argument")
)

func policyf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrPolicy, fmt.Sprintf(format, a...))
}
func transportf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrTransport, fmt.Sprintf(format, a...))
}
func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Access is the session policy.
type Access string

const (
	// AdapterOnly talks to the adapter itself, never to the target.
	AdapterOnly Access = "adapter-only"
	// ExistingDebugSession asserts that a debug session with the target
	// already exists (a halted, reset-attached target); nothing is reset.
	ExistingDebugSession Access = "existing-debug-session"
	// ResetDebugSession lets this session attach by resetting the target into
	// debug halt. The target's running firmware is interrupted.
	ResetDebugSession Access = "reset-debug-session"
)

// ParseAccess validates an access policy name.
func ParseAccess(s string) (Access, error) {
	switch a := Access(s); a {
	case AdapterOnly, ExistingDebugSession, ResetDebugSession:
		return a, nil
	case "":
		return AdapterOnly, nil
	}
	return "", invalidf("access %q: want adapter-only, existing-debug-session or reset-debug-session", s)
}

// Permissions are the independent grants of a session.
type Permissions struct {
	CPUControl   bool // halt, resume, step
	TargetReset  bool // reset into debug halt; required for ResetDebugSession
	MemoryAccess bool // register-preserving reads of core SFRs, RAM and CODE
	MemoryWrite  bool // verified XDATA writes below 0x1E00; never flash or MMIO
	Breakpoints  bool // the four unbanked CODE hardware breakpoints
	DMAEnable    bool // clear DMA_PAUSE once, right after this session's reset

	// Flash and configuration tiers. Each is dangerous and independent.
	FlashErase   bool // erase individual flash pages (needs MemoryAccess+MemoryWrite)
	FlashProgram bool // erase+program+verify flash images (needs FlashErase)
	ChipErase    bool // erase the whole chip, destroying firmware, lock bits and secondary IEEE
	ConfigWrite  bool // rewrite the lock-bit page: lock bits and secondary IEEE (needs FlashProgram)
}

// Config configures a [Debugger].
type Config struct {
	Access      Access
	Permissions Permissions
	// Timeout bounds one whole operation (several USB exchanges); 1 ms..60 s.
	Timeout time.Duration
	// Now is the monotonic clock; defaults to time.Now.
	Now func() time.Time
	// Sleep pauses between status polls; defaults to time.Sleep.
	Sleep func(time.Duration)
}

// Validate checks the policy and permission combination without any USB access.
func (c Config) Validate() error {
	if c.Timeout < time.Millisecond || c.Timeout > 60*time.Second {
		return invalidf("timeout must be between 1ms and 60s")
	}
	switch c.Access {
	case AdapterOnly, ExistingDebugSession, ResetDebugSession:
	default:
		return invalidf("unknown access policy %q", c.Access)
	}
	p := c.Permissions
	grants := map[string]bool{
		"cpu control": p.CPUControl, "target reset": p.TargetReset, "memory access": p.MemoryAccess,
		"memory write": p.MemoryWrite, "breakpoints": p.Breakpoints, "dma enable": p.DMAEnable,
		"flash erase": p.FlashErase, "flash program": p.FlashProgram, "chip erase": p.ChipErase, "config write": p.ConfigWrite,
	}
	if c.Access == AdapterOnly {
		for name, on := range grants {
			if on {
				return policyf("%s requires an explicit debug-session policy (existing-debug-session or reset-debug-session)", name)
			}
		}
	}
	if c.Access == ResetDebugSession && !p.TargetReset {
		return policyf("reset-debug-session requires separate target-reset permission")
	}
	if p.MemoryWrite && !p.MemoryAccess {
		return policyf("memory write requires separate memory-access permission")
	}
	if p.DMAEnable && !p.TargetReset {
		return policyf("dma enable requires separate target-reset permission")
	}
	if (p.FlashErase || p.FlashProgram) && (!p.MemoryAccess || !p.MemoryWrite) {
		return policyf("flash erase/program stage data in RAM and require memory-access and memory-write permissions")
	}
	if p.FlashProgram && !p.FlashErase {
		return policyf("flash program erases pages first and requires separate flash-erase permission")
	}
	if p.ChipErase && !p.FlashErase {
		return policyf("chip erase requires flash-erase permission")
	}
	if p.ConfigWrite && !p.FlashProgram {
		return policyf("config write requires flash-program permission")
	}
	return nil
}

// Results.

// AdapterState is the adapter's self-report.
type AdapterState struct {
	TargetID        uint16 `json:"target_id"`
	FirmwareVersion uint16 `json:"firmware_version"`
	FirmwareRev     uint16 `json:"firmware_revision"`
}

// CPUControlResult reports a halt/resume/reset.
type CPUControlResult struct {
	StatusBefore Status
	StatusAfter  Status
	CommandSent  bool
}

// StepResult reports one single-stepped instruction.
type StepResult struct {
	StatusBefore Status
	StatusAfter  Status
	Accumulator  uint8
}

// AttachResult reports a reset-attach.
type AttachResult struct {
	StatusAfter Status
	ResetSent   bool
}

// Registers is a CPU snapshot.
type Registers struct {
	PC    uint16
	Bank  uint8
	A     uint8
	PSW   uint8
	B     uint8
	SP    uint8
	DPTR0 uint16
	DPTR1 uint16
	DPS   uint8
	MPAGE uint8
	R     [8]uint8
}

// BreakpointResult reports a configured hardware breakpoint.
type BreakpointResult struct {
	Slot        int
	Address     uint16
	Enabled     bool
	StatusAfter Status
}
