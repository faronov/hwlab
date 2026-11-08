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
func ReadEvidence(ctx context.Context, dm Bus) (Evidence, error) {
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
	// SPI1 is the base of the SPI1 (SPI_MEM1) flash controller. The register
	// layout behind it is the same on every profiled chip (CMD 0x00, ADDR
	// 0x04, CTRL 0x08, USER 0x18, USER1 0x1C, USER2 0x20, MOSI_DLEN 0x24,
	// MISO_DLEN 0x28, W0 0x58, USR bit 18, PE bit 17; OpenFlashChip).
	SPI1 uint32 `json:"spi1_base"`
	// EfuseRepeat0 is EFUSE_RD_REPEAT_DATA0; the security fields read from
	// it and the next three words are at the same bit positions on C6, H2
	// and S3, except DIS_USB_JTAG (EfuseDisUSBJTAG).
	EfuseRepeat0 uint32 `json:"efuse_rd_repeat_data0"`
	// EfuseDisUSBJTAG is where DIS_USB_JTAG sits in the RD_REPEAT words; nil
	// means word 0 bit 9 (ESP32-C6/H2).
	EfuseDisUSBJTAG *EfuseBit `json:"efuse_dis_usb_jtag,omitempty"`
	// MMUTable is the flash MMU table (one 32-bit entry per 64 KiB page of
	// the external-memory windows); zero means not profiled. Used only to
	// tell whether the CPU's flash window is mapped before reading it.
	MMUTable uint32 `json:"mmu_table,omitempty"`
	// FlashPins are the IO_MUX registers of the SPI flash pins (MCU_SEL
	// field bits 12..14, function 0 = SPI) that OpenFlashChip switches to the
	// SPI function while it has the controller and puts back on Close; nil
	// means the pins need no attach (ESP32-C6/H2: never needed on the boards
	// verified). The ESP32-S3 ROM download mode leaves them in GPIO mode, so
	// SPI1 sees no flash chip until they are attached (esptool calls the ROM
	// SPI_ATTACH for the same reason).
	FlashPins []uint32 `json:"flash_pins_iomux,omitempty"`
	// ROMSPIAttach is the mask-ROM entry of spi_flash_attach
	// (esp_rom_spiflash_attach) from the ESP-IDF ROM linker script. It is
	// called only when the flash does not answer while the hart is halted in
	// ROM (the download loader has not attached it yet). Zero means the
	// profile never calls it, and a flash that does not answer stays an error.
	ROMSPIAttach uint32 `json:"rom_spi_flash_attach,omitempty"`
}

// EfuseBit is a bit position in the EFUSE_RD_REPEAT_DATA0.. words.
type EfuseBit struct {
	Word int `json:"word"`
	Bit  int `json:"bit"`
}

// MemoryMap holds the address windows used to classify an address.
type MemoryMap struct {
	ROMBase, ROMEnd       uint32
	HPSRAMBase, HPSRAMEnd uint32
	LPSRAMBase, LPSRAMEnd uint32
	FlashMapBase          uint32
	FlashMapEnd           uint32
	PeriphBase, PeriphEnd uint32
	// Windows, when set, replaces the fixed fields above for Classify and
	// WritableSRAM: the first window containing an address decides its class,
	// and a write must stay inside one SRAM window. Chips whose memory map does
	// not fit the fixed fields (the ESP32-S3 has separate instruction- and
	// data-bus views of the same SRAM) use it; C6 and H2 do not.
	Windows []Window `json:",omitempty"`
}

// Window is one named address range of a memory map.
type Window struct {
	Name  string `json:"name"`
	Base  uint32 `json:"base"`
	End   uint32 `json:"end"` // inclusive
	Class Class  `json:"class"`
	// Exec marks a window the CPU fetches instructions from (an instruction
	// breakpoint elsewhere can never hit).
	Exec bool `json:"exec,omitempty"`
}

func (m *MemoryMap) window(addr uint32) *Window {
	for i := range m.Windows {
		if w := &m.Windows[i]; addr >= w.Base && addr <= w.End {
			return w
		}
	}
	return nil
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
	// SWD_AUTO_FEED_EN (SWDAutoFeed) for the halt, as esptool's ESP32H2ROM
	// and ESP32S3ROM do, and Restore writes the previous SWD_CONF back.
	SWDConf, SWDProtect uint32
	// SWDAutoFeed is the SWD_AUTO_FEED_EN bit of SWD_CONF; zero means bit 18
	// (the ESP32-H2 position).
	SWDAutoFeed uint32 `json:",omitempty"`
	// SWDKey is the SWD_WPROTECT unlock key; zero means the watchdog key
	// 0x50D83AA1 (ESP32-H2). The ESP32-S3 uses 0x8F1D312A.
	SWDKey uint32 `json:",omitempty"`
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
	// esp32h2.rom.ld (ESP-IDF v5.x, esp-rom-sys 0.1.4): spi_flash_attach = 0x400001d4
	ROMSPIAttach: 0x400001d4,
}

// ChipS3 is the ESP32-S3 profile (dual Xtensa LX7; reached through the
// Xtensa debug module, not a RISC-V DM). Sources: ESP32-S3 TRM "System and
// Memory" for the address map; esptool ESP32S3ROM for IMAGE_CHIP_ID 9, SPI1
// 0x60002000, RTC_CNTL WDT offsets (CONFIG0 0x98, CONFIG1 0x9C, FEED 0xAC,
// WPROTECT 0xB0, SWD_CONF 0xB4 with AUTO_FEED bit 31, SWD_WPROTECT 0xB8) and
// the timer groups at 0x6001F000/0x60020000; all WDT words were read back on
// a real ESP32-S3 in download mode (docs/notes). The ROM word 0x00000009 at
// 0x40001000 was observed on that board (older esptool listed it as the S3
// CHIP_DETECT_MAGIC_VALUE). ESP-IDF v5.3 soc/esp32s3 headers confirm every
// register used: reg_base.h (SPI1 0x60002000, EFUSE 0x60007000, RTCCNTL
// 0x60008000, TIMG0/1 0x6001F000/0x60020000), spi_mem_reg.h (same SPI1
// layout as the C6), rtc_cntl_reg.h (WDTCONFIG0 0x98, CONFIG1 0x9C, FEED 0xAC
// bit 31, WPROTECT 0xB0, SWD_CONF 0xB4 with SWD_AUTO_FEED_EN bit 31,
// SWD_WPROTECT 0xB8 with key 0x8F1D312A), timer_group_reg.h (WDTCONFIG0
// 0x48, FEED 0x60, WPROTECT 0x64) and efuse_reg.h / esp_efuse_table.csv
// (RD_REPEAT_DATA0 0x60007030; SOFT_DIS_JTAG BLK0 bit 48, DIS_PAD_JTAG 51,
// SPI_BOOT_CRYPT_CNT 82..84, SECURE_BOOT_EN 116, DIS_USB_JTAG 118,
// DIS_DOWNLOAD_MODE 128, ENABLE_SECURITY_DOWNLOAD 133, i.e. the C6 positions
// except DIS_USB_JTAG at word 2 bit 22). ext_mem_defs.h: MMU table
// 0x600C5000. io_mux_reg.h / spi_pins.h: the flash pins GPIO27..32 at IO_MUX
// 0x60009070..0x84, SPI function 0 (download mode leaves them on function 1,
// GPIO; observed).
var ChipS3 = Chip{
	Name: "ESP32-S3", ImageChipID: ChipIDESP32S3, IDCode: 0x120034e5,
	ROMWord: 0x00000009, ROMWordBasis: "ROM word observed on an ESP32-S3 board (older esptool ESP32S3ROM.CHIP_DETECT_MAGIC_VALUE [0x9])",
	Mem: MemoryMap{
		ROMBase: 0x40000000, ROMEnd: 0x4005FFFF, HPSRAMBase: 0x3FC88000, HPSRAMEnd: 0x3FCFFFFF,
		LPSRAMBase: 0x50000000, LPSRAMEnd: 0x50001FFF, FlashMapBase: 0x42000000, FlashMapEnd: 0x43FFFFFF,
		PeriphBase: 0x60000000, PeriphEnd: 0x600D0FFF,
		Windows: []Window{
			{"ROM0 (IBUS)", 0x40000000, 0x4003FFFF, ClassROM, true},
			{"ROM1 (IBUS)", 0x40040000, 0x4005FFFF, ClassROM, true},
			{"ROM1 (DBUS)", 0x3FF00000, 0x3FF1FFFF, ClassROM, false},
			{"SRAM0 (IBUS)", 0x40370000, 0x40377FFF, ClassSRAM, true},
			{"SRAM1 (IBUS)", 0x40378000, 0x403DFFFF, ClassSRAM, true},
			{"SRAM1 (DBUS)", 0x3FC88000, 0x3FCEFFFF, ClassSRAM, false},
			{"SRAM2 (DBUS)", 0x3FCF0000, 0x3FCFFFFF, ClassSRAM, false},
			{"RTC FAST memory", 0x600FE000, 0x600FFFFF, ClassSRAM, true},
			{"RTC SLOW memory", 0x50000000, 0x50001FFF, ClassSRAM, false},
			{"external memory (DBUS)", 0x3C000000, 0x3DFFFFFF, ClassFlashMap, false},
			{"external memory (IBUS)", 0x42000000, 0x43FFFFFF, ClassFlashMap, true},
			{"peripherals", 0x60000000, 0x600D0FFF, ClassPeripheral, false},
		},
	},
	WDT: WDTMap{TIMG0: 0x6001F000, TIMG1: 0x60020000, LP: 0x60008000, LPConfig0: 0x98, LPConfig1: 0x9C, LPFeed: 0xAC, LPProtect: 0xB0,
		SWDConf: 0xB4, SWDProtect: 0xB8, SWDAutoFeed: 1 << 31, SWDKey: 0x8F1D312A},
	SPI1: 0x60002000, EfuseRepeat0: 0x60007030, EfuseDisUSBJTAG: &EfuseBit{Word: 2, Bit: 22},
	MMUTable: 0x600C5000,
	// IO_MUX (DR_REG_IO_MUX_BASE 0x60009000) SPIHD/SPIWP/SPICS0/SPICLK/SPIQ/SPID
	// = GPIO27..32 at +0x70..+0x84 (io_mux_reg.h, spi_pins.h SPI_FUNC_NUM 0).
	// SPICS1 (GPIO26, PSRAM chip select) is left alone.
	FlashPins: []uint32{0x60009070, 0x60009074, 0x60009078, 0x6000907C, 0x60009080, 0x60009084},
}

// Chips lists the supported profiles.
var Chips = []*Chip{&ChipC6, &ChipH2, &ChipS3}

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
	return nil, fmt.Errorf("%w: JTAG IDCODE 0x%08x and ROM word 0x%08x at %#x match no supported chip (ESP32-C6, ESP32-H2, ESP32-S3)", ErrUnknownChip, idcode, romWord, ChipMagicAddr)
}

// Classify names the memory region of addr in this chip's memory map.
func (c *Chip) Classify(addr uint32) Class {
	m := &c.Mem
	if len(m.Windows) > 0 {
		if w := m.window(addr); w != nil {
			return w.Class
		}
		return ClassUnmapped
	}
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

// Executable reports whether the CPU can fetch instructions at addr: ROM,
// SRAM and the flash-mapped window, restricted to the instruction-bus
// windows on chips with a split memory map.
func (c *Chip) Executable(addr uint32) bool {
	if len(c.Mem.Windows) > 0 {
		w := c.Mem.window(addr)
		return w != nil && w.Exec
	}
	switch c.Classify(addr) {
	case ClassROM, ClassFlashMap, ClassSRAM:
		return true
	}
	return false
}

// WritableSRAM reports whether [addr, addr+n) lies entirely inside one SRAM
// block of this chip.
func (c *Chip) WritableSRAM(addr uint32, n int) bool {
	if n <= 0 || uint64(addr)+uint64(n) > 1<<32 {
		return false
	}
	last := addr + uint32(n-1)
	if len(c.Mem.Windows) > 0 {
		w := c.Mem.window(addr)
		return w != nil && w.Class == ClassSRAM && last <= w.End
	}
	return c.Classify(addr) == ClassSRAM && c.Classify(last) == ClassSRAM &&
		(addr <= c.Mem.HPSRAMEnd) == (last <= c.Mem.HPSRAMEnd)
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// ReadWord reads one 32-bit register or memory word.
func ReadWord(ctx context.Context, dm Bus, addr uint32) (uint32, error) {
	r, err := dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusRead, Addr: addr, Words: 1}})
	if err != nil {
		return 0, err
	}
	return r[0][0], nil
}

// WriteWord writes one 32-bit register or memory word. It is deliberately
// unexported-policy-free: callers inside this package are the only ones
// allowed to touch peripheral registers.
func WriteWord(ctx context.Context, dm Bus, addr, v uint32) error {
	_, err := dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusWrite, Addr: addr, Vals: []uint32{v}}})
	return err
}
