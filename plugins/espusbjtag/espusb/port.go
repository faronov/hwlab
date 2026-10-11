package espusb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/usb"
)

// Transfer sizing. The device has almost no endpoint RAM: its IN FIFO holds one
// 64-byte packet plus a few bytes, and the OUT endpoint stalls when the FIFO is
// full of unread capture data. So the driver never has more than maxCapBits
// captured bits outstanding: it ends each segment with FLUSH and reads the
// reply before sending the next one.
const (
	maxCapBits = 60 * 8
	maxSegCmds = 8192
	drainSlice = 20 * time.Millisecond
)

// Config tunes a Probe.
type Config struct {
	// ClockKHz is the requested TCK frequency (default 1000). The device's
	// divider limits decide the actual value.
	ClockKHz int
	// Timeout bounds each bulk phase (default 2 s).
	Timeout time.Duration
}

// Probe is an open Espressif USB-JTAG interface. It implements jtag.Port.
// Operations are serialised; Close is idempotent.
type Probe struct {
	mu      sync.Mutex
	u       usb.Port
	in, out uint8
	cfg     Config
	caps    Caps
	div     int
	khz     int
	dirty   bool
	closed  bool
}

// Open takes ownership of u, a claimed vendor interface with bulk endpoints in
// and out. It reads the capabilities descriptor, sets the TCK divider and drains
// stale IN data. It does not clock or drive any pin.
func Open(ctx context.Context, u usb.Port, in, out uint8, cfg Config) (*Probe, error) {
	if cfg.ClockKHz <= 0 {
		cfg.ClockKHz = 1000
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	p := &Probe{u: u, in: in, out: out, cfg: cfg, dirty: true}
	raw, err := u.ControlIn(ctx, 0x80, 6, capsValue, 0, capsLength)
	if err != nil {
		return nil, fmt.Errorf("espusb: read JTAG capabilities: %w", err)
	}
	if p.caps, err = ParseCaps(raw); err != nil {
		return nil, err
	}
	if err := p.SetClock(ctx, cfg.ClockKHz); err != nil {
		return nil, err
	}
	if err := p.drain(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// Caps returns the device capabilities.
func (p *Probe) Caps() Caps { return p.caps }

// ClockKHz is the TCK frequency in effect.
func (p *Probe) ClockKHz() int { p.mu.Lock(); defer p.mu.Unlock(); return p.khz }

// Divisor is the divider in effect.
func (p *Probe) Divisor() int { p.mu.Lock(); defer p.mu.Unlock(); return p.div }

// SetClock programs the TCK divider for about khz. The request bypasses the
// command pipe, so call it only between operations (every Probe method
// returns with its commands already accepted by the device).
func (p *Probe) SetClock(ctx context.Context, khz int) error {
	div, actual := p.caps.Divisor(khz)
	if err := p.u.ControlOut(ctx, reqTypeOut, reqSetDiv, uint16(div), 0, nil); err != nil {
		return fmt.Errorf("espusb: set TCK divider %d: %w", div, err)
	}
	p.div, p.khz = div, actual
	return nil
}

// TDO samples the TDO pin through the control pipe.
func (p *Probe) TDO(ctx context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false, usb.ErrClosed
	}
	b, err := p.u.ControlIn(ctx, reqTypeIn, reqGetTDO, 0, 0, 1)
	if err != nil {
		return false, fmt.Errorf("espusb: read TDO: %w", err)
	}
	if len(b) < 1 {
		return false, errors.New("espusb: empty TDO reply")
	}
	return b[0]&1 == 1, nil
}

// Describe names the transport.
func (p *Probe) Describe() string {
	return fmt.Sprintf("espusb %s, TCK %d kHz (div %d of %d kHz)", p.u.Describe(), p.khz, p.div, p.caps.BaseKHz)
}

// Close releases the USB interface. It is idempotent.
func (p *Probe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.u.Close()
}

// drain discards IN data left over from an aborted operation.
func (p *Probe) drain(ctx context.Context) error {
	for i := 0; i < 16; i++ {
		b, err := p.u.BulkIn(ctx, p.in, 64, drainSlice)
		if errors.Is(err, usb.ErrTimeout) {
			p.dirty = false
			return nil
		}
		if err != nil {
			return fmt.Errorf("espusb: drain: %w", err)
		}
		_ = b
	}
	return errors.New("espusb: IN endpoint keeps producing data")
}

// TMS implements jtag.Port.
func (p *Probe) TMS(ctx context.Context, tms []bool, tdi bool) error {
	cmds := make([]byte, len(tms))
	for i, t := range tms {
		cmds[i] = clk(false, t, tdi)
	}
	_, err := p.exec(ctx, cmds)
	return err
}

// Shift implements jtag.Port.
func (p *Probe) Shift(ctx context.Context, tdi []byte, nbits int, exit, capture bool) ([]byte, error) {
	if nbits < 1 {
		return nil, errors.New("espusb: shift of zero bits")
	}
	need := (nbits + 7) / 8
	if tdi == nil {
		tdi = make([]byte, need)
	}
	if len(tdi) < need {
		return nil, fmt.Errorf("espusb: %d bit(s) need %d byte(s) of data, have %d", nbits, need, len(tdi))
	}
	cmds := make([]byte, nbits)
	for i := range cmds {
		cmds[i] = clk(capture, exit && i == nbits-1, tdi[i/8]>>(i%8)&1 == 1)
	}
	got, err := p.exec(ctx, cmds)
	if err != nil || !capture {
		return nil, err
	}
	return got, nil
}

// Batch implements jtag.Batcher: every op is encoded into one command stream,
// so the number of USB round-trips depends on the amount of captured data
// rather than on the number of scans.
func (p *Probe) Batch(ctx context.Context, ops []jtag.Op) ([][]byte, error) {
	var cmds []byte
	var caps []int
	for _, op := range ops {
		if op.NBits == 0 {
			for _, t := range op.TMS {
				cmds = append(cmds, clk(false, t, op.TDI))
			}
			continue
		}
		need := (op.NBits + 7) / 8
		if op.Data != nil && len(op.Data) < need {
			return nil, fmt.Errorf("espusb: %d bit(s) need %d byte(s) of data, have %d", op.NBits, need, len(op.Data))
		}
		if op.Capture < 0 || op.Capture > op.NBits {
			return nil, fmt.Errorf("espusb: capture %d of %d bit(s)", op.Capture, op.NBits)
		}
		for i := 0; i < op.NBits; i++ {
			tdi := op.Data != nil && op.Data[i/8]>>(i%8)&1 == 1
			cmds = append(cmds, clk(i < op.Capture, op.Exit && i == op.NBits-1, tdi))
		}
		if op.Capture > 0 {
			caps = append(caps, op.Capture)
		}
	}
	got, err := p.exec(ctx, cmds)
	if err != nil {
		return nil, err
	}
	res := make([][]byte, 0, len(caps))
	off := 0
	for _, n := range caps {
		b := make([]byte, (n+7)/8)
		for i := 0; i < n; i++ {
			if got[(off+i)/8]>>((off+i)%8)&1 == 1 {
				b[i/8] |= 1 << (i % 8)
			}
		}
		res = append(res, b)
		off += n
	}
	return res, nil
}

// exec runs a command list and returns the captured TDO bits packed LSB first
// in capture order (nil when nothing is captured).
func (p *Probe) exec(ctx context.Context, cmds []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, usb.ErrClosed
	}
	if p.dirty {
		if err := p.drain(ctx); err != nil {
			return nil, err
		}
	}
	var res []byte
	nres := 0
	for i := 0; i < len(cmds); {
		var enc Encoder
		capBits, n := 0, 0
		for i < len(cmds) && capBits < maxCapBits && n < maxSegCmds {
			c := cmds[i]
			enc.Add(c)
			if c&4 != 0 {
				capBits++
			}
			i++
			n++
		}
		if capBits > 0 {
			enc.Add(cmdFlush)
		}
		if err := p.write(ctx, enc.Bytes()); err != nil {
			p.dirty = true
			return nil, err
		}
		if capBits == 0 {
			continue
		}
		seg, err := p.read(ctx, (capBits+7)/8)
		if err != nil {
			p.dirty = true
			return nil, err
		}
		if need := (nres + capBits + 7) / 8; len(res) < need {
			res = append(res, make([]byte, need-len(res))...)
		}
		for b := 0; b < capBits; b++ {
			if seg[b/8]>>(b%8)&1 == 1 {
				res[(nres+b)/8] |= 1 << ((nres + b) % 8)
			}
		}
		nres += capBits
	}
	return res, nil
}

func (p *Probe) write(ctx context.Context, data []byte) error {
	for len(data) > 0 {
		n, err := p.u.BulkOut(ctx, p.out, data, p.cfg.Timeout)
		if err != nil {
			return fmt.Errorf("espusb: bulk out: %w", err)
		}
		if n <= 0 {
			return errors.New("espusb: bulk out made no progress")
		}
		data = data[min(n, len(data)):]
	}
	return nil
}

func (p *Probe) read(ctx context.Context, need int) ([]byte, error) {
	deadline := time.Now().Add(p.cfg.Timeout)
	buf := make([]byte, 0, need)
	for len(buf) < need {
		left := time.Until(deadline)
		if left <= 0 {
			return nil, fmt.Errorf("espusb: bulk in: %w (%d of %d byte(s))", usb.ErrTimeout, len(buf), need)
		}
		b, err := p.u.BulkIn(ctx, p.in, need-len(buf), left)
		if err != nil {
			if errors.Is(err, usb.ErrTimeout) {
				return nil, fmt.Errorf("espusb: bulk in: %w (%d of %d byte(s))", err, len(buf), need)
			}
			return nil, fmt.Errorf("espusb: bulk in: %w", err)
		}
		// The device may answer with an empty packet instead of NAKing.
		if len(b) == 0 {
			time.Sleep(time.Millisecond)
		}
		buf = append(buf, b...)
	}
	if len(buf) > need {
		return nil, fmt.Errorf("espusb: device returned %d byte(s), expected %d", len(buf), need)
	}
	return buf, nil
}
