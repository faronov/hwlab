package esp

import (
	"context"
	"fmt"
)

// WriteReport says what WriteRegion actually did.
type WriteReport struct {
	Sectors         int `json:"sectors_in_span"`
	Identical       int `json:"sectors_already_identical"`
	ProgrammedOnly  int `json:"sectors_programmed_without_erase"`
	Erased          int `json:"sectors_erased"`
	BytesProgrammed int `json:"bytes_programmed"`
	BytesPreserved  int `json:"bytes_preserved_outside_region"`
}

// WriteRegion writes data at addr and touches only the 4 KiB sectors that
// need to change. Bytes of an edge sector that lie outside [addr, addr+len)
// are read first and written back unchanged, so unrelated regions survive.
// Every changed sector is read back and compared; any mismatch is an error.
func (f *Flash) WriteRegion(ctx context.Context, addr uint32, data []byte, prog Progress) (WriteReport, error) {
	var r WriteReport
	if err := f.checkRange(addr, len(data)); err != nil {
		return r, err
	}
	start, span := RoundSectors(addr, len(data))
	r.Sectors = span / SectorSize
	r.BytesPreserved = span - len(data)
	for off := 0; off < span; off += SectorSize {
		sa := start + uint32(off)
		cur, err := f.Read(ctx, sa, SectorSize)
		if err != nil {
			return r, fmt.Errorf("read sector %#x: %w", sa, err)
		}
		want := append([]byte(nil), cur...)
		lo, hi := max(int64(addr), int64(sa)), min(int64(addr)+int64(len(data)), int64(sa)+SectorSize)
		copy(want[lo-int64(sa):hi-int64(sa)], data[lo-int64(addr):hi-int64(addr)])
		if string(cur) == string(want) {
			r.Identical++
		} else {
			canOverwrite := true
			for i := range want {
				if cur[i]&want[i] != want[i] { // would need a 0->1 transition
					canOverwrite = false
					break
				}
			}
			if canOverwrite {
				r.ProgrammedOnly++
			} else {
				if err := f.EraseSector(ctx, sa); err != nil {
					return r, fmt.Errorf("erase sector %#x: %w", sa, err)
				}
				r.Erased++
				cur = nil
			}
			if err := f.programSector(ctx, sa, want, cur); err != nil {
				return r, err
			}
			r.BytesProgrammed += SectorSize
			if err := f.Verify(ctx, sa, want); err != nil {
				return r, fmt.Errorf("sector %#x after programming: %w", sa, err)
			}
		}
		if prog != nil {
			prog(off+SectorSize, span)
		}
	}
	return r, nil
}

// programSector programs a whole sector. cur is the current content when no
// erase happened (only changed chunks are written); nil means erased.
func (f *Flash) programSector(ctx context.Context, sa uint32, want, cur []byte) error {
	for o := 0; o < len(want); o += MaxChunk {
		c := want[o : o+MaxChunk]
		if cur == nil && allFF(c) {
			continue
		}
		if cur != nil && string(cur[o:o+MaxChunk]) == string(c) {
			continue
		}
		if err := f.ProgramChunk(ctx, sa+uint32(o), c); err != nil {
			return fmt.Errorf("program %#x: %w", sa+uint32(o), err)
		}
	}
	return nil
}
