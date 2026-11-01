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
	"errors"
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

// Chip is a per-chip profile: everything chip-specific this package touches.
// A profile is selected only when BOTH the JTAG IDCODE and the ROM word at
// ChipMagicAddr match it (Detect); an unknown chip gets no profile, and every
// operation that writes a peripheral register refuses without one. Addresses
// and bit positions come from the ESP-IDF register headers / the svd2rust
// PACs generated from them (esp32c6 0.23, esp32h2 0.19) and the TRMs.
type Chip struct {
	Name string `json:"name"`
	// ImageChipID is the chip id an application/bootloader image for this
	// chip carries in its header (esptool IMAGE_CHIP_ID).
	ImageChipID uint16 `json:"image_chip_id"`
	// IDCode is the JTAG IDCODE of the chip's RISC-V TAP.
	IDCode uint32 `json:"jtag_idcode"`
	// ROMWord is the 32-bit word the mask ROM holds at ChipMagicAddr.
	ROMWord      uint32 `json:"rom_word"`
	ROMWordBasis string `json:"rom_word_basis"`

	Mem MemoryMap `json:"memory_map"`
	WDT WDTMap    `json:"watchdogs"`
	// SPI1 is the base of the SPI1 (SPI_MEM1) flash controller. The flash
	// driver supports only the register layout at SPI1Base (checked equal
	// for both chips: CMD 0x00, ADDR 0x04, CTRL 0x08, USER 0x18, USER1 0x1C,
	// USER2 0x20, MOSI_DLEN 0x24, MISO_DLEN 0x28, W0 0x58, USR bit 18, PE bit
	// 17), so a profile with another base is refused by the plugin before any flash access.
	SPI1 uint32 `json:"spi1_base"`
	// EfuseRepeat0 is EFUSE_RD_REPEAT_DATA0; the security fields read from
	// it and the next three words are at the same bit positions on C6 and H2.
	EfuseRepeat0 uint32 `json:"efuse_rd_repeat_data0"`
}

// MemoryMap holds the address windows used to classify an address.
type MemoryMap struct {
	ROMBase, ROMEnd       uint32
	HPSRAMBase, HPSRAMEnd uint32
	LPSRAMBase, LPSRAMEnd uint32
	FlashMapBase          uint32
	FlashMapEnd           uint32
	PeriphBase, PeriphEnd uint32
}

// WDTMap is the timer-group and RTC (LP) watchdog register map.
type WDTMap struct {
	TIMG0, TIMG1 uint32 // timer group bases (WDT CONFIG0 +0x48, FEED +0x60, WPROTECT +0x64 on both chips)
	LP           uint32 // LP_WDT base
	LPConfig0    uint32 // RWDT CONFIG0 offset (EN bit 31, STG0 bits 28..30, SYS_RESET_LENGTH 13..15, FLASHBOOT_MOD_EN 12)
	LPConfig1    uint32 // RWDT CONFIG1 (stage-0 hold) offset
	LPFeed       uint32 // RWDT FEED offset
	LPProtect    uint32 // RWDT WPROTECT offset
	// SWDConf and SWDProtect are the super-watchdog SWD_CONF / SWD_WPROTECT
	// offsets from LP. Zero means the profile leaves the SWD untouched (the
	// ESP32-C6 behaviour this plugin has always had). When set, Quiesce sets
	// SWD_AUTO_FEED_EN (bit 18) for the halt, as esptool's ESP32H2ROM does,
	// and Restore writes the previous SWD_CONF back.
	SWDConf, SWDProtect uint32
}

// ChipC6 is the ESP32-C6 profile. Its values are exactly the constants this
// package used before profiles existed.
var ChipC6 = Chip{
	Name: "ESP32-C6", ImageChipID: ChipIDESP32C6, IDCode: 0x0000dc25,
	ROMWord: ChipMagicC6, ROMWordBasis: "esptool ESP32C6ROM.CHIP_DETECT_MAGIC_VALUE",
	Mem: MemoryMap{
		ROMBase: ROMBase, ROMEnd: ROMEnd, HPSRAMBase: HPSRAMBase, HPSRAMEnd: HPSRAMEnd,
		LPSRAMBase: LPSRAMBase, LPSRAMEnd: LPSRAMEnd, FlashMapBase: FlashMapBase, FlashMapEnd: FlashMapEnd,
		PeriphBase: PeriphBase, PeriphEnd: PeriphEnd,
	},
	WDT:  WDTMap{TIMG0: 0x60008000, TIMG1: 0x60009000, LP: 0x600B1C00, LPConfig0: 0x00, LPConfig1: 0x04, LPFeed: 0x14, LPProtect: 0x18},
	SPI1: SPI1Base, EfuseRepeat0: 0x600B0830,
}

// ChipH2 is the ESP32-H2 profile. Differences from the C6 that matter here:
// the timer groups moved up one 4 KiB slot (LEDC sits at 0x60008000), LP_WDT
// gained CONFIG5 at +0x14 so FEED/WPROTECT are at +0x18/+0x1C (esptool's
// ESP32H2ROM agrees: WDTWPROTECT 0x600B1C1C), ROM is 128 KiB, HP SRAM 320 KiB
// and LP SRAM 4 KiB. SPI1 and the eFuse security fields are unchanged. esptool
// identifies the H2 through the ROM loader (USES_MAGIC_VALUE=False), so the
// ROM word is the value observed on a revision v1.x ESP32-H2 board; another
// ROM revision is refused as an unknown chip rather than guessed.
var ChipH2 = Chip{
	Name: "ESP32-H2", ImageChipID: ChipIDESP32H2, IDCode: 0x00010c25,
	ROMWord: 0x97E30068, ROMWordBasis: "ROM word observed on an ESP32-H2 v1.x board (esptool does not use a magic value for the H2)",
	Mem: MemoryMap{
		ROMBase: 0x40000000, ROMEnd: 0x4001FFFF, HPSRAMBase: 0x40800000, HPSRAMEnd: 0x4084FFFF,
		LPSRAMBase: 0x50000000, LPSRAMEnd: 0x50000FFF, FlashMapBase: 0x42000000, FlashMapEnd: 0x42FFFFFF,
		PeriphBase: 0x60000000, PeriphEnd: 0x6FFFFFFF,
	},
	WDT:  WDTMap{TIMG0: 0x60009000, TIMG1: 0x6000A000, LP: 0x600B1C00, LPConfig0: 0x00, LPConfig1: 0x04, LPFeed: 0x18, LPProtect: 0x1C, SWDConf: 0x20, SWDProtect: 0x24},
	SPI1: SPI1Base, EfuseRepeat0: 0x600B0830,
}

// Chips lists the supported profiles.
var Chips = []*Chip{&ChipC6, &ChipH2}

// ErrUnknownChip is returned by Detect when no profile matches.
var ErrUnknownChip = errors.New("unknown chip")

// Detect selects the profile whose JTAG IDCODE and ROM word both match. Any
// other combination (an unknown chip, or evidence that disagrees) is refused:
// nothing chip-specific may be written without a profile.
func Detect(idcode, romWord uint32) (*Chip, error) {
	for _, c := range Chips {
		switch {
		case c.IDCode == idcode && c.ROMWord == romWord:
			return c, nil
		case c.IDCode == idcode:
			return nil, fmt.Errorf("%w: JTAG IDCODE 0x%08x is the %s's but the ROM word at %#x is 0x%08x, not 0x%08x", ErrUnknownChip, idcode, c.Name, ChipMagicAddr, romWord, c.ROMWord)
		case c.ROMWord == romWord:
			return nil, fmt.Errorf("%w: the ROM word 0x%08x is the %s's but the JTAG IDCODE is 0x%08x, not 0x%08x", ErrUnknownChip, romWord, c.Name, idcode, c.IDCode)
		}
	}
	return nil, fmt.Errorf("%w: JTAG IDCODE 0x%08x and ROM word 0x%08x at %#x match no supported chip (ESP32-C6, ESP32-H2)", ErrUnknownChip, idcode, romWord, ChipMagicAddr)
}

// Classify names the memory region of addr in this chip's memory map.
func (c *Chip) Classify(addr uint32) Class {
	m := &c.Mem
	switch {
	case addr >= m.HPSRAMBase && addr <= m.HPSRAMEnd, addr >= m.LPSRAMBase && addr <= m.LPSRAMEnd:
		return ClassSRAM
	case addr >= m.ROMBase && addr <= m.ROMEnd:
		return ClassROM
	case addr >= m.FlashMapBase && addr <= m.FlashMapEnd:
		return ClassFlashMap
	case addr >= m.PeriphBase && addr <= m.PeriphEnd:
		return ClassPeripheral
	}
	return ClassUnmapped
}

// WritableSRAM reports whether [addr, addr+n) lies entirely inside one SRAM
// block of this chip.
func (c *Chip) WritableSRAM(addr uint32, n int) bool {
	if n <= 0 || uint64(addr)+uint64(n) > 1<<32 {
		return false
	}
	last := addr + uint32(n-1)
	return c.Classify(addr) == ClassSRAM && c.Classify(last) == ClassSRAM &&
		(addr <= c.Mem.HPSRAMEnd) == (last <= c.Mem.HPSRAMEnd)
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
