// Package profiles describes adapters built around FTDI-style MPSSE engines:
// which USB identity they present, which channel carries JTAG, how the
// MPSSE GPIO lines are wired, and which documented MPSSE features they do not
// implement. The architecture (usb, ftdi, mpsse, jtag) knows nothing about any
// particular adapter; every adapter-specific fact lives here.
//
// Provenance of the built-in data:
//   - generic-ft2232: the pin layout is the de-facto standard one that FTDI's
//     own application notes (AN_135, AN_129) use for JTAG on ADBUS0..3.
//   - sipeed-bl702: the GPIO layout is a hardware fact taken from the
//     OpenOCD interface file sipeed-rv-debugger.cfg (a configuration, not
//     code, and no code is copied); the quirks were observed on the device
//     and are consistent with the open-source firmware's documented command
//     subset. See plugins/ftdi/NOTICE.
package profiles

import (
	"fmt"
	"sort"
	"strings"
)

// Implementation says whether the USB device is real FTDI silicon.
type Implementation string

// Implementation values.
const (
	RealFTDI           Implementation = "real-ftdi"
	CompatibleEmulator Implementation = "compatible-emulator"
)

// Pin is a bit position of the low GPIO byte (ADBUS0..7), or -1 when the
// signal is not available.
type Pin int

// NoPin marks a signal the adapter does not provide.
const NoPin Pin = -1

// Layout is the MPSSE low-byte GPIO wiring.
type Layout struct {
	TCK, TDI, TDO, TMS Pin
	// NSRST and NTRST are active-low reset outputs (NoPin when absent or
	// unverified). They are never driven by the scan path.
	NSRST, NTRST Pin
	// InitValue and InitDir are the low-byte value and direction (1 =
	// output) written when the adapter is initialised: TCK, TDI, TMS as
	// outputs, TDO as input, TCK low and TMS high.
	InitValue, InitDir byte
}

// Mask returns the bit mask of a pin (0 for NoPin).
func (l Layout) Mask(p Pin) byte {
	if p < 0 || p > 7 {
		return 0
	}
	return 1 << p
}

// Validate checks the layout is self-consistent.
func (l Layout) Validate() error {
	seen := map[Pin]string{}
	for name, p := range map[string]Pin{"TCK": l.TCK, "TDI": l.TDI, "TDO": l.TDO, "TMS": l.TMS} {
		if p < 0 || p > 7 {
			return fmt.Errorf("layout: %s pin %d out of range 0..7", name, p)
		}
		if prev, dup := seen[p]; dup {
			return fmt.Errorf("layout: %s and %s share pin %d", name, prev, p)
		}
		seen[p] = name
	}
	// MPSSE hard-wires the JTAG pins to ADBUS0 (TCK), ADBUS1 (TDI),
	// ADBUS2 (TDO) and ADBUS3 (TMS).
	if l.TCK != 0 || l.TDI != 1 || l.TDO != 2 || l.TMS != 3 {
		return fmt.Errorf("layout: MPSSE fixes TCK/TDI/TDO/TMS on ADBUS0..3, got %d/%d/%d/%d", l.TCK, l.TDI, l.TDO, l.TMS)
	}
	out := l.Mask(l.TCK) | l.Mask(l.TDI) | l.Mask(l.TMS)
	if l.InitDir&out != out {
		return fmt.Errorf("layout: direction 0x%02x does not make TCK/TDI/TMS outputs", l.InitDir)
	}
	if l.InitDir&l.Mask(l.TDO) != 0 {
		return fmt.Errorf("layout: direction 0x%02x makes TDO an output", l.InitDir)
	}
	return nil
}

// Quirks are deviations from the FTDI MPSSE documentation.
type Quirks struct {
	// LoopbackIgnored: opcodes 0x84/0x85 are accepted but do nothing.
	LoopbackIgnored bool
	// FixedClock: the divisor opcode is accepted but ignored; the JTAG clock
	// is whatever the firmware uses.
	FixedClock bool
	// FakeGPIO: GPIO set/get opcodes are accepted but do not drive or read
	// the pins; reads return constants.
	FakeGPIO bool
	// OnlyReadWriteShifts: read-only data opcodes (0x2x) are rejected; every
	// capture must be a write+read command. The JTAG layer always uses such
	// commands, so this is informational.
	OnlyReadWriteShifts bool
	// OptionalControl lists SIO control requests the device may stall.
	OptionalControl []uint8
}

// Profile is one adapter definition.
type Profile struct {
	Name           string
	Description    string
	Implementation Implementation
	// VID/PID select candidate devices. Strings, when set, only raise the
	// match confidence; they are never required for the generic profile.
	VID, PID uint16
	// Manufacturer/Product are matched case-insensitively as substrings.
	Manufacturer, Product string
	// JTAGInterface is the USB interface carrying MPSSE/JTAG; UARTInterface
	// is the companion serial port (-1 if none). The JTAG function claims
	// only JTAGInterface.
	JTAGInterface int
	UARTInterface int
	// NominalClockHz is the JTAG clock the profile expects (the fixed clock
	// of a FixedClock device, or the default request otherwise).
	NominalClockHz uint32
	Layout         Layout
	Quirks         Quirks
	// LatencyMs is the latency timer to set (default 16 when 0).
	LatencyMs int
	// Sources documents where the facts in this profile come from.
	Sources []string
	// Verified describes what has actually been observed on hardware.
	Verified string
}

// StandardLayout is the common FTDI JTAG wiring.
var StandardLayout = Layout{TCK: 0, TDI: 1, TDO: 2, TMS: 3, NSRST: NoPin, NTRST: NoPin, InitValue: 0x08, InitDir: 0x0B}

// Generic is the fallback for any FTDI MPSSE adapter.
var Generic = Profile{
	Name:           "generic-ft2232",
	Description:    "Any FTDI MPSSE adapter with the standard ADBUS0..3 JTAG wiring",
	Implementation: RealFTDI,
	VID:            0x0403, PID: 0x6010,
	JTAGInterface:  0,
	UARTInterface:  1,
	NominalClockHz: 1_000_000,
	Layout:         StandardLayout,
	LatencyMs:      2,
	Sources:        []string{"FTDI AN_135 / AN_108 (public application notes)"},
}

// SipeedBL702 is the Sipeed RV-Debugger-BL702 firmware (XT-ZB1 and similar):
// a BL702 microcontroller emulating an FT2232D.
var SipeedBL702 = Profile{
	Name:           "sipeed-bl702",
	Description:    "Sipeed RV-Debugger-BL702 firmware emulating an FT2232D (JTAG on channel A, UART on channel B)",
	Implementation: CompatibleEmulator,
	VID:            0x0403, PID: 0x6010,
	Manufacturer:   "SIPEED",
	Product:        "JTAG Debugger",
	JTAGInterface:  0,
	UARTInterface:  1,
	NominalClockHz: 6_000_000,
	Layout: Layout{
		TCK: 0, TDI: 1, TDO: 2, TMS: 3,
		// nSRST is bit 5 in the published OpenOCD configuration, but whether the
		// XT-ZB1 routes it to a pin is unknown, so it is not exposed.
		NSRST: NoPin, NTRST: NoPin,
		InitValue: 0x08, InitDir: 0x1B,
	},
	Quirks: Quirks{
		LoopbackIgnored:     true,
		FixedClock:          true,
		FakeGPIO:            true,
		OnlyReadWriteShifts: true,
		OptionalControl:     []uint8{0x02, 0x06, 0x07, 0x09, 0x0A, 0x0B},
	},
	LatencyMs: 2,
	Sources: []string{
		"OpenOCD tcl/interface/ftdi/sipeed-rv-debugger.cfg: layout_init 0x0008 0x001b, adapter speed 6000 (hardware facts; GPL-2.0 source, no code copied)",
		"Sipeed RV-Debugger-BL702 firmware sources (Apache-2.0 headers): behavioural reference for the supported MPSSE command subset",
	},
	Verified: "USB identity, ftdi_sio detach/re-bind on interface 0, MPSSE sync and clocked shifts, empty-chain scan on one attached device; no JTAG target scanned (docs/notes/phase16-ftdi-mpsse-jtag-results.md)",
}

var builtin = []Profile{SipeedBL702, Generic}

// All returns the built-in profiles, most specific first.
func All() []Profile { return append([]Profile(nil), builtin...) }

// ByName looks a profile up by name.
func ByName(name string) (Profile, bool) {
	for _, p := range builtin {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// Names lists the profile names.
func Names() []string {
	var n []string
	for _, p := range builtin {
		n = append(n, p.Name)
	}
	sort.Strings(n)
	return n
}

func contains(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// Match picks the best profile for a USB identity: a profile with matching
// strings wins over the generic one; the generic profile accepts any
// VID/PID the caller already decided is an FTDI MPSSE adapter.
func Match(vid, pid uint16, manufacturer, product string) Profile {
	for _, p := range builtin {
		if p.Name == Generic.Name || p.VID != vid || p.PID != pid {
			continue
		}
		if p.Manufacturer != "" && !contains(manufacturer, p.Manufacturer) {
			continue
		}
		if p.Product != "" && !contains(product, p.Product) {
			continue
		}
		return p
	}
	return Generic
}
