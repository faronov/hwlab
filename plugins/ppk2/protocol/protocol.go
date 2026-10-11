// Package protocol implements the Nordic Power Profiler Kit II (PPK2) serial
// protocol in pure Go: command encoding, calibration metadata parsing and
// sample decoding. It has no I/O and no hwlab dependencies so it can be
// tested and fuzzed in isolation.
//
// The wire details below follow the behaviour of the open-source reference
// implementation ppk2-api-python (IRNAS). They are not covered by a public
// Nordic specification; see
// docs/adr/0008-ppk2-protocol-sources.md for what is verified and what is
// assumed.
package protocol

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Serial command opcodes.
const (
	CmdAverageStart     byte = 0x06
	CmdAverageStop      byte = 0x07
	CmdDeviceRunningSet byte = 0x0C
	CmdRegulatorSet     byte = 0x0D
	CmdSetPowerMode     byte = 0x11
	CmdGetMetaData      byte = 0x19
	CmdReset            byte = 0x20
)

// Power modes for CmdSetPowerMode.
const (
	PowerModeAmpere byte = 1
	PowerModeSource byte = 2
)

// Source voltage limits in millivolts.
const (
	VddMinMV = 800
	VddMaxMV = 5000
)

// Sampling characteristics.
const (
	SampleRateHz = 100_000
	SamplePeriod = 10_000 // ns
	SampleSize   = 4      // bytes on the wire

	numRanges     = 5
	adcMult       = 1.8 / 163840
	adcMask       = 1<<14 - 1
	rangeShift    = 14
	rangeMask     = 0x7
	logicShift    = 24
	adcResolution = 4 // raw ADC value is multiplied by 4 before calibration
)

// Mode is the measurement mode.
type Mode string

// Supported modes.
const (
	ModeAmpere Mode = "ampere"
	ModeSource Mode = "source"
)

// ParseMode accepts "ampere" and "source" (case-insensitive, with an
// optional "_meter"/"_mode" suffix).
func ParseMode(s string) (Mode, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "_mode"), "_meter")
	switch s {
	case "ampere", "amp":
		return ModeAmpere, nil
	case "source", "src":
		return ModeSource, nil
	}
	return "", fmt.Errorf("unknown ppk2 mode %q (want ampere|source)", s)
}

// CmdPowerMode returns the command selecting mode m.
func CmdPowerMode(m Mode) []byte {
	if m == ModeSource {
		return []byte{CmdSetPowerMode, PowerModeSource}
	}
	return []byte{CmdSetPowerMode, PowerModeAmpere}
}

// CmdDUTPower returns the command switching the DUT supply on or off.
func CmdDUTPower(on bool) []byte {
	if on {
		return []byte{CmdDeviceRunningSet, 1}
	}
	return []byte{CmdDeviceRunningSet, 0}
}

// CmdSetRegulator returns the command that sets the source voltage. The
// value is clamped to [VddMinMV, VddMaxMV].
func CmdSetRegulator(mv int) []byte {
	mv = max(VddMinMV, min(VddMaxMV, mv))
	// The device expects (3,32) for 800 mV, increasing linearly; the byte
	// pair is a base-256 number offset by 32.
	diff := mv - VddMinMV + 32
	return []byte{CmdRegulatorSet, byte(3 + diff/256), byte(diff % 256)}
}

// Calibration holds the per-range coefficients from the device metadata.
type Calibration struct {
	R, GS, GI, O, S, I, UG [numRanges]float64
}

// DefaultCalibration is used when the device reports no coefficients. Real
// devices ship with calibrated values; defaults only give the right order of
// magnitude.
func DefaultCalibration() Calibration {
	c := Calibration{R: [numRanges]float64{1031.64, 101.65, 10.15, 0.94, 0.043}}
	for i := range c.GS {
		c.GS[i], c.GI[i], c.UG[i] = 1, 1, 1
	}
	return c
}

// Metadata is the parsed GET_META_DATA reply.
type Metadata struct {
	Calibration Calibration
	// Raw holds every "key: value" line, e.g. "Calibrated", "HW", "IA".
	Raw map[string]string
}

// ErrIncomplete means the metadata reply has not ended yet.
var ErrIncomplete = errors.New("ppk2: metadata incomplete (no END marker)")

// ParseMetadata parses the textual metadata reply. It returns
// ErrIncomplete until the "END" marker has been received.
func ParseMetadata(text string) (*Metadata, error) {
	if !strings.Contains(text, "END") {
		return nil, ErrIncomplete
	}
	md := &Metadata{Calibration: DefaultCalibration(), Raw: map[string]string{}}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), ": ")
		if !ok {
			continue
		}
		md.Raw[k] = v
		if len(k) < 2 {
			continue
		}
		idx, err := strconv.Atoi(k[len(k)-1:])
		if err != nil || idx < 0 || idx >= numRanges {
			continue
		}
		var dst *[numRanges]float64
		switch k[:len(k)-1] {
		case "R":
			dst = &md.Calibration.R
		case "GS":
			dst = &md.Calibration.GS
		case "GI":
			dst = &md.Calibration.GI
		case "O":
			dst = &md.Calibration.O
		case "S":
			dst = &md.Calibration.S
		case "I":
			dst = &md.Calibration.I
		case "UG":
			dst = &md.Calibration.UG
		default:
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil, fmt.Errorf("ppk2: metadata %s: %w", k, err)
		}
		if dst == &md.Calibration.R && f == 0 {
			continue // some devices report a bogus zero resistor
		}
		dst[idx] = f
	}
	return md, sc.Err()
}

// Decoder converts the raw sample stream into current and logic samples. It
// carries partial samples across calls because serial reads do not respect
// the 4-byte sample boundaries.
type Decoder struct {
	Cal Calibration
	// VddMV is the supply voltage used by the calibration's S term.
	VddMV float64

	rem  [SampleSize]byte
	nrem int
}

// Reset drops any buffered partial sample.
func (d *Decoder) Reset() { d.nrem = 0 }

// Decode appends the samples contained in buf to cur (nanoamps) and logic
// (D0..D7 bits) and returns the extended slices.
func (d *Decoder) Decode(buf []byte, cur []int32, logic []uint32) ([]int32, []uint32) {
	for len(buf) > 0 {
		if d.nrem > 0 || len(buf) < SampleSize {
			n := copy(d.rem[d.nrem:], buf)
			d.nrem += n
			buf = buf[n:]
			if d.nrem < SampleSize {
				break
			}
			c, l := d.convert(le32(d.rem[:]))
			cur, logic = append(cur, c), append(logic, l)
			d.nrem = 0
			continue
		}
		c, l := d.convert(le32(buf))
		buf = buf[SampleSize:]
		cur, logic = append(cur, c), append(logic, l)
	}
	return cur, logic
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func (d *Decoder) convert(raw uint32) (int32, uint32) {
	rng := min(int(raw>>rangeShift)&rangeMask, numRanges-1)
	adc := float64(raw&adcMask) * adcResolution
	c := &d.Cal
	amps := (adc - c.O[rng]) * (adcMult / c.R[rng])
	amps = c.UG[rng] * (amps*(c.GS[rng]*amps+c.GI[rng]) + (c.S[rng]*d.VddMV/1000 + c.I[rng]))
	na := math.Round(amps * 1e9)
	na = math.Max(math.MinInt32, math.Min(math.MaxInt32, na))
	return int32(na), raw >> logicShift
}
