package native

import (
	"fmt"
	"strings"
)

func adapterList() string {
	var s []string
	for _, a := range Adapters {
		s = append(s, fmt.Sprintf("%04x:%04x %s", a.VID, a.PID, a.Name))
	}
	return strings.Join(s, ", ")
}

// checkBulkEndpoints verifies that interface 0, alternate setting 0, of
// configuration 1 in the raw descriptors (device descriptor followed by the
// configuration descriptors, as usbfs returns them) has bulk endpoints in and
// out.
func checkBulkEndpoints(desc []byte, in, out uint8) error {
	var cfg uint8
	inIface0 := false
	foundIn, foundOut := false, false
	for len(desc) >= 2 {
		n := int(desc[0])
		if n < 2 || n > len(desc) {
			return transportf("malformed USB descriptor")
		}
		d := desc[:n]
		desc = desc[n:]
		switch d[1] {
		case 2: // configuration
			if n < 6 {
				return transportf("malformed USB configuration descriptor")
			}
			cfg, inIface0 = d[5], false
		case 4: // interface
			if n < 4 {
				return transportf("malformed USB interface descriptor")
			}
			inIface0 = cfg == 1 && d[2] == 0 && d[3] == 0
		case 5: // endpoint
			if n < 4 {
				return transportf("malformed USB endpoint descriptor")
			}
			if !inIface0 || d[3]&0x03 != 0x02 {
				continue
			}
			foundIn = foundIn || d[2] == in
			foundOut = foundOut || d[2] == out
		}
	}
	if !foundIn || !foundOut {
		return transportf("USB interface 0 has no bulk endpoints 0x%02x/0x%02x", in, out)
	}
	return nil
}
