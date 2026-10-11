// Package energytrace is a native, CGO-free implementation of the LP-XDS110ET
// EnergyTrace data path. The wire protocol was reverse engineered from real
// USB traffic; see docs/xds110-energytrace-protocol.md for the evidence and
// the confidence of every statement. Nothing here is copied from TI software.
package energytrace

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Stream constants. Evidence levels are those of the protocol document.
const (
	// SampleRateHz is the sample rate of the stream (CONFIRMED against the
	// oracle CSV: 3.90625 us per row).
	SampleRateHz = 256000
	// SamplePeriodNs is the exact period truncated to whole nanoseconds
	// (3906.25 ns); the 64 ppm truncation is why SampleRateHz is also
	// reported as a stream attribute.
	SamplePeriodNs = 3906
	// SamplesPerPacket is the sample count of every observed data packet.
	SamplesPerPacket = 256

	// GainNAPerCount converts one 24-bit code step to nanoamps on range 0
	// (STRONG_EVIDENCE: least-squares fit against the oracle CSV, rms residual
	// 0.29 nA which is the CSV rounding). Other ranges are not decoded.
	GainNAPerCount = 15.135849

	dataMarker    = 0x68 // data packet: marker, seq, u16 LE payload length, 2 reserved bytes, samples
	preludeMarker = 0x35 // 3-byte stream prelude: marker + u16 LE (0xbb80 = 48000, meaning UNKNOWN)
	headerLen     = 6
	preludeLen    = 3
	maxPayload    = 4096
	maxPacket     = headerLen + maxPayload
)

// Packet is one decoded data packet.
type Packet struct {
	Seq uint8
	// Lost is the number of packets missing before this one, counted modulo 256.
	Lost int
	// Codes holds the 24-bit sample codes.
	Codes []uint32
}

// Stats counts what a Parser saw.
type Stats struct {
	Packets      uint64
	Samples      uint64
	Preludes     int
	LastPrelude  uint16
	LostPackets  uint64
	SkippedBytes uint64
}

// Parser turns the byte stream of the data endpoint into packets. It
// resynchronizes on garbage and never buffers more than one packet.
type Parser struct {
	buf     []byte
	stats   Stats
	nextSeq uint8
	started bool
}

// Stats returns the counters so far.
func (p *Parser) Stats() Stats { return p.stats }

// Feed appends b and calls emit for every complete packet. The Packet it
// receives is only valid during the call.
func (p *Parser) Feed(b []byte, emit func(Packet)) {
	p.buf = append(p.buf, b...)
	for {
		if len(p.buf) == 0 {
			return
		}
		switch p.buf[0] {
		case preludeMarker:
			if len(p.buf) < preludeLen {
				return
			}
			p.stats.Preludes++
			p.stats.LastPrelude = binary.LittleEndian.Uint16(p.buf[1:])
			p.buf = p.buf[preludeLen:]
		case dataMarker:
			if len(p.buf) < headerLen {
				return
			}
			n := int(binary.LittleEndian.Uint16(p.buf[2:]))
			if n == 0 || n%4 != 0 || n > maxPayload {
				p.skip(1)
				continue
			}
			if len(p.buf) < headerLen+n {
				return
			}
			p.emit(p.buf[:headerLen+n], emit)
			p.buf = p.buf[headerLen+n:]
		default:
			p.skip(1)
		}
	}
}

func (p *Parser) skip(n int) {
	p.stats.SkippedBytes += uint64(n)
	p.buf = p.buf[n:]
}

func (p *Parser) emit(raw []byte, emit func(Packet)) {
	seq := raw[1]
	lost := 0
	if p.started {
		lost = int(seq - p.nextSeq)
	}
	p.started = true
	p.nextSeq = seq + 1
	codes := make([]uint32, (len(raw)-headerLen)/4)
	for i := range codes {
		codes[i] = DecodeCode(binary.BigEndian.Uint32(raw[headerLen+4*i:]))
	}
	p.stats.Packets++
	p.stats.Samples += uint64(len(codes))
	p.stats.LostPackets += uint64(lost)
	emit(Packet{Seq: seq, Lost: lost, Codes: codes})
}

// DecodeCode extracts the 24-bit code of a sample word (big endian u32). The
// low byte is not used: its low nibble is always zero and its high nibble
// carries no measurable signal (fit weight about 0.001).
func DecodeCode(w uint32) uint32 { return w >> 8 }

// Calibration is the reply of the per-range calibration command: the sum of
// Count raw readings taken at device power-up with no load (offset).
type Calibration struct {
	Sum   uint32
	Count uint32
}

// ErrCalibration reports an unusable calibration reply.
var ErrCalibration = errors.New("energytrace: bad calibration reply")

// ParseCalibration decodes the 8-byte reply (u32 LE sum, u32 LE count).
func ParseCalibration(d []byte) (Calibration, error) {
	if len(d) < 8 {
		return Calibration{}, fmt.Errorf("%w: %d bytes", ErrCalibration, len(d))
	}
	return Calibration{Sum: binary.LittleEndian.Uint32(d), Count: binary.LittleEndian.Uint32(d[4:])}, nil
}

// Offset is the mean code at zero current.
func (c Calibration) Offset() (float64, error) {
	if c.Count == 0 {
		return 0, fmt.Errorf("%w: count is 0", ErrCalibration)
	}
	return float64(c.Sum) / float64(c.Count), nil
}

// ToCurrentNA converts range-0 sample codes to nanoamps.
func ToCurrentNA(codes []uint32, offset float64) []int32 {
	out := make([]int32, len(codes))
	for i, c := range codes {
		out[i] = int32(math.Round(GainNAPerCount * (float64(c) - offset)))
	}
	return out
}
