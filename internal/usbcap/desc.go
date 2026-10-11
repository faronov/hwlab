package usbcap

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Setup is a decoded control setup packet.
type Setup struct {
	RequestType byte
	Request     byte
	Value       uint16
	Index       uint16
	Length      uint16
}

// ParseSetup decodes the 8 setup bytes.
func ParseSetup(b [8]byte) Setup {
	le := binary.LittleEndian
	return Setup{b[0], b[1], le.Uint16(b[2:]), le.Uint16(b[4:]), le.Uint16(b[6:])}
}

// In reports a device-to-host data stage.
func (s Setup) In() bool { return s.RequestType&0x80 != 0 }

// Kind is "standard", "class", "vendor" or "reserved".
func (s Setup) Kind() string {
	return [...]string{"standard", "class", "vendor", "reserved"}[(s.RequestType>>5)&3]
}

// Recipient is "device", "interface", "endpoint" or "other".
func (s Setup) Recipient() string {
	switch s.RequestType & 0x1f {
	case 0:
		return "device"
	case 1:
		return "interface"
	case 2:
		return "endpoint"
	case 3:
		return "other"
	}
	return "reserved"
}

// Standard request codes.
const (
	ReqGetStatus        = 0
	ReqClearFeature     = 1
	ReqSetFeature       = 3
	ReqSetAddress       = 5
	ReqGetDescriptor    = 6
	ReqSetDescriptor    = 7
	ReqGetConfiguration = 8
	ReqSetConfiguration = 9
	ReqGetInterface     = 10
	ReqSetInterface     = 11
	ReqSynchFrame       = 12
)

var stdRequests = map[byte]string{
	0: "GET_STATUS", 1: "CLEAR_FEATURE", 3: "SET_FEATURE", 5: "SET_ADDRESS", 6: "GET_DESCRIPTOR", 7: "SET_DESCRIPTOR",
	8: "GET_CONFIGURATION", 9: "SET_CONFIGURATION", 10: "GET_INTERFACE", 11: "SET_INTERFACE", 12: "SYNCH_FRAME",
}

var descTypes = map[byte]string{
	1: "DEVICE", 2: "CONFIGURATION", 3: "STRING", 4: "INTERFACE", 5: "ENDPOINT", 6: "DEVICE_QUALIFIER",
	7: "OTHER_SPEED", 8: "INTERFACE_POWER", 0x0b: "INTERFACE_ASSOCIATION", 0x0f: "BOS",
	0x21: "HID", 0x22: "HID_REPORT", 0x23: "HID_PHYSICAL", 0x24: "CS_INTERFACE", 0x25: "CS_ENDPOINT",
}

// USB class codes.
const (
	ClassAudio   = 0x01
	ClassCDC     = 0x02
	ClassHID     = 0x03
	ClassMSC     = 0x08
	ClassHub     = 0x09
	ClassCDCData = 0x0a
	ClassApp     = 0xfe // application specific; subclass 1 is DFU
	ClassVendor  = 0xff
)

// ClassName names a class code.
func ClassName(c byte) string {
	switch c {
	case 0:
		return "per-interface"
	case ClassAudio:
		return "audio"
	case ClassCDC:
		return "CDC"
	case ClassHID:
		return "HID"
	case 0x05:
		return "physical"
	case 0x06:
		return "image"
	case 0x07:
		return "printer"
	case ClassMSC:
		return "mass-storage"
	case ClassHub:
		return "hub"
	case ClassCDCData:
		return "CDC-data"
	case 0x0b:
		return "smart-card"
	case 0x0e:
		return "video"
	case 0xe0:
		return "wireless"
	case 0xef:
		return "miscellaneous"
	case ClassApp:
		return "application"
	case ClassVendor:
		return "vendor"
	}
	return fmt.Sprintf("class-0x%02x", c)
}

// ifaceClass returns the class name of an interface for naming class requests;
// DFU is an application-specific class with subclass 1.
func ifaceClass(i *Interface) string {
	if i == nil {
		return ""
	}
	if i.Class == ClassApp && i.Subclass == 1 {
		return "DFU"
	}
	return ClassName(i.Class)
}

var classRequests = map[string]map[byte]string{
	"HID": {1: "GET_REPORT", 2: "GET_IDLE", 3: "GET_PROTOCOL", 9: "SET_REPORT", 0x0a: "SET_IDLE", 0x0b: "SET_PROTOCOL"},
	"CDC": {0: "SEND_ENCAPSULATED_COMMAND", 1: "GET_ENCAPSULATED_RESPONSE", 0x20: "SET_LINE_CODING", 0x21: "GET_LINE_CODING",
		0x22: "SET_CONTROL_LINE_STATE", 0x23: "SEND_BREAK"},
	"mass-storage": {0xfe: "GET_MAX_LUN", 0xff: "BULK_ONLY_RESET"},
	"DFU":          {0: "DFU_DETACH", 1: "DFU_DNLOAD", 2: "DFU_UPLOAD", 3: "DFU_GETSTATUS", 4: "DFU_CLRSTATUS", 5: "DFU_GETSTATE", 6: "DFU_ABORT"},
}

// DescribeSetup names a setup packet. ifc is the interface a class request
// addresses, when known; without it a class request is only labelled "class".
func DescribeSetup(s Setup, ifc *Interface) string {
	switch s.Kind() {
	case "standard":
		name := stdRequests[s.Request]
		if name == "" {
			name = fmt.Sprintf("request 0x%02x", s.Request)
		}
		if s.Request == ReqGetDescriptor || s.Request == ReqSetDescriptor {
			t := byte(s.Value >> 8)
			d := descTypes[t]
			if d == "" {
				d = fmt.Sprintf("0x%02x", t)
			}
			name += "(" + d
			if t == 3 || t == 2 {
				name += fmt.Sprintf(" #%d", s.Value&0xff)
			}
			name += ")"
		}
		return name
	case "class":
		cls := ifaceClass(ifc)
		if names, ok := classRequests[cls]; ok {
			if n, ok := names[s.Request]; ok {
				return cls + " " + n
			}
		}
		if cls != "" {
			return fmt.Sprintf("%s class request 0x%02x", cls, s.Request)
		}
		return fmt.Sprintf("class request 0x%02x", s.Request)
	case "vendor":
		return fmt.Sprintf("vendor request 0x%02x value 0x%04x index 0x%04x", s.Request, s.Value, s.Index)
	}
	return fmt.Sprintf("reserved request 0x%02x", s.Request)
}

// DeviceDescriptor holds the fields of a device descriptor that fit a short read.
type DeviceDescriptor struct {
	USB        uint16
	Class      byte
	Subclass   byte
	Protocol   byte
	MaxPacket0 byte
	VID, PID   uint16 // 0 when the capture holds fewer than 12 bytes
	BCD        uint16
	Full       bool
}

// ParseDevice parses a (possibly truncated, possibly 8-byte) device descriptor.
func ParseDevice(b []byte) (DeviceDescriptor, bool) {
	if len(b) < 8 || b[1] != 1 {
		return DeviceDescriptor{}, false
	}
	le := binary.LittleEndian
	d := DeviceDescriptor{USB: le.Uint16(b[2:]), Class: b[4], Subclass: b[5], Protocol: b[6], MaxPacket0: b[7]}
	if len(b) >= 14 {
		d.VID, d.PID, d.BCD, d.Full = le.Uint16(b[8:]), le.Uint16(b[10:]), le.Uint16(b[12:]), true
	}
	return d, true
}

// Endpoint is one endpoint descriptor.
type Endpoint struct {
	Addr      byte
	Transfer  Transfer
	MaxPacket uint16
	Interval  byte
}

// In reports an IN endpoint.
func (e Endpoint) In() bool { return e.Addr&0x80 != 0 }

// Interface is one interface alternate setting.
type Interface struct {
	Number    byte
	Alt       byte
	Class     byte
	Subclass  byte
	Protocol  byte
	Endpoints []Endpoint
}

// Config is a parsed configuration descriptor.
type Config struct {
	Value      byte
	Interfaces []Interface
	// Truncated is set when the capture held less than the descriptor's
	// declared total length (usbmon's text API keeps at most 32 data bytes).
	Truncated bool
}

// ParseConfig walks a configuration descriptor and its sub-descriptors.
func ParseConfig(b []byte) (Config, bool) {
	if len(b) < 9 || b[1] != 2 {
		return Config{}, false
	}
	le := binary.LittleEndian
	c := Config{Value: b[5]}
	total := int(le.Uint16(b[2:]))
	if len(b) < total {
		c.Truncated = true
	} else {
		b = b[:total]
	}
	for i := int(b[0]); i+2 <= len(b); {
		n := int(b[i])
		if n < 2 || i+n > len(b) {
			c.Truncated = true
			break
		}
		d := b[i : i+n]
		switch d[1] {
		case 4:
			if n >= 9 {
				c.Interfaces = append(c.Interfaces, Interface{Number: d[2], Alt: d[3], Class: d[5], Subclass: d[6], Protocol: d[7]})
			}
		case 5:
			if n >= 7 && len(c.Interfaces) > 0 {
				ifc := &c.Interfaces[len(c.Interfaces)-1]
				ifc.Endpoints = append(ifc.Endpoints, Endpoint{Addr: d[2], Transfer: endpointTransfer(d[3]), MaxPacket: le.Uint16(d[4:]), Interval: d[6]})
			}
		}
		i += n
	}
	return c, true
}

// Classes lists the distinct interface class names of a configuration.
func (c Config) Classes() []string {
	var out []string
	seen := map[string]bool{}
	for i := range c.Interfaces {
		n := ifaceClass(&c.Interfaces[i])
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// LineCoding decodes the 7 bytes of CDC SET/GET_LINE_CODING.
func LineCoding(b []byte) (string, bool) {
	if len(b) < 7 {
		return "", false
	}
	par := map[byte]string{0: "N", 1: "O", 2: "E", 3: "M", 4: "S"}[b[5]]
	stop := map[byte]string{0: "1", 1: "1.5", 2: "2"}[b[4]]
	if par == "" || stop == "" {
		return fmt.Sprintf("%d baud (invalid format)", binary.LittleEndian.Uint32(b)), true
	}
	return fmt.Sprintf("%d baud %d%s%s", binary.LittleEndian.Uint32(b), b[6], par, stop), true
}

var dfuStates = []string{"appIDLE", "appDETACH", "dfuIDLE", "dfuDNLOAD-SYNC", "dfuDNBUSY", "dfuDNLOAD-IDLE", "dfuMANIFEST-SYNC",
	"dfuMANIFEST", "dfuMANIFEST-WAIT-RESET", "dfuUPLOAD-IDLE", "dfuERROR"}

// DFUStatus decodes a DFU_GETSTATUS response.
func DFUStatus(b []byte) (string, bool) {
	if len(b) < 6 {
		return "", false
	}
	state := "state " + fmt.Sprint(b[4])
	if int(b[4]) < len(dfuStates) {
		state = dfuStates[b[4]]
	}
	poll := int(b[1]) | int(b[2])<<8 | int(b[3])<<16
	st := "ok"
	if b[0] != 0 {
		st = fmt.Sprintf("error 0x%02x", b[0])
	}
	return fmt.Sprintf("status %s, %s, poll %d ms", st, state, poll), true
}

var scsiOps = map[byte]string{
	0x00: "TEST_UNIT_READY", 0x03: "REQUEST_SENSE", 0x12: "INQUIRY", 0x1a: "MODE_SENSE(6)", 0x1b: "START_STOP_UNIT",
	0x1e: "PREVENT_ALLOW_MEDIUM_REMOVAL", 0x23: "READ_FORMAT_CAPACITIES", 0x25: "READ_CAPACITY(10)", 0x28: "READ(10)",
	0x2a: "WRITE(10)", 0x2f: "VERIFY(10)", 0x35: "SYNCHRONIZE_CACHE(10)", 0x5a: "MODE_SENSE(10)",
	0x9e: "SERVICE_ACTION_IN(16)", 0xa8: "READ(12)", 0xaa: "WRITE(12)",
}

// MSCCommandBlock decodes a bulk-only Command Block Wrapper (31 bytes, "USBC").
func MSCCommandBlock(b []byte) (string, bool) {
	if len(b) < 16 || string(b[:4]) != "USBC" {
		return "", false
	}
	le := binary.LittleEndian
	op := b[15]
	name := scsiOps[op]
	if name == "" {
		name = fmt.Sprintf("SCSI 0x%02x", op)
	}
	dir := "no data"
	if n := le.Uint32(b[8:]); n > 0 {
		dir = fmt.Sprintf("%d bytes %s", n, map[bool]string{true: "in", false: "out"}[b[12]&0x80 != 0])
	}
	s := fmt.Sprintf("CBW tag %#x LUN %d %s, %s", le.Uint32(b[4:]), b[13], name, dir)
	if (op == 0x28 || op == 0x2a) && len(b) >= 24 {
		s += fmt.Sprintf(" LBA %d x%d", binary.BigEndian.Uint32(b[17:]), binary.BigEndian.Uint16(b[22:]))
	}
	return s, true
}

// MSCStatusWrapper decodes a bulk-only Command Status Wrapper (13 bytes, "USBS").
func MSCStatusWrapper(b []byte) (string, bool) {
	if len(b) < 13 || string(b[:4]) != "USBS" {
		return "", false
	}
	st := map[byte]string{0: "passed", 1: "failed", 2: "phase error"}[b[12]]
	if st == "" {
		st = fmt.Sprintf("0x%02x", b[12])
	}
	return fmt.Sprintf("CSW tag %#x residue %d %s", binary.LittleEndian.Uint32(b[4:]), binary.LittleEndian.Uint32(b[8:]), st), true
}

// printable returns b as text when it is mostly printable ASCII.
func printable(b []byte, max int) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) > max {
		b = b[:max]
	}
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c == '\r':
			sb.WriteString(`\r`)
		case c == '\n':
			sb.WriteString(`\n`)
		case c >= 0x20 && c < 0x7f:
			sb.WriteByte(c)
		default:
			return ""
		}
	}
	return sb.String()
}

// endpointTransfer maps the bmAttributes transfer bits of an endpoint
// descriptor (0 control, 1 isochronous, 2 bulk, 3 interrupt) to the usbmon
// numbering used by Transfer.
func endpointTransfer(attr byte) Transfer {
	return [...]Transfer{Control, Isochronous, Bulk, Interrupt}[attr&3]
}
