package modbus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Exchange is the raw record of one request/response round trip.
type Exchange struct {
	ReqADU  []byte
	RespADU []byte // nil for a broadcast or when nothing arrived
	RespPDU []byte
	// Sent is taken just before the request is written, Received when the last
	// byte of the response was read. Neither is a wire time.
	Sent, Received time.Time
	// TID is the MBAP transaction id (TCP only).
	TID uint16
}

// Transport sends one request and waits for the answer. Implementations
// serialise calls: Modbus allows one outstanding request per connection here.
type Transport interface {
	RoundTrip(ctx context.Context, unit byte, pdu []byte) (*Exchange, error)
	Close() error
	Describe() string
	// Kind is "rtu" or "tcp".
	Kind() string
}

// Serial is the byte stream an RTU master needs. uart.Port satisfies it.
type Serial interface {
	io.ReadWriter
	// SetReadTimeout bounds each Read; a timed-out Read returns 0, nil.
	SetReadTimeout(d time.Duration) error
}

type inputResetter interface{ ResetInput() error }

// RTU is a master on a serial line.
type RTU struct {
	port    Serial
	desc    string
	Timeout time.Duration // per request, default 1 s
	Silence time.Duration // quiet time before each request, default RTUSilence(0)

	mu   sync.Mutex
	last time.Time
}

// NewRTU wraps an open serial port. baud only sets the inter-frame silence.
func NewRTU(port Serial, baud int, desc string) *RTU {
	return &RTU{port: port, desc: desc, Timeout: time.Second, Silence: RTUSilence(baud)}
}

func (t *RTU) Kind() string     { return "rtu" }
func (t *RTU) Describe() string { return t.desc }

func (t *RTU) Close() error {
	if c, ok := t.port.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

const readSlice = 20 * time.Millisecond

// RoundTrip implements Transport.
func (t *RTU) RoundTrip(ctx context.Context, unit byte, pdu []byte) (*Exchange, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if wait := t.Silence - time.Since(t.last); wait > 0 {
		time.Sleep(wait)
	}
	if r, ok := t.port.(inputResetter); ok {
		_ = r.ResetInput()
	}
	ex := &Exchange{ReqADU: EncodeRTU(unit, pdu)}
	ex.Sent = time.Now()
	if _, err := t.port.Write(ex.ReqADU); err != nil {
		return ex, fmt.Errorf("serial write: %w", err)
	}
	t.last = time.Now()
	if unit == 0 {
		return ex, nil // broadcast: slaves do not answer
	}
	deadline := time.Now().Add(t.Timeout)
	var buf []byte
	chunk := make([]byte, 512)
	for {
		if err := ctx.Err(); err != nil {
			return ex, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			if len(buf) > 0 {
				ex.RespADU = buf
				return ex, fmt.Errorf("%w: %d stray byte(s) %s", ErrTimeout, len(buf), Hex(buf))
			}
			return ex, ErrTimeout
		}
		if err := t.port.SetReadTimeout(min(left, readSlice)); err != nil {
			return ex, err
		}
		n, err := t.port.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			t.last = time.Now()
		}
		if err != nil && (!errors.Is(err, io.EOF) || n <= 0) {
			if errors.Is(err, io.EOF) {
				err = errors.New("the serial port was closed")
			}
			return ex, fmt.Errorf("serial read: %w", err)
		}
		frames, _, _ := ExtractRTU(buf, RoleResponse, false)
		for _, f := range frames {
			if f.Unit != unit {
				continue // a late answer to something else
			}
			ex.Received = time.Now()
			ex.RespADU = f.Raw
			if f.BadCRC {
				return ex, ErrCRC
			}
			ex.RespPDU = f.PDU
			return ex, nil
		}
		if len(frames) > 0 {
			buf = nil
			continue
		}
		// A function this package cannot measure (a raw request): accept the
		// buffer as one frame once the line has been idle for a read slice.
		if n == 0 && len(buf) >= 4 {
			if u, p, derr := DecodeRTU(buf); derr == nil && u == unit {
				ex.Received = time.Now()
				ex.RespADU, ex.RespPDU = buf, p
				return ex, nil
			}
		}
	}
}

// TCP is a master on a Modbus TCP connection.
type TCP struct {
	conn    net.Conn
	desc    string
	Timeout time.Duration

	mu  sync.Mutex
	tid uint16
}

// NewTCP wraps an established connection.
func NewTCP(conn net.Conn, desc string) *TCP {
	return &TCP{conn: conn, desc: desc, Timeout: time.Second}
}

// DialTCP connects to host:port (port 502 when omitted).
func DialTCP(addr string, timeout time.Duration) (*TCP, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "502")
	}
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	t := NewTCP(c, "tcp://"+addr)
	t.Timeout = timeout
	return t, nil
}

func (t *TCP) Kind() string     { return "tcp" }
func (t *TCP) Describe() string { return t.desc }
func (t *TCP) Close() error     { return t.conn.Close() }

// RoundTrip implements Transport.
func (t *TCP) RoundTrip(ctx context.Context, unit byte, pdu []byte) (*Exchange, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tid++
	ex := &Exchange{TID: t.tid, ReqADU: EncodeTCP(t.tid, unit, pdu)}
	deadline := time.Now().Add(t.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = t.conn.SetDeadline(deadline)
	ex.Sent = time.Now()
	if _, err := t.conn.Write(ex.ReqADU); err != nil {
		return ex, fmt.Errorf("tcp write: %w", err)
	}
	for {
		hdr := make([]byte, mbapLen)
		if _, err := io.ReadFull(t.conn, hdr); err != nil {
			return ex, tcpReadErr(err)
		}
		n := int(be16(hdr[4:]))
		if n < 2 || n > MaxPDU+1 {
			return ex, fmt.Errorf("%w: MBAP length %d", ErrProtocol, n)
		}
		body := make([]byte, n-1)
		if _, err := io.ReadFull(t.conn, body); err != nil {
			return ex, tcpReadErr(err)
		}
		frame := append(hdr, body...)
		ex.Received = time.Now()
		tid, rUnit, rPDU, err := DecodeTCP(frame)
		if err != nil {
			ex.RespADU = frame
			return ex, err
		}
		if tid != t.tid {
			continue // a late answer to an earlier, timed-out request
		}
		ex.RespADU, ex.RespPDU = frame, rPDU
		if rUnit != unit {
			return ex, fmt.Errorf("%w: answer from unit %d to a request for unit %d", ErrProtocol, rUnit, unit)
		}
		return ex, nil
	}
}

func tcpReadErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("the slave closed the connection")
	}
	return fmt.Errorf("tcp read: %w", err)
}

// Result is the outcome of one request.
type Result struct {
	Request  *Request
	Response *Response // nil on a transport error, or for a broadcast
	Exchange *Exchange
	Attempts int
}

// Client issues typed requests over a Transport.
type Client struct {
	T Transport
	// Retries are extra attempts after a timeout or a CRC error. Only reads are
	// ever retried: repeating a write could apply it twice.
	Retries int
}

// Do sends req and decodes the answer. On an exception it returns both the
// Result and an *ExceptionError.
func (c *Client) Do(ctx context.Context, req *Request) (*Result, error) {
	pdu, err := req.PDU()
	if err != nil {
		return nil, err
	}
	res := &Result{Request: req}
	for attempt := 0; ; attempt++ {
		res.Attempts = attempt + 1
		ex, err := c.T.RoundTrip(ctx, req.Unit, pdu)
		res.Exchange = ex
		if err != nil {
			retry := (errors.Is(err, ErrTimeout) || errors.Is(err, ErrCRC)) && !IsWrite(req.Function) && attempt < c.Retries && ctx.Err() == nil
			if retry {
				continue
			}
			return res, err
		}
		if req.Unit == 0 {
			return res, nil
		}
		resp, err := ParseResponse(req.Unit, ex.RespPDU, req)
		if err != nil {
			return res, err
		}
		res.Response = resp
		if resp.Exception != 0 {
			return res, &ExceptionError{Function: resp.Function, Code: resp.Exception}
		}
		return res, nil
	}
}
