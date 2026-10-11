package app

import (
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/faronov/hwlab/internal/timeline"
	"github.com/faronov/hwlab/internal/usbcap"
)

const (
	defaultUSBLimit = 100
	maxUSBLimit     = 1000
)

// USBQuery asks for the analysis of the USB packets a session recorded with
// the usbcap plugin (or any plugin emitting protocol "usb" packets in the
// usbmon layout).
type USBQuery struct {
	Session    string
	Source     string // usb source alias; empty = every source that has USB packets
	Device     string // bus:device filter on the transaction list ("1:5"); stats stay global
	OnlyErrors bool   // list only transactions that did not complete with status 0
	FromNs     int64
	ToNs       int64 // <= FromNs: to the end
	Limit      int   // transactions listed per source (default 100, max 1000)
}

// USBStream is the analysis of one USB source.
type USBStream struct {
	Source       string                 `json:"source"`
	Stats        usbcap.Stats           `json:"stats"`
	Devices      []usbcap.DeviceInfo    `json:"devices"`
	Endpoints    []usbcap.EndpointStats `json:"endpoints"`
	ErrorCounts  []usbcap.ErrorCount    `json:"error_counts,omitempty"`
	Transactions []usbcap.Transaction   `json:"transactions"`
	// Matching is how many transactions matched the filters before Limit.
	Matching  int  `json:"matching"`
	Truncated bool `json:"truncated,omitempty"`
}

// USBReport is the result of AnalyzeUSB.
type USBReport struct {
	Session string      `json:"session"`
	Streams []USBStream `json:"streams"`
	Note    string      `json:"note"`
}

const usbNote = "Decoded offline from recorded URB events (usbmon level: no SOF, tokens or handshakes). Latencies are differences of kernel URB timestamps, not bus times. Devices enumerated before the capture began stay unlabelled: classes come only from descriptors the capture contains. Payload beyond the capture's snaplen (and usbmon's 32-byte text limit) is not available."

// AnalyzeUSB pairs URB submissions with completions, identifies devices and
// classes from the enumeration traffic and decodes control, HID, CDC, MSC and
// DFU transfers. It works on recorded data and on replays, with no hardware.
func (a *App) AnalyzeUSB(q USBQuery) (*USBReport, error) {
	if q.Limit <= 0 {
		q.Limit = defaultUSBLimit
	}
	if q.Limit > maxUSBLimit {
		q.Limit = maxUSBLimit
	}
	tq := TimelineQuery{Session: q.Session, FromNs: q.FromNs, ToNs: q.ToNs, Types: []string{timeline.TypePacket}, Limit: 1 << 30}
	if q.Source != "" {
		tq.Sources = []string{q.Source}
	}
	evs, err := a.QueryTimeline(tq)
	if err != nil {
		return nil, err
	}
	bySource := map[string][]usbcap.Packet{}
	for _, e := range evs {
		if e.Data["protocol"] != usbcap.Protocol {
			continue
		}
		h, _ := e.Data["hex"].(string)
		raw, err := hex.DecodeString(h)
		if err != nil {
			continue
		}
		p, err := usbcap.Decode(raw)
		if err != nil {
			continue
		}
		p.TimestampNs = e.TimestampNs // the event carries the nanosecond time; the header only microseconds
		bySource[e.SourceID] = append(bySource[e.SourceID], p)
	}
	if len(bySource) == 0 {
		return nil, fmt.Errorf("session has no USB packets to analyze; record them with the usbcap plugin")
	}
	names := make([]string, 0, len(bySource))
	for s := range bySource {
		names = append(names, s)
	}
	sort.Strings(names)
	rep := &USBReport{Session: q.Session, Note: usbNote}
	for _, s := range names {
		an := usbcap.Analyze(bySource[s])
		st := USBStream{Source: s, Stats: an.Stats, Devices: an.Devices, Endpoints: an.Endpoints, ErrorCounts: an.ErrorCounts}
		for _, t := range an.Transactions {
			if q.Device != "" && t.Device != q.Device {
				continue
			}
			if q.OnlyErrors && t.Status == 0 {
				continue
			}
			st.Matching++
			if len(st.Transactions) < q.Limit {
				st.Transactions = append(st.Transactions, t)
			}
		}
		st.Truncated = st.Matching > len(st.Transactions)
		rep.Streams = append(rep.Streams, st)
	}
	return rep, nil
}
