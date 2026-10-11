// Package espusb is the host side of the Espressif USB Serial/JTAG protocol:
// the vendor control requests and the 4-bit command stream that the built-in
// USB-JTAG peripheral of ESP32-C3/S3/C6/H2 (and the ESP USB Bridge) accepts on
// a vendor-class bulk interface.
//
// It is a probe transport only. It exposes a jtag.Port and knows nothing about
// the CPU behind the TAP. The CDC-ACM interfaces of the same USB device are
// never touched.
//
// Provenance: the wire format was written from the protocol description that
// Espressif publishes (the header comment of openocd-esp32 esp_usb_jtag.c, a
// GPL-2.0 file, read as documentation only) and from observation of the real
// device. No implementation code was copied; see docs/licenses.md.
package espusb

import (
	"errors"
	"fmt"
)

// Vendor control requests (bmRequestType 0x40 out, 0xC0 in; wIndex 0). The
// protocol also has SETIO (1) and SET_CHIPID (3); this driver never needs
// them, so it never changes pin levels outside the command stream.
const (
	reqSetDiv  = 0
	reqGetTDO  = 2
	reqTypeOut = 0x40
	reqTypeIn  = 0xC0

	// capsValue selects the built-in JTAG capabilities descriptor through a
	// standard GET_DESCRIPTOR request.
	capsValue  = 0x2000
	capsLength = 255

	capsProtoVer  = 1
	capsSpeedType = 1
)

// Caps is the parsed JTAG capabilities descriptor.
type Caps struct {
	ProtoVersion int    `json:"proto_version"`
	APBKHz       int    `json:"apb_khz"`  // SoC APB clock the divider derives from
	BaseKHz      int    `json:"base_khz"` // TCK base clock = APB / 2
	DivMin       int    `json:"div_min"`
	DivMax       int    `json:"div_max"`
	Raw          []byte `json:"-"`
}

// ErrCaps is a malformed capabilities descriptor.
var ErrCaps = errors.New("espusb: bad JTAG capabilities descriptor")

// ParseCaps decodes the capabilities descriptor: a {version, total length}
// header followed by {type, length, payload} records. Unknown record types are
// skipped, as the protocol requires, but a missing speed record is an error
// because the divider cannot be computed without it.
func ParseCaps(b []byte) (Caps, error) {
	c := Caps{Raw: append([]byte(nil), b...)}
	if len(b) < 2 {
		return c, fmt.Errorf("%w: %d byte(s)", ErrCaps, len(b))
	}
	if int(b[0]) != capsProtoVer {
		return c, fmt.Errorf("%w: protocol version %d, want %d", ErrCaps, b[0], capsProtoVer)
	}
	c.ProtoVersion = int(b[0])
	total := int(b[1])
	if total < 2 || total > len(b) {
		return c, fmt.Errorf("%w: header length %d with %d byte(s) received", ErrCaps, total, len(b))
	}
	found := false
	for p := 2; p+2 <= total; {
		typ, n := b[p], int(b[p+1])
		if n < 2 || p+n > total {
			return c, fmt.Errorf("%w: record at offset %d has length %d", ErrCaps, p, n)
		}
		if typ == capsSpeedType {
			if n < 8 {
				return c, fmt.Errorf("%w: speed record is %d byte(s)", ErrCaps, n)
			}
			c.APBKHz = int(uint16(b[p+2])|uint16(b[p+3])<<8) * 10
			c.BaseKHz = c.APBKHz / 2
			c.DivMin = int(uint16(b[p+4]) | uint16(b[p+5])<<8)
			c.DivMax = int(uint16(b[p+6]) | uint16(b[p+7])<<8)
			found = true
		}
		p += n
	}
	if !found {
		return c, fmt.Errorf("%w: no speed record", ErrCaps)
	}
	if c.BaseKHz <= 0 || c.DivMin < 1 || c.DivMax < c.DivMin {
		return c, fmt.Errorf("%w: base %d kHz, divider %d..%d", ErrCaps, c.BaseKHz, c.DivMin, c.DivMax)
	}
	return c, nil
}

// Divisor returns the divider closest to khz (never faster than requested) and
// the resulting TCK frequency.
func (c Caps) Divisor(khz int) (div, actualKHz int) {
	if khz < 1 {
		khz = 1
	}
	div = (c.BaseKHz + khz - 1) / khz
	div = min(max(div, c.DivMin), c.DivMax)
	return div, c.BaseKHz / div
}
