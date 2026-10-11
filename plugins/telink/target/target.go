// Package target holds the chip-family drivers of the Telink plugin. The SWire
// transport and the programmer protocol (plugins/telink/sws) know nothing about
// flash geometry; everything chip- or board-specific lives here.
//
// Flash protection fails closed. A board's protection map (where the factory
// EUI-64, calibration and product NV state live) is a property of the board,
// not of the chip, so a layout only becomes active when it is selected
// explicitly AND the flash contents agree with it. Anything else yields an
// unverified Protection, which refuses erase and program.
package target

import (
	"fmt"
	"strings"

	"github.com/faronov/hwlab/plugins/telink/sws"
)

// AutoLayout is the layout name that means "use whatever can be verified".
const AutoLayout = "auto"

// ReadFlash reads n bytes of flash at addr (a safe, read-only operation).
type ReadFlash func(addr uint32, n int) ([]byte, error)

// Family is one Telink chip family driven by the common SWire protocol.
type Family struct {
	Name    string
	ChipIDs []uint16
	// SRAMStart/SRAMEnd bound the writable SRAM window of the largest member.
	SRAMStart, SRAMEnd uint32
	// VariantNote says why members cannot be told apart.
	VariantNote string
	layouts     []layoutDef
}

// layoutDef is a documented board layout. Evidence lists factory regions that a
// genuine board of this layout has programmed: if one reads blank, the board
// is not the one the layout describes.
type layoutDef struct {
	sws.Layout
	Source   string
	Evidence []string
}

// Identity is what is known about the connected target.
type Identity struct {
	ChipID     uint16
	Revision   byte
	Family     string // "unknown" if no driver matches the chip id
	Variant    string // "undetermined" unless the driver can tell
	FlashJEDEC uint32 // 0 if not read
	FlashBytes uint32 // 0 if unknown
}

// Protection is the result of resolving a layout for one target.
type Protection struct {
	Layout   sws.Layout
	Verified bool
	// Reason says why the map is not usable for modification, or what it rests on.
	Reason string
}

var families = []*Family{tlsr825x}

// Lookup returns the driver for a target chip id, or nil.
func Lookup(chipID uint16) *Family {
	for _, f := range families {
		for _, id := range f.ChipIDs {
			if id == chipID {
				return f
			}
		}
	}
	return nil
}

// FlashSize returns the size encoded in a JEDEC id (the last byte is log2 of
// the size), or 0 if the code is implausible.
func FlashSize(jedec uint32) uint32 {
	code := jedec & 0xff
	if code < 10 || code > 24 {
		return 0
	}
	return 1 << code
}

// Identify builds the target identity from the SWire chip id and the flash id.
func Identify(chipID uint16, rev byte, jedec uint32) Identity {
	id := Identity{ChipID: chipID, Revision: rev, Family: "unknown", Variant: "undetermined"}
	if f := Lookup(chipID); f != nil {
		id.Family = f.Name
	}
	id.FlashJEDEC, id.FlashBytes = jedec, FlashSize(jedec)
	return id
}

// LayoutNames lists the named layouts of a family.
func (f *Family) LayoutNames() []string {
	var out []string
	for _, l := range f.layouts {
		out = append(out, l.Name)
	}
	return out
}

// Protection resolves the flash protection map. name is AutoLayout or a named
// layout of the family. flashBytes comes from the flash JEDEC id (0 if
// unknown). read performs the evidence reads and may be nil.
func (f *Family) Protection(name string, flashBytes uint32, read ReadFlash) Protection {
	unverified := func(reason string) Protection {
		return Protection{Layout: sws.Layout{Name: "unverified", Size: flashBytes}, Reason: reason}
	}
	if f == nil {
		return unverified("no driver for this target chip: its flash protection map is unknown")
	}
	if name == "" || name == AutoLayout {
		return unverified(fmt.Sprintf("protection map of this board is not verified; select one of %s only if the board is known to match it", strings.Join(f.LayoutNames(), ", ")))
	}
	var def *layoutDef
	for i := range f.layouts {
		if f.layouts[i].Name == name {
			def = &f.layouts[i]
		}
	}
	if def == nil {
		return unverified(fmt.Sprintf("unknown layout %q (want %s)", name, strings.Join(f.LayoutNames(), ", ")))
	}
	if flashBytes == 0 {
		return unverified("flash size unknown (JEDEC id unreadable; halt the CPU and retry)")
	}
	if flashBytes != def.Size {
		return unverified(fmt.Sprintf("flash is %d KiB but layout %s describes %d KiB", flashBytes>>10, def.Name, def.Size>>10))
	}
	if read == nil {
		return unverified("layout evidence cannot be read")
	}
	for _, rn := range def.Evidence {
		for _, r := range def.Regions {
			if r.Name != rn {
				continue
			}
			b, err := read(r.Start, 16)
			if err != nil {
				return unverified(fmt.Sprintf("cannot read %s at 0x%05x to confirm layout %s: %v", r.Name, r.Start, def.Name, err))
			}
			if blank(b) {
				return unverified(fmt.Sprintf("%s at 0x%05x reads blank (0xFF): this board does not carry the factory block layout %s expects, so that map is not confirmed", r.Name, r.Start, def.Name))
			}
		}
	}
	return Protection{Layout: def.Layout, Verified: true, Reason: "layout " + def.Name + " (" + def.Source + ") matches flash size and factory evidence"}
}

func blank(b []byte) bool {
	for _, c := range b {
		if c != 0xff {
			return false
		}
	}
	return len(b) > 0
}

// ValidLayout reports whether name is AutoLayout or a layout of any family.
func ValidLayout(name string) bool {
	if name == AutoLayout {
		return true
	}
	for _, f := range families {
		for _, n := range f.LayoutNames() {
			if n == name {
				return true
			}
		}
	}
	return false
}

// AllLayoutNames lists AutoLayout and every named layout.
func AllLayoutNames() []string {
	out := []string{AutoLayout}
	for _, f := range families {
		out = append(out, f.LayoutNames()...)
	}
	return out
}
