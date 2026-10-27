package usb

import "fmt"

// descInterface is one interface alternate setting of a configuration
// descriptor.
type descInterface struct {
	Number, Alt, Class int
	Endpoints          []Endpoint
}

var endpointTypes = [4]string{"Control", "Isoc", "Bulk", "Interrupt"}

// parseConfigDescriptor walks a full configuration descriptor (as returned by
// GET_DESCRIPTOR(CONFIGURATION) with wTotalLength bytes) and lists every
// interface alternate setting with its endpoints in descriptor order.
// Class-specific and other descriptors are skipped. wMaxPacketSize is kept
// raw, as sysfs shows it.
func parseConfigDescriptor(b []byte) ([]descInterface, error) {
	if len(b) < 9 || b[1] != 0x02 {
		return nil, fmt.Errorf("usb: not a configuration descriptor")
	}
	total := int(b[2]) | int(b[3])<<8
	if total < 9 || total > len(b) {
		return nil, fmt.Errorf("usb: configuration descriptor wTotalLength %d, have %d bytes", total, len(b))
	}
	b = b[:total]
	var out []descInterface
	for off := 0; off < len(b); {
		n := int(b[off])
		if n < 2 || off+n > len(b) {
			return out, fmt.Errorf("usb: malformed descriptor at offset %d", off)
		}
		d := b[off : off+n]
		switch d[1] {
		case 0x04: // INTERFACE
			if n >= 9 {
				out = append(out, descInterface{Number: int(d[2]), Alt: int(d[3]), Class: int(d[5])})
			}
		case 0x05: // ENDPOINT
			if n >= 7 && len(out) > 0 {
				cur := &out[len(out)-1]
				cur.Endpoints = append(cur.Endpoints, Endpoint{
					Address:   d[2],
					Type:      endpointTypes[d[3]&0x03],
					MaxPacket: int(d[4]) | int(d[5])<<8,
				})
			}
		}
		off += n
	}
	return out, nil
}
