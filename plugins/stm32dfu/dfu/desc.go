// Package dfu speaks USB DFU 1.1 with the ST DfuSe extensions used by the
// STM32 system-memory (ROM) bootloader (AN3156, UM0424). It runs over the
// control pipe of a usb.Port; it needs no endpoints and no cgo.
//
// Every DNLOAD passes one guard (Client.checkDownload). The guard refuses the
// commands hwlab never sends (mass erase, read unprotect), any download to an
// alternate setting other than the configured flash one, and any data or
// page erase outside the write window, which is empty until the caller sets
// it after establishing the secure boundary.
package dfu

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Functional is the DFU functional descriptor (type 0x21).
type Functional struct {
	Attributes    uint8
	DetachTimeout uint16 // ms
	TransferSize  uint16 // wTransferSize, the maximum control payload
	DFUVersion    uint16 // bcdDFUVersion, 0x011A for DfuSe
}

// Attribute bits of bmAttributes.
const (
	AttrCanDownload           = 1 << 0
	AttrCanUpload             = 1 << 1
	AttrManifestationTolerant = 1 << 2
	AttrWillDetach            = 1 << 3
)

// Has reports an attribute bit.
func (f Functional) Has(bit uint8) bool { return f.Attributes&bit != 0 }

// AltSetting is one alternate setting of the DFU interface.
type AltSetting struct {
	Interface uint8
	Alt       uint8
	IString   uint8
	Name      string // the raw interface string, e.g. "@Internal Flash  /0x08000000/256*04Kg"
	Memory    *Memory
	ParseErr  string
}

// ConfigInfo is what the configuration descriptor says about DFU.
type ConfigInfo struct {
	Alts       []AltSetting
	Functional *Functional
}

// ParseConfig decodes a full configuration descriptor and keeps the DFU
// interfaces (class 0xFE, subclass 1) and the DFU functional descriptor.
func ParseConfig(b []byte) (ConfigInfo, error) {
	var ci ConfigInfo
	if len(b) < 9 || b[1] != 2 {
		return ci, errors.New("dfu: not a configuration descriptor")
	}
	if total := int(binary.LittleEndian.Uint16(b[2:])); total < len(b) {
		b = b[:total]
	}
	inDFU := false
	for i := 0; i+2 <= len(b); {
		l := int(b[i])
		if l < 2 || i+l > len(b) {
			return ci, fmt.Errorf("dfu: malformed descriptor at offset %d", i)
		}
		d := b[i : i+l]
		switch d[1] {
		case 4: // interface
			if l < 9 {
				return ci, errors.New("dfu: short interface descriptor")
			}
			inDFU = d[5] == 0xFE && d[6] == 0x01
			if inDFU {
				ci.Alts = append(ci.Alts, AltSetting{Interface: d[2], Alt: d[3], IString: d[8]})
			}
		case 0x21:
			if inDFU && l >= 7 && ci.Functional == nil {
				f := &Functional{Attributes: d[2], DetachTimeout: binary.LittleEndian.Uint16(d[3:]), TransferSize: binary.LittleEndian.Uint16(d[5:])}
				if l >= 9 {
					f.DFUVersion = binary.LittleEndian.Uint16(d[7:])
				}
				ci.Functional = f
			}
		}
		i += l
	}
	if len(ci.Alts) == 0 {
		return ci, errors.New("dfu: no DFU interface (class 0xFE subclass 1) in the configuration")
	}
	return ci, nil
}

// Segment attribute bits (the DfuSe letter minus 'a' plus one).
const (
	SegReadable = 1 << 0
	SegErasable = 1 << 1
	SegWritable = 1 << 2
)

// Segment is one run of equal pages in a DfuSe memory description.
type Segment struct {
	Start    uint32
	Pages    int
	PageSize uint32
	Attr     uint8
}

// End is the first address after the segment.
func (s Segment) End() uint32 { return s.Start + uint32(s.Pages)*s.PageSize }

// Readable reports the 'r' attribute.
func (s Segment) Readable() bool { return s.Attr&SegReadable != 0 }

// Erasable reports the 'e' attribute.
func (s Segment) Erasable() bool { return s.Attr&SegErasable != 0 }

// Writable reports the 'w' attribute.
func (s Segment) Writable() bool { return s.Attr&SegWritable != 0 }

// AttrString renders Attr as r/e/w flags.
func (s Segment) AttrString() string {
	b := []byte("---")
	if s.Readable() {
		b[0] = 'r'
	}
	if s.Erasable() {
		b[1] = 'e'
	}
	if s.Writable() {
		b[2] = 'w'
	}
	return string(b)
}

// Memory is a parsed DfuSe alternate-setting string.
type Memory struct {
	Name     string
	Segments []Segment
}

// Start is the lowest address described.
func (m *Memory) Start() uint32 { return m.Segments[0].Start }

// End is the first address after the description.
func (m *Memory) End() uint32 {
	e := uint32(0)
	for _, s := range m.Segments {
		e = max(e, s.End())
	}
	return e
}

// Find returns the segment holding addr.
func (m *Memory) Find(addr uint32) (Segment, bool) {
	for _, s := range m.Segments {
		if addr >= s.Start && addr < s.End() {
			return s, true
		}
	}
	return Segment{}, false
}

// Covers reports whether [addr, addr+n) lies inside the segments.
func (m *Memory) Covers(addr uint32, n int) bool {
	a, end := uint64(addr), uint64(addr)+uint64(n)
	for a < end {
		s, ok := m.Find(uint32(a))
		if !ok {
			return false
		}
		a = uint64(s.End())
	}
	return true
}

// ParseMemory decodes "@Name /0xADDR/NN*SSSMa,NN*SSSMa[/0xADDR/...]".
// The multiplier M is ' ', 'B', 'K' or 'M'; the attribute letter a is
// 'a'..'g', its offset from 'a' plus one being readable|erasable|writable.
func ParseMemory(s string) (*Memory, error) {
	s = strings.TrimRight(s, "\x00")
	if !strings.HasPrefix(s, "@") {
		return nil, fmt.Errorf("dfu: %q is not a DfuSe memory description", s)
	}
	parts := strings.Split(s[1:], "/")
	if len(parts) < 3 || len(parts)%2 == 0 {
		return nil, fmt.Errorf("dfu: %q: want @name/0xaddr/segments[/0xaddr/segments]", s)
	}
	m := &Memory{Name: strings.TrimSpace(parts[0])}
	for i := 1; i+1 < len(parts); i += 2 {
		base, err := strconv.ParseUint(strings.TrimSpace(parts[i]), 0, 32)
		if err != nil {
			return nil, fmt.Errorf("dfu: %q: address %q: %w", s, parts[i], err)
		}
		addr := base
		for _, seg := range strings.Split(parts[i+1], ",") {
			sg, err := parseSegment(seg)
			if err != nil {
				return nil, fmt.Errorf("dfu: %q: %w", s, err)
			}
			sg.Start = uint32(addr)
			addr += uint64(sg.Pages) * uint64(sg.PageSize)
			if addr > 1<<32 {
				return nil, fmt.Errorf("dfu: %q: segment passes the end of the address space", s)
			}
			m.Segments = append(m.Segments, sg)
		}
	}
	return m, nil
}

func parseSegment(seg string) (Segment, error) {
	seg = strings.TrimSpace(seg)
	n, rest, ok := strings.Cut(seg, "*")
	if !ok {
		return Segment{}, fmt.Errorf("segment %q: want NN*SSSMa", seg)
	}
	pages, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil || pages < 1 {
		return Segment{}, fmt.Errorf("segment %q: page count", seg)
	}
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	size, err := strconv.Atoi(rest[:j])
	if err != nil || size < 1 || len(rest) < j+2 {
		return Segment{}, fmt.Errorf("segment %q: page size", seg)
	}
	mult := uint64(1)
	switch rest[j] {
	case ' ', 'B':
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	default:
		return Segment{}, fmt.Errorf("segment %q: multiplier %q", seg, rest[j])
	}
	a := rest[j+1]
	if a < 'a' || a > 'g' {
		return Segment{}, fmt.Errorf("segment %q: attribute %q", seg, a)
	}
	ps := uint64(size) * mult
	if ps > 1<<31 {
		return Segment{}, fmt.Errorf("segment %q: page size too large", seg)
	}
	return Segment{Pages: pages, PageSize: uint32(ps), Attr: a - 'a' + 1}, nil
}

// DecodeString decodes a USB string descriptor (UTF-16LE).
func DecodeString(b []byte) (string, error) {
	if len(b) < 2 || b[1] != 3 {
		return "", errors.New("not a string descriptor")
	}
	n := min(int(b[0]), len(b))
	var r []rune
	for i := 2; i+1 < n; i += 2 {
		r = append(r, rune(binary.LittleEndian.Uint16(b[i:])))
	}
	return string(r), nil
}
