package modbus

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Slave is a simulated Modbus device with four tables. It exists for tests,
// demos and recorded fixtures: its behaviour is this package's reading of the
// specification, not evidence about any real device.
type Slave struct {
	Unit byte

	mu       sync.Mutex
	coils    []bool
	discrete []bool
	holding  []uint16
	input    []uint16
	served   uint64

	// OnRequest runs under the slave's lock before each request is applied; the
	// fake uses it to make an input register move.
	OnRequest func(s *Slave, r *Request)
	// Mangle, if set, rewrites each RTU response frame before it is sent.
	Mangle func(frame []byte) []byte
}

// NewSlave creates a slave answering as unit with size entries per table.
func NewSlave(unit byte, size int) *Slave {
	return &Slave{Unit: unit, coils: make([]bool, size), discrete: make([]bool, size),
		holding: make([]uint16, size), input: make([]uint16, size)}
}

// Served is the number of requests handled.
func (s *Slave) Served() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served
}

// SetHolding, SetInput, SetCoil and SetDiscrete preload the tables.
func (s *Slave) SetHolding(addr int, v ...uint16) {
	s.mu.Lock()
	copy(s.holding[addr:], v)
	s.mu.Unlock()
}
func (s *Slave) SetInput(addr int, v ...uint16) { s.mu.Lock(); copy(s.input[addr:], v); s.mu.Unlock() }
func (s *Slave) SetCoil(addr int, v bool)       { s.mu.Lock(); s.coils[addr] = v; s.mu.Unlock() }
func (s *Slave) SetDiscrete(addr int, v bool)   { s.mu.Lock(); s.discrete[addr] = v; s.mu.Unlock() }

// Holding returns a copy of holding registers [addr, addr+n).
func (s *Slave) Holding(addr, n int) []uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint16(nil), s.holding[addr:addr+n]...)
}

// Coil returns coil addr.
func (s *Slave) Coil(addr int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coils[addr]
}

// AddInput adds delta to input register addr, from OnRequest.
func (s *Slave) AddInput(addr int, delta uint16) { s.input[addr] += delta }

func ex(fc, code byte) *Response { return &Response{Function: fc, Exception: code} }

// Handle applies a request and returns the answer. The caller addresses it to
// the right unit.
func (s *Slave) Handle(r *Request) *Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served++
	if s.OnRequest != nil {
		s.OnRequest(s, r)
	}
	resp := &Response{Unit: r.Unit, Function: r.Function}
	inRange := func(addr, n, size int) bool { return n >= 1 && addr+n <= size }
	switch r.Function {
	case FCReadCoils, FCReadDiscreteInputs:
		t := s.coils
		if r.Function == FCReadDiscreteInputs {
			t = s.discrete
		}
		if r.Quantity < 1 || r.Quantity > MaxReadBits {
			return ex(r.Function, ExIllegalDataValue)
		}
		if !inRange(int(r.Address), int(r.Quantity), len(t)) {
			return ex(r.Function, ExIllegalDataAddress)
		}
		resp.Bits = append([]bool(nil), t[r.Address:int(r.Address)+int(r.Quantity)]...)
	case FCReadHoldingRegisters, FCReadInputRegisters:
		t := s.holding
		if r.Function == FCReadInputRegisters {
			t = s.input
		}
		if r.Quantity < 1 || r.Quantity > MaxReadRegisters {
			return ex(r.Function, ExIllegalDataValue)
		}
		if !inRange(int(r.Address), int(r.Quantity), len(t)) {
			return ex(r.Function, ExIllegalDataAddress)
		}
		resp.Registers = append([]uint16(nil), t[r.Address:int(r.Address)+int(r.Quantity)]...)
	case FCWriteSingleCoil:
		if !inRange(int(r.Address), 1, len(s.coils)) {
			return ex(r.Function, ExIllegalDataAddress)
		}
		s.coils[r.Address] = r.Coils[0]
		resp.Address, resp.Value = r.Address, 0
		if r.Coils[0] {
			resp.Value = 0xFF00
		}
	case FCWriteSingleRegister:
		if !inRange(int(r.Address), 1, len(s.holding)) {
			return ex(r.Function, ExIllegalDataAddress)
		}
		s.holding[r.Address] = r.Registers[0]
		resp.Address, resp.Value = r.Address, r.Registers[0]
	case FCWriteMultipleCoils:
		n := len(r.Coils)
		if n < 1 || n > MaxWriteBits {
			return ex(r.Function, ExIllegalDataValue)
		}
		if !inRange(int(r.Address), n, len(s.coils)) {
			return ex(r.Function, ExIllegalDataAddress)
		}
		copy(s.coils[r.Address:], r.Coils)
		resp.Address, resp.Quantity = r.Address, uint16(n)
	case FCWriteMultipleRegisters:
		n := len(r.Registers)
		if n < 1 || n > MaxWriteRegs {
			return ex(r.Function, ExIllegalDataValue)
		}
		if !inRange(int(r.Address), n, len(s.holding)) {
			return ex(r.Function, ExIllegalDataAddress)
		}
		copy(s.holding[r.Address:], r.Registers)
		resp.Address, resp.Quantity = r.Address, uint16(n)
	case FCMaskWriteRegister:
		if !inRange(int(r.Address), 1, len(s.holding)) {
			return ex(r.Function, ExIllegalDataAddress)
		}
		cur := s.holding[r.Address]
		s.holding[r.Address] = cur&r.AndMask | r.OrMask&^r.AndMask
		resp.Address, resp.AndMask, resp.OrMask = r.Address, r.AndMask, r.OrMask
	case FCReadWriteRegisters:
		if r.ReadQuantity < 1 || r.ReadQuantity > MaxRWReadRegs || len(r.Registers) < 1 || len(r.Registers) > MaxRWWriteRegs {
			return ex(r.Function, ExIllegalDataValue)
		}
		if !inRange(int(r.ReadAddress), int(r.ReadQuantity), len(s.holding)) || !inRange(int(r.Address), len(r.Registers), len(s.holding)) {
			return ex(r.Function, ExIllegalDataAddress)
		}
		copy(s.holding[r.Address:], r.Registers) // the write happens before the read
		resp.Registers = append([]uint16(nil), s.holding[r.ReadAddress:int(r.ReadAddress)+int(r.ReadQuantity)]...)
	default:
		return ex(r.Function, ExIllegalFunction)
	}
	return resp
}

func (s *Slave) handlePDU(unit byte, pdu []byte) []byte {
	req, err := ParseRequest(unit, pdu)
	if err != nil {
		code := byte(ExIllegalDataValue)
		return (&Response{Function: pdu[0] & 0x7F, Exception: code}).PDU()
	}
	return s.Handle(req).PDU()
}

// ServeRTU answers requests arriving on a serial-like stream until ctx ends or
// the stream closes. Bad CRCs and other units are ignored, as on a real bus.
func (s *Slave) ServeRTU(ctx context.Context, port Serial) {
	var buf []byte
	chunk := make([]byte, 512)
	_ = port.SetReadTimeout(10 * time.Millisecond)
	idle := 0
	for ctx.Err() == nil {
		n, err := port.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			idle = 0
		} else {
			idle++
			if idle > 5 && len(buf) > 0 {
				buf = nil // a stale partial frame
			}
		}
		if err != nil && n == 0 {
			return
		}
		frames, rest, _ := ExtractRTU(buf, RoleRequest, false)
		buf = append([]byte(nil), rest...)
		for _, f := range frames {
			if f.BadCRC || (f.Unit != s.Unit && f.Unit != 0) {
				continue
			}
			out := s.handlePDU(f.Unit, f.PDU)
			if f.Unit == 0 {
				continue
			}
			frame := EncodeRTU(s.Unit, out)
			if s.Mangle != nil {
				frame = s.Mangle(frame)
			}
			if _, werr := port.Write(frame); werr != nil {
				return
			}
		}
	}
}

// ServeTCP accepts connections on l and answers MBAP requests until l closes.
func (s *Slave) ServeTCP(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go s.serveConn(c)
	}
}

func (s *Slave) serveConn(c net.Conn) {
	defer func() {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Warn("close simulated Modbus connection", "err", err)
		}
	}()
	for {
		hdr := make([]byte, mbapLen)
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(hdr[4:]))
		if n < 2 || n > MaxPDU+1 {
			return
		}
		body := make([]byte, n-1)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		unit := hdr[6]
		if unit != s.Unit && unit != 0 && unit != 0xFF {
			continue
		}
		out := s.handlePDU(unit, body)
		if unit == 0 {
			continue
		}
		if _, err := c.Write(EncodeTCP(be16(hdr), unit, out)); err != nil {
			return
		}
	}
}

// LinkEnd is one end of an in-memory serial link: bytes written to one end are
// read from the other. Reads honour SetReadTimeout like a real port: a timed
// out Read returns 0, nil.
type LinkEnd struct {
	in      chan []byte
	peer    *LinkEnd
	closed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	pending []byte
	timeout time.Duration
}

// NewLink returns the two ends of an in-memory serial link.
func NewLink() (*LinkEnd, *LinkEnd) {
	a := &LinkEnd{in: make(chan []byte, 256), closed: make(chan struct{})}
	b := &LinkEnd{in: make(chan []byte, 256), closed: make(chan struct{})}
	a.peer, b.peer = b, a
	return a, b
}

// SetReadTimeout implements Serial.
func (l *LinkEnd) SetReadTimeout(d time.Duration) error {
	l.mu.Lock()
	l.timeout = d
	l.mu.Unlock()
	return nil
}

// ResetInput discards received bytes that were not read yet.
func (l *LinkEnd) ResetInput() error {
	l.mu.Lock()
	l.pending = nil
	l.mu.Unlock()
	for {
		select {
		case <-l.in:
		default:
			return nil
		}
	}
}

// Write implements io.Writer.
func (l *LinkEnd) Write(p []byte) (int, error) {
	select {
	case <-l.closed:
		return 0, io.ErrClosedPipe
	case <-l.peer.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	select {
	case l.peer.in <- append([]byte(nil), p...):
		return len(p), nil
	case <-l.closed:
		return 0, io.ErrClosedPipe
	}
}

// Read implements io.Reader.
func (l *LinkEnd) Read(p []byte) (int, error) {
	l.mu.Lock()
	if len(l.pending) > 0 {
		n := copy(p, l.pending)
		l.pending = l.pending[n:]
		l.mu.Unlock()
		return n, nil
	}
	timeout := l.timeout
	l.mu.Unlock()
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case b := <-l.in:
		n := copy(p, b)
		if n < len(b) {
			l.mu.Lock()
			l.pending = append(l.pending, b[n:]...)
			l.mu.Unlock()
		}
		return n, nil
	case <-timer:
		return 0, nil
	case <-l.closed:
		return 0, io.EOF
	}
}

// Close implements io.Closer.
func (l *LinkEnd) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}
