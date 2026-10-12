// Package esp holds everything ESP32-specific that sits above the generic
// RISC-V debug module: the memory map and write-safety classes, eFuse security
// state (read only), watchdog quiescing, the SPI flash driver, firmware image
// parsing and flash backup.
//
// Register addresses and bit positions come from the Espressif ESP-IDF headers
// (Apache-2.0) and were checked against the real device; see docs/licenses.md.
// Nothing here is part of internal/riscv.
package esp

import (
	"context"
	"fmt"

	"github.com/faronov/hwlab/internal/riscv"
)

// Memory map (ESP32-C6 technical reference manual, "System and Memory").
const (
	ROMBase, ROMEnd       = 0x40000000, 0x4004FFFF
	HPSRAMBase, HPSRAMEnd = 0x40800000, 0x4087FFFF
	LPSRAMBase, LPSRAMEnd = 0x50000000, 0x50003FFF
	FlashMapBase          = 0x42000000
	FlashMapEnd           = 0x42FFFFFF
	PeriphBase, PeriphEnd = 0x60000000, 0x6FFFFFFF

	// ChipMagicAddr holds a chip-family constant in ROM (esptool's detection
	// register).
	ChipMagicAddr = 0x40001000
	// ChipMagicC6 is the value esptool recognises as ESP32-C6.
	ChipMagicC6 = 0x2CE0806F
)

// Class says what kind of memory an address is in.
type Class string

// Address classes.
const (
	ClassSRAM       Class = "sram"
	ClassROM        Class = "rom"
	ClassFlashMap   Class = "flash-mapped"
	ClassPeripheral Class = "peripheral"
	ClassUnmapped   Class = "unmapped"
)

// Classify names the memory region of addr.
func Classify(addr uint32) Class {
	switch {
	case addr >= HPSRAMBase && addr <= HPSRAMEnd, addr >= LPSRAMBase && addr <= LPSRAMEnd:
		return ClassSRAM
	case addr >= ROMBase && addr <= ROMEnd:
		return ClassROM
	case addr >= FlashMapBase && addr <= FlashMapEnd:
		return ClassFlashMap
	case addr >= PeriphBase && addr <= PeriphEnd:
		return ClassPeripheral
	}
	return ClassUnmapped
}

// WritableSRAM reports whether [addr, addr+n) lies entirely inside one SRAM
// block. Generic memory writes are only ever allowed there: ROM and the
// flash-mapped window are not writable memory and peripheral registers have
// side effects.
func WritableSRAM(addr uint32, n int) bool {
	if n <= 0 || uint64(addr)+uint64(n) > 1<<32 {
		return false
	}
	last := addr + uint32(n-1)
	return Classify(addr) == ClassSRAM && Classify(last) == ClassSRAM &&
		(addr <= HPSRAMEnd) == (last <= HPSRAMEnd)
}

// Evidence is what the connected target actually showed, kept separate from
// any board knowledge.
type Evidence struct {
	ROMMagicAddr   uint32 `json:"rom_magic_address"`
	ROMMagic       uint32 `json:"rom_magic"`
	MagicMatchesC6 bool   `json:"rom_magic_matches_esp32c6"`
	Basis          string `json:"basis"`
}

// ReadEvidence reads the ROM chip-magic word through the system bus.
func ReadEvidence(ctx context.Context, dm *riscv.DM) (Evidence, error) {
	b, err := dm.ReadMem(ctx, ChipMagicAddr, 4)
	if err != nil {
		return Evidence{}, err
	}
	v := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	ev := Evidence{ROMMagicAddr: ChipMagicAddr, ROMMagic: v, MagicMatchesC6: v == ChipMagicC6}
	if ev.MagicMatchesC6 {
		ev.Basis = fmt.Sprintf("ROM word at %#x equals the ESP32-C6 chip-detect constant %#x", ChipMagicAddr, ChipMagicC6)
	} else {
		ev.Basis = fmt.Sprintf("ROM word at %#x is %#x, not the ESP32-C6 constant", ChipMagicAddr, v)
	}
	return ev, nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// ReadWord reads one 32-bit register or memory word.
func ReadWord(ctx context.Context, dm *riscv.DM, addr uint32) (uint32, error) {
	r, err := dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusRead, Addr: addr, Words: 1}})
	if err != nil {
		return 0, err
	}
	return r[0][0], nil
}

// WriteWord writes one 32-bit register or memory word. It is deliberately
// unexported-policy-free: callers inside this package are the only ones
// allowed to touch peripheral registers.
func WriteWord(ctx context.Context, dm *riscv.DM, addr, v uint32) error {
	_, err := dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusWrite, Addr: addr, Vals: []uint32{v}}})
	return err
}
