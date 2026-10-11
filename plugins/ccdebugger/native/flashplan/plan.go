// Package flashplan turns firmware images into bounded, validated flash
// programming plans for the CC253x/CC254x family. It is pure Go with no
// hardware access: images are parsed, checked against the geometry and the
// device-preservation policy, and expanded into erase pages and word-aligned
// program blocks with all-0xFF blocks skipped.
//
// Written from TI SWRU191F (flash controller, lock bits); independent of the
// GPL-2.0 cc-tool sources.
package flashplan

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/faronov/hwlab/plugins/ccdebugger/native/target"
)

// Mode selects which device-specific data a plan may overwrite.
type Mode string

// Preservation modes.
const (
	// PreserveDeviceConfig (the default) never touches the lock-bit page:
	// secondary IEEE address and lock bits stay exactly as they are.
	PreserveDeviceConfig Mode = "preserve-device-config"
	// PreserveMAC may rewrite the lock-bit page (for example to set lock
	// bits) but keeps the secondary IEEE address bytes unchanged.
	PreserveMAC Mode = "preserve-mac"
	// FullChipImage may overwrite everything in main flash, including the
	// secondary IEEE address and lock bits. Inherently destructive.
	FullChipImage Mode = "full-chip-image"
)

// ParseMode validates a mode name; empty means PreserveDeviceConfig.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case PreserveDeviceConfig, PreserveMAC, FullChipImage:
		return m, nil
	case "":
		return PreserveDeviceConfig, nil
	}
	return "", fmt.Errorf("mode %q: want preserve-device-config, preserve-mac or full-chip-image", s)
}

// MaxImageBytes bounds parsed image memory.
const MaxImageBytes = 1 << 20

// Section is a contiguous run of image bytes at a physical flash address.
type Section struct {
	Addr int
	Data []byte
}

// End is the exclusive end address.
func (s Section) End() int { return s.Addr + len(s.Data) }

// Image is a sorted set of non-overlapping sections.
type Image struct{ Sections []Section }

// Size is the number of image bytes.
func (i Image) Size() int {
	n := 0
	for _, s := range i.Sections {
		n += len(s.Data)
	}
	return n
}

// Highest returns the exclusive highest used address, 0 for an empty image.
func (i Image) Highest() int {
	if len(i.Sections) == 0 {
		return 0
	}
	return i.Sections[len(i.Sections)-1].End()
}

// Normalize sorts, merges adjacent sections and rejects overlap and emptiness.
func Normalize(secs []Section) (Image, error) {
	var in []Section
	for _, s := range secs {
		if len(s.Data) > 0 {
			in = append(in, Section{s.Addr, append([]byte(nil), s.Data...)})
		}
	}
	if len(in) == 0 {
		return Image{}, errors.New("image contains no data")
	}
	sort.Slice(in, func(a, b int) bool { return in[a].Addr < in[b].Addr })
	out := []Section{in[0]}
	for _, s := range in[1:] {
		last := &out[len(out)-1]
		switch {
		case s.Addr < last.End():
			return Image{}, fmt.Errorf("image sections overlap at 0x%X", s.Addr)
		case s.Addr == last.End():
			last.Data = append(last.Data, s.Data...)
		default:
			out = append(out, s)
		}
		if last := out[len(out)-1]; last.End() > MaxImageBytes*4 {
			return Image{}, errors.New("image address range is implausibly large")
		}
	}
	if total := (Image{out}).Size(); total > MaxImageBytes {
		return Image{}, fmt.Errorf("image is larger than %d bytes", MaxImageBytes)
	}
	return Image{out}, nil
}

// FromBinary wraps a raw binary placed at base.
func FromBinary(data []byte, base int) (Image, error) {
	if base < 0 {
		return Image{}, errors.New("negative base address")
	}
	if len(data) > MaxImageBytes {
		return Image{}, fmt.Errorf("image is larger than %d bytes", MaxImageBytes)
	}
	return Normalize([]Section{{base, data}})
}

// ParseIntelHex reads an Intel HEX stream (data, EOF, extended segment and
// linear address records; start-address records are ignored).
func ParseIntelHex(r io.Reader) (Image, error) {
	var secs []Section
	var base, total, line int
	eof := false
	sc := bufio.NewScanner(io.LimitReader(r, 8*MaxImageBytes))
	for sc.Scan() {
		line++
		t := strings.TrimSpace(sc.Text())
		if t == "" {
			continue
		}
		if eof {
			return Image{}, fmt.Errorf("hex line %d: data after end-of-file record", line)
		}
		if t[0] != ':' {
			return Image{}, fmt.Errorf("hex line %d: missing ':'", line)
		}
		b, err := hex.DecodeString(t[1:])
		if err != nil || len(b) < 5 {
			return Image{}, fmt.Errorf("hex line %d: malformed record", line)
		}
		if int(b[0]) != len(b)-5 {
			return Image{}, fmt.Errorf("hex line %d: length mismatch", line)
		}
		var sum byte
		for _, v := range b {
			sum += v
		}
		if sum != 0 {
			return Image{}, fmt.Errorf("hex line %d: bad checksum", line)
		}
		data := b[4 : 4+int(b[0])]
		switch b[3] {
		case 0x00:
			total += len(data)
			if total > MaxImageBytes {
				return Image{}, fmt.Errorf("image is larger than %d bytes", MaxImageBytes)
			}
			secs = append(secs, Section{base + int(b[1])<<8 + int(b[2]), append([]byte(nil), data...)})
		case 0x01:
			eof = true
		case 0x02, 0x04:
			if len(data) != 2 {
				return Image{}, fmt.Errorf("hex line %d: bad address record", line)
			}
			v := int(data[0])<<8 | int(data[1])
			if b[3] == 0x02 {
				base = v << 4
			} else {
				base = v << 16
			}
		case 0x03, 0x05:
		default:
			return Image{}, fmt.Errorf("hex line %d: unsupported record type 0x%02X", line, b[3])
		}
	}
	if err := sc.Err(); err != nil {
		return Image{}, err
	}
	if !eof {
		return Image{}, errors.New("hex file has no end-of-file record")
	}
	return Normalize(secs)
}

// Range is a half-open physical address range.
type Range struct{ Addr, Len int }

// Block is one word-aligned program unit inside a single page.
type Block struct {
	Addr int
	Data []byte
}

// Plan is the validated programming plan.
type Plan struct {
	Geometry   target.Geometry // the hardware identity the plan was built for
	Mode       Mode
	ChipErase  bool    // erase the whole chip (full-chip-image only)
	ErasePages []int   // pages to erase, ascending; empty with ChipErase
	Blocks     []Block // blocks to program, ascending, all-0xFF blocks skipped
	// Preserve lists ranges that must be read before their page is erased and
	// rewritten afterwards (the secondary IEEE in preserve-mac mode).
	Preserve []Range
	// TouchesConfig is true if the plan modifies the lock-bit page; such a plan
	// needs the device-configuration write permission.
	TouchesConfig bool
	ImageBytes    int
	SkippedBlocks int
}

// BlockSize is the largest program block: one debug-interface burst.
const BlockSize = 1024

// Options controls Build.
type Options struct {
	Mode      Mode
	ChipErase bool
}

// Build validates an image against the geometry, the lock state and the
// preservation policy. lock may be nil only when the lock structure could not
// be read, which fails closed except for a full-chip-image chip erase.
func Build(g target.Geometry, lock *target.Lock, img Image, opt Options) (Plan, error) {
	if opt.Mode == "" {
		opt.Mode = PreserveDeviceConfig
	}
	if _, err := ParseMode(string(opt.Mode)); err != nil {
		return Plan{}, err
	}
	if !g.Programmable {
		return Plan{}, fmt.Errorf("flash programming is not implemented for %s", g.Chip)
	}
	if len(img.Sections) == 0 {
		return Plan{}, errors.New("image contains no data")
	}
	if opt.ChipErase && opt.Mode != FullChipImage {
		return Plan{}, errors.New("chip erase destroys device configuration; it requires mode full-chip-image")
	}
	if img.Highest() > g.FlashSize {
		return Plan{}, fmt.Errorf("image ends at 0x%X, beyond the %d-byte flash", img.Highest(), g.FlashSize)
	}
	if lock == nil && !opt.ChipErase {
		return Plan{}, errors.New("lock state unknown; refusing to erase or program")
	}
	p := Plan{Geometry: g, Mode: opt.Mode, ChipErase: opt.ChipErase, ImageBytes: img.Size()}
	lockStart, macStart := g.LockPageAddr(), g.SecondaryIEEEAddr()
	pages := map[int]bool{}
	for _, s := range img.Sections {
		if s.Addr < 0 {
			return Plan{}, errors.New("negative image address")
		}
		if s.End() > lockStart {
			over := Range{max(s.Addr, lockStart), s.End() - max(s.Addr, lockStart)}
			p.TouchesConfig = true
			switch opt.Mode {
			case PreserveDeviceConfig:
				return Plan{}, fmt.Errorf("image writes the lock-bit page at 0x%X..0x%X; mode preserve-device-config protects the secondary IEEE address and lock bits", over.Addr, over.Addr+over.Len-1)
			case PreserveMAC:
				if over.Addr < macStart+target.IEEESize && over.Addr+over.Len > macStart {
					return Plan{}, fmt.Errorf("image overlaps the secondary IEEE address at 0x%X; mode preserve-mac keeps it", macStart)
				}
			}
		}
		first, last, err := g.PageRange(s.Addr, len(s.Data))
		if err != nil {
			return Plan{}, err
		}
		for pg := first; pg <= last; pg++ {
			pages[pg] = true
		}
	}
	if p.TouchesConfig && opt.Mode == PreserveMAC {
		p.Preserve = []Range{{macStart, target.IEEESize}}
	}
	if !opt.ChipErase {
		for pg := range pages {
			if lock != nil && lock.PageLocked(pg) {
				return Plan{}, fmt.Errorf("page %d is write-protected by its lock bit; only a chip erase clears it", pg)
			}
			p.ErasePages = append(p.ErasePages, pg)
		}
		sort.Ints(p.ErasePages)
	}
	// Preserved bytes are rewritten from the backup, so the image must not
	// carry them: Build already rejected overlap above.
	for _, s := range img.Sections {
		for off := 0; off < len(s.Data); {
			addr := s.Addr + off
			// Cut at BlockSize alignment so a block never crosses a page.
			end := min(s.End(), (addr/BlockSize+1)*BlockSize)
			bs := addr &^ (target.WordSize - 1)
			be := (end + target.WordSize - 1) &^ (target.WordSize - 1)
			buf := make([]byte, be-bs)
			for i := range buf {
				buf[i] = 0xFF
			}
			copy(buf[addr-bs:], s.Data[off:off+(end-addr)])
			if allFF(buf) {
				p.SkippedBlocks++
			} else {
				p.Blocks = mergeBlock(p.Blocks, Block{bs, buf})
			}
			off += end - addr
		}
	}
	return p, nil
}

// mergeBlock adds b, coalescing with the previous block when contiguous and
// the combined block stays inside one BlockSize unit.
func mergeBlock(bl []Block, b Block) []Block {
	if n := len(bl); n > 0 {
		last := &bl[n-1]
		if last.Addr+len(last.Data) == b.Addr && last.Addr/BlockSize == b.Addr/BlockSize {
			last.Data = append(last.Data, b.Data...)
			return bl
		}
		if last.Addr+len(last.Data) > b.Addr {
			// Two sections shared a word: overlay the later bytes.
			off := b.Addr - last.Addr
			for i, v := range b.Data {
				if off+i < len(last.Data) {
					if v != 0xFF {
						last.Data[off+i] = v
					}
				} else {
					last.Data = append(last.Data, v)
				}
			}
			return bl
		}
	}
	return append(bl, b)
}

func allFF(b []byte) bool {
	for _, v := range b {
		if v != 0xFF {
			return false
		}
	}
	return true
}

// CRC16 is the CRC the CC253x hardware computes over the bytes DMA'd (or
// written) into RNDH after RNDL was loaded with 0xFF twice: init 0xFFFF,
// polynomial 0x8005, MSB first, no reflection, no final XOR. Determined on a
// real CC2530 (seed 0xFFFF + byte 0x02 -> 0x7D0D; it is not the CCITT 0x1021
// polynomial).
func CRC16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
