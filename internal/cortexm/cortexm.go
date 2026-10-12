// Package cortexm drives the architectural debug registers of an Arm Cortex-M
// core (ARMv6-M, ARMv7-M, ARMv8-M) through any memory bus that reaches the
// System Control Space, such as the MEM-AP behind an SWD or JTAG debug port.
//
// It knows nothing about the probe and nothing about the vendor of the chip:
// DHCSR/DCRSR/DCRDR/DEMCR/DFSR/AIRCR, the Flash Patch and Breakpoint unit and
// the Data Watchpoint and Trace unit are defined by the Arm architecture.
// Vendor flash controllers and chip identification live elsewhere.
package cortexm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Bus is 32-bit access to the target address space. Addresses are word
// aligned.
type Bus interface {
	Read32(ctx context.Context, addr uint32) (uint32, error)
	Write32(ctx context.Context, addr, v uint32) error
}

// System Control Space registers.
const (
	AddrCPUID = 0xE000ED00
	AddrVTOR  = 0xE000ED08
	AddrAIRCR = 0xE000ED0C
	AddrDFSR  = 0xE000ED30
	AddrDHCSR = 0xE000EDF0
	AddrDCRSR = 0xE000EDF4
	AddrDCRDR = 0xE000EDF8
	AddrDEMCR = 0xE000EDFC
)

// DHCSR bits.
const (
	dhcsrKey      = 0xA05F << 16
	DHCSRDebugEn  = 1 << 0
	DHCSRHalt     = 1 << 1
	DHCSRStep     = 1 << 2
	DHCSRMaskInts = 1 << 3
	DHCSRRegRdy   = 1 << 16
	DHCSRSHalt    = 1 << 17
	DHCSRSSleep   = 1 << 18
	DHCSRSLockup  = 1 << 19
	DHCSRRetire   = 1 << 24
	DHCSRResetSt  = 1 << 25
)

// DEMCR and DFSR bits.
const (
	DEMCRVCCoreReset = 1 << 0
	DEMCRTraceEn     = 1 << 24
	DFSRHalted       = 1 << 0
	DFSRBkpt         = 1 << 1
	DFSRDWTTrap      = 1 << 2
	DFSRVCatch       = 1 << 3
	DFSRExternal     = 1 << 4
)

const aircrKey = 0x05FA << 16

// Errors.
var (
	ErrNotHalted = errors.New("the core is not halted")
	ErrTimeout   = errors.New("the core did not reach the requested state in time")
	ErrVerify    = errors.New("read-back differs from what was written")
	// ErrAbsent marks a debug resource the attached core does not implement
	// (no breakpoint unit, no watchpoint comparators, no VECTRESET). It is a
	// property of the hardware, never a missing feature of this package.
	ErrAbsent = errors.New("not implemented by the attached core")
)

// RegisterID selects a core register (DCRSR REGSEL).
type RegisterID uint32

// Core register numbers (Arm DCRSR.REGSEL).
const (
	RegSP      RegisterID = 13
	RegLR      RegisterID = 14
	RegPC      RegisterID = 15
	RegXPSR    RegisterID = 16
	RegMSP     RegisterID = 17
	RegPSP     RegisterID = 18
	RegSpecial RegisterID = 20 // CONTROL[31:24] FAULTMASK[23:16] BASEPRI[15:8] PRIMASK[7:0]
)

// RegisterName is the conventional name of a core register.
func RegisterName(id RegisterID) string {
	switch {
	case id <= 12:
		return fmt.Sprintf("r%d", id)
	case id == RegSP:
		return "sp"
	case id == RegLR:
		return "lr"
	case id == RegPC:
		return "pc"
	case id == RegXPSR:
		return "xpsr"
	case id == RegMSP:
		return "msp"
	case id == RegPSP:
		return "psp"
	case id == RegSpecial:
		return "special"
	}
	return fmt.Sprintf("reg%d", id)
}

// ParseRegister maps a name ("r3", "pc", "msp", ...) to its number.
func ParseRegister(name string) (RegisterID, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for id := RegisterID(0); id <= 20; id++ {
		if n := RegisterName(id); n == name && !strings.HasPrefix(n, "reg") {
			return id, true
		}
	}
	return 0, false
}

// NativeRegs is implemented by buses whose probe can read or write core
// registers with one command (ST-Link READREG/READALLREGS). The generic
// DCRSR/DCRDR path is used for everything else and as a cross-check.
type NativeRegs interface {
	// ReadCoreRegs returns r0-r12, sp, lr, pc, xpsr, msp, psp in that order.
	ReadCoreRegs(ctx context.Context) ([]uint32, error)
	ReadCoreReg(ctx context.Context, id RegisterID) (uint32, error)
	WriteCoreReg(ctx context.Context, id RegisterID, v uint32) error
}

// Core is one Cortex-M core behind a Bus.
type Core struct {
	Bus Bus
	dwt *DWT
	fpb *FPB
	// Timeout bounds every wait for a core state change.
	Timeout time.Duration
}

// New returns a core with a 2 s state-change timeout.
func New(b Bus) *Core { return &Core{Bus: b, Timeout: 2 * time.Second} }

// State is a decoded DHCSR.
type State struct {
	DHCSR        uint32
	Halted       bool
	Sleeping     bool
	Lockup       bool
	ResetSeen    bool // S_RESET_ST, sticky until read
	DebugEnabled bool
	Stepping     bool
	MaskInts     bool
}

func decodeDHCSR(v uint32) State {
	return State{
		DHCSR: v, Halted: v&DHCSRSHalt != 0, Sleeping: v&DHCSRSSleep != 0, Lockup: v&DHCSRSLockup != 0,
		ResetSeen: v&DHCSRResetSt != 0, DebugEnabled: v&DHCSRDebugEn != 0, Stepping: v&DHCSRStep != 0, MaskInts: v&DHCSRMaskInts != 0,
	}
}

// Running reports whether the core executes instructions.
func (s State) Running() bool { return !s.Halted }

// Status reads DHCSR. Reading it clears S_RESET_ST.
func (c *Core) Status(ctx context.Context) (State, error) {
	v, err := c.Bus.Read32(ctx, AddrDHCSR)
	if err != nil {
		return State{}, err
	}
	return decodeDHCSR(v), nil
}

func (c *Core) writeDHCSR(ctx context.Context, bits uint32) error {
	return c.Bus.Write32(ctx, AddrDHCSR, dhcsrKey|bits)
}

func (c *Core) wait(ctx context.Context, what string, ok func(State) bool) (State, error) {
	deadline := time.Now().Add(c.Timeout)
	for {
		st, err := c.Status(ctx)
		if err != nil {
			return st, err
		}
		if ok(st) {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("%w: %s (DHCSR 0x%08x)", ErrTimeout, what, st.DHCSR)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Halt enables debug, requests a halt and waits until S_HALT confirms it.
func (c *Core) Halt(ctx context.Context) (State, error) {
	if err := c.writeDHCSR(ctx, DHCSRDebugEn|DHCSRHalt); err != nil {
		return State{}, err
	}
	return c.wait(ctx, "halt", func(s State) bool { return s.Halted })
}

// Resume releases a halted core. The core keeps debug enabled so that
// breakpoints still work; use Detach to drop it.
func (c *Core) Resume(ctx context.Context) (State, error) {
	st, err := c.Status(ctx)
	if err != nil {
		return st, err
	}
	if !st.Halted {
		return st, nil
	}
	if err := c.writeDHCSR(ctx, DHCSRDebugEn); err != nil {
		return st, err
	}
	// A breakpoint on the very next instruction may halt the core again; the
	// caller sees that in the returned state, never a false "running".
	return c.Status(ctx)
}

// RunMasked releases a halted core with interrupts masked (C_MASKINTS), so no
// exception handler of the firmware can run while a helper routine executes.
// The mask is dropped by the next Halt.
func (c *Core) RunMasked(ctx context.Context) error {
	return c.writeDHCSR(ctx, DHCSRDebugEn|DHCSRMaskInts)
}

// WaitHalted polls until the core halts or the timeout expires.
func (c *Core) WaitHalted(ctx context.Context, timeout time.Duration) (State, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := c.Status(ctx)
		if err != nil {
			return st, err
		}
		if st.Halted {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("%w: the core did not halt within %s", ErrTimeout, timeout)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Detach turns debug off (C_DEBUGEN=0). Only valid while running.
func (c *Core) Detach(ctx context.Context) error { return c.Bus.Write32(ctx, AddrDHCSR, dhcsrKey) }

// Cause is the decoded Debug Fault Status Register.
type Cause struct {
	DFSR  uint32
	Names []string
}

// String lists the reasons, e.g. "breakpoint".
func (c Cause) String() string {
	if len(c.Names) == 0 {
		return "none"
	}
	return strings.Join(c.Names, "+")
}

// Has reports whether the cause list contains name.
func (c Cause) Has(name string) bool {
	for _, n := range c.Names {
		if n == name {
			return true
		}
	}
	return false
}

func decodeDFSR(v uint32) Cause {
	c := Cause{DFSR: v}
	for _, b := range []struct {
		m uint32
		n string
	}{{DFSRBkpt, "breakpoint"}, {DFSRDWTTrap, "watchpoint"}, {DFSRVCatch, "vector-catch"}, {DFSRExternal, "external"}} {
		if v&b.m != 0 {
			c.Names = append(c.Names, b.n)
		}
	}
	if v&DFSRHalted != 0 && len(c.Names) == 0 {
		c.Names = []string{"halt-or-step"}
	}
	return c
}

// HaltCause reads DFSR. ClearCause resets it.
func (c *Core) HaltCause(ctx context.Context) (Cause, error) {
	v, err := c.Bus.Read32(ctx, AddrDFSR)
	return decodeDFSR(v), err
}

// ClearCause clears the sticky DFSR bits.
func (c *Core) ClearCause(ctx context.Context) error { return c.Bus.Write32(ctx, AddrDFSR, 0x1F) }

// StepResult is one confirmed single step.
type StepResult struct {
	PCBefore, PCAfter uint32
	Cause             Cause
}

// Step executes exactly one instruction of the halted core and confirms that
// the core halted again. When maskInts is set interrupts are masked during
// the step so it cannot enter an exception handler. STEP and MASKINTS are
// cleared afterwards and the core stays halted.
func (c *Core) Step(ctx context.Context, maskInts bool) (StepResult, error) {
	var r StepResult
	st, err := c.Status(ctx)
	if err != nil {
		return r, err
	}
	if !st.Halted {
		return r, ErrNotHalted
	}
	if r.PCBefore, err = c.ReadRegister(ctx, RegPC); err != nil {
		return r, err
	}
	if err := c.ClearCause(ctx); err != nil {
		return r, err
	}
	bits := uint32(DHCSRDebugEn | DHCSRStep)
	if maskInts {
		bits |= DHCSRMaskInts
	}
	// MASKINTS may only change while the core is halted.
	if err := c.writeDHCSR(ctx, DHCSRDebugEn|DHCSRHalt|(bits&DHCSRMaskInts)); err != nil {
		return r, err
	}
	if err := c.writeDHCSR(ctx, bits); err != nil {
		return r, err
	}
	if _, err := c.wait(ctx, "halt after step", func(s State) bool { return s.Halted }); err != nil {
		return r, err
	}
	if err := c.writeDHCSR(ctx, DHCSRDebugEn|DHCSRHalt); err != nil { // clears STEP and MASKINTS
		return r, err
	}
	if r.Cause, err = c.HaltCause(ctx); err != nil {
		return r, err
	}
	if r.Cause.DFSR&DFSRHalted == 0 {
		return r, fmt.Errorf("core halted after the step but DFSR.HALTED is clear (DFSR 0x%02x)", r.Cause.DFSR)
	}
	r.PCAfter, err = c.ReadRegister(ctx, RegPC)
	return r, err
}

func (c *Core) native() NativeRegs {
	n, _ := c.Bus.(NativeRegs)
	return n
}

// ReadRegisterDCRSR reads a core register through DCRSR/DCRDR, the path
// defined by the architecture. The core must be halted.
func (c *Core) ReadRegisterDCRSR(ctx context.Context, id RegisterID) (uint32, error) {
	if err := c.Bus.Write32(ctx, AddrDCRSR, uint32(id)); err != nil {
		return 0, err
	}
	if _, err := c.wait(ctx, "register transfer", func(s State) bool { return s.DHCSR&DHCSRRegRdy != 0 }); err != nil {
		return 0, err
	}
	return c.Bus.Read32(ctx, AddrDCRDR)
}

// WriteRegisterDCRSR writes a core register through DCRDR/DCRSR.
func (c *Core) WriteRegisterDCRSR(ctx context.Context, id RegisterID, v uint32) error {
	if err := c.Bus.Write32(ctx, AddrDCRDR, v); err != nil {
		return err
	}
	if err := c.Bus.Write32(ctx, AddrDCRSR, 1<<16|uint32(id)); err != nil {
		return err
	}
	_, err := c.wait(ctx, "register transfer", func(s State) bool { return s.DHCSR&DHCSRRegRdy != 0 })
	return err
}

// ReadRegister reads one register of the halted core, with the probe's
// native command when it has one.
func (c *Core) ReadRegister(ctx context.Context, id RegisterID) (uint32, error) {
	if n := c.native(); n != nil && id != RegSpecial {
		return n.ReadCoreReg(ctx, id)
	}
	return c.ReadRegisterDCRSR(ctx, id)
}

// WriteRegister writes one register and reads it back; a different value is
// an error.
func (c *Core) WriteRegister(ctx context.Context, id RegisterID, v uint32) error {
	var err error
	if n := c.native(); n != nil && id != RegSpecial {
		err = n.WriteCoreReg(ctx, id, v)
	} else {
		err = c.WriteRegisterDCRSR(ctx, id, v)
	}
	if err != nil {
		return err
	}
	got, err := c.ReadRegister(ctx, id)
	if err != nil {
		return err
	}
	if id == RegPC {
		got &^= 1 // bit 0 of DebugReturnAddress is not stored
		v &^= 1
	}
	if got != v {
		return fmt.Errorf("%w: %s wrote 0x%08x, read 0x%08x", ErrVerify, RegisterName(id), v, got)
	}
	return nil
}

// Registers is the core register set of a halted core.
type Registers struct {
	R       [13]uint32
	SP, LR  uint32
	PC      uint32
	XPSR    uint32
	MSP     uint32
	PSP     uint32
	Special uint32 // CONTROL/FAULTMASK/BASEPRI/PRIMASK packed as DCRSR 20
}

// Control returns CONTROL.
func (r Registers) Control() uint8 { return uint8(r.Special >> 24) }

// Primask returns PRIMASK.
func (r Registers) Primask() uint8 { return uint8(r.Special) }

// Basepri returns BASEPRI.
func (r Registers) Basepri() uint8 { return uint8(r.Special >> 8) }

// Faultmask returns FAULTMASK.
func (r Registers) Faultmask() uint8 { return uint8(r.Special >> 16) }

// ReadAll reads the whole core register set (one probe command plus one
// DCRSR read when the probe has a native block read).
func (c *Core) ReadAll(ctx context.Context) (Registers, error) {
	var r Registers
	st, err := c.Status(ctx)
	if err != nil {
		return r, err
	}
	if !st.Halted {
		return r, ErrNotHalted
	}
	if n := c.native(); n != nil {
		vals, err := n.ReadCoreRegs(ctx)
		if err != nil {
			return r, err
		}
		if len(vals) < 19 {
			return r, fmt.Errorf("the probe returned %d registers, want 19", len(vals))
		}
		copy(r.R[:], vals[:13])
		r.SP, r.LR, r.PC, r.XPSR, r.MSP, r.PSP = vals[13], vals[14], vals[15], vals[16], vals[17], vals[18]
	} else {
		for i := 0; i < 13; i++ {
			if r.R[i], err = c.ReadRegisterDCRSR(ctx, RegisterID(i)); err != nil {
				return r, err
			}
		}
		for _, f := range []struct {
			id RegisterID
			p  *uint32
		}{{RegSP, &r.SP}, {RegLR, &r.LR}, {RegPC, &r.PC}, {RegXPSR, &r.XPSR}, {RegMSP, &r.MSP}, {RegPSP, &r.PSP}} {
			if *f.p, err = c.ReadRegisterDCRSR(ctx, f.id); err != nil {
				return r, err
			}
		}
	}
	r.Special, err = c.ReadRegisterDCRSR(ctx, RegSpecial)
	return r, err
}

// ResetKind selects how the core is reset.
type ResetKind string

// Core reset kinds. A pin reset (NRST) is a probe function, not a core one.
const (
	ResetSystem ResetKind = "system" // AIRCR.SYSRESETREQ
	ResetCore   ResetKind = "core"   // AIRCR.VECTRESET (absent on ARMv6-M and ARMv8-M Baseline)
)

// ResetResult is what was observed around a reset.
type ResetResult struct {
	Kind      ResetKind
	Halted    bool
	ResetSeen bool   // S_RESET_ST was set after the request
	DebugKept bool   // C_DEBUGEN survived (a pin or power-on reset clears it)
	PC        uint32 // valid when Halted
	// ResetVector is vector table entry 1 (Thumb bit cleared) of the table the
	// core uses AFTER the reset (VTOR is reset too, so firmware that relocated
	// its vector table before the reset boots from the default one again).
	ResetVector uint32
	// PreResetVector is the same entry as seen before the reset request.
	PreResetVector uint32
	AtVector       bool // Halted and PC == ResetVector
	// VectorCatch is DFSR.VCATCH after the halt: the core stopped on the reset
	// vector catch armed for this request. It proves the reset even where the
	// first instruction is not the application's vector (a boot ROM that runs
	// before the vector table in flash, as on the Silicon Labs EFR32 Series 1).
	VectorCatch bool
	DebugLost   bool // the debug access went down with the reset request
}

// ErrDebugLost marks a reset after which the debug access did not answer any
// more: the reset probably took the debug port down with it. It is not proof
// that the reset happened; callers must look for corroborating evidence.
var ErrDebugLost = errors.New("debug access lost by the reset request")

// Reset resets the core through AIRCR and verifies that a reset really
// happened: S_RESET_ST must be seen or, when haltAfter is set, the core must
// be halted at the reset vector. A reset that cannot be proven is an error.
func (c *Core) Reset(ctx context.Context, kind ResetKind, haltAfter bool) (ResetResult, error) {
	res := ResetResult{Kind: kind}
	var req uint32
	switch kind {
	case ResetSystem:
		req = 1 << 2
	case ResetCore:
		id, err := c.ReadIdentity(ctx)
		if err != nil {
			return res, err
		}
		if !id.HasVectReset() {
			return res, fmt.Errorf("%w: AIRCR.VECTRESET does not exist on %s (%s); use a system reset", ErrAbsent, orUnknown(id.Name), orUnknown(id.ArchName))
		}
		req = 1 << 0
	default:
		return res, fmt.Errorf("unknown reset kind %q", kind)
	}
	if err := c.CleanupDebug(ctx); err != nil {
		return res, fmt.Errorf("debug trigger cleanup before reset: %w", err)
	}
	vtor, err := c.Bus.Read32(ctx, AddrVTOR)
	if err != nil {
		return res, err
	}
	if vec, err := c.Bus.Read32(ctx, (vtor&^0x7F)+4); err == nil {
		res.PreResetVector = vec &^ 1
	}
	was, err := c.Status(ctx)
	if err != nil {
		return res, err
	}
	keep := uint32(DHCSRDebugEn)
	if was.Halted {
		keep |= DHCSRHalt
	}
	if err := c.writeDHCSR(ctx, keep); err != nil {
		return res, err
	}
	demcr, err := c.Bus.Read32(ctx, AddrDEMCR)
	if err != nil {
		return res, err
	}
	if haltAfter {
		if err := c.Bus.Write32(ctx, AddrDEMCR, demcr|DEMCRVCCoreReset); err != nil {
			return res, err
		}
		defer func() { _ = c.Bus.Write32(ctx, AddrDEMCR, demcr&^DEMCRVCCoreReset) }()
	}
	_ = c.ClearCause(ctx)
	if _, err := c.Status(ctx); err != nil { // clear the sticky S_RESET_ST
		return res, err
	}
	if err := c.Bus.Write32(ctx, AddrAIRCR, aircrKey|req); err != nil {
		// Some devices reset the debug port together with the core, so the
		// write itself is the last thing the link carries.
		res.DebugLost = true
		return res, fmt.Errorf("%w: the link went down at the AIRCR write (%w)", ErrDebugLost, err)
	}
	deadline := time.Now().Add(c.Timeout)
	debugAnswered := false // C_DEBUGEN read back as set at least once after the request
	for {
		time.Sleep(5 * time.Millisecond)
		st, err := c.Status(ctx)
		if err == nil {
			res.ResetSeen = res.ResetSeen || st.ResetSeen
			res.DebugKept = st.DebugEnabled
			debugAnswered = debugAnswered || st.DebugEnabled
			if haltAfter && st.Halted {
				res.Halted = true
				break
			}
			if !haltAfter && res.ResetSeen {
				res.Halted = st.Halted
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
	}
	if res.Halted {
		res.ResetVector = c.ResetVectorNow(ctx)
		if pc, err := c.ReadRegister(ctx, RegPC); err == nil {
			res.PC = pc &^ 1
			res.AtVector = res.ResetVector != 0 && res.PC == res.ResetVector
		}
		if dfsr, err := c.Bus.Read32(ctx, AddrDFSR); err == nil {
			res.VectorCatch = dfsr&DFSRVCatch != 0
		}
	}
	if !res.Halted && !res.ResetSeen && !debugAnswered {
		// Neither a halt nor S_RESET_ST nor a live debug unit: the debug
		// access went away with the request (a gated or powered-down DAP).
		res.DebugLost = true
		return res, fmt.Errorf("%w: the core did not answer with debug enabled for %s after the request", ErrDebugLost, c.Timeout)
	}
	switch {
	case haltAfter && res.Halted && (res.AtVector || res.VectorCatch):
		return res, nil
	case haltAfter && res.Halted:
		return res, fmt.Errorf("the core halted but at 0x%08x, not at the reset vector 0x%08x: a reset is not proven", res.PC, res.ResetVector)
	case haltAfter:
		return res, fmt.Errorf("%w: no halt at the reset vector after the reset request", ErrTimeout)
	case res.ResetSeen:
		return res, nil
	}
	return res, fmt.Errorf("%w: S_RESET_ST was never seen after the reset request; a reset is not proven", ErrTimeout)
}

// ResetVectorNow reads vector table entry 1 through the current VTOR (Thumb
// bit cleared); 0 when it cannot be read.
func (c *Core) ResetVectorNow(ctx context.Context) uint32 {
	vtor, err := c.Bus.Read32(ctx, AddrVTOR)
	if err != nil {
		return 0
	}
	vec, err := c.Bus.Read32(ctx, (vtor&^0x7F)+4)
	if err != nil {
		return 0
	}
	return vec &^ 1
}

// Identity is the Arm identification of the core, decoded from CPUID.
type Identity struct {
	CPUID        uint32
	Implementer  uint8
	Variant      uint8
	Architecture uint8
	PartNo       uint16
	Revision     uint8
	Name         string // e.g. "Cortex-M4"; "" when the part number is not in the table
	ArchName     string
}

// DecodeCPUID decodes a CPUID value.
func DecodeCPUID(v uint32) Identity {
	id := Identity{
		CPUID: v, Implementer: uint8(v >> 24), Variant: uint8(v >> 20 & 0xF), Architecture: uint8(v >> 16 & 0xF),
		PartNo: uint16(v >> 4 & 0xFFF), Revision: uint8(v & 0xF),
	}
	if id.Implementer == 0x41 {
		id.Name = map[uint16]string{
			0xC20: "Cortex-M0", 0xC21: "Cortex-M1", 0xC23: "Cortex-M3", 0xC24: "Cortex-M4", 0xC27: "Cortex-M7",
			0xC60: "Cortex-M0+", 0xD20: "Cortex-M23", 0xD21: "Cortex-M33", 0xD22: "Cortex-M55", 0xD23: "Cortex-M85",
		}[id.PartNo]
	}
	switch id.Architecture {
	case 0xC:
		id.ArchName = "ARMv6-M"
	case 0xF:
		id.ArchName = "ARMv7-M"
	}
	if id.PartNo >= 0xD20 && id.Name != "" {
		id.ArchName = "ARMv8-M"
	}
	return id
}

// ReadIdentity reads and decodes CPUID.
func (c *Core) ReadIdentity(ctx context.Context) (Identity, error) {
	v, err := c.Bus.Read32(ctx, AddrCPUID)
	if err != nil {
		return Identity{}, err
	}
	return DecodeCPUID(v), nil
}

// IsV7M reports whether the core uses the ARMv7-M programming model.
func (i Identity) IsV7M() bool { return i.Implementer == 0x41 && i.ArchName == "ARMv7-M" }

// HasVectReset reports whether the architecture defines AIRCR.VECTRESET: the
// Baseline profiles (ARMv6-M, ARMv8-M Baseline) only define SYSRESETREQ.
func (i Identity) HasVectReset() bool {
	switch i.Name {
	case "Cortex-M0", "Cortex-M0+", "Cortex-M1", "Cortex-M23":
		return false
	}
	return i.ArchName != "ARMv6-M"
}
