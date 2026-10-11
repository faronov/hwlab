// Package stm32 is the STM32-specific part of the ST-Link backend: it
// recognises an STM32 from DBGMCU_IDCODE and drives the on-chip flash
// controller directly through the debug port. No loader is uploaded to the
// target and no external tool is used.
//
// Scope, deliberately narrow:
//
//   - STM32F4 single-bank parts: full sector erase and 32-bit programming
//     (validated against a simulated controller only).
//   - STM32F1 (0x410, 0x412, 0x414, 0x418): page erase and halfword
//     programming. F1 flash is programmed in halfwords, which the ST-Link V2
//     debug port cannot issue, so a small hwlab-owned Thumb loader runs from
//     target SRAM (see loader.go). Exercised on real hardware for DBGMCU
//     0x410 only.
//   - Dual-bank parts, option bytes, read-out protection and OTP are never
//     written and are not modelled.
//
// The device-id and register tables come from ST reference manuals.
package stm32

import (
	"context"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/internal/cortexm"
)

// Addresses read for identification. All reads are side-effect free.
const (
	AddrDBGMCUIDCode = 0xE0042000
	AddrFlashSizeF1  = 0x1FFFF7E0
	AddrFlashSizeF4  = 0x1FFF7A22
	FlashBase        = 0x08000000
)

// Family is the STM32 series.
type Family string

// Supported families.
const (
	F1 Family = "STM32F1"
	F4 Family = "STM32F4"
)

// Errors.
var (
	// ErrNotSTM32 means DBGMCU_IDCODE did not hold a device id this package
	// knows: the target is some other part.
	ErrNotSTM32 = errors.New("the target is not a recognised STM32")
	// ErrUnsupported means the id is an STM32 whose flash this package does
	// not drive.
	ErrUnsupported = errors.New("recognised STM32 variant is not supported")
)

// Sector is one flash erase unit.
type Sector struct {
	Index int    `json:"index"`
	Addr  uint32 `json:"addr"`
	Size  uint32 `json:"size"`
}

// Profile is a recognised STM32 and its flash geometry.
type Profile struct {
	Name   string `json:"name"`
	Family Family `json:"family"`
	DevID  uint16 `json:"dev_id"`
	RevID  uint16 `json:"rev_id"`
	// FlashKB is what the flash-size register reports.
	FlashKB int `json:"flash_kb"`
	// FlashSizeSource is "register" when the flash-size register held a
	// plausible value, or "probed" when it was blank and the size is the
	// largest memory-mapped window that reads without a bus fault.
	FlashSizeSource string `json:"flash_size_source"`
	// SRAMKB is the readable SRAM window at 0x20000000 (probed, 0 = unknown).
	SRAMKB int `json:"sram_kb,omitempty"`
	// PageSize is the F1 erase page; 0 for sector-based families.
	PageSize uint32 `json:"page_size,omitempty"`
	// Sectors is the erase layout (F4) or the page layout (F1).
	Sectors []Sector `json:"-"`
	// Programmable is false for families where only reads are implemented.
	Programmable bool   `json:"programmable"`
	Note         string `json:"note,omitempty"`
}

// FlashSize is the flash size in bytes.
func (p *Profile) FlashSize() uint32 { return uint32(p.FlashKB) << 10 }

type devEntry struct {
	name   string
	family Family
	page   uint32 // F1 page size
	max    int    // largest plausible flash in KB
}

var devices = map[uint16]devEntry{
	0x410: {"STM32F1 medium-density", F1, 1024, 128},
	0x412: {"STM32F1 low-density", F1, 1024, 32},
	0x414: {"STM32F1 high-density", F1, 2048, 512},
	0x418: {"STM32F1 connectivity line", F1, 2048, 256},
	0x413: {"STM32F405/407/415/417", F4, 0, 1024},
	0x423: {"STM32F401xB/C", F4, 0, 256},
	0x433: {"STM32F401xD/E", F4, 0, 512},
	0x431: {"STM32F411", F4, 0, 512},
	0x421: {"STM32F446", F4, 0, 512},
	0x441: {"STM32F412", F4, 0, 1024},
}

// unsupported ids are recognised so they are refused with a reason instead of
// being treated as "not an STM32".
var unsupported = map[uint16]string{
	0x419: "STM32F42x/F43x: dual-bank flash layout is not implemented",
	0x434: "STM32F469/479: dual-bank flash layout is not implemented",
	0x430: "STM32F1 XL-density: dual-bank flash is not implemented",
}

// f4Layout is the single-bank sector map: 4 x 16 KiB, 1 x 64 KiB, then
// 128 KiB sectors.
func f4Layout(kb int) []Sector {
	var out []Sector
	addr := uint32(FlashBase)
	end := uint32(FlashBase) + uint32(kb)<<10
	add := func(size uint32) bool {
		if addr+size > end {
			return false
		}
		out = append(out, Sector{Index: len(out), Addr: addr, Size: size})
		addr += size
		return true
	}
	for i := 0; i < 4 && add(16<<10); i++ {
	}
	add(64 << 10)
	for add(128 << 10) {
	}
	return out
}

func f1Layout(kb int, page uint32) []Sector {
	var out []Sector
	for a := uint32(0); a < uint32(kb)<<10; a += page {
		out = append(out, Sector{Index: len(out), Addr: FlashBase + a, Size: page})
	}
	return out
}

// Detect reads DBGMCU_IDCODE and the flash-size register. The accesses are
// plain reads of read-only locations. ErrNotSTM32 is returned (wrapped, with
// the raw value) for anything else; that is a normal outcome on non-ST parts.
func Detect(ctx context.Context, c *cortexm.Core) (*Profile, error) {
	idc, err := c.Bus.Read32(ctx, AddrDBGMCUIDCode)
	if err != nil {
		return nil, fmt.Errorf("%w: DBGMCU_IDCODE is not readable (%v)", ErrNotSTM32, err)
	}
	dev := uint16(idc & 0xFFF)
	if why, ok := unsupported[dev]; ok {
		return nil, fmt.Errorf("%w: DBGMCU_IDCODE 0x%08x: %s", ErrUnsupported, idc, why)
	}
	e, ok := devices[dev]
	if !ok {
		return nil, fmt.Errorf("%w: DBGMCU_IDCODE reads 0x%08x", ErrNotSTM32, idc)
	}
	sizeAddr := uint32(AddrFlashSizeF4)
	if e.family == F1 {
		sizeAddr = AddrFlashSizeF1
	}
	w, err := c.Bus.Read32(ctx, sizeAddr&^3)
	if err != nil {
		return nil, fmt.Errorf("%w: DBGMCU_IDCODE 0x%08x matches %s but the flash-size register is unreadable: %v", ErrNotSTM32, idc, e.name, err)
	}
	kb := int(w >> (8 * (sizeAddr & 3)) & 0xFFFF)
	source := "register"
	blank := kb == 0 || kb == 0xFFFF
	if e.family == F1 && blank {
		// An unprogrammed size register: fall back to the memory-mapped
		// window. That is an upper bound, not a datasheet value.
		kb, source = probeFlashKB(ctx, c, e.max), "probed"
	}
	if kb == 0 || kb > e.max {
		return nil, fmt.Errorf("%w: DBGMCU_IDCODE 0x%08x matches %s but the flash-size register says %d KB (plausible: 1..%d)", ErrNotSTM32, idc, e.name, kb, e.max)
	}
	p := &Profile{Name: e.name, Family: e.family, DevID: dev, RevID: uint16(idc >> 16), FlashKB: kb, FlashSizeSource: source}
	switch e.family {
	case F4:
		p.Sectors, p.Programmable = f4Layout(kb), true
	case F1:
		p.PageSize = e.page
		p.Sectors = f1Layout(kb, e.page)
		p.Programmable = true
		p.SRAMKB = probeSRAMKB(ctx, c)
		p.Note = "STM32F1 flash is programmed in halfwords by a hwlab-owned loader that runs from SRAM; option bytes, RDP and write protection are only read"
		if source == "probed" {
			p.Note += "; the flash-size register at 0x1FFFF7E0 is blank, so the size is the readable memory window (an upper bound)"
		}
	}
	return p, nil
}

// probeFlashKB returns the largest power-of-two-descending candidate whose
// last word reads without a fault (0 = none readable).
func probeFlashKB(ctx context.Context, c *cortexm.Core, max int) int {
	for kb := max; kb >= 16; kb /= 2 {
		if _, err := c.Bus.Read32(ctx, FlashBase+uint32(kb)<<10-4); err == nil {
			return kb
		}
	}
	return 0
}

// probeSRAMKB returns the readable SRAM window at 0x20000000 in KiB.
func probeSRAMKB(ctx context.Context, c *cortexm.Core) int {
	for _, kb := range []int{96, 80, 64, 48, 40, 32, 24, 20, 16, 12, 10, 8, 6, 4} {
		if _, err := c.Bus.Read32(ctx, 0x20000000+uint32(kb)<<10-4); err == nil {
			return kb
		}
	}
	return 0
}

// SectorAt returns the sector containing addr.
func (p *Profile) SectorAt(addr uint32) (Sector, bool) {
	for _, s := range p.Sectors {
		if addr >= s.Addr && addr < s.Addr+s.Size {
			return s, true
		}
	}
	return Sector{}, false
}

// Covering returns the sectors overlapping [addr, addr+n).
func (p *Profile) Covering(addr uint32, n int) []Sector {
	var out []Sector
	end := uint64(addr) + uint64(n)
	for _, s := range p.Sectors {
		if uint64(s.Addr) < end && uint64(s.Addr)+uint64(s.Size) > uint64(addr) {
			out = append(out, s)
		}
	}
	return out
}
