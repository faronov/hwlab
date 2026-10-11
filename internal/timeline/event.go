// Package timeline holds the unified, time-ordered view of low-rate events
// from all devices of a session. High-rate samples live in the sample store
// (internal/session) and are never kept as individual events.
package timeline

import (
	"encoding/json"
	"fmt"
	"time"
)

// Event types produced by the core. Plugins may add their own strings.
const (
	TypeMarker  = "marker"
	TypeLog     = "log"
	TypePacket  = "packet"
	TypeTrigger = "trigger"
	TypeDecoded = "decoded"
	TypeDebug   = "debug"
	// TypeInvoke records a scheduled method call on a device (an experiment
	// step) with its arguments, result or error, so command/response
	// devices such as debug probes can be replayed.
	TypeInvoke = "invoke"
	// TypeDigitalEdge is derived by the core from DigitalBatch payloads.
	TypeDigitalEdge = "digital.edge"
	// TypePowerSpike and friends are emitted by analyzers.
	TypePowerRise = "analysis.power_rise"
	TypePowerFall = "analysis.power_fall"
)

// Event is the extensible envelope of the unified timeline.
type Event struct {
	// TimestampNs is nanoseconds since the session monotonic epoch.
	TimestampNs int64 `json:"t_ns"`
	// DurationNs is optional; 0 for instantaneous events.
	DurationNs int64 `json:"dur_ns,omitempty"`
	// SourceID is the experiment alias (or device id) of the producer.
	SourceID string `json:"source"`
	DeviceID string `json:"device,omitempty"`
	StreamID string `json:"stream,omitempty"`
	Type     string `json:"type"`
	// Summary is a short human-readable description.
	Summary string         `json:"summary,omitempty"`
	Data    map[string]any `json:"data,omitempty"`

	// Seq is the arrival sequence number, used for stable ordering.
	Seq uint64 `json:"seq,omitempty"`
}

// Time returns the event timestamp as a duration since the epoch.
func (e Event) Time() time.Duration { return time.Duration(e.TimestampNs) }

func (e Event) String() string {
	s := fmt.Sprintf("%s  %-14s %-12s %s", FormatNs(e.TimestampNs), e.Type, e.SourceID, e.Summary)
	return s
}

// MarshalLine encodes e as a single JSON line (no trailing newline).
func (e Event) MarshalLine() ([]byte, error) { return json.Marshal(e) }

// FormatNs renders a timestamp with microsecond-and-below precision.
func FormatNs(ns int64) string {
	neg := ""
	if ns < 0 {
		neg, ns = "-", -ns
	}
	return fmt.Sprintf("%s%d.%06d ms", neg, ns/1_000_000, ns%1_000_000)
}
