package esp

import (
	"bytes"
	"crypto/md5" //nolint:gosec // the partition table format defines an MD5 checksum entry
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// PartitionTableOffset is where the ESP-IDF default layout keeps the table.
// It is a convention of the image, not a property of the chip: callers should
// treat a missing or invalid table as "unknown layout".
const PartitionTableOffset = 0x8000

// Partition is one 32-byte entry of an ESP partition table.
type Partition struct {
	Label     string `json:"label"`
	Type      uint8  `json:"type"`
	Subtype   uint8  `json:"subtype"`
	Offset    uint32 `json:"offset"`
	Size      uint32 `json:"size"`
	Encrypted bool   `json:"encrypted,omitempty"`
}

// Kind names the well-known type/subtype pairs.
func (p Partition) Kind() string {
	switch p.Type {
	case 0x00:
		switch {
		case p.Subtype == 0x00:
			return "app/factory"
		case p.Subtype == 0x20:
			return "app/test"
		case p.Subtype >= 0x10 && p.Subtype <= 0x1F:
			return fmt.Sprintf("app/ota_%d", p.Subtype-0x10)
		}
		return fmt.Sprintf("app/0x%02x", p.Subtype)
	case 0x01:
		names := map[uint8]string{0x00: "data/ota", 0x01: "data/phy", 0x02: "data/nvs", 0x03: "data/coredump", 0x04: "data/nvs_keys",
			0x05: "data/efuse_em", 0x80: "data/esphttpd", 0x81: "data/fat", 0x82: "data/spiffs", 0x83: "data/littlefs"}
		if n, ok := names[p.Subtype]; ok {
			return n
		}
		return fmt.Sprintf("data/0x%02x", p.Subtype)
	case 0x02:
		return "bootloader-or-pt"
	}
	return fmt.Sprintf("0x%02x/0x%02x", p.Type, p.Subtype)
}

// Protected reports a partition that can hold per-device data (settings, keys,
// RF calibration, network state, user files) which reflashing cannot
// regenerate. Every data partition except OTA selection data is protected.
func (p Partition) Protected() bool {
	return p.Type == 0x01 && p.Subtype != 0x00
}

var ErrPartitionTable = errors.New("partition table")

// ParsePartitionTable parses a table image (up to 0xC00 bytes at 0x8000).
// A table without entries, with a bad magic or with a failing MD5 entry is an
// error; the caller decides whether that is fatal.
func ParsePartitionTable(b []byte) ([]Partition, error) {
	var out []Partition
	sawMD5 := false
	for off := 0; off+32 <= len(b); off += 32 {
		e := b[off : off+32]
		if bytes.Equal(e, bytes.Repeat([]byte{0xFF}, 32)) {
			break
		}
		magic := binary.LittleEndian.Uint16(e[0:2])
		if magic == 0xEBEB {
			sawMD5 = true
			sum := md5.Sum(b[:off]) //nolint:gosec
			if !bytes.Equal(sum[:], e[16:32]) {
				return nil, fmt.Errorf("%w: MD5 entry does not match the entries", ErrPartitionTable)
			}
			break
		}
		if magic != 0x50AA {
			return nil, fmt.Errorf("%w: bad entry magic 0x%04x at +%#x", ErrPartitionTable, magic, off)
		}
		p := Partition{
			Type: e[2], Subtype: e[3],
			Offset: binary.LittleEndian.Uint32(e[4:8]), Size: binary.LittleEndian.Uint32(e[8:12]),
			Label:     strings.TrimRight(string(e[12:28]), "\x00"),
			Encrypted: binary.LittleEndian.Uint32(e[28:32])&1 != 0,
		}
		out = append(out, p)
		if len(out) > 95 {
			return nil, fmt.Errorf("%w: more than 95 entries", ErrPartitionTable)
		}
	}
	_ = sawMD5
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no entries", ErrPartitionTable)
	}
	return out, nil
}

// BuildPartitionTable is used by tests and the acceptance image builder.
func BuildPartitionTable(ps []Partition) []byte {
	var b []byte
	for _, p := range ps {
		e := make([]byte, 32)
		binary.LittleEndian.PutUint16(e[0:2], 0x50AA)
		e[2], e[3] = p.Type, p.Subtype
		binary.LittleEndian.PutUint32(e[4:8], p.Offset)
		binary.LittleEndian.PutUint32(e[8:12], p.Size)
		copy(e[12:28], p.Label)
		if p.Encrypted {
			e[28] = 1
		}
		b = append(b, e...)
	}
	sum := md5.Sum(b) //nolint:gosec
	e := bytes.Repeat([]byte{0xFF}, 32)
	e[0], e[1] = 0xEB, 0xEB
	copy(e[16:], sum[:])
	b = append(b, e...)
	return b
}

// Overlap returns the partitions intersecting [addr, addr+n).
func Overlap(ps []Partition, addr uint32, n int) []Partition {
	var out []Partition
	end := uint64(addr) + uint64(n)
	for _, p := range ps {
		if uint64(p.Offset) < end && uint64(addr) < uint64(p.Offset)+uint64(p.Size) {
			out = append(out, p)
		}
	}
	return out
}

// Range is a named flash byte range.
type Range struct {
	Name string `json:"name"`
	Addr uint32 `json:"address"`
	Size uint32 `json:"size"`
}

// BootloaderEnd is the end of the second-stage bootloader slot of the
// ESP-IDF layout (the partition table follows at PartitionTableOffset).
const BootloaderEnd = PartitionTableOffset

// ProtectedRanges lists the flash ranges that a routine write must never
// touch silently: the bootloader slot, the partition table sector and every
// protected partition. Changing them needs an extra explicit confirmation.
func ProtectedRanges(ps []Partition) []Range {
	rs := []Range{
		{"bootloader slot", 0, BootloaderEnd},
		{"partition table", PartitionTableOffset, SectorSize},
	}
	for _, p := range ps {
		if p.Protected() {
			rs = append(rs, Range{"partition " + p.Label + " (" + p.Kind() + ")", p.Offset, p.Size})
		}
	}
	return rs
}

// OverlapRanges returns the ranges intersecting [addr, addr+n).
func OverlapRanges(rs []Range, addr uint32, n int) []Range {
	var out []Range
	end := uint64(addr) + uint64(n)
	for _, r := range rs {
		if uint64(r.Addr) < end && uint64(addr) < uint64(r.Addr)+uint64(r.Size) {
			out = append(out, r)
		}
	}
	return out
}
