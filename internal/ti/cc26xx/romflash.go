package cc26xx

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
)

// ROM tables of the CC13x2/CC26x2 (driverlib rom.h): ROM_API_TABLE at
// 0x10000180, whose entry 10 points at ROM_API_FLASH_TABLE; entry 5 of that
// is FlashSectorErase(address) and entry 6 FlashProgram(buffer, address,
// count). The HAPI table at 0x10000048 holds the older entry points (Crc32
// first, sector erase and program at 3 and 4), reported for reference.
const (
	ROMAPITable       = 0x10000180
	ROMAPIFlashIndex  = 10
	ROMFlashEraseIdx  = 5
	ROMFlashProgIdx   = 6
	HAPITable         = 0x10000048
	ROMBase           = 0x10000000
	ROMSize           = 0x40000
	SRAMBase          = 0x20000000
	FlashRegBase      = 0x40030000
	FlashRegCFG       = FlashRegBase + 0x24
	FlashCFGDisStdby  = 1 << 1
	VIMSStat          = 0x40034000
	VIMSCtl           = 0x40034004
	vimsModeChanging  = 1 << 3
	vimsOffNoLineBufs = 0x33
)

// ROM flash API status codes.
const (
	FAPISuccess         = 0x0
	FAPIBusy            = 0x1
	FAPIReady           = 0x2
	FAPIBadBufferLength = 0x3
	FAPIFSMError        = 0x4
)

// FAPIName names a ROM flash API status.
func FAPIName(v uint32) string {
	switch v {
	case FAPISuccess:
		return "success"
	case FAPIBusy:
		return "FSM busy"
	case FAPIReady:
		return "FSM ready"
	case FAPIBadBufferLength:
		return "incorrect data buffer length"
	case FAPIFSMError:
		return "FSM error (protected sector or flash failure)"
	}
	return fmt.Sprintf("status 0x%x", v)
}

// Work area in SRAM: a BKPT stub the ROM functions return to, the stack
// below StackTop and the program buffer above it.
const (
	StubAddr   = SRAMBase
	StackTop   = SRAMBase + 0x1000
	BufferAddr = SRAMBase + 0x1000
	BufferSize = 0x2000
)

// Mem is the target memory access the flash driver needs.
type Mem interface {
	Read(ctx context.Context, addr uint32, n int) ([]byte, error)
	Write(ctx context.Context, addr uint32, data []byte) error
	Read32(ctx context.Context, addr uint32) (uint32, error)
	Write32(ctx context.Context, addr, v uint32) error
}

// CoreMem adapts a cortexm.Core to Mem.
type CoreMem struct{ C *cortexm.Core }

// Read implements Mem.
func (m CoreMem) Read(ctx context.Context, addr uint32, n int) ([]byte, error) {
	return m.C.Read(ctx, addr, n)
}

// Write implements Mem.
func (m CoreMem) Write(ctx context.Context, addr uint32, data []byte) error {
	return m.C.Write(ctx, addr, data)
}

// Read32 implements Mem.
func (m CoreMem) Read32(ctx context.Context, addr uint32) (uint32, error) {
	return m.C.Bus.Read32(ctx, addr)
}

// Write32 implements Mem.
func (m CoreMem) Write32(ctx context.Context, addr, v uint32) error {
	return m.C.Bus.Write32(ctx, addr, v)
}

// ROMEntries are the ROM flash entry points found on the target.
type ROMEntries struct {
	ROMVersion  uint32
	FlashTable  uint32
	SectorErase uint32
	Program     uint32
	HAPICrc32   uint32
	HAPIErase   uint32
	HAPIProgram uint32
}

func inROM(fn uint32) bool { return fn&1 == 1 && fn >= ROMBase && fn < ROMBase+ROMSize }

// ErrROMTable means the ROM tables do not look like a CC13x2/CC26x2 ROM.
var ErrROMTable = errors.New("the ROM flash API table does not look valid")

// ReadROMEntries reads the ROM tables and checks that the flash entry points
// are Thumb addresses inside the ROM.
func ReadROMEntries(ctx context.Context, m Mem) (ROMEntries, error) {
	var e ROMEntries
	var err error
	if e.ROMVersion, err = m.Read32(ctx, ROMAPITable); err != nil {
		return e, err
	}
	if e.FlashTable, err = m.Read32(ctx, ROMAPITable+4*ROMAPIFlashIndex); err != nil {
		return e, err
	}
	if e.FlashTable < ROMBase || e.FlashTable >= ROMBase+ROMSize || e.FlashTable&3 != 0 {
		return e, fmt.Errorf("%w: ROM_API_FLASH_TABLE 0x%08x", ErrROMTable, e.FlashTable)
	}
	if e.SectorErase, err = m.Read32(ctx, e.FlashTable+4*ROMFlashEraseIdx); err != nil {
		return e, err
	}
	if e.Program, err = m.Read32(ctx, e.FlashTable+4*ROMFlashProgIdx); err != nil {
		return e, err
	}
	if !inROM(e.SectorErase) || !inROM(e.Program) {
		return e, fmt.Errorf("%w: FlashSectorErase 0x%08x, FlashProgram 0x%08x", ErrROMTable, e.SectorErase, e.Program)
	}
	hapi, err := m.Read(ctx, HAPITable, 20)
	if err == nil {
		e.HAPICrc32 = binary.LittleEndian.Uint32(hapi)
		e.HAPIErase = binary.LittleEndian.Uint32(hapi[12:])
		e.HAPIProgram = binary.LittleEndian.Uint32(hapi[16:])
	}
	return e, nil
}

// ROMFlash erases and programs CC13x2/CC26x2 flash by running the ROM flash
// API on the halted core, the way TI's flash loaders do: the arguments go in
// r0-r2, LR points at a BKPT in SRAM, and the core runs with interrupts
// masked until the BKPT halts it again. The flash cache (VIMS) is switched
// off for the duration and restored by Finish. SRAM below BufferAddr +
// BufferSize is overwritten, so the firmware must be reset afterwards.
type ROMFlash struct {
	Core    *cortexm.Core
	Mem     Mem
	Entries ROMEntries
	// Timeout bounds one ROM call.
	Timeout time.Duration

	vimsCtl  uint32
	prepared bool
}

// NewROMFlash checks the ROM entry points of the attached chip.
func NewROMFlash(ctx context.Context, core *cortexm.Core, m Mem) (*ROMFlash, error) {
	e, err := ReadROMEntries(ctx, m)
	if err != nil {
		return nil, err
	}
	return &ROMFlash{Core: core, Mem: m, Entries: e, Timeout: 5 * time.Second}, nil
}

// bkpt #0; b . (never reached)
var stub = []byte{0x00, 0xBE, 0xFE, 0xE7}

// Prepare halts the core, installs the BKPT stub and switches the flash
// cache off.
func (f *ROMFlash) Prepare(ctx context.Context) error {
	if _, err := f.Core.Halt(ctx); err != nil {
		return fmt.Errorf("halt the core: %w", err)
	}
	if err := f.Mem.Write(ctx, StubAddr, stub); err != nil {
		return fmt.Errorf("write the BKPT stub: %w", err)
	}
	got, err := f.Mem.Read(ctx, StubAddr, len(stub))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, stub) {
		return fmt.Errorf("SRAM at 0x%08x reads back %x after writing the BKPT stub %x", StubAddr, got, stub)
	}
	ctl, err := f.waitVIMS(ctx)
	if err != nil {
		return err
	}
	f.vimsCtl = ctl
	if err := f.Mem.Write32(ctx, VIMSCtl, ctl|vimsOffNoLineBufs); err != nil {
		return err
	}
	if _, err := f.waitVIMS(ctx); err != nil {
		return err
	}
	f.prepared = true
	return nil
}

// waitVIMS waits until the cache is not changing mode and returns VIMS_CTL.
func (f *ROMFlash) waitVIMS(ctx context.Context) (uint32, error) {
	deadline := time.Now().Add(time.Second)
	for {
		st, err := f.Mem.Read32(ctx, VIMSStat)
		if err != nil {
			return 0, err
		}
		if st&vimsModeChanging == 0 {
			return f.Mem.Read32(ctx, VIMSCtl)
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("VIMS stays in a mode change (VIMS_STAT 0x%08x)", st)
		}
		time.Sleep(time.Millisecond)
	}
}

// Finish restores the flash cache mode. The core stays halted.
func (f *ROMFlash) Finish(ctx context.Context) error {
	if !f.prepared {
		return nil
	}
	f.prepared = false
	if err := f.Mem.Write32(ctx, VIMSCtl, f.vimsCtl); err != nil {
		return err
	}
	_, err := f.waitVIMS(ctx)
	return err
}

// Call runs the ROM function fn with up to four arguments on the halted core
// and returns r0.
func (f *ROMFlash) Call(ctx context.Context, fn uint32, args ...uint32) (uint32, error) {
	if !f.prepared {
		return 0, errors.New("ROM flash driver used before Prepare")
	}
	if len(args) > 4 {
		return 0, fmt.Errorf("%d arguments: at most 4", len(args))
	}
	c := f.Core
	regs := []struct {
		id cortexm.RegisterID
		v  uint32
	}{
		// CONTROL 0 (MSP, privileged, no FP context), PRIMASK 1.
		{cortexm.RegSpecial, 0x00000001},
		{cortexm.RegMSP, StackTop},
		{cortexm.RegSP, StackTop},
		{cortexm.RegLR, StubAddr | 1},
		{cortexm.RegPC, fn &^ 1},
		{cortexm.RegXPSR, 1 << 24},
	}
	for i, a := range args {
		regs = append(regs, struct {
			id cortexm.RegisterID
			v  uint32
		}{cortexm.RegisterID(i), a})
	}
	for _, r := range regs {
		if err := c.WriteRegisterDCRSR(ctx, r.id, r.v); err != nil {
			return 0, fmt.Errorf("set %s: %w", cortexm.RegisterName(r.id), err)
		}
	}
	if err := c.ClearCause(ctx); err != nil {
		return 0, err
	}
	// C_MASKINTS may only change while halted: set it first, then release.
	const key = 0xA05F0000
	if err := f.Mem.Write32(ctx, cortexm.AddrDHCSR, key|cortexm.DHCSRDebugEn|cortexm.DHCSRHalt|cortexm.DHCSRMaskInts); err != nil {
		return 0, err
	}
	if err := f.Mem.Write32(ctx, cortexm.AddrDHCSR, key|cortexm.DHCSRDebugEn|cortexm.DHCSRMaskInts); err != nil {
		return 0, err
	}
	if _, err := c.WaitHalted(ctx, f.Timeout); err != nil {
		_, _ = c.Halt(context.WithoutCancel(ctx))
		return 0, fmt.Errorf("ROM function 0x%08x did not return: %w", fn, err)
	}
	pc, err := c.ReadRegisterDCRSR(ctx, cortexm.RegPC)
	if err != nil {
		return 0, err
	}
	if pc&^1 != StubAddr {
		return 0, fmt.Errorf("ROM function 0x%08x stopped at 0x%08x, not at the return stub 0x%08x", fn, pc, StubAddr)
	}
	r0, err := c.ReadRegisterDCRSR(ctx, 0)
	if err != nil {
		return 0, err
	}
	// The ROM functions may leave flash standby disabled (driverlib does the same).
	if cfg, err := f.Mem.Read32(ctx, FlashRegCFG); err == nil && cfg&FlashCFGDisStdby != 0 {
		_ = f.Mem.Write32(ctx, FlashRegCFG, cfg&^FlashCFGDisStdby)
	}
	return r0, nil
}

// FAPIError is a ROM flash API call that did not return success.
type FAPIError struct {
	Op     string
	Addr   uint32
	Status uint32
}

func (e *FAPIError) Error() string {
	return fmt.Sprintf("ROM %s at 0x%08x: %s", e.Op, e.Addr, FAPIName(e.Status))
}

// EraseSector erases the sector at addr.
func (f *ROMFlash) EraseSector(ctx context.Context, addr uint32) error {
	st, err := f.Call(ctx, f.Entries.SectorErase, addr)
	if err != nil {
		return err
	}
	if st != FAPISuccess {
		return &FAPIError{Op: "FlashSectorErase", Addr: addr, Status: st}
	}
	return nil
}

// Program writes data at addr (erased flash) through the SRAM buffer, in
// chunks of BufferSize. The buffer content is read back before every call.
func (f *ROMFlash) Program(ctx context.Context, addr uint32, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), BufferSize)
		chunk := data[:n]
		if err := f.Mem.Write(ctx, BufferAddr, chunk); err != nil {
			return fmt.Errorf("write the SRAM buffer: %w", err)
		}
		got, err := f.Mem.Read(ctx, BufferAddr, n)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, chunk) {
			return fmt.Errorf("the SRAM buffer at 0x%08x reads back different data", BufferAddr)
		}
		st, err := f.Call(ctx, f.Entries.Program, BufferAddr, addr, uint32(n))
		if err != nil {
			return err
		}
		if st != FAPISuccess {
			return &FAPIError{Op: "FlashProgram", Addr: addr, Status: st}
		}
		addr += uint32(n)
		data = data[n:]
	}
	return nil
}
