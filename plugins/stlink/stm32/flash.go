package stm32

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
)

// STM32F4 flash interface registers (RM0090/RM0368/RM0383 FLASH chapter).
const (
	f4Base  = 0x40023C00
	f4KeyR  = f4Base + 0x04
	f4OptKR = f4Base + 0x08 // never written: option bytes are out of scope
	f4SR    = f4Base + 0x0C
	f4CR    = f4Base + 0x10
	f4OptCR = f4Base + 0x14

	key1, key2 = 0x45670123, 0xCDEF89AB

	crPG    = 1 << 0
	crSER   = 1 << 1
	crSNB   = 3 // shift
	crPSize = 8 // shift
	crSTRT  = 1 << 16
	crLock  = 1 << 31

	srEOP  = 1 << 0
	srBSY  = 1 << 16
	srErrs = 1<<1 | 1<<4 | 1<<5 | 1<<6 | 1<<7 // OPERR WRPERR PGAERR PGPERR PGSERR
	srW1C  = srErrs | srEOP

	psizeX32 = 2
)

// Errors.
var (
	ErrLocked     = errors.New("the flash controller stayed locked after the unlock sequence")
	ErrController = errors.New("flash controller error")
	ErrProtected  = errors.New("flash is protected")
	ErrAlign      = errors.New("address or length is not word aligned")
	ErrRange      = errors.New("range is outside the flash")
	ErrVerify     = errors.New("read-back differs from the data written")
)

// Flash drives the F4 flash controller through the core's debug port.
type Flash struct {
	C *cortexm.Core
	P *Profile
	// VoltageMV is the target supply the probe measured (0 = unknown). 32-bit
	// programming needs 2.7 V or more.
	VoltageMV int
	// Timeout bounds every wait for BSY.
	Timeout time.Duration
}

// NewFlash returns a driver for a recognised, programmable profile.
func NewFlash(c *cortexm.Core, p *Profile, mv int) *Flash {
	return &Flash{C: c, P: p, VoltageMV: mv, Timeout: 15 * time.Second}
}

// Protection is the read-only protection state.
type Protection struct {
	OPTCR uint32 `json:"optcr"`
	// RDP is the readout-protection level: 0, 1 or 2.
	RDP int `json:"rdp_level"`
	// WriteProtected lists sector indexes whose nWRP bit is clear.
	WriteProtected []int `json:"write_protected_sectors"`

	// F1 only: FLASH_OBR, FLASH_WRPR and the 16 option-byte bytes at
	// 0x1FFFF800 as read (never written).
	OBR         uint32 `json:"obr,omitempty"`
	WRPR        uint32 `json:"wrpr,omitempty"`
	OptionBytes []byte `json:"option_bytes,omitempty"`
}

// ReadProtection reads FLASH_OPTCR. Nothing is written.
func (f *Flash) ReadProtection(ctx context.Context) (Protection, error) {
	if f.P.Family == F1 {
		return f.f1Protection(ctx)
	}
	if f.P.Family != F4 {
		return Protection{}, fmt.Errorf("protection state is only read for F4 and F1 (this is %s)", f.P.Family)
	}
	v, err := f.C.Bus.Read32(ctx, f4OptCR)
	if err != nil {
		return Protection{}, err
	}
	p := Protection{OPTCR: v}
	switch rdp := byte(v >> 8); rdp {
	case 0xAA:
		p.RDP = 0
	case 0xCC:
		p.RDP = 2
	default:
		p.RDP = 1
	}
	nwrp := v >> 16 & 0xFFF
	for i := range f.P.Sectors {
		if i < 12 && nwrp>>uint(i)&1 == 0 {
			p.WriteProtected = append(p.WriteProtected, i)
		}
	}
	return p, nil
}

// Contains reports whether [addr, addr+n) lies inside the flash.
func (f *Flash) Contains(addr uint32, n int) bool {
	return n >= 0 && addr >= FlashBase && uint64(addr)+uint64(n) <= uint64(FlashBase)+uint64(f.P.FlashSize())
}

// Read reads flash through the debug port. The flash is memory mapped on
// STM32, so this is the same path the CPU uses; readout protection makes it
// fail rather than return data.
func (f *Flash) Read(ctx context.Context, addr uint32, n int) ([]byte, error) {
	if !f.Contains(addr, n) {
		return nil, fmt.Errorf("%w: [0x%08x, +%d) vs 0x%08x..0x%08x", ErrRange, addr, n, uint32(FlashBase), FlashBase+f.P.FlashSize())
	}
	return f.C.Read(ctx, addr, n)
}

func (f *Flash) wait(ctx context.Context) (uint32, error) {
	deadline := time.Now().Add(f.Timeout)
	for {
		sr, err := f.C.Bus.Read32(ctx, f4SR)
		if err != nil {
			return 0, err
		}
		if sr&srBSY == 0 {
			return sr, nil
		}
		if time.Now().After(deadline) {
			return sr, fmt.Errorf("%w: FLASH_SR.BSY still set after %s (SR 0x%08x)", ErrController, f.Timeout, sr)
		}
		select {
		case <-ctx.Done():
			return sr, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (f *Flash) checkSR(sr uint32, what string) error {
	if sr&srErrs != 0 {
		return fmt.Errorf("%w: %s: FLASH_SR 0x%08x (OPERR=%d WRPERR=%d PGAERR=%d PGPERR=%d PGSERR=%d)", ErrController, what, sr,
			sr>>1&1, sr>>4&1, sr>>5&1, sr>>6&1, sr>>7&1)
	}
	return nil
}

func (f *Flash) clearErrors(ctx context.Context) error {
	return f.C.Bus.Write32(ctx, f4SR, srW1C)
}

// unlock runs the documented key sequence and verifies the lock bit cleared.
func (f *Flash) unlock(ctx context.Context) error {
	cr, err := f.C.Bus.Read32(ctx, f4CR)
	if err != nil {
		return err
	}
	if cr&crLock == 0 {
		return nil
	}
	if err := f.C.Bus.Write32(ctx, f4KeyR, key1); err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, f4KeyR, key2); err != nil {
		return err
	}
	if cr, err = f.C.Bus.Read32(ctx, f4CR); err != nil {
		return err
	}
	if cr&crLock != 0 {
		return ErrLocked
	}
	return nil
}

func (f *Flash) lock(ctx context.Context) error {
	return f.C.Bus.Write32(ctx, f4CR, crLock)
}

// begin checks the preconditions common to erase and program: a halted
// core, an F4 profile, no readout protection, and an unlocked controller.
func (f *Flash) begin(ctx context.Context) error {
	if !f.P.Programmable {
		return fmt.Errorf("%w: %s", ErrUnsupported, f.P.Note)
	}
	st, err := f.C.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Halted {
		return errors.New("the core must be halted before flash is modified")
	}
	prot, err := f.ReadProtection(ctx)
	if err != nil {
		return err
	}
	if prot.RDP != 0 {
		return fmt.Errorf("%w: readout protection level %d is active (OPTCR 0x%08x); this tool never changes option bytes", ErrProtected, prot.RDP, prot.OPTCR)
	}
	if _, err := f.wait(ctx); err != nil {
		return err
	}
	if err := f.clearErrors(ctx); err != nil {
		return err
	}
	return f.unlock(ctx)
}

// EraseSector erases one sector and checks the controller status and that
// every word reads back erased.
func (f *Flash) EraseSector(ctx context.Context, s Sector) (err error) {
	if f.P.Family == F1 {
		return f.f1Erase(ctx, s)
	}
	if err := f.begin(ctx); err != nil {
		return err
	}
	prot, err := f.ReadProtection(ctx)
	if err != nil {
		return err
	}
	for _, w := range prot.WriteProtected {
		if w == s.Index {
			return fmt.Errorf("%w: sector %d is write-protected (OPTCR 0x%08x)", ErrProtected, s.Index, prot.OPTCR)
		}
	}
	defer func() {
		if lerr := f.lock(context.WithoutCancel(ctx)); lerr != nil && err == nil {
			err = fmt.Errorf("erase done but the controller could not be re-locked: %w", lerr)
		}
	}()
	if f.VoltageMV > 0 && f.VoltageMV < 2700 {
		// Erase works at lower voltage but with x8 parallelism; this driver
		// only implements x32 and refuses rather than guess.
		return fmt.Errorf("target supply %d mV is below 2.7 V: x32 flash operations are not supported there", f.VoltageMV)
	}
	cr := uint32(crSER) | uint32(s.Index)<<crSNB | psizeX32<<crPSize
	if err := f.C.Bus.Write32(ctx, f4CR, cr); err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, f4CR, cr|crSTRT); err != nil {
		return err
	}
	sr, err := f.wait(ctx)
	if err != nil {
		return err
	}
	if err := f.C.Bus.Write32(ctx, f4CR, 0); err != nil {
		return err
	}
	if err := f.checkSR(sr, fmt.Sprintf("erase of sector %d", s.Index)); err != nil {
		return err
	}
	if err := f.clearErrors(ctx); err != nil {
		return err
	}
	data, err := f.C.Read(ctx, s.Addr, int(s.Size))
	if err != nil {
		return fmt.Errorf("sector %d: erase finished but the read-back failed: %w", s.Index, err)
	}
	for i, b := range data {
		if b != 0xFF {
			return fmt.Errorf("%w: sector %d: erase finished but 0x%08x reads 0x%02x", ErrVerify, s.Index, s.Addr+uint32(i), b)
		}
	}
	return nil
}

const programChunk = 1024

// Program writes data at a word-aligned address in 32-bit units (F4) or at a
// halfword-aligned address in 16-bit units (F1, through the SRAM loader). Words that
// are all ones are skipped (that is the erased state). The caller must have
// erased the target sectors; the result is read back and compared.
func (f *Flash) Program(ctx context.Context, addr uint32, data []byte) (err error) {
	if f.P.Family == F1 {
		return f.f1Program(ctx, addr, data)
	}
	if addr&3 != 0 || len(data)&3 != 0 {
		return fmt.Errorf("%w: address 0x%08x, length %d", ErrAlign, addr, len(data))
	}
	if !f.Contains(addr, len(data)) {
		return fmt.Errorf("%w: [0x%08x, +%d)", ErrRange, addr, len(data))
	}
	if len(data) == 0 {
		return nil
	}
	if err := f.begin(ctx); err != nil {
		return err
	}
	defer func() {
		_ = f.C.Bus.Write32(context.WithoutCancel(ctx), f4CR, 0)
		if lerr := f.lock(context.WithoutCancel(ctx)); lerr != nil && err == nil {
			err = fmt.Errorf("programming done but the controller could not be re-locked: %w", lerr)
		}
	}()
	if f.VoltageMV > 0 && f.VoltageMV < 2700 {
		return fmt.Errorf("target supply %d mV is below 2.7 V: x32 programming is not supported there", f.VoltageMV)
	}
	prot, err := f.ReadProtection(ctx)
	if err != nil {
		return err
	}
	for _, s := range f.P.Covering(addr, len(data)) {
		for _, w := range prot.WriteProtected {
			if w == s.Index {
				return fmt.Errorf("%w: sector %d is write-protected", ErrProtected, s.Index)
			}
		}
	}
	if err := f.C.Bus.Write32(ctx, f4CR, crPG|psizeX32<<crPSize); err != nil {
		return err
	}
	for off := 0; off < len(data); {
		end := min(off+programChunk, len(data))
		// Contiguous non-blank words are written as one block; blank words
		// are skipped so partly empty images stay fast.
		for off < end && isBlank(data[off:off+4]) {
			off += 4
		}
		start := off
		for off < end && !isBlank(data[off:off+4]) {
			off += 4
		}
		if off == start {
			continue
		}
		if err := f.C.Write(ctx, addr+uint32(start), data[start:off]); err != nil {
			return fmt.Errorf("programming 0x%08x: %w", addr+uint32(start), err)
		}
		sr, err := f.wait(ctx)
		if err != nil {
			return err
		}
		if err := f.checkSR(sr, fmt.Sprintf("programming near 0x%08x", addr+uint32(start))); err != nil {
			return err
		}
	}
	if err := f.C.Bus.Write32(ctx, f4CR, 0); err != nil {
		return err
	}
	got, err := f.C.Read(ctx, addr, len(data))
	if err != nil {
		return fmt.Errorf("programming finished but the read-back failed: %w", err)
	}
	for i := range data {
		if got[i] != data[i] {
			return fmt.Errorf("%w: 0x%08x wrote 0x%02x, read 0x%02x", ErrVerify, addr+uint32(i), data[i], got[i])
		}
	}
	return nil
}

func isBlank(w []byte) bool { return binary.LittleEndian.Uint32(w) == 0xFFFFFFFF }
