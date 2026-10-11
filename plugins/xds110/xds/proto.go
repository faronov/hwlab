// Package xds speaks the TI XDS110 probe's native USB protocol: a framed
// command/response exchange on one vendor-class bulk interface. It knows
// nothing about plugins, ADIv5 or Cortex-M; internal/adiv5 and internal/jtag
// sit on top through the adapters in this package.
//
// The wire format was learned from the probe's behaviour and from public
// descriptions of the XDS110 command set (command ids, status codes and
// payload layouts); this implementation is original.
package xds

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Frame layout: '*', payload length (u16 LE), payload. A request payload is the
// command id followed by its arguments (little endian). A response payload is
// an int32 status followed by the command's results.
const (
	frameStart = '*'
	headerLen  = 3
	statusLen  = 4
	maxPayload = 4096 + 60 // largest data block plus parameters
)

// Command ids.
const (
	CmdConnect       = 0x01
	CmdDisconnect    = 0x02
	CmdVersion       = 0x03
	CmdSetTCK        = 0x04
	CmdSetTRST       = 0x05
	CmdCycleTCK      = 0x07
	CmdGotoState     = 0x09
	CmdJTAGScan      = 0x0c
	CmdSetSRST       = 0x0e
	CmdCMAPIConnect  = 0x0f
	CmdCMAPIDisc     = 0x10
	CmdCMAPIAcquire  = 0x11
	CmdCMAPIRelease  = 0x12
	CmdCMAPIRegRead  = 0x15
	CmdCMAPIRegWrite = 0x16
	CmdSWDConnect    = 0x17
	CmdSWDDisconnect = 0x18
	CmdCJTAGConnect  = 0x2b
	CmdCJTAGDisc     = 0x2c
	CmdSetSupply     = 0x32
	CmdDapRequest    = 0x3a
	CmdScanRequest   = 0x3b
	CmdPathMove      = 0x3c
)

// Status is the signed result code every response starts with.
type Status int32

// Known status codes.
const (
	StatusOK        Status = 0
	StatusFail      Status = -261
	StatusSWDWait   Status = -613
	StatusSWDFault  Status = -614
	StatusSWDProto  Status = -615
	StatusSWDParity Status = -616
	StatusSWDDevID  Status = -617
)

// String names a status.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusFail:
		return "XDS110 failure"
	case StatusSWDWait:
		return "SWD WAIT"
	case StatusSWDFault:
		return "SWD FAULT"
	case StatusSWDProto:
		return "SWD protocol error"
	case StatusSWDParity:
		return "SWD parity error"
	case StatusSWDDevID:
		return "SWD device ID error"
	}
	return fmt.Sprintf("status %d", int32(s))
}

// Error is a command the probe answered with a non-zero status.
type Error struct {
	Cmd    byte
	Status Status
}

func (e *Error) Error() string {
	return fmt.Sprintf("XDS110 command 0x%02x: %s (%d)", e.Cmd, e.Status, int32(e.Status))
}

// Sentinels for errors.Is.
var (
	ErrShortReply = errors.New("xds110: short or malformed reply")
	ErrDesync     = errors.New("xds110: reply does not match the request")
)

func bcd(v uint32, shift uint) uint32 { return (v>>(shift+4)&0xf)*10 + v>>shift&0xf }

// FirmwareString renders 0x03000022 as "3.0.0.22".
func FirmwareString(fw uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", bcd(fw, 24), bcd(fw, 16), bcd(fw, 8), bcd(fw, 0))
}

// Firmware generations that changed behaviour.
const (
	// FWOptimizedDAP is the first firmware with the OCD_DAP_REQUEST queue.
	FWOptimizedDAP uint32 = 0x02030011
	// FWFastTCK reworked the TCK routines (up to 14 MHz).
	FWFastTCK uint32 = 0x03000000
	// FWFastTCKPlus added the 10 and 12 MHz settings.
	FWFastTCKPlus uint32 = 0x03000003
)

// Hardware ids. HWStandAlone (0x21) is the stand-alone XDS110, the only
// variant with a programmable target supply (the OpenOCD driver sends
// XDS_SET_SUPPLY only to this id). HWLPXDS110ET (0x23) is what the real
// LP-XDS110ET reported. Measured on it: the supply command is acknowledged
// with success and the 3V3 pin voltage does not change (phase 27).
const (
	HWStandAlone uint16 = 0x21
	HWLPXDS110ET uint16 = 0x23
)

// Target supply limits of the stand-alone probe in millivolts.
const (
	SupplyMinMV = 1800
	SupplyMaxMV = 3600
)

// Clock limits in kHz.
const (
	MinKHz     = 100
	MaxSlowKHz = 2500
	MaxFastKHz = 14000
)

// Fixed delay encodings of the fast TCK settings.
const (
	delay14000 uint32 = 0
	delay12000 uint32 = 0xfffffffe
	delay10000 uint32 = 0xfffffffd
	delay8500  uint32 = 1
	delay5500  uint32 = 2
)

// ClockPlan is what a requested clock turns into.
type ClockPlan struct {
	RequestedKHz uint32 `json:"requested_khz"`
	// NominalKHz is the setting the encoding is meant to produce, not a
	// measurement.
	NominalKHz uint32 `json:"nominal_khz"`
	Delay      uint32 `json:"delay_count"`
	Clamped    bool   `json:"clamped"`
}

// PlanClock encodes a TCK/SWCLK request for a probe running firmware fw.
func PlanClock(fw uint32, khz uint32) ClockPlan {
	plan := ClockPlan{RequestedKHz: khz}
	if khz < MinKHz {
		khz, plan.Clamped = MinKHz, true
	}
	if fw < FWFastTCK {
		return planSlow(plan, khz)
	}
	switch {
	case khz >= MaxFastKHz:
		plan.Clamped = plan.Clamped || khz > MaxFastKHz
		plan.NominalKHz, plan.Delay = MaxFastKHz, delay14000
	case khz >= 12000 && fw >= FWFastTCKPlus:
		plan.NominalKHz, plan.Delay = 12000, delay12000
	case khz >= 10000 && fw >= FWFastTCKPlus:
		plan.NominalKHz, plan.Delay = 10000, delay10000
	case khz >= 8500:
		plan.NominalKHz, plan.Delay = 8500, delay8500
	case khz >= 5500:
		plan.NominalKHz, plan.Delay = 5500, delay5500
	default:
		// Linear fit of delay count against the period, valid below 5.5 MHz.
		const slope, offset = 17100000.0, -1.02
		// Round the count up so the clock never exceeds the request.
		n := uint32(1)
		if d := math.Ceil(slope/(float64(khz)*1000) + offset); d >= 1 {
			n = uint32(d)
		}
		plan.Delay = n
		plan.NominalKHz = uint32(slope / (float64(n) - offset) / 1000)
	}
	return plan
}

// planSlow encodes clocks for firmware older than the fast-TCK generation:
// a 400 ns minimum period that grows by 66 ns per count.
func planSlow(plan ClockPlan, khz uint32) ClockPlan {
	const base, step = 1e9 / (MaxSlowKHz * 1000.0), 66.0
	if khz >= MaxSlowKHz {
		plan.Clamped = plan.Clamped || khz > MaxSlowKHz
		plan.NominalKHz, plan.Delay = MaxSlowKHz, 0
		return plan
	}
	want := 1e9 / (float64(khz) * 1000)
	n := 0
	for base+step*float64(n) < want {
		n++
	}
	plan.NominalKHz, plan.Delay = uint32(1e9/(base+step*float64(n))/1000), uint32(n)
	return plan
}

// frame builds a request frame.
func frame(cmd byte, args []byte) []byte {
	n := 1 + len(args)
	out := make([]byte, headerLen+n)
	out[0] = frameStart
	binary.LittleEndian.PutUint16(out[1:], uint16(n))
	out[headerLen] = cmd
	copy(out[headerLen+1:], args)
	return out
}

// parseHeader checks the first packet of a response and returns the payload
// length it announces.
func parseHeader(pkt []byte) (int, error) {
	if len(pkt) < headerLen+statusLen || pkt[0] != frameStart {
		return 0, fmt.Errorf("%w: first packet %x", ErrShortReply, head(pkt))
	}
	n := int(binary.LittleEndian.Uint16(pkt[1:]))
	if n < statusLen || n > maxPayload {
		return 0, fmt.Errorf("%w: payload length %d", ErrShortReply, n)
	}
	if len(pkt)-headerLen > n {
		return 0, fmt.Errorf("%w: %d bytes after a %d byte payload", ErrShortReply, len(pkt)-headerLen, n)
	}
	return n, nil
}

func head(b []byte) []byte {
	if len(b) > 16 {
		return b[:16]
	}
	return b
}

func le32(v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return b[:]
}
