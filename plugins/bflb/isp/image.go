package isp

import (
	"encoding/binary"
	"fmt"
)

const (
	bootHeaderLen = 176
	segHeaderLen  = 16
	// segCountOff is where the boot header stores the segment count of a RAM
	// image (it shares the flash-image "img_len" field).
	segCountOff = 120
	maxSegments = 8
)

// Segment is one RAM segment of a boot image.
type Segment struct {
	Header []byte // 16 bytes: destination, length, reserved, CRC32 of the first 12
	Dest   uint32
	Data   []byte
}

// Image is a RAM boot image (BFNP boot header followed by segments), the
// format of the vendor eflash_loader helper.
type Image struct {
	Header   []byte
	Segments []Segment
}

// ParseImage checks the structure of a RAM boot image. The ROM verifies the
// header and segment CRCs itself; this only rejects files that are not such
// an image before anything is sent.
func ParseImage(b []byte) (*Image, error) {
	if len(b) < bootHeaderLen+segHeaderLen {
		return nil, fmt.Errorf("image is %d bytes: too short for a boot header and a segment", len(b))
	}
	if string(b[:4]) != "BFNP" {
		return nil, fmt.Errorf("image does not start with the BFNP boot header magic (got % x)", b[:4])
	}
	n := binary.LittleEndian.Uint32(b[segCountOff:])
	if n == 0 || n > maxSegments {
		return nil, fmt.Errorf("boot header segment count %d: want 1..%d (is this a RAM helper image?)", n, maxSegments)
	}
	img := &Image{Header: b[:bootHeaderLen]}
	off := bootHeaderLen
	for i := 0; i < int(n); i++ {
		if off+segHeaderLen > len(b) {
			return nil, fmt.Errorf("segment %d header is past the end of the image", i)
		}
		h := b[off : off+segHeaderLen]
		size := int(binary.LittleEndian.Uint32(h[4:]))
		off += segHeaderLen
		if size == 0 || off+size > len(b) {
			return nil, fmt.Errorf("segment %d length %d does not fit the image", i, size)
		}
		img.Segments = append(img.Segments, Segment{Header: h, Dest: binary.LittleEndian.Uint32(h), Data: b[off : off+size]})
		off += size
	}
	if off != len(b) {
		return nil, fmt.Errorf("%d trailing bytes after the last segment", len(b)-off)
	}
	return img, nil
}
