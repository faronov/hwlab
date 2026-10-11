package mpsse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"
)

// Link is the byte pipe an MPSSE engine runs over. ftdi.Conn implements it.
type Link interface {
	// Write sends the whole slice to the device.
	Write(ctx context.Context, data []byte) error
	// Read returns exactly n bytes or an error; on a timeout it returns the
	// bytes that did arrive together with the error.
	Read(ctx context.Context, n int, timeout time.Duration) ([]byte, error)
	// PurgeRX discards unread device and host-side receive data.
	PurgeRX(ctx context.Context) error
}

// Config tunes an Engine.
type Config struct {
	// ChunkReply bounds the reply bytes requested by one USB write, so the
	// device's small receive FIFO cannot fill up while the host is still
	// writing. Default 112.
	ChunkReply int
	// ChunkWrite bounds the command bytes of one USB write. Default 1024.
	ChunkWrite int
	// Timeout is the budget for one reply read. Default 1 s.
	Timeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.ChunkReply <= 0 {
		c.ChunkReply = DefaultMaxCmdBytes
	}
	if c.ChunkWrite <= 0 {
		c.ChunkWrite = 1024
	}
	if c.Timeout <= 0 {
		c.Timeout = time.Second
	}
	return c
}

// ErrBadCommand reports that the device answered 0xFA, i.e. it rejected an
// opcode. The opcode echoed by the device is wrapped in a BadCommandError.
var ErrBadCommand = errors.New("mpsse: device rejected a command")

// BadCommandError carries the opcode the device echoed back.
type BadCommandError struct{ Opcode byte }

func (e *BadCommandError) Error() string {
	return fmt.Sprintf("mpsse: device rejected opcode 0x%02x (bad command)", e.Opcode)
}

func (e *BadCommandError) Is(t error) bool { return t == ErrBadCommand }

// Engine executes Builders over a Link.
type Engine struct {
	link Link
	cfg  Config
}

// NewEngine returns an Engine over link.
func NewEngine(link Link, cfg Config) *Engine {
	return &Engine{link: link, cfg: cfg.withDefaults()}
}

// Link exposes the underlying pipe.
func (e *Engine) Link() Link { return e.link }

// Exec sends the commands of b and returns the whole reply. A reply of 0xFA
// where a reply byte is expected is not distinguishable from data in
// general; Exec therefore only reports bad commands where it can prove one
// (see Sync). Commands are grouped into USB writes that never exceed the
// reply budget, and every group that expects a reply ends with send-immediate.
func (e *Engine) Exec(ctx context.Context, b *Builder) ([]byte, error) {
	if err := b.Err(); err != nil {
		return nil, err
	}
	reply := make([]byte, 0, b.reply)
	i := 0
	for i < len(b.spans) {
		start := i
		var wr, rp int
		for i < len(b.spans) {
			s := b.spans[i]
			if i > start && (wr+s.n > e.cfg.ChunkWrite || rp+s.reply > e.cfg.ChunkReply) {
				break
			}
			wr += s.n
			rp += s.reply
			i++
		}
		first := b.spans[start]
		last := b.spans[i-1]
		out := make([]byte, 0, wr+1)
		out = append(out, b.buf[first.off:last.off+last.n]...)
		if rp > 0 {
			out = append(out, OpSendImmediate)
		}
		if err := e.link.Write(ctx, out); err != nil {
			return reply, err
		}
		if rp > 0 {
			got, err := e.link.Read(ctx, rp, e.cfg.Timeout)
			reply = append(reply, got...)
			if err != nil {
				return reply, fmt.Errorf("mpsse: waiting for %d reply byte(s): %w", rp, err)
			}
		}
	}
	return reply, nil
}

// Sync verifies the command processor is alive and in sync: it sends an
// invalid opcode and expects the documented FA <opcode> answer. Any stale
// receive data is purged first.
func (e *Engine) Sync(ctx context.Context) error {
	if err := e.link.PurgeRX(ctx); err != nil {
		return err
	}
	for _, op := range []byte{OpBadCommand, OpBadCommandAlt} {
		if err := e.link.Write(ctx, []byte{op, OpSendImmediate}); err != nil {
			return err
		}
		got, err := e.link.Read(ctx, 2, e.cfg.Timeout)
		if err != nil {
			return fmt.Errorf("mpsse sync: %w (got % x)", err, got)
		}
		if !bytes.Equal(got, []byte{ReplyBadCommand, op}) {
			return fmt.Errorf("mpsse sync: unexpected answer % x, want fa %02x", got, op)
		}
	}
	return nil
}

// LoopbackResult reports what the loopback self-test observed.
type LoopbackResult struct {
	// Supported is true when the data written came back through the
	// internal loopback exactly.
	Supported bool   `json:"supported"`
	Sent      []byte `json:"sent"`
	Received  []byte `json:"received,omitempty"`
	// Note explains an unsupported result.
	Note string `json:"note,omitempty"`
}

// LoopbackTest enables internal loopback, shifts a pattern out and back, and
// disables loopback again. An emulator that ignores the loopback opcodes
// (and so leaves TDO to the pins) yields Supported=false rather than an
// error; only a transport failure returns an error. Loopback is always
// switched off before returning.
func (e *Engine) LoopbackTest(ctx context.Context, pattern []byte) (LoopbackResult, error) {
	res := LoopbackResult{Sent: append([]byte(nil), pattern...)}
	b := NewBuilder()
	b.Loopback(true)
	rs := b.ShiftBytes(pattern, true)
	b.Loopback(false)
	reply, err := e.Exec(ctx, b)
	// Always try to leave loopback off, even after a failure.
	off := NewBuilder()
	off.Loopback(false)
	_, offErr := e.Exec(ctx, off)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return res, err
		}
		res.Note = "no reply with loopback enabled: " + err.Error()
		_ = e.link.PurgeRX(ctx)
		return res, nil
	}
	if offErr != nil {
		return res, offErr
	}
	res.Received = ReadAll(reply, rs)
	res.Supported = bytes.Equal(res.Received, pattern)
	if !res.Supported {
		res.Note = "reply did not match the pattern: loopback opcodes are ignored or unsupported by this device"
	}
	return res, nil
}
