package esp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ESP application/bootloader image format (header, segments, XOR checksum).
// Only the fields hwlab validates are modelled; unknown ones are preserved by
// never rewriting a parsed image.

const (
	ImageMagic = 0xE9
	// ChipIDESP32C6 is the chip id the image header carries for the ESP32-C6.
	ChipIDESP32C6 = 0x000D
	imageHdrLen   = 24
	segHdrLen     = 8
)

var ErrImage = errors.New("esp image")

type Segment struct {
	LoadAddr uint32 `json:"load_addr"`
	Length   uint32 `json:"length"`
	Offset   uint32 `json:"file_offset"`
}

// Image is the parsed header of a firmware image.
type Image struct {
	Segments     []Segment `json:"segments"`
	Entry        uint32    `json:"entry"`
	ChipID       uint16    `json:"chip_id"`
	FlashMode    uint8     `json:"flash_mode"`
	SizeBytes    int       `json:"size_bytes"` // header..checksum, padded to 16
	HashAppended bool      `json:"hash_appended"`
}

// ParseImage validates an image: magic, segment bounds against the data, the
// XOR checksum, and that nothing is truncated.
func ParseImage(b []byte) (Image, error) {
	if len(b) < imageHdrLen {
		return Image{}, fmt.Errorf("%w: %d bytes is shorter than the %d-byte header", ErrImage, len(b), imageHdrLen)
	}
	if b[0] != ImageMagic {
		return Image{}, fmt.Errorf("%w: magic 0x%02x, want 0x%02x", ErrImage, b[0], ImageMagic)
	}
	n := int(b[1])
	if n < 1 || n > 16 {
		return Image{}, fmt.Errorf("%w: %d segments", ErrImage, n)
	}
	im := Image{Entry: binary.LittleEndian.Uint32(b[4:8]), FlashMode: b[2], ChipID: binary.LittleEndian.Uint16(b[12:14]), HashAppended: b[23] == 1}
	off := imageHdrLen
	sum := byte(0xEF)
	for i := 0; i < n; i++ {
		if off+segHdrLen > len(b) {
			return im, fmt.Errorf("%w: truncated in the header of segment %d", ErrImage, i)
		}
		s := Segment{LoadAddr: binary.LittleEndian.Uint32(b[off:]), Length: binary.LittleEndian.Uint32(b[off+4:]), Offset: uint32(off + segHdrLen)}
		if s.Length > 16<<20 || int(s.Offset)+int(s.Length) > len(b) {
			return im, fmt.Errorf("%w: segment %d (%d bytes at file offset %#x) runs past the %d-byte image: truncated", ErrImage, i, s.Length, s.Offset, len(b))
		}
		for _, v := range b[s.Offset : s.Offset+s.Length] {
			sum ^= v
		}
		im.Segments = append(im.Segments, s)
		off = int(s.Offset + s.Length)
	}
	pad := (16 - (off+1)%16) % 16
	ck := off + pad
	if ck >= len(b) {
		return im, fmt.Errorf("%w: truncated before the checksum byte", ErrImage)
	}
	if b[ck] != sum {
		return im, fmt.Errorf("%w: checksum 0x%02x, computed 0x%02x", ErrImage, b[ck], sum)
	}
	im.SizeBytes = ck + 1
	return im, nil
}

// BuildImage makes a minimal single- or multi-segment image with a correct
// checksum and no appended hash. Segment data is padded to 4 bytes.
func BuildImage(chipID uint16, entry uint32, segs map[uint32][]byte, order []uint32) []byte {
	b := make([]byte, imageHdrLen)
	b[0], b[1], b[2], b[3] = ImageMagic, byte(len(order)), 0x02, 0x20
	binary.LittleEndian.PutUint32(b[4:], entry)
	binary.LittleEndian.PutUint16(b[12:], chipID)
	b[15] = 0xFF // min chip rev is unspecified in the full-rev field
	sum := byte(0xEF)
	for _, a := range order {
		d := append([]byte(nil), segs[a]...)
		for len(d)%4 != 0 {
			d = append(d, 0)
		}
		h := make([]byte, segHdrLen)
		binary.LittleEndian.PutUint32(h, a)
		binary.LittleEndian.PutUint32(h[4:], uint32(len(d)))
		b = append(b, h...)
		b = append(b, d...)
		for _, v := range d {
			sum ^= v
		}
	}
	for (len(b)+1)%16 != 0 {
		b = append(b, 0)
	}
	b = append(b, sum)
	return b
}
