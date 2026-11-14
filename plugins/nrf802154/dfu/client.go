package dfu

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"
)

// SLIP framing (RFC 1055) as used by the nRF5 SDK serial DFU transport.
const (
	slipEnd    = 0xC0
	slipEsc    = 0xDB
	slipEscEnd = 0xDC
	slipEscEsc = 0xDD
)

// SlipEncode frames one message. Like nrfutil, only a trailing END is sent.
func SlipEncode(msg []byte) []byte {
	out := make([]byte, 0, len(msg)+len(msg)/8+1)
	for _, b := range msg {
		switch b {
		case slipEnd:
			out = append(out, slipEsc, slipEscEnd)
		case slipEsc:
			out = append(out, slipEsc, slipEscEsc)
		default:
			out = append(out, b)
		}
	}
	return append(out, slipEnd)
}

// SlipDecoder reassembles frames from a byte stream.
type SlipDecoder struct {
	buf []byte
	esc bool
	bad bool
}

// Pending is the number of bytes of an unfinished frame.
func (d *SlipDecoder) Pending() int { return len(d.buf) }

// Feed consumes bytes and calls frame for each complete, well-formed,
// non-empty frame.
func (d *SlipDecoder) Feed(b []byte, frame func([]byte)) {
	for _, c := range b {
		switch {
		case c == slipEnd:
			if len(d.buf) > 0 && !d.bad && !d.esc {
				frame(d.buf)
			}
			d.buf, d.esc, d.bad = nil, false, false
		case d.esc:
			d.esc = false
			switch c {
			case slipEscEnd:
				d.buf = append(d.buf, slipEnd)
			case slipEscEsc:
				d.buf = append(d.buf, slipEsc)
			default:
				d.bad = true
			}
		case c == slipEsc:
			d.esc = true
		default:
			d.buf = append(d.buf, c)
		}
	}
}

// Serial DFU op codes (nrf_dfu_req_handler.h) and result codes.
const (
	OpProtocolVersion = 0x00
	OpCreate          = 0x01
	OpSetPRN          = 0x02
	OpCRCGet          = 0x03
	OpExecute         = 0x04
	OpSelect          = 0x06
	OpMTUGet          = 0x07
	OpWrite           = 0x08
	OpPing            = 0x09
	OpAbort           = 0x0C
	OpResponse        = 0x60

	ObjCommand = 0x01
	ObjData    = 0x02

	ResSuccess       = 0x01
	ResExtendedError = 0x0B
)

var resultNames = map[byte]string{
	0x00: "invalid opcode", 0x02: "opcode not supported", 0x03: "invalid parameter",
	0x04: "insufficient resources", 0x05: "invalid object", 0x07: "unsupported type",
	0x08: "operation not permitted", 0x0A: "operation failed", 0x0B: "extended error",
}

var extNames = map[byte]string{
	0x00: "no error", 0x01: "invalid error code", 0x02: "wrong command format", 0x03: "unknown command",
	0x04: "init command invalid", 0x05: "firmware version failure (downgrade refused?)",
	0x06: "hardware version failure", 0x07: "SoftDevice version failure", 0x08: "signature missing",
	0x09: "wrong hash type", 0x0A: "hash failed", 0x0B: "wrong signature type", 0x0C: "verification failed",
	0x0D: "insufficient space",
}

// Error is a non-success bootloader response.
type Error struct {
	Op, Result, Ext byte
}

func (e *Error) Error() string {
	s := fmt.Sprintf("bootloader rejected op 0x%02X: %s (0x%02X)", e.Op, resultNames[e.Result], e.Result)
	if e.Result == ResExtendedError {
		s += fmt.Sprintf(": %s (0x%02X)", extNames[e.Ext], e.Ext)
	}
	return s
}

// ErrTimeout is returned when the bootloader does not answer in time.
var ErrTimeout = errors.New("no response from the DFU bootloader")

// Client speaks serial DFU over a port whose Read returns (0, nil) on a
// read timeout (usbserial.Port).
type Client struct {
	Port io.ReadWriter
	// Timeout bounds each response; 5 s when zero.
	Timeout time.Duration
	// Progress, if set, is called after each data object is executed.
	Progress func(done, total int)
	// Trace, if set, records requests, raw reads, write durations and
	// timeouts for diagnosis.
	Trace *Transcript

	dec    SlipDecoder
	frames [][]byte
	mtu    int
	ping   byte
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 5 * time.Second
}

func (c *Client) send(msg []byte) error {
	enc := SlipEncode(msg)
	start := time.Now()
	n, err := c.Port.Write(enc)
	note := fmt.Sprintf("op=0x%02X slip=%d wrote=%d in %s", msg[0], len(enc), n, time.Since(start).Round(time.Microsecond))
	if err != nil {
		note += " error: " + err.Error()
	}
	c.Trace.add("tx", msg, note)
	return err
}

func (c *Client) recv(ctx context.Context, wait time.Duration) ([]byte, error) {
	start := time.Now()
	deadline := start.Add(wait)
	buf := make([]byte, 512)
	idle := 0
	for len(c.frames) == 0 {
		if err := ctx.Err(); err != nil {
			c.Trace.add("err", nil, fmt.Sprintf("context ended after %s (%d empty reads): %v", time.Since(start).Round(time.Millisecond), idle, err))
			return nil, err
		}
		if time.Now().After(deadline) {
			c.Trace.add("err", nil, fmt.Sprintf("timeout after %s (%d empty reads, %d undecoded bytes buffered)", time.Since(start).Round(time.Millisecond), idle, c.dec.Pending()))
			return nil, ErrTimeout
		}
		n, err := c.Port.Read(buf)
		if n > 0 {
			c.Trace.add("rx", buf[:n], fmt.Sprintf("after %d empty reads", idle))
			idle = 0
			c.dec.Feed(buf[:n], func(f []byte) { c.frames = append(c.frames, append([]byte(nil), f...)) })
		} else {
			idle++
		}
		if err != nil {
			c.Trace.add("err", nil, "read: "+err.Error())
			return nil, fmt.Errorf("read: %w", err)
		}
	}
	f := c.frames[0]
	c.frames = c.frames[1:]
	return f, nil
}

// request sends op+args and returns the response payload after the result.
func (c *Client) request(ctx context.Context, wait time.Duration, op byte, args ...byte) ([]byte, error) {
	if err := c.send(append([]byte{op}, args...)); err != nil {
		return nil, err
	}
	for {
		r, err := c.recv(ctx, wait)
		if err != nil {
			return nil, fmt.Errorf("op 0x%02X: %w", op, err)
		}
		if len(r) < 3 || r[0] != OpResponse {
			return nil, fmt.Errorf("op 0x%02X: malformed response % X", op, r)
		}
		if r[1] != op {
			c.Trace.add("note", r, fmt.Sprintf("skipped response to op 0x%02X while waiting for 0x%02X", r[1], op))
			continue // a late answer to an earlier request (e.g. ping retries)
		}
		if r[2] != ResSuccess {
			e := &Error{Op: op, Result: r[2]}
			if r[2] == ResExtendedError && len(r) > 3 {
				e.Ext = r[3]
			}
			return nil, e
		}
		return r[3:], nil
	}
}

// Ping checks that a DFU bootloader answers, retrying for up to wait.
func (c *Client) Ping(ctx context.Context, wait time.Duration) error {
	end := time.Now().Add(wait)
	for {
		c.ping++
		r, err := c.request(ctx, 500*time.Millisecond, OpPing, c.ping)
		if err == nil && len(r) >= 1 && r[0] == c.ping {
			return nil
		}
		var de *Error
		if errors.As(err, &de) {
			return nil // an error response still proves a DFU bootloader is listening
		}
		if ctx.Err() != nil || time.Now().After(end) {
			if err == nil {
				err = fmt.Errorf("ping id mismatch")
			}
			return fmt.Errorf("ping: %w", err)
		}
	}
}

// Open pings the bootloader, disables packet receipt notifications and
// reads the transport MTU.
func (c *Client) Open(ctx context.Context) error {
	if err := c.Ping(ctx, 3*time.Second); err != nil {
		return err
	}
	if _, err := c.request(ctx, c.timeout(), OpSetPRN, 0, 0); err != nil {
		return err
	}
	r, err := c.request(ctx, c.timeout(), OpMTUGet)
	if err != nil {
		return err
	}
	if len(r) < 2 {
		return fmt.Errorf("MTU response too short: % X", r)
	}
	c.mtu = int(binary.LittleEndian.Uint16(r))
	if c.mtu < 8 {
		return fmt.Errorf("bootloader MTU %d too small", c.mtu)
	}
	return nil
}

type selectResp struct{ maxSize, offset, crc uint32 }

func (c *Client) selectObj(ctx context.Context, typ byte) (selectResp, error) {
	r, err := c.request(ctx, c.timeout(), OpSelect, typ)
	if err != nil {
		return selectResp{}, err
	}
	if len(r) < 12 {
		return selectResp{}, fmt.Errorf("select response too short: % X", r)
	}
	le := binary.LittleEndian
	return selectResp{le.Uint32(r), le.Uint32(r[4:]), le.Uint32(r[8:])}, nil
}

func (c *Client) create(ctx context.Context, typ byte, size int) error {
	var a [5]byte
	a[0] = typ
	binary.LittleEndian.PutUint32(a[1:], uint32(size))
	_, err := c.request(ctx, c.timeout(), OpCreate, a[:]...)
	return err
}

// stream writes data in MTU-sized chunks (no per-chunk response) and checks
// the bootloader's cumulative offset and CRC-32 afterwards.
func (c *Client) stream(ctx context.Context, data []byte, crc uint32, offset int) (uint32, error) {
	chunk := (c.mtu-1)/2 - 1
	for i := 0; i < len(data); i += chunk {
		part := data[i:min(i+chunk, len(data))]
		if err := c.send(append([]byte{OpWrite}, part...)); err != nil {
			return 0, err
		}
		crc = crc32.Update(crc, crc32.IEEETable, part)
	}
	offset += len(data)
	r, err := c.request(ctx, c.timeout(), OpCRCGet)
	if err != nil {
		return 0, err
	}
	if len(r) < 8 {
		return 0, fmt.Errorf("CRC response too short: % X", r)
	}
	gotOff, gotCRC := binary.LittleEndian.Uint32(r), binary.LittleEndian.Uint32(r[4:])
	if int(gotOff) != offset || gotCRC != crc {
		return 0, fmt.Errorf("transfer check failed: bootloader has offset %d crc %08X, expected %d %08X", gotOff, gotCRC, offset, crc)
	}
	return crc, nil
}

// Result describes a completed DFU.
type Result struct {
	MTU     int
	Objects int
	// Activated is false when the bootloader reset before acknowledging
	// the final execute (the image was fully transferred and checked).
	Activated bool
}

// Program transfers the init packet and the application. It always starts
// from offset 0 (no resume). On success the bootloader validates the image
// and resets into it.
func (c *Client) Program(ctx context.Context, img *Image) (Result, error) {
	if err := c.Open(ctx); err != nil {
		return Result{}, err
	}
	res := Result{MTU: c.mtu}
	sel, err := c.selectObj(ctx, ObjCommand)
	if err != nil {
		return res, err
	}
	if int(sel.maxSize) < len(img.Init) {
		return res, fmt.Errorf("init packet (%d B) exceeds the bootloader's command object (%d B)", len(img.Init), sel.maxSize)
	}
	if err := c.create(ctx, ObjCommand, len(img.Init)); err != nil {
		return res, err
	}
	if _, err := c.stream(ctx, img.Init, 0, 0); err != nil {
		return res, err
	}
	if _, err := c.request(ctx, c.timeout(), OpExecute); err != nil {
		return res, fmt.Errorf("init packet: %w", err)
	}
	sel, err = c.selectObj(ctx, ObjData)
	if err != nil {
		return res, err
	}
	if sel.maxSize == 0 {
		return res, errors.New("bootloader reports a zero data object size")
	}
	var crc uint32
	for off := 0; off < len(img.Bin); off += int(sel.maxSize) {
		obj := img.Bin[off:min(off+int(sel.maxSize), len(img.Bin))]
		if err := c.create(ctx, ObjData, len(obj)); err != nil {
			return res, err
		}
		if crc, err = c.stream(ctx, obj, crc, off); err != nil {
			return res, err
		}
		last := off+len(obj) >= len(img.Bin)
		// Executing the last object validates the image, writes the
		// settings page and resets; flash writes can take a while.
		_, err := c.request(ctx, 3*c.timeout(), OpExecute)
		if err != nil {
			var de *Error
			if last && !errors.As(err, &de) && ctx.Err() == nil {
				res.Objects++
				return res, nil // reset before the acknowledgement
			}
			return res, err
		}
		res.Objects++
		if c.Progress != nil {
			c.Progress(off+len(obj), len(img.Bin))
		}
	}
	res.Activated = true
	return res, nil
}
