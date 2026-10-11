package protocol

import "math"

// ClockMap maps the sniffer's device timer (µs since boot) onto the hwlab
// session timeline.
//
// Each frame gives one sample of offset = host_rx_ns - device_ns. The sample
// can only be too large (USB, OS and scheduling delay are never negative), so
// the best estimate of the true offset is a running minimum. A minimum over
// the whole capture would ignore crystal drift between the device and the
// host, so minima are kept per one-second window and the estimate is the
// minimum over the most recent windows only.
//
// What this does and does not give you: timestamps between two frames of this
// device are accurate to the device timer (1 µs resolution, ppm-level drift).
// The absolute alignment to the session timeline is biased late by the
// unknown minimum transit latency of the fastest path (never negative), and
// jittered by the estimator. Latency statistics are reported so callers can
// state that uncertainty instead of assuming synchronization.
type ClockMap struct {
	windowNs int64
	keep     int

	base    int64 // host ns of the first sample
	wins    []clockWin
	all     []clockWin // one entry per window, for the drift regression
	lastDev int64
	lastTs  int64
	started bool

	stats ClockStats
}

type clockWin struct {
	idx int64
	min int64 // minimum offset in the window
	at  int64 // host ns of the window start (relative to base)
}

// ClockStats summarises the mapping quality after a capture.
type ClockStats struct {
	Samples      int
	Resets       int     // device timer restarts (reboots) detected
	OffsetNs     int64   // current offset estimate: session_ns = device_ns + OffsetNs
	LatencyMinNs int64   // host receive time minus mapped timestamp
	LatencyMaxNs int64   //
	LatencyAvgNs int64   //
	DriftPPM     float64 // device-vs-host rate difference; only meaningful with >= 3 windows
	Windows      int
	latencySum   int64
}

// NewClockMap returns an estimator with a 1 s window and an 8 s horizon.
func NewClockMap() *ClockMap { return &ClockMap{windowNs: 1_000_000_000, keep: 8} }

// Map converts a device timestamp (µs) received at hostNs (session ns) to
// session ns. Results are non-decreasing in call order.
func (c *ClockMap) Map(devUs, hostNs int64) int64 {
	devNs := devUs * 1000
	if c.started && devUs < c.lastDev-1_000_000 {
		// The device timer jumped back by more than a second: it rebooted.
		c.wins = c.wins[:0]
		c.all = c.all[:0]
		c.started = false
		c.stats.Resets++
	}
	if !c.started {
		c.base = hostNs
		c.started = true
	}
	c.lastDev = devUs
	off := hostNs - devNs
	idx := (hostNs - c.base) / c.windowNs
	if n := len(c.wins); n > 0 && c.wins[n-1].idx == idx {
		if off < c.wins[n-1].min {
			c.wins[n-1].min = off
		}
	} else {
		c.wins = append(c.wins, clockWin{idx: idx, min: off, at: idx * c.windowNs})
		if len(c.wins) > c.keep {
			c.wins = c.wins[len(c.wins)-c.keep:]
		}
	}
	if n := len(c.all); n > 0 && c.all[n-1].idx == idx {
		if off < c.all[n-1].min {
			c.all[n-1].min = off
		}
	} else {
		c.all = append(c.all, clockWin{idx: idx, min: off, at: idx * c.windowNs})
	}
	est := c.wins[0].min
	for _, w := range c.wins {
		if w.min < est {
			est = w.min
		}
	}
	ts := devNs + est
	if ts < c.lastTs {
		ts = c.lastTs
	}
	c.lastTs = ts

	lat := hostNs - ts
	s := &c.stats
	if s.Samples == 0 || lat < s.LatencyMinNs {
		s.LatencyMinNs = lat
	}
	if lat > s.LatencyMaxNs {
		s.LatencyMaxNs = lat
	}
	s.latencySum += lat
	s.Samples++
	s.OffsetNs = est
	return ts
}

// Stats returns the mapping summary so far.
func (c *ClockMap) Stats() ClockStats {
	s := c.stats
	if s.Samples > 0 {
		s.LatencyAvgNs = s.latencySum / int64(s.Samples)
	}
	s.Windows = len(c.all)
	s.DriftPPM = c.drift()
	return s
}

// drift is the least-squares slope of the per-window minimum offset against
// host time. offset = host - device, so a positive slope means the device
// timer runs slow relative to the host.
func (c *ClockMap) drift() float64 {
	n := float64(len(c.all))
	if n < 3 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for _, w := range c.all {
		x := float64(w.at) / 1e9
		y := float64(w.min) / 1e9 // seconds
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	slope := (n*sxy - sx*sy) / den // seconds per second
	if math.IsNaN(slope) || math.IsInf(slope, 0) {
		return 0
	}
	return slope * 1e6
}
