// Package protocol implements the serial line protocol of Nordic's
// "nRF Sniffer for 802.15.4" firmware (a Zephyr shell over USB CDC) and the
// timestamp mapping for its frames. It has no dependency on hwlab's plugin
// machinery, so everything here is testable with recorded lines or the
// simulator in sim.go.
//
// Protocol (verified against Nordic's reference extcap script and a real
// nRF52840 DK; see docs/adr/0010-nrf802154-sniffer-protocol.md):
//
//	host -> device   sleep | shell echo off | channel N | receive   (CRLF-terminated)
//	device -> host   received: <hex> power: <dBm> lqi: <n> time: <us>
//
// The last two bytes of <hex> are not a CRC: the firmware appends a
// pseudo-FCS derived from the LQI, which is stripped here. <us> is the device
// timer in microseconds since boot.
package protocol

import (
	"regexp"
	"strconv"
	"strings"
)

// Valid IEEE 802.15.4 2.4 GHz channels.
const (
	MinChannel = 11
	MaxChannel = 26
)

const (
	pseudoFCSLen = 2
	// MinFrameLen is an ACK without FCS: frame control (2) + sequence (1).
	MinFrameLen = 3
	// MaxFrameLen is the 802.15.4 PHY payload limit (127) minus the FCS.
	MaxFrameLen = 127 - pseudoFCSLen
	maxLineLen  = 1024
)

// Frame is one received radio frame.
type Frame struct {
	Data  []byte // MAC frame without FCS
	RSSI  int    // dBm as reported by the firmware
	LQI   int
	DevUs int64 // device timer, microseconds since boot
}

// LineKind classifies a parsed line.
type LineKind int

const (
	// Noise is anything that is not a frame line: shell echo, prompts, banners.
	Noise LineKind = iota
	// FrameLine is a valid frame.
	FrameLine
	// Malformed looked like a frame line but failed validation (typically
	// interleaved with shell echo while the firmware streams).
	Malformed
)

var (
	frameRE = regexp.MustCompile(`received:\s+([0-9a-fA-F]+)\s+power:\s+(-?\d+)\s+lqi:\s+(\d+)\s+time:\s+(-?\d+)(?:\s|$)`)
	ansiRE  = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
)

// ParseLine parses one output line. The match is a search, not anchored, so
// escape sequences and shell prompts before "received:" do not matter.
func ParseLine(line string) (Frame, LineKind) {
	line = strings.TrimSpace(ansiRE.ReplaceAllString(line, ""))
	if !strings.Contains(line, "received:") {
		return Frame{}, Noise
	}
	m := frameRE.FindStringSubmatch(line)
	if m == nil {
		return Frame{}, Malformed
	}
	hex := m[1]
	if len(hex)%2 != 0 {
		return Frame{}, Malformed
	}
	n := len(hex) / 2
	if n-pseudoFCSLen < MinFrameLen || n-pseudoFCSLen > MaxFrameLen {
		return Frame{}, Malformed
	}
	data := make([]byte, n-pseudoFCSLen)
	for i := range data {
		v, err := strconv.ParseUint(hex[2*i:2*i+2], 16, 8)
		if err != nil {
			return Frame{}, Malformed
		}
		data[i] = byte(v)
	}
	rssi, err1 := strconv.Atoi(m[2])
	lqi, err2 := strconv.Atoi(m[3])
	us, err3 := strconv.ParseInt(m[4], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || lqi > 255 || rssi < -140 || rssi > 40 {
		return Frame{}, Malformed
	}
	return Frame{Data: data, RSSI: rssi, LQI: lqi, DevUs: us}, FrameLine
}

// LineBuffer assembles lines from arbitrary read chunks. Both CR and LF end a
// line; empty lines are skipped; over-long lines are discarded.
type LineBuffer struct {
	buf      []byte
	overflow bool
}

// Feed appends p and calls emit for every completed line.
func (l *LineBuffer) Feed(p []byte, emit func(line string)) {
	for _, b := range p {
		if b == '\n' || b == '\r' {
			if !l.overflow && len(l.buf) > 0 {
				emit(string(l.buf))
			}
			l.buf = l.buf[:0]
			l.overflow = false
			continue
		}
		if l.overflow {
			continue
		}
		if len(l.buf) >= maxLineLen {
			l.overflow = true
			l.buf = l.buf[:0]
			continue
		}
		l.buf = append(l.buf, b)
	}
}
