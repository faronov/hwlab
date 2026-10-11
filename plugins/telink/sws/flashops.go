package sws

import (
	"bytes"
	"fmt"
)

// FlashReport says what a flash operation did, including the persistent
// side effect of unlocking the flash.
type FlashReport struct {
	Start, End     uint32 // sector-aligned range actually erased
	SectorsErased  int
	PagesWritten   int
	PagesSkipped   int // all-0xFF pages need no write after an erase
	UnlockedFlash  bool
	VerifiedBytes  int
	JEDEC          uint32
	FlashSizeBytes uint32
}

func (p *Programmer) prepareModify(l Layout, addr, n uint32, pol Policy) (FlashReport, error) {
	var rep FlashReport
	if err := l.CheckModify(addr, n, pol); err != nil {
		return rep, err
	}
	id, err := p.JEDEC()
	if err != nil {
		return rep, fmt.Errorf("read flash id: %w", err)
	}
	if err := l.CheckCapacity(id); err != nil {
		return rep, err
	}
	rep.JEDEC, rep.FlashSizeBytes = id, uint32(1)<<(id&0xff)
	rep.Start, rep.End = ExpandToSectors(addr, n)
	if err := p.UnlockFlash(); err != nil {
		return rep, err
	}
	rep.UnlockedFlash = true
	return rep, nil
}

// EraseRange erases the sectors covering [addr, addr+n) after the layout
// guards pass. It unlocks the flash first (see UnlockFlash).
func (p *Programmer) EraseRange(l Layout, addr, n uint32, pol Policy) (FlashReport, error) {
	rep, err := p.prepareModify(l, addr, n, pol)
	if err != nil {
		return rep, err
	}
	for a := rep.Start; a < rep.End; a += FlashSector {
		if err := p.EraseSector(a); err != nil {
			return rep, err
		}
		rep.SectorsErased++
	}
	return rep, nil
}

// Program erases the sectors covering the image, writes it page by page and
// reads it back. addr must be sector aligned so no neighbouring data in the
// first sector is lost.
func (p *Programmer) Program(l Layout, addr uint32, image []byte, pol Policy) (FlashReport, error) {
	if addr&(FlashSector-1) != 0 {
		return FlashReport{}, fmt.Errorf("program address 0x%x is not 4 KiB aligned: erasing would destroy the rest of its sector", addr)
	}
	rep, err := p.prepareModify(l, addr, uint32(len(image)), pol)
	if err != nil {
		return rep, err
	}
	for a := rep.Start; a < rep.End; a += FlashSector {
		if err := p.EraseSector(a); err != nil {
			return rep, err
		}
		rep.SectorsErased++
	}
	for off := 0; off < len(image); off += FlashPage {
		end := off + FlashPage
		if end > len(image) {
			end = len(image)
		}
		page := image[off:end]
		if allFF(page) {
			rep.PagesSkipped++
			continue
		}
		if err := p.WriteFlashPage(addr+uint32(off), page); err != nil {
			return rep, err
		}
		rep.PagesWritten++
	}
	got, err := p.FlashRead(addr, len(image))
	if err != nil {
		return rep, fmt.Errorf("verify read: %w", err)
	}
	if !bytes.Equal(got, image) {
		return rep, fmt.Errorf("verify failed: flash differs from the image at 0x%x", addr+uint32(firstDiff(got, image)))
	}
	rep.VerifiedBytes = len(image)
	return rep, nil
}

func allFF(b []byte) bool {
	for _, v := range b {
		if v != 0xff {
			return false
		}
	}
	return true
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return 0
}
