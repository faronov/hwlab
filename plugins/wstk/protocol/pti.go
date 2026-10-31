package protocol

import "fmt"

// EFR32 Packet Trace (PTI) payload of DCH message types DCHEFRTX/RX/Other:
//
//	hw start (0xF8 rx, 0xFC tx), over-the-air bytes (PHR, MPDU, FCS),
//	hw end (0xF9 rx ok, 0xFA rx abort, 0xFD tx ok, 0xFE tx abort),
//	appended info: [rssi] [syncword x4] [radio config x1..2], radio info,
//	status, appended-info config (last byte).
//
// The config byte gives direction (bit 6), the variable appended length
// (bits 3..5) and the format version (bits 0..2); the status byte the
// protocol (low nibble) and an error code (high nibble); the radio info byte
// the channel index (bits 0..5). Layout per Silicon Labs' public PTI
// documentation and the Wireshark silabs-dch dissector, used as references.

// PTI protocol ids (status low nibble).
const (
	PTIProtoCustom    = 0
	PTIProtoEmberPHY  = 1
	PTIProtoThread    = 2
	PTIProtoBLE       = 3
	PTIProtoConnect   = 4
	PTIProtoZigbee    = 5
	PTIProtoZWave     = 6
	PTIProtoWiSUN     = 7
	PTIProto802154Raw = 8
)

// PTIFrame is one decoded EFR32 packet trace record.
type PTIFrame struct {
	RX       bool
	Aborted  bool // hw end reported an abort
	Protocol int
	Error    int // status high nibble
	// Channel is the IEEE 802.15.4 channel for 2.4 GHz O-QPSK (radio info
	// index + 11, per the reference), otherwise the raw radio info index.
	Channel    int
	RawChannel int
	RSSIdBm    int  // valid when HasRSSI
	HasRSSI    bool // only received frames carry RSSI
	HWStart    byte
	HWEnd      byte
	// OTA is the over-the-air payload after the PHR, FCS included.
	OTA []byte
	// IEEE802154 is set when OTA is an IEEE 802.15.4 MPDU with a 2-byte FCS.
	IEEE802154 bool
}

// MPDU returns the 802.15.4 MPDU without FCS; nil for other protocols.
func (f PTIFrame) MPDU() []byte {
	if !f.IEEE802154 || len(f.OTA) < 2 {
		return nil
	}
	return f.OTA[:len(f.OTA)-2]
}

// ParsePTI decodes an EFR32 packet trace payload.
func ParsePTI(b []byte) (PTIFrame, error) {
	// hw start, hw end, radio info, status, config
	if len(b) < 5 {
		return PTIFrame{}, fmt.Errorf("%w: PTI record of %d bytes", ErrProtocol, len(b))
	}
	last := len(b) - 1
	cfg, status := b[last], b[last-1]
	if cfg>>4 == 0xf || status>>4 == 0xf {
		return PTIFrame{}, fmt.Errorf("%w: legacy PTI trailer % x not supported", ErrProtocol, b[last-1:])
	}
	f := PTIFrame{RX: cfg&0x40 != 0, Protocol: int(status & 0x0f), Error: int(status >> 4), HWStart: b[0]}
	varLen := int(cfg&0x38) >> 3
	version := cfg & 0x07
	var rssiLen, syncLen, radioCfgLen int
	if f.RX {
		switch varLen {
		case 1:
			rssiLen = 1
		case 2, 3:
			rssiLen, radioCfgLen = 1, 1
		case 5:
			rssiLen, syncLen = 1, 4
		case 6, 7:
			rssiLen, syncLen, radioCfgLen = 1, 4, 1
		default:
			return f, fmt.Errorf("%w: PTI rx appended length %d", ErrProtocol, varLen)
		}
		if varLen == 3 || varLen == 7 {
			radioCfgLen = 2
		}
	} else {
		switch varLen {
		case 0:
		case 1, 2:
			radioCfgLen = 1
		case 4:
			syncLen = 4
		case 5, 6:
			syncLen, radioCfgLen = 4, 1
		default:
			return f, fmt.Errorf("%w: PTI tx appended length %d", ErrProtocol, varLen)
		}
		if varLen == 2 || varLen == 6 {
			radioCfgLen = 2
		}
	}
	appended := varLen + 3
	end := last + 1 - appended - 1 // index of hw end
	if end < 1 {
		return f, fmt.Errorf("%w: PTI record of %d bytes with %d appended", ErrProtocol, len(b), appended)
	}
	f.HWEnd = b[end]
	f.Aborted = f.HWEnd == 0xFA || f.HWEnd == 0xFE
	p := end + 1
	if rssiLen > 0 {
		r := int(int8(b[p]))
		if version == 1 {
			r = int(int8(b[p] - 0x32))
		}
		f.RSSIdBm, f.HasRSSI = r, true
		p += rssiLen
	}
	p += syncLen
	radioCfgID, phrLen := 0, 1
	if radioCfgLen > 0 {
		rc := b[p]
		radioCfgID = int(rc & 0x07)
		switch {
		case rc&0x40 != 0:
			phrLen = 4
		case rc&0x80 != 0:
			phrLen = 2
		}
		p += radioCfgLen
	}
	f.RawChannel = int(b[p] & 0x3f)
	f.Channel = f.RawChannel
	ota := b[1:end]
	switch f.Protocol {
	case PTIProtoEmberPHY, PTIProtoThread, PTIProtoZigbee, PTIProto802154Raw:
		if radioCfgID == 0 {
			f.Channel = f.RawChannel + 11
		}
		if phrLen == 1 && len(ota) >= 1 {
			f.OTA = ota[1:]
			f.IEEE802154 = radioCfgID == 0 && len(f.OTA) >= 2
			return f, nil
		}
	case PTIProtoBLE:
		phrLen = 2
	}
	if len(ota) >= phrLen {
		f.OTA = ota[phrLen:]
	}
	return f, nil
}
