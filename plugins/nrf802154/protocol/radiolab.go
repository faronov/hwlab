package protocol

// Transmit extension of the sniffer protocol, implemented by the zigbee-rs
// radio lab firmware (github.com/faronov/zigbee-rs,
// tools/nrf52840-radio-lab). It is a superset of Nordic's v0.8.0 shell: the
// commands and the "received:" line are unchanged, so capture code works
// with either firmware. Additions:
//
//	host -> device   version
//	device -> host   version: zigbee-rs-radio-lab <ver> protocol: <n> caps: <a,b,..>
//	                 (Nordic's firmware: "version: command not found")
//
//	host -> device   power [dBm]
//	device -> host   power: <dBm>                      (only when queried)
//
//	host -> device   transmit <hex mpdu, no FCS> [cca] [ack] [repeat <n> <interval_ms>]
//	device -> host   transmitted: <hex incl. FCS> channel: <ch> power: <dBm> time: <us>
//	                 tx: <i>/<n> status: sent|acked|no_ack|cca_busy|error time: <us>
//	                 tx_done: attempts: .. sent: .. acked: .. no_ack: .. cca_busy: .. error: .. result: ok|aborted
//	                 or, for a rejected command: error: <reason>
//
// "transmitted:" is printed only for attempts that went on air, before that
// attempt's "tx:" line. "time" is the device timer at the end of the SFD,
// the same clock as "received:" frames. The radio appends the FCS; the
// "transmitted:" hex carries the real FCS.

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// RadioLabName is the firmware name reported by the radio lab's version line.
	RadioLabName = "zigbee-rs-radio-lab"
	// NordicSnifferName identifies Nordic's receive-only sniffer firmware.
	NordicSnifferName = "nordic-nrf-sniffer"

	// MaxTxPSDU is the longest MPDU without FCS the firmware accepts (127-2).
	MaxTxPSDU = 125
	// FirmwareMaxRepeat is the firmware's own repeat bound.
	FirmwareMaxRepeat = 1000
	// FirmwareMinIntervalMs is the firmware's shortest repeat interval.
	FirmwareMinIntervalMs = 5
	// FirmwareMaxIntervalMs is the firmware's longest repeat interval.
	FirmwareMaxIntervalMs = 60000
)

// TxPowerLevels are the nRF52840 TXPOWER settings the firmware accepts (dBm).
var TxPowerLevels = []int{-40, -20, -16, -12, -8, -4, 0, 2, 3, 4, 5, 6, 7, 8}

// Firmware is what the version probe learned about the device firmware.
type Firmware struct {
	Name     string   `json:"name"`
	Version  string   `json:"version,omitempty"`
	Protocol int      `json:"protocol,omitempty"`
	Caps     []string `json:"caps,omitempty"`
	// Raw is the device's reply, trimmed (diagnostics).
	Raw string `json:"raw,omitempty"`
}

// Has reports whether the firmware declared capability c.
func (f Firmware) Has(c string) bool {
	for _, x := range f.Caps {
		if x == c {
			return true
		}
	}
	return false
}

// CanTransmit reports whether the firmware implements "transmit".
func (f Firmware) CanTransmit() bool { return f.Name == RadioLabName && f.Has("transmit") }

var versionRE = regexp.MustCompile(`version:\s+(\S+)\s+(\S+)\s+protocol:\s+(\d+)\s+caps:\s*(\S*)`)

// ParseVersion interprets the output of the "version" command. Nordic's
// firmware does not know the command; anything unrecognised is reported as
// receive-only with the raw reply kept for diagnosis.
func ParseVersion(resp string) Firmware {
	clean := strings.TrimSpace(ansiRE.ReplaceAllString(resp, ""))
	for _, line := range strings.FieldsFunc(clean, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if m := versionRE.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[3])
			var caps []string
			if m[4] != "" {
				caps = strings.Split(m[4], ",")
			}
			return Firmware{Name: m[1], Version: m[2], Protocol: n, Caps: caps, Raw: strings.TrimSpace(line)}
		}
	}
	if strings.Contains(clean, "command not found") {
		return Firmware{Name: NordicSnifferName, Caps: []string{"receive"}, Raw: clean}
	}
	return Firmware{Name: "unknown", Raw: clean}
}

// TxRequest is one transmit command.
type TxRequest struct {
	// PSDU is the MAC frame without FCS (the radio appends it).
	PSDU       []byte
	CCA        bool
	AckWait    bool
	Repeat     int // 1 = single shot
	IntervalMs int // between repeats; ignored when Repeat == 1
}

// Validate checks the request against the firmware's limits.
func (r TxRequest) Validate() error {
	if len(r.PSDU) == 0 || len(r.PSDU) > MaxTxPSDU {
		return fmt.Errorf("frame must be 1..%d bytes without FCS, got %d", MaxTxPSDU, len(r.PSDU))
	}
	if r.AckWait && len(r.PSDU) < 3 {
		return fmt.Errorf("ack wait needs a frame with a sequence number (>= 3 bytes)")
	}
	if r.Repeat < 1 || r.Repeat > FirmwareMaxRepeat {
		return fmt.Errorf("repeat must be 1..%d, got %d", FirmwareMaxRepeat, r.Repeat)
	}
	if r.Repeat > 1 && (r.IntervalMs < FirmwareMinIntervalMs || r.IntervalMs > FirmwareMaxIntervalMs) {
		return fmt.Errorf("interval_ms must be %d..%d, got %d", FirmwareMinIntervalMs, FirmwareMaxIntervalMs, r.IntervalMs)
	}
	return nil
}

// Command renders the shell command (without line ending).
func (r TxRequest) Command() string {
	var b strings.Builder
	b.WriteString("transmit ")
	b.WriteString(hex.EncodeToString(r.PSDU))
	if r.CCA {
		b.WriteString(" cca")
	}
	if r.AckWait {
		b.WriteString(" ack")
	}
	if r.Repeat > 1 {
		fmt.Fprintf(&b, " repeat %d %d", r.Repeat, r.IntervalMs)
	}
	return b.String()
}

// TxKind classifies a transmit output line.
type TxKind int

const (
	// TxNone is not a transmit line.
	TxNone TxKind = iota
	// TxFrame is a "transmitted:" line.
	TxFrame
	// TxAttempt is a "tx:" result line.
	TxAttempt
	// TxDone is the final "tx_done:" summary.
	TxDone
	// TxError is an "error:" reply (command rejected).
	TxError
)

// TxEvent is one parsed transmit output line.
type TxEvent struct {
	Kind TxKind
	// TxFrame: MAC frame without FCS, the FCS on air, channel and power.
	Data    []byte
	FCS     []byte
	Channel int
	PowerDb int
	// TxFrame and TxAttempt: device timer at end of SFD (or of the failure).
	DevUs int64
	// TxAttempt.
	Attempt, Total int
	Status         string
	// TxDone.
	Summary TxSummary
	// TxError.
	Message string
}

// TxSummary is the "tx_done:" line.
type TxSummary struct {
	Attempts int    `json:"attempts"`
	Sent     int    `json:"sent"`
	Acked    int    `json:"acked"`
	NoAck    int    `json:"no_ack"`
	CCABusy  int    `json:"cca_busy"`
	Error    int    `json:"error"`
	Result   string `json:"result"`
}

var (
	txFrameRE   = regexp.MustCompile(`^transmitted:\s+([0-9a-fA-F]+)\s+channel:\s+(\d+)\s+power:\s+(-?\d+)\s+time:\s+(\d+)$`)
	txAttemptRE = regexp.MustCompile(`^tx:\s+(\d+)/(\d+)\s+status:\s+(sent|acked|no_ack|cca_busy|error)\s+time:\s+(\d+)$`)
	txDoneRE    = regexp.MustCompile(`^tx_done:\s+attempts:\s+(\d+)\s+sent:\s+(\d+)\s+acked:\s+(\d+)\s+no_ack:\s+(\d+)\s+cca_busy:\s+(\d+)\s+error:\s+(\d+)\s+result:\s+(ok|aborted)$`)
)

// ParseTxLine parses a transmit output line. ok is false for anything else
// (received frames, echo, prompts).
func ParseTxLine(line string) (TxEvent, bool) {
	line = strings.TrimSpace(ansiRE.ReplaceAllString(line, ""))
	switch {
	case strings.HasPrefix(line, "transmitted:"):
		m := txFrameRE.FindStringSubmatch(line)
		if m == nil || len(m[1])%2 != 0 {
			return TxEvent{}, false
		}
		raw, err := hex.DecodeString(m[1])
		if err != nil || len(raw) < 1+pseudoFCSLen || len(raw) > MaxTxPSDU+pseudoFCSLen {
			return TxEvent{}, false
		}
		ch, _ := strconv.Atoi(m[2])
		pw, _ := strconv.Atoi(m[3])
		us, err := strconv.ParseInt(m[4], 10, 64)
		if err != nil {
			return TxEvent{}, false
		}
		n := len(raw) - pseudoFCSLen
		return TxEvent{Kind: TxFrame, Data: raw[:n], FCS: raw[n:], Channel: ch, PowerDb: pw, DevUs: us}, true
	case strings.HasPrefix(line, "tx_done:"):
		m := txDoneRE.FindStringSubmatch(line)
		if m == nil {
			return TxEvent{}, false
		}
		v := make([]int, 6)
		for i := range v {
			v[i], _ = strconv.Atoi(m[i+1])
		}
		return TxEvent{Kind: TxDone, Summary: TxSummary{Attempts: v[0], Sent: v[1], Acked: v[2], NoAck: v[3], CCABusy: v[4], Error: v[5], Result: m[7]}}, true
	case strings.HasPrefix(line, "tx:"):
		m := txAttemptRE.FindStringSubmatch(line)
		if m == nil {
			return TxEvent{}, false
		}
		a, _ := strconv.Atoi(m[1])
		t, _ := strconv.Atoi(m[2])
		us, err := strconv.ParseInt(m[4], 10, 64)
		if err != nil {
			return TxEvent{}, false
		}
		return TxEvent{Kind: TxAttempt, Attempt: a, Total: t, Status: m[3], DevUs: us}, true
	case strings.HasPrefix(line, "error:"):
		return TxEvent{Kind: TxError, Message: strings.TrimSpace(strings.TrimPrefix(line, "error:"))}, true
	case strings.HasPrefix(line, "transmit") && strings.Contains(line, "command not found"):
		// Nordic's receive-only firmware.
		return TxEvent{Kind: TxError, Message: line}, true
	}
	return TxEvent{}, false
}

// FCS returns the IEEE 802.15.4 FCS (CRC-16/KERMIT, little endian) of mpdu.
func FCS(mpdu []byte) []byte {
	var crc uint16
	for _, b := range mpdu {
		crc ^= uint16(b)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0x8408
			} else {
				crc >>= 1
			}
		}
	}
	return []byte{byte(crc), byte(crc >> 8)}
}
