package esp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/faronov/hwlab/internal/riscv"
)

// Info is the flash identity and geometry as discovered, with the evidence
// each value came from.
type Info struct {
	JEDEC        string `json:"jedec_id"`
	Manufacturer string `json:"manufacturer"`
	MemoryType   string `json:"memory_type"`
	CapacityCode string `json:"capacity_code"`
	SizeBytes    int    `json:"size_bytes"`
	SizeBasis    string `json:"size_basis"`
	JEDECSize    int    `json:"jedec_size_bytes,omitempty"`
	SFDPSize     int    `json:"sfdp_size_bytes,omitempty"`
	SFDPRev      string `json:"sfdp_revision,omitempty"`
	StatusReg    byte   `json:"status_register"`
	SectorSize   int    `json:"sector_size"`
	BlockSize    int    `json:"block_size"`
	PageSize     int    `json:"page_size"`
}

var manufacturers = map[byte]string{
	0xEF: "Winbond", 0xC8: "GigaDevice", 0x20: "XMC/Micron", 0xA1: "Fudan Micro", 0xC2: "Macronix",
	0x1C: "EON", 0x68: "Boya", 0x85: "Puya", 0x0B: "XTX", 0x5E: "Zbit", 0x9D: "ISSI", 0x01: "Spansion/Cypress",
	0x25: "Tristate/Hua Hong", 0x4A: "Zbit/ESMT",
}

// ReadInfo reads the JEDEC id, the status register and the SFDP table and
// derives the size from SFDP (preferred) or the JEDEC capacity code.
func (f *Flash) ReadInfo(ctx context.Context) (Info, error) {
	id, err := f.JEDEC(ctx)
	if err != nil {
		return Info{}, err
	}
	if id == [3]byte{} || id == [3]byte{0xFF, 0xFF, 0xFF} {
		return Info{}, fmt.Errorf("flash: JEDEC id %x says no flash chip answers", id)
	}
	st, err := f.Status(ctx)
	if err != nil {
		return Info{}, err
	}
	in := Info{
		JEDEC: hex.EncodeToString(id[:]), StatusReg: st, SectorSize: SectorSize, BlockSize: BlockSize, PageSize: PageSize,
		Manufacturer: manufacturers[id[0]], MemoryType: fmt.Sprintf("0x%02x", id[1]), CapacityCode: fmt.Sprintf("0x%02x", id[2]),
	}
	if in.Manufacturer == "" {
		in.Manufacturer = fmt.Sprintf("unknown (0x%02x)", id[0])
	}
	if c := int(id[2]); c >= 0x10 && c <= 0x1F {
		in.JEDECSize = 1 << c
	}
	if sz, rev, ok := f.sfdpSize(ctx); ok {
		in.SFDPSize, in.SFDPRev = sz, rev
	}
	switch {
	case in.SFDPSize > 0 && (in.JEDECSize == 0 || in.SFDPSize == in.JEDECSize):
		in.SizeBytes, in.SizeBasis = in.SFDPSize, "SFDP basic parameter table density (JEDEC capacity code agrees)"
	case in.SFDPSize > 0:
		in.SizeBytes, in.SizeBasis = in.SFDPSize, "SFDP basic parameter table density (DISAGREES with the JEDEC capacity code)"
	case in.JEDECSize > 0:
		in.SizeBytes, in.SizeBasis = in.JEDECSize, "JEDEC capacity code 2^n (no SFDP)"
	default:
		return in, fmt.Errorf("flash: cannot determine the size (JEDEC %x, no SFDP)", id)
	}
	f.size = in.SizeBytes
	return in, nil
}

func (f *Flash) sfdpSize(ctx context.Context) (int, string, bool) {
	h, err := f.SFDP(ctx, 0, 16)
	if err != nil || string(h[:4]) != "SFDP" {
		return 0, "", false
	}
	rev := fmt.Sprintf("%d.%d", h[5], h[4])
	// First parameter header at 8: id lsb, minor, major, length(dwords), 24-bit pointer.
	if h[8] != 0x00 || h[11] < 2 {
		return 0, rev, false
	}
	ptr := uint32(h[12]) | uint32(h[13])<<8 | uint32(h[14])<<16
	t, err := f.SFDP(ctx, ptr, 8)
	if err != nil {
		return 0, rev, false
	}
	d := le32(t[4:8])
	var bits uint64
	if d&(1<<31) == 0 {
		bits = uint64(d) + 1
	} else if d&0x7FFFFFFF < 40 {
		bits = 1 << (d & 0x7FFFFFFF)
	} else {
		return 0, rev, false
	}
	return int(bits / 8), rev, true
}

// Size is the flash size found by ReadInfo.
func (f *Flash) Size() int { return f.size }

// SetSize overrides the size (tests, or a caller that already knows it).
func (f *Flash) SetSize(n int) { f.size = n }

// CheckRange validates [addr, addr+n) against the flash size.
func (f *Flash) CheckRange(addr uint32, n int) error { return f.checkRange(addr, n) }

func (f *Flash) checkRange(addr uint32, n int) error {
	if f.size <= 0 {
		return fmt.Errorf("flash: size unknown; call ReadInfo first")
	}
	if n <= 0 {
		return fmt.Errorf("%w: zero-length request", ErrRange)
	}
	if uint64(addr)+uint64(n) > uint64(f.size) {
		return fmt.Errorf("%w: [%#x, %#x) exceeds the %d-byte flash", ErrRange, addr, uint64(addr)+uint64(n), f.size)
	}
	return nil
}

// readBatch is how many 64-byte SPI transactions are pipelined into one
// transfer.
const readBatch = 6

// Read reads n bytes of physical flash through the SPI controller. Any
// address and length inside the flash are allowed.
func (f *Flash) Read(ctx context.Context, addr uint32, n int) ([]byte, error) {
	if err := f.checkRange(addr, n); err != nil {
		return nil, err
	}
	out := make([]byte, 0, n)
	for len(out) < n {
		b, err := f.readBatchAt(ctx, addr+uint32(len(out)), min(n-len(out), readBatch*MaxChunk))
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

func (f *Flash) readBatchAt(ctx context.Context, addr uint32, n int) ([]byte, error) {
	for try := 0; try < 4; try++ {
		var ops []riscv.BusOp
		type chunk struct{ doneAt, dataAt, n int }
		var cs []chunk
		for off := 0; off < n; off += MaxChunk {
			c := min(MaxChunk, n-off)
			ops = append(ops, txn{cmd: 0x0B, addr: addr + uint32(off), hasA: true, dummy: 8, miso: c}.ops()...)
			ops = append(ops, riscv.BusOp{Kind: riscv.BusRead, Addr: SPI1Base + spiCmd, Words: 1})
			d := len(ops) - 1
			ops = append(ops, riscv.BusOp{Kind: riscv.BusRead, Addr: SPI1Base + spiW0, Words: (c + 3) / 4})
			cs = append(cs, chunk{d, d + 1, c})
		}
		r, err := f.dm.BusBatch(ctx, ops)
		if err != nil {
			return nil, err
		}
		f.Transactions += len(cs)
		out := make([]byte, 0, n)
		ok := true
		for _, c := range cs {
			if r[c.doneAt][0] != 0 {
				ok = false
				break
			}
			out = append(out, unpackWords(r[c.dataAt], c.n)...)
		}
		if ok {
			return out, nil
		}
		if err := f.waitHostIdle(ctx); err != nil {
			return nil, err
		}
		if n > MaxChunk {
			// Fall back to one transaction at a time: slower but each one is
			// confirmed finished before its data is read.
			return f.readSlow(ctx, addr, n)
		}
	}
	return nil, ErrController
}

func (f *Flash) readSlow(ctx context.Context, addr uint32, n int) ([]byte, error) {
	var out []byte
	for off := 0; off < n; off += MaxChunk {
		b, err := f.ReadChunk(ctx, addr+uint32(off), min(MaxChunk, n-off))
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

// Progress reports bytes done of total.
type Progress func(done, total int)

// Dump streams n bytes of flash starting at addr to w and returns the SHA-256
// of what was written.
func (f *Flash) Dump(ctx context.Context, addr uint32, n int, w io.Writer, prog Progress) ([32]byte, error) {
	if err := f.checkRange(addr, n); err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	const step = 4096
	for done := 0; done < n; {
		b, err := f.Read(ctx, addr+uint32(done), min(step, n-done))
		if err != nil {
			return [32]byte{}, fmt.Errorf("read at %#x: %w", addr+uint32(done), err)
		}
		h.Write(b)
		if _, err := w.Write(b); err != nil {
			return [32]byte{}, err
		}
		done += len(b)
		if prog != nil {
			prog(done, n)
		}
	}
	var s [32]byte
	copy(s[:], h.Sum(nil))
	return s, nil
}

// Erase erases [addr, addr+n). Both must be multiples of the sector size.
// 64 KiB blocks are used where aligned. Every erased sector is read back and
// must be all 0xFF.
func (f *Flash) Erase(ctx context.Context, addr uint32, n int, prog Progress) error {
	if err := f.checkRange(addr, n); err != nil {
		return err
	}
	if addr%SectorSize != 0 || n%SectorSize != 0 {
		return fmt.Errorf("%w: [%#x, +%#x) must be multiples of %d", ErrAlign, addr, n, SectorSize)
	}
	end := addr + uint32(n)
	for a := addr; a < end; {
		var err error
		step := uint32(SectorSize)
		if a%BlockSize == 0 && end-a >= BlockSize {
			step = BlockSize
			err = f.EraseBlock64(ctx, a)
		} else {
			err = f.EraseSector(ctx, a)
		}
		if err != nil {
			return fmt.Errorf("erase at %#x: %w", a, err)
		}
		a += step
		if prog != nil {
			prog(int(a-addr), n)
		}
	}
	return f.VerifyErased(ctx, addr, n)
}

// VerifyErased requires every byte of the region to be 0xFF.
func (f *Flash) VerifyErased(ctx context.Context, addr uint32, n int) error {
	for done := 0; done < n; {
		b, err := f.Read(ctx, addr+uint32(done), min(SectorSize, n-done))
		if err != nil {
			return err
		}
		for i, v := range b {
			if v != 0xFF {
				return fmt.Errorf("%w: byte %#02x at %#x", ErrNotErased, v, addr+uint32(done+i))
			}
		}
		done += len(b)
	}
	return nil
}

// Program writes data at addr. The region must already be erased; trailing
// 0xFF chunks are skipped (they are already in the erased state). It does not
// verify; call Verify.
func (f *Flash) Program(ctx context.Context, addr uint32, data []byte, prog Progress) error {
	if err := f.checkRange(addr, len(data)); err != nil {
		return err
	}
	for off := 0; off < len(data); {
		a := addr + uint32(off)
		n := min(MaxChunk, PageSize-int(a%PageSize), len(data)-off)
		chunk := data[off : off+n]
		if !allFF(chunk) {
			if err := f.ProgramChunk(ctx, a, chunk); err != nil {
				return fmt.Errorf("program at %#x: %w", a, err)
			}
		}
		off += n
		if prog != nil && off%4096 < n {
			prog(off, len(data))
		}
	}
	return nil
}

func allFF(b []byte) bool {
	for _, v := range b {
		if v != 0xFF {
			return false
		}
	}
	return true
}

// Verify reads the region back and requires it to equal data.
func (f *Flash) Verify(ctx context.Context, addr uint32, data []byte) error {
	for off := 0; off < len(data); {
		n := min(SectorSize, len(data)-off)
		b, err := f.Read(ctx, addr+uint32(off), n)
		if err != nil {
			return err
		}
		if !bytes.Equal(b, data[off:off+n]) {
			for i := range b {
				if b[i] != data[off+i] {
					return fmt.Errorf("%w: flash %#02x, expected %#02x at %#x", ErrVerify, b[i], data[off+i], addr+uint32(off+i))
				}
			}
		}
		off += n
	}
	return nil
}

// RoundSectors widens [addr, addr+n) to whole erase sectors.
func RoundSectors(addr uint32, n int) (uint32, int) {
	start := addr &^ (SectorSize - 1)
	end := (uint64(addr) + uint64(n) + SectorSize - 1) &^ (SectorSize - 1)
	return start, int(end - uint64(start))
}
