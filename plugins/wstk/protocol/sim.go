package protocol

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sync"
)

// SimKit is a simulated kit behind a Link, answering kit control and
// streaming synthetic AEM buffers. It is used by tests and by the plugin's
// simulated device.
type SimKit struct {
	mu sync.Mutex
	// Combined makes reads answer length and data in one transfer (as seen
	// for some channels) instead of two.
	Combined bool
	// CurrentmA and VoltageV shape the synthetic AEM buffers: sample i of
	// a buffer is CurrentmA + 0.001*i.
	CurrentmA, VoltageV float32
	// StartUs is the kit time of the first AEM buffer.
	StartUs uint32

	pendingCh  uint32
	pendingLen int
	pending    bool
	queues     map[uint32][]byte
	out        []byte // data half of a split read
	chunk      int
	aemOn      bool
	seq        uint16
	nextUs     uint32
}

// NewSimKit returns a kit with plausible defaults.
func NewSimKit() *SimKit {
	return &SimKit{CurrentmA: 1.5, VoltageV: 3.3, StartUs: 1_000_000, queues: map[uint32][]byte{}}
}

// Exchange implements Link.
func (s *SimKit) Exchange(_ context.Context, cmd []byte, n int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.pending:
		s.pending = false
		if len(cmd) != s.pendingLen || n != 4 {
			return nil, fmt.Errorf("sim: write data %d/%d, reply %d", len(cmd), s.pendingLen, n)
		}
		s.handle(s.pendingCh, cmd)
		return binary.LittleEndian.AppendUint32(nil, uint32(len(cmd))), nil
	case len(cmd) == 10 && cmd[0] == cmdEmucom && cmd[1] == 1 && n == 0:
		s.pendingCh = binary.LittleEndian.Uint32(cmd[2:])
		s.pendingLen = int(binary.LittleEndian.Uint32(cmd[6:]))
		s.pending = true
		return nil, nil
	}
	return nil, fmt.Errorf("sim: unexpected exchange % x / %d", cmd, n)
}

// ExchangeUpTo implements Link.
func (s *SimKit) ExchangeUpTo(_ context.Context, cmd []byte, maxLen int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(cmd) == 0 {
		// The rest of a split read, in transfers like the probe's (32, 64,
		// then up to 1024 bytes). A read that asks for exactly what is left
		// would leave the real probe's trailing zero-length packet pending.
		if maxLen <= len(s.out) {
			return nil, fmt.Errorf("sim: read of at most %d with %d pending would strand a zero-length packet", maxLen, len(s.out))
		}
		s.chunk++
		n := min(len(s.out), []int{32, 64, 1024}[min(s.chunk-1, 2)])
		r := s.out[:n]
		s.out = s.out[n:]
		return r, nil
	}
	s.chunk = 0
	if len(cmd) != 10 || cmd[0] != cmdEmucom || cmd[1] != 0 {
		return nil, fmt.Errorf("sim: unexpected read % x", cmd)
	}
	ch := binary.LittleEndian.Uint32(cmd[2:])
	want := int(binary.LittleEndian.Uint32(cmd[6:]))
	if ch == ChanAEM && s.aemOn && len(s.queues[ch]) == 0 {
		s.queues[ch] = s.aemBuffer()
	}
	q := s.queues[ch]
	n := min(want, len(q), maxLen-4)
	data := q[:n]
	s.queues[ch] = q[n:]
	hdr := binary.LittleEndian.AppendUint32(nil, uint32(n))
	if s.Combined {
		return append(hdr, data...), nil
	}
	s.out = append(s.out, data...)
	return hdr, nil
}

func (s *SimKit) handle(ch uint32, data []byte) {
	switch ch {
	case ChanKit:
		if len(data) >= kitHeaderLen {
			s.queues[ch] = append(s.queues[ch], simKitReply(data[5], data[6])...)
		}
	case ChanAEM:
		if len(data) >= 12 && binary.LittleEndian.Uint16(data[8:]) == AEMTypeStart {
			s.aemOn, s.nextUs = true, s.StartUs
			s.queues[ch] = append(s.queues[ch], simAEMConfig(s.StartUs/1000)...)
		}
	}
}

func simKitReply(cmd, arg byte) []byte {
	body := []byte{cmd, arg}
	field := func(tag byte, v ...byte) {
		body = append(body, tag, byte(len(v)), byte(len(v)>>8))
		body = append(body, v...)
	}
	status := byte(1)
	switch {
	case cmd == KitCmdInfo && arg == 1:
		field(0x00, 0, 0)
		field(0x01, 0x06, 0x14)
		field(0x02, 0x93, 0x04)
		field(0x07, 0x01, 0x40)
		field(0x0b, 0x01, 0xc2, 0xeb, 0x0b)
	case cmd == KitCmdBoards && arg == 1:
		recs := make([]byte, 160)
		for i := range recs {
			recs[i] = 0xff
		}
		copy(recs, []byte{0x01, 0x40, 0, 0, 0, 0, 0x02, 0xff, 0x01, 0xc2, 0xeb, 0x0b, 0xe4, 0x07, 0x0b, 0x1e})
		copy(recs[20:], []byte{0x83, 0x41, 0, 0, 0, 0, 0x00, 0xff, 0x02, 0xc2, 0xeb, 0x0b, 0xe4, 0x07, 0x0b, 0x14})
		field(0x00, 2)
		field(0x01, recs...)
	default:
		status = 2
	}
	h := []byte{1, 0, 0, 0, 0, status, 0, byte(len(body)), byte(len(body) >> 8)}
	h = append(h, checksum(h))
	return append(h, body...)
}

func simAEMConfig(ms uint32) []byte {
	b := make([]byte, 64)
	copy(b, aemMagic)
	binary.LittleEndian.PutUint32(b[4:], ms)
	binary.LittleEndian.PutUint16(b[8:], AEMTypeConfig)
	binary.LittleEndian.PutUint16(b[10:], 64)
	p := b[32:]
	copy(p, []byte{0, 0, 0xae, 0xae, 1, 0, 0, 0})
	binary.LittleEndian.PutUint32(p[8:], 10000)
	binary.LittleEndian.PutUint32(p[12:], 10000)
	return b
}

// aemBuffer builds the next 256-sample data message; the kit queues it
// 30 ms after the buffer started.
func (s *SimKit) aemBuffer() []byte {
	const n = 256
	b := make([]byte, aemHeaderLen+aemDataHeaderLen+4*n)
	copy(b, aemMagic)
	binary.LittleEndian.PutUint32(b[4:], s.nextUs/1000+30)
	binary.LittleEndian.PutUint16(b[8:], AEMTypeData)
	binary.LittleEndian.PutUint16(b[10:], uint16(len(b)))
	p := b[aemHeaderLen:]
	binary.LittleEndian.PutUint32(p, 10000)
	binary.LittleEndian.PutUint16(p[4:], n)
	binary.LittleEndian.PutUint16(p[6:], s.seq)
	binary.LittleEndian.PutUint32(p[32:], math.Float32bits(s.VoltageV))
	binary.LittleEndian.PutUint32(p[36:], s.nextUs)
	p[52] = 0x11
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(p[aemDataHeaderLen+4*i:], math.Float32bits(s.CurrentmA+0.001*float32(i)))
	}
	s.seq++
	s.nextUs += n * 100
	return b
}
