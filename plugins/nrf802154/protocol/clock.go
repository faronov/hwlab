package protocol

import "github.com/faronov/hwlab/internal/clockmap"

// ClockMap maps the sniffer's device timer onto the session timeline; see
// internal/clockmap.
type ClockMap = clockmap.ClockMap

// ClockStats describes a ClockMap's mapping quality.
type ClockStats = clockmap.ClockStats

// NewClockMap returns a ClockMap with one-second windows.
func NewClockMap() *ClockMap { return clockmap.NewClockMap() }
