package sws

import "fmt"

// Class says what a flash region holds, which decides how it may be touched.
type Class string

const (
	ClassApp     Class = "app"     // application image: writable by default
	ClassJournal Class = "journal" // product NV state: writable only on explicit opt-in
	ClassFactory Class = "factory" // EUI-64, config, ADC calibration: never written or erased
)

// Region is a half-open flash range [Start, End).
type Region struct {
	Name       string
	Start, End uint32
	Class      Class
}

// Layout is the flash map of one board. It exists to stop the tool from
// destroying data that cannot be regenerated, such as the factory EUI-64.
type Layout struct {
	Name    string
	Size    uint32
	Regions []Region
}

// Policy is what the caller has explicitly allowed beyond the default.
type Policy struct {
	// Journals allows writing the NV journal regions.
	Journals bool
}

// ExpandToSectors widens [addr, addr+n) to whole erase sectors, which is what
// an erase or an erasing program really touches.
func ExpandToSectors(addr, n uint32) (start, end uint32) {
	start = addr &^ (FlashSector - 1)
	end = (addr + n + FlashSector - 1) &^ (FlashSector - 1)
	return
}

// CheckModify decides whether the sectors covering [addr, addr+n) may be
// erased or programmed. Flash that the layout does not describe is refused.
func (l Layout) CheckModify(addr, n uint32, pol Policy) error {
	if n == 0 {
		return fmt.Errorf("empty flash range")
	}
	if uint64(addr)+uint64(n) > uint64(l.Size) {
		return fmt.Errorf("flash range 0x%x+0x%x is outside the %s layout (size 0x%x)", addr, n, l.Name, l.Size)
	}
	start, end := ExpandToSectors(addr, n)
	covered := start
	for _, r := range l.Regions {
		if r.End <= start || r.Start >= end {
			continue
		}
		switch r.Class {
		case ClassFactory:
			return fmt.Errorf("refusing to modify %s (0x%05x-0x%05x): factory data (EUI-64, config, calibration) is never erased or overwritten", r.Name, r.Start, r.End)
		case ClassJournal:
			if !pol.Journals {
				return fmt.Errorf("refusing to modify %s (0x%05x-0x%05x): product NV state; pass allow_journals to widen the range deliberately", r.Name, r.Start, r.End)
			}
		}
		if r.Start > covered {
			return fmt.Errorf("flash at 0x%05x is not described by the %s layout", covered, l.Name)
		}
		if r.End > covered {
			covered = r.End
		}
	}
	if covered < end {
		return fmt.Errorf("flash at 0x%05x-0x%05x is not described by the %s layout", covered, end, l.Name)
	}
	return nil
}

// CheckCapacity compares the layout size with the capacity code of a JEDEC id
// (the last byte is log2 of the size in bytes). A mismatch means the board is
// not the one the layout describes, so no modification is allowed.
func (l Layout) CheckCapacity(jedec uint32) error {
	code := jedec & 0xff
	if code < 10 || code > 30 {
		return fmt.Errorf("unusable JEDEC capacity code 0x%02x in id 0x%06x", code, jedec)
	}
	if size := uint32(1) << code; size != l.Size {
		return fmt.Errorf("flash is %d KiB (JEDEC 0x%06x) but the %s layout describes %d KiB; refusing to modify flash", size>>10, jedec, l.Name, l.Size>>10)
	}
	return nil
}
