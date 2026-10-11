// Package ftdi implements the minimal FTDI USB protocol needed to run an
// MPSSE engine: SIO control requests, bit modes, latency, purge, and bulk
// transfers with the two modem-status bytes that FT2232-class devices put at
// the start of every IN packet.
//
// Provenance: written from FTDI's public application notes (AN_108 MPSSE
// command processor, AN_135 MPSSE basics, the FT2232D/FT2232H datasheets) and
// from observation of the hardware. No GPL source was copied (see
// plugins/ftdi/NOTICE).
//
// The package does not assume genuine FTDI silicon: devices that emulate the
// protocol (for example a BL702 running the Sipeed RV-Debugger firmware) are
// driven the same way, and any deviation is a profile quirk, not a branch on
// "is this a real FTDI".
package ftdi

import "fmt"

// FTDI vendor control requests (bmRequestType 0x40 out / 0xC0 in).
const (
	reqTypeOut = 0x40
	reqTypeIn  = 0xC0

	ReqReset        = 0x00
	ReqSetModemCtrl = 0x01
	ReqSetFlowCtrl  = 0x02
	ReqSetBaudrate  = 0x03
	ReqSetData      = 0x04
	ReqGetStatus    = 0x05
	ReqSetEventChar = 0x06
	ReqSetErrorChar = 0x07
	ReqSetLatency   = 0x09
	ReqGetLatency   = 0x0A
	ReqSetBitMode   = 0x0B
	ReqReadPins     = 0x0C
)

// SIO_RESET values.
const (
	resetSIO     = 0
	resetPurgeRX = 1
	resetPurgeTX = 2
)

// BitMode is the FTDI bit mode selector.
type BitMode uint8

// Bit modes (SET_BITMODE high byte).
const (
	BitModeReset    BitMode = 0x00
	BitModeBitBang  BitMode = 0x01
	BitModeMPSSE    BitMode = 0x02
	BitModeSyncBB   BitMode = 0x04
	BitModeMCU      BitMode = 0x08
	BitModeOpto     BitMode = 0x10
	BitModeCBUS     BitMode = 0x20
	BitModeSyncFIFO BitMode = 0x40
)

func (m BitMode) String() string {
	switch m {
	case BitModeReset:
		return "reset"
	case BitModeBitBang:
		return "bitbang"
	case BitModeMPSSE:
		return "mpsse"
	case BitModeSyncBB:
		return "sync-bitbang"
	case BitModeMCU:
		return "mcu-host"
	case BitModeOpto:
		return "opto"
	case BitModeCBUS:
		return "cbus-bitbang"
	case BitModeSyncFIFO:
		return "sync-fifo"
	}
	return fmt.Sprintf("0x%02x", uint8(m))
}

// ChipType is the FTDI silicon (or emulated) generation, taken from the USB
// bcdDevice field.
type ChipType int

// Chip generations.
const (
	ChipUnknown ChipType = iota
	ChipFT2232C          // FT2232C/D, bcdDevice 0x0500
	ChipFT2232H          // bcdDevice 0x0700
	ChipFT4232H          // bcdDevice 0x0800
	ChipFT232H           // bcdDevice 0x0900
)

// ChipFromBCD maps bcdDevice to a chip type.
func ChipFromBCD(bcd uint16) ChipType {
	switch bcd {
	case 0x0500:
		return ChipFT2232C
	case 0x0700:
		return ChipFT2232H
	case 0x0800:
		return ChipFT4232H
	case 0x0900:
		return ChipFT232H
	}
	return ChipUnknown
}

// ChipInfo describes what a chip generation can do.
type ChipInfo struct {
	Name string
	// MPSSEChannels lists the 0-based channels with an MPSSE engine.
	MPSSEChannels []int
	// BaseClockHz is the MPSSE master clock before the divisor.
	BaseClockHz uint32
	// HighSpeed marks the "H" parts: divide-by-5, 3-phase and adaptive
	// clocking and the clock-only commands exist.
	HighSpeed bool
	// MaxPacket is the bulk packet size at the native USB speed.
	MaxPacket int
}

var chipInfos = map[ChipType]ChipInfo{
	ChipFT2232C: {Name: "FT2232C/D", MPSSEChannels: []int{0}, BaseClockHz: 6_000_000, MaxPacket: 64},
	ChipFT2232H: {Name: "FT2232H", MPSSEChannels: []int{0, 1}, BaseClockHz: 60_000_000, HighSpeed: true, MaxPacket: 512},
	ChipFT4232H: {Name: "FT4232H", MPSSEChannels: []int{0, 1}, BaseClockHz: 60_000_000, HighSpeed: true, MaxPacket: 512},
	ChipFT232H:  {Name: "FT232H", MPSSEChannels: []int{0}, BaseClockHz: 60_000_000, HighSpeed: true, MaxPacket: 512},
}

// Info returns the capabilities of t; ok is false for ChipUnknown.
func (t ChipType) Info() (ChipInfo, bool) {
	i, ok := chipInfos[t]
	return i, ok
}

func (t ChipType) String() string {
	if i, ok := chipInfos[t]; ok {
		return i.Name
	}
	return "unknown"
}

// HasMPSSE reports whether channel (0-based) has an MPSSE engine.
func (t ChipType) HasMPSSE(channel int) bool {
	i, ok := chipInfos[t]
	if !ok {
		return false
	}
	for _, c := range i.MPSSEChannels {
		if c == channel {
			return true
		}
	}
	return false
}

// Descriptor identifies a USB VID/PID this package may drive.
type Descriptor struct {
	VID, PID uint16
	Name     string
}

// Known is the table of USB identities treated as FTDI MPSSE adapters. Adding
// a device is adding a row here (and, if it needs quirks, a profile).
var Known = []Descriptor{
	{0x0403, 0x6010, "FT2232C/D/H dual UART/FIFO"},
}

// IsKnown reports whether vid:pid is in the table.
func IsKnown(vid, pid uint16) bool {
	for _, d := range Known {
		if d.VID == vid && d.PID == pid {
			return true
		}
	}
	return false
}

// StripStatus removes the two modem-status bytes that start every USB packet
// of maxPacket bytes (the last packet may be shorter) and returns the payload
// together with the last status pair seen. A packet shorter than two bytes
// is malformed.
func StripStatus(raw []byte, maxPacket int) (payload []byte, status [2]byte, err error) {
	if maxPacket < 3 {
		return nil, status, fmt.Errorf("ftdi: invalid packet size %d", maxPacket)
	}
	payload = make([]byte, 0, len(raw))
	for off := 0; off < len(raw); off += maxPacket {
		end := min(off+maxPacket, len(raw))
		pkt := raw[off:end]
		if len(pkt) < 2 {
			return nil, status, fmt.Errorf("ftdi: truncated packet of %d byte(s) (no status bytes)", len(pkt))
		}
		status = [2]byte{pkt[0], pkt[1]}
		payload = append(payload, pkt[2:]...)
	}
	return payload, status, nil
}
