package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

// AEM stream on ChanAEM. Every message starts with a 32-byte header:
// magic ea ea ae ae, u32 kit time in ms, u16 type, u16 total length
// (header included), 20 reserved bytes.
var aemMagic = []byte{0xea, 0xea, 0xae, 0xae}

const (
	aemHeaderLen = 32
	// AEMTypeStart is the host's start request; the kit answers with
	// AEMTypeConfig and then streams AEMTypeData messages.
	AEMTypeStart  uint16 = 0x8010
	AEMTypeConfig uint16 = 0x4010
	AEMTypeData   uint16 = 0x0001

	aemDataHeaderLen = 64
	// AEMMaxRead is the read size used while streaming (it fits many
	// messages; the kit returns what it holds).
	AEMMaxRead = 0x9000
)

// AEMStartRequest is the 32-byte request that makes the kit stream AEM data.
// Silicon Labs' tool sends uninitialised bytes after the length; hwlab sends
// zeros.
func AEMStartRequest() []byte {
	b := make([]byte, aemHeaderLen)
	copy(b, aemMagic)
	binary.LittleEndian.PutUint16(b[8:], AEMTypeStart)
	binary.LittleEndian.PutUint16(b[10:], aemHeaderLen)
	return b
}

// AEMMessage is one message of the AEM channel.
type AEMMessage struct {
	KitMs   uint32 // kit time when the message was queued, ms
	Type    uint16
	Payload []byte // after the 32-byte header
}

// AEMConfig is the kit's answer to the start request. The meaning of the
// float fields is not established; they are kept raw.
type AEMConfig struct {
	KitMs      uint32
	Version    uint32
	SampleRate uint32
	MaxRate    uint32
	Params     [4]float32
}

// ParseAEMConfig decodes an AEMTypeConfig message.
func ParseAEMConfig(m AEMMessage) (AEMConfig, error) {
	p := m.Payload
	if m.Type != AEMTypeConfig || len(p) < 32 {
		return AEMConfig{}, fmt.Errorf("%w: AEM config message type %#x, %d bytes", ErrProtocol, m.Type, len(p))
	}
	le := binary.LittleEndian
	c := AEMConfig{KitMs: m.KitMs, Version: le.Uint32(p[4:]), SampleRate: le.Uint32(p[8:]), MaxRate: le.Uint32(p[12:])}
	for i := range c.Params {
		c.Params[i] = math.Float32frombits(le.Uint32(p[16+4*i:]))
	}
	return c, nil
}

// AEMBuffer is one buffer of current samples.
type AEMBuffer struct {
	KitMs      uint32 // message queue time, ms
	SampleRate uint32 // Hz
	Seq        uint16
	VoltageV   float32
	StartUs    uint32 // kit time of the first sample, us (wraps every 71.6 min)
	Status     uint32 // raw status word, meaning unknown (0x11 observed)
	CurrentmA  []float32
}

// ParseAEMData decodes an AEMTypeData message: a 64-byte data header (u32
// sample rate, u16 sample count, u16 sequence, reserved, f32 voltage at 32,
// u32 start time in us at 36, status at 52), then f32 currents in mA.
func ParseAEMData(m AEMMessage) (AEMBuffer, error) {
	p := m.Payload
	if m.Type != AEMTypeData || len(p) < aemDataHeaderLen {
		return AEMBuffer{}, fmt.Errorf("%w: AEM data message type %#x, %d bytes", ErrProtocol, m.Type, len(p))
	}
	le := binary.LittleEndian
	n := int(le.Uint16(p[4:]))
	if aemDataHeaderLen+4*n > len(p) {
		return AEMBuffer{}, fmt.Errorf("%w: AEM buffer of %d samples in %d bytes", ErrProtocol, n, len(p))
	}
	b := AEMBuffer{
		KitMs: m.KitMs, SampleRate: le.Uint32(p), Seq: le.Uint16(p[6:]),
		VoltageV: math.Float32frombits(le.Uint32(p[32:])), StartUs: le.Uint32(p[36:]),
		Status: le.Uint32(p[52:]), CurrentmA: make([]float32, n),
	}
	for i := range b.CurrentmA {
		b.CurrentmA[i] = math.Float32frombits(le.Uint32(p[aemDataHeaderLen+4*i:]))
	}
	if b.SampleRate == 0 {
		return b, fmt.Errorf("%w: AEM buffer with sample rate 0", ErrProtocol)
	}
	return b, nil
}

// AEMSplitter cuts the channel byte stream into messages.
type AEMSplitter struct {
	buf []byte
	// Skipped counts bytes discarded while searching for a header.
	Skipped int
}

// Feed appends data and returns the complete messages.
func (s *AEMSplitter) Feed(data []byte) []AEMMessage {
	s.buf = append(s.buf, data...)
	var out []AEMMessage
	for {
		i := bytes.Index(s.buf, aemMagic)
		if i < 0 {
			keep := min(len(s.buf), len(aemMagic)-1)
			s.Skipped += len(s.buf) - keep
			s.buf = s.buf[len(s.buf)-keep:]
			return out
		}
		s.Skipped += i
		s.buf = s.buf[i:]
		if len(s.buf) < aemHeaderLen {
			return out
		}
		total := int(binary.LittleEndian.Uint16(s.buf[10:]))
		if total < aemHeaderLen {
			s.buf = s.buf[1:]
			s.Skipped++
			continue
		}
		if len(s.buf) < total {
			return out
		}
		out = append(out, AEMMessage{
			KitMs: binary.LittleEndian.Uint32(s.buf[4:]), Type: binary.LittleEndian.Uint16(s.buf[8:]),
			Payload: append([]byte(nil), s.buf[aemHeaderLen:total]...),
		})
		s.buf = s.buf[total:]
	}
}

// AEMTimeline turns buffers into a gap-aware, duplicate-free sequence on a
// 64-bit kit time axis.
//
// The kit re-sends its newest buffer (new sequence number, same start time)
// when it is polled faster than a buffer fills, so buffers are identified by
// their start time. The 32-bit microsecond start time wraps every 71.6
// minutes; the millisecond queue time in the message header (same kit clock)
// resolves the wrap.
type AEMTimeline struct {
	lastStart int64
	have      bool
	// Duplicates counts re-sent buffers that were dropped.
	Duplicates int
	// Stale counts buffers older than the newest one that were dropped.
	Stale int
}

// Place returns the buffer's start on the 64-bit kit time axis in us and
// whether it is new.
func (t *AEMTimeline) Place(b AEMBuffer) (startUs int64, fresh bool) {
	msUs := int64(b.KitMs) * 1000
	startUs = msUs - int64(int32(uint32(msUs)-b.StartUs))
	if t.have {
		switch {
		case startUs == t.lastStart:
			t.Duplicates++
			return startUs, false
		case startUs < t.lastStart:
			t.Stale++
			return startUs, false
		}
	}
	t.lastStart, t.have = startUs, true
	return startUs, true
}
