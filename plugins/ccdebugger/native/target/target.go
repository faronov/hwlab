// Package target holds the pure, hardware-independent description of the TI
// 8051 flash targets a CC-Debugger can program: chip identification, memory
// geometry, lock-bit decoding and address helpers.
//
// Sources: TI SWRU191F (CC253x/4x user's guide: debug interface, flash
// controller, lock bits, chip-information registers) and, for the secondary
// IEEE address placement, a read-only comparison against a real CC2530. This
// package does not derive from the GPL-2.0 cc-tool sources.
//
// Only the CC2530 is marked programmable. The other CC253x/CC254x chips are
// recognised so that a mismatch is reported precisely, and so that further
// families can be added later without touching the USB transport.
package target

import (
	"errors"
	"fmt"
)

// Family names a group of chips sharing one debug and flash-controller model.
type Family string

// Families. FamilyCC251x111x and FamilyCC243x are reserved for future use.
const (
	FamilyCC253x254x Family = "cc253x254x"
	FamilyCC251x111x Family = "cc251x111x"
	FamilyCC243x     Family = "cc243x"
)

// Fixed layout of the CC253x/CC254x family.
const (
	WordSize       = 4      // flash write granularity
	BankSize       = 0x8000 // CODE bank window
	LockSize       = 16     // lock-bit structure at the end of flash
	IEEESize       = 8
	InfoPageXDATA  = 0x7800 // read-only 2 KB information page
	InfoPageSize   = 0x800
	SRAMWriteLimit = 0x1E00 // RAM below this address is safe scratch
	CHVERAddr      = 0x6249 // XDATA chip-information registers
	CHIPIDAddr     = 0x624A
	CHIPINFO0Addr  = 0x6276
	CHIPINFO1Addr  = 0x6277
)

var (
	// ErrUnknownChip is returned for a chip id that is not in the table.
	ErrUnknownChip = errors.New("unknown chip id")
	// ErrMalformed is returned when the chip-information bytes are inconsistent.
	ErrMalformed = errors.New("inconsistent chip information")
)

// Chip is a table entry.
type Chip struct {
	Name         string
	Family       Family
	ChipID       uint8
	PageSize     int
	PrimaryIEEE  int  // XDATA address of the primary IEEE in the info page
	Programmable bool // flash erase/program implemented for this chip
}

var chips = []Chip{
	{"CC2530", FamilyCC253x254x, 0xA5, 2048, 0x780C, true},
	{"CC2531", FamilyCC253x254x, 0xB5, 2048, 0x780C, false},
	{"CC2533", FamilyCC253x254x, 0x95, 1024, 0x780C, false},
	{"CC2540", FamilyCC253x254x, 0x8D, 2048, 0x780E, false},
	{"CC2541", FamilyCC253x254x, 0x41, 2048, 0x780E, false},
}

// Chips returns a copy of the known-chip table.
func Chips() []Chip { return append([]Chip(nil), chips...) }

// Geometry is what the hardware reported, decoded.
type Geometry struct {
	Chip         string `json:"chip"`
	Family       Family `json:"family"`
	ChipID       uint8  `json:"chip_id"`
	Revision     uint8  `json:"revision"`
	FlashSize    int    `json:"flash_size"`
	RAMSize      int    `json:"ram_size"`
	PageSize     int    `json:"page_size"`
	Pages        int    `json:"pages"`
	Banks        int    `json:"banks"`
	USB          bool   `json:"usb"`
	Programmable bool   `json:"programmable"`
	PrimaryIEEE  int    `json:"primary_ieee_xdata"`
}

// Decode builds a Geometry from the four chip-information registers
// (CHIPID, CHVER, CHIPINFO0, CHIPINFO1). It never trusts a requested name.
func Decode(chipID, chver, info0, info1 uint8) (Geometry, error) {
	var c *Chip
	for i := range chips {
		if chips[i].ChipID == chipID {
			c = &chips[i]
		}
	}
	if c == nil {
		return Geometry{}, fmt.Errorf("%w 0x%02X", ErrUnknownChip, chipID)
	}
	if info0&0x04 == 0 || info0&0x80 != 0 {
		return Geometry{}, fmt.Errorf("%w: CHIPINFO0=0x%02X (bit 2 must be 1, bit 7 0)", ErrMalformed, info0)
	}
	kb := map[uint8]int{1: 32, 2: 64, 3: 128, 4: 256}[info0>>4&7]
	if kb == 0 {
		return Geometry{}, fmt.Errorf("%w: CHIPINFO0=0x%02X flash size code %d", ErrMalformed, info0, info0>>4&7)
	}
	g := Geometry{
		Chip: c.Name, Family: c.Family, ChipID: chipID, Revision: chver,
		FlashSize: kb * 1024, RAMSize: (int(info1&7) + 1) * 1024, PageSize: c.PageSize,
		USB: info0&0x08 != 0, Programmable: c.Programmable, PrimaryIEEE: c.PrimaryIEEE,
	}
	g.Pages = g.FlashSize / g.PageSize
	g.Banks = (g.FlashSize + BankSize - 1) / BankSize
	return g, nil
}

// LockPage is the index of the lock-bit page, the last flash page.
func (g Geometry) LockPage() int { return g.Pages - 1 }

// LockPageAddr is the physical start of the lock-bit page.
func (g Geometry) LockPageAddr() int { return g.LockPage() * g.PageSize }

// LockAddr is the physical address of the 16-byte lock-bit structure.
func (g Geometry) LockAddr() int { return g.FlashSize - LockSize }

// SecondaryIEEEAddr is the physical address of the secondary IEEE address,
// the 8 bytes directly below the lock-bit structure.
func (g Geometry) SecondaryIEEEAddr() int { return g.LockAddr() - IEEESize }

// PageOf returns the page index of a physical flash address.
func (g Geometry) PageOf(addr int) (int, error) {
	if addr < 0 || addr >= g.FlashSize {
		return 0, fmt.Errorf("flash address 0x%X outside 0..0x%X", addr, g.FlashSize-1)
	}
	return addr / g.PageSize, nil
}

// PageRange returns the inclusive page range covering [addr, addr+length).
func (g Geometry) PageRange(addr, length int) (first, last int, err error) {
	if length < 1 {
		return 0, 0, errors.New("empty range")
	}
	if first, err = g.PageOf(addr); err != nil {
		return
	}
	last, err = g.PageOf(addr + length - 1)
	return
}

// FlashWordAddr converts a physical byte address to the 16-bit word address
// the flash controller (FADDRH:FADDRL) expects. addr must be word aligned.
func (g Geometry) FlashWordAddr(addr int) (uint16, error) {
	if addr < 0 || addr >= g.FlashSize || addr%WordSize != 0 {
		return 0, fmt.Errorf("flash write address 0x%X is not a word-aligned address below 0x%X", addr, g.FlashSize)
	}
	return uint16(addr / WordSize), nil
}

// Bank returns the CODE bank holding a physical address and the offset of the
// address inside the 32 KiB window.
func Bank(addr int) (bank, offset int) { return addr / BankSize, addr % BankSize }

// Lock is the decoded lock-bit structure.
type Lock struct {
	Raw         [LockSize]byte `json:"-"`
	DebugLocked bool           `json:"debug_locked"`
	LockedPages []int          `json:"locked_pages"`
	// LockPageLocked is always reported true: the lock-bit page is implicitly
	// locked outside debug mode.
	LockPageLocked bool `json:"lock_page_implicitly_locked"`
}

// DecodeLock interprets the 16 bytes at LockAddr. Bit n of the structure is
// the lock bit of page n (0 = locked); bit 127 is DBGLOCK (0 = locked).
func DecodeLock(g Geometry, raw []byte) (Lock, error) {
	var l Lock
	if len(raw) != LockSize {
		return l, fmt.Errorf("lock structure must be %d bytes, got %d", LockSize, len(raw))
	}
	copy(l.Raw[:], raw)
	l.DebugLocked = raw[LockSize-1]&0x80 == 0
	l.LockPageLocked = true
	l.LockedPages = []int{}
	for p := 0; p < g.Pages-1 && p < LockSize*8-1; p++ {
		if raw[p/8]>>(p%8)&1 == 0 {
			l.LockedPages = append(l.LockedPages, p)
		}
	}
	return l, nil
}

// PageLocked reports whether the page-lock bit forbids erasing or writing a
// page outside debug mode. The lock page itself is handled by the caller.
func (l Lock) PageLocked(page int) bool {
	for _, p := range l.LockedPages {
		if p == page {
			return true
		}
	}
	return false
}

// IEEE formats an 8-byte address stored LSB first as the usual MSB-first hex.
func IEEE(raw []byte) string {
	if len(raw) != IEEESize {
		return ""
	}
	return fmt.Sprintf("%02X%02X%02X%02X%02X%02X%02X%02X",
		raw[7], raw[6], raw[5], raw[4], raw[3], raw[2], raw[1], raw[0])
}
