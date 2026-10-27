// Package rttplugin is the plugin-side glue of target-memory RTT shared by the
// debug-probe plugins (jlinknative, xds110, cmsisdap): the configuration keys,
// the DebugProbe.rtt_info / rtt_write methods and the capture that polls one up
// buffer and emits its lines as Log events. The RTT engine itself is
// internal/rtt; a plugin supplies target memory access through a Target.
package rttplugin

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/rtt"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Stream is the stream id of RTT capture output.
const Stream = "rtt"

// The default search window is the first 32 KiB of SRAM. A read past the end
// of RAM faults, so the window is never widened silently; set rtt_search_size
// for a larger part.
const (
	DefaultSearchStart = 0x20000000
	DefaultSearchSize  = 0x8000
)

const (
	maxLineLen = 64 << 10
	maxWrite   = 4096
)

// Keys are the RTT configuration keys of the methods and the capture.
var Keys = []string{"rtt_address", "rtt_search_start", "rtt_search_size", "rtt_channel", "rtt_wait_ms", "rtt_poll_min_ms", "rtt_poll_max_ms"}

// Props are the RTT location options shared by the RTT methods and capture,
// as JSON schema properties with a leading comma.
const Props = `,"rtt_address":{},"rtt_search_start":{},"rtt_search_size":{"type":"number"},"rtt_channel":{"type":"number"}`

// Config is a parsed RTT configuration.
type Config struct {
	Address    uint32
	HasAddress bool
	Window     rtt.Range
	Channel    int
	Wait       time.Duration
	PollMin    time.Duration
	PollMax    time.Duration
}

// HasKeys reports whether cfg sets any RTT key.
func HasKeys(cfg map[string]any) bool {
	for _, k := range Keys {
		if _, ok := cfg[k]; ok {
			return true
		}
	}
	return false
}

// Without returns a copy of cfg without the RTT keys and the extra keys.
func Without(cfg map[string]any, extra ...string) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	for _, k := range Keys {
		delete(out, k)
	}
	for _, k := range extra {
		delete(out, k)
	}
	return out
}

// ParseConfig reads the RTT keys of cfg; other keys are ignored. Errors are
// plain errors describing a bad argument.
func ParseConfig(cfg map[string]any) (Config, error) {
	c := Config{Window: rtt.Range{Start: DefaultSearchStart, Size: DefaultSearchSize}}
	var err error
	if _, ok := cfg["rtt_address"]; ok {
		if c.Address, err = u32(cfg, "rtt_address"); err != nil {
			return c, err
		}
		c.HasAddress = true
	}
	if _, ok := cfg["rtt_search_start"]; ok {
		if c.Window.Start, err = u32(cfg, "rtt_search_start"); err != nil {
			return c, err
		}
	}
	if _, ok := cfg["rtt_search_size"]; ok {
		if c.Window.Size, err = u32(cfg, "rtt_search_size"); err != nil {
			return c, err
		}
		if c.Window.Size < 64 || uint64(c.Window.Start)+uint64(c.Window.Size) > 1<<32 {
			return c, fmt.Errorf("rtt_search_size %d: want 64 and a window inside the 32-bit address space", c.Window.Size)
		}
	}
	if c.Channel, err = optInt(cfg, "rtt_channel", 0, 0, 31); err != nil {
		return c, err
	}
	w, err := optInt(cfg, "rtt_wait_ms", 3000, 0, 600000)
	if err != nil {
		return c, err
	}
	lo, err := optInt(cfg, "rtt_poll_min_ms", 5, 1, 10000)
	if err != nil {
		return c, err
	}
	hi, err := optInt(cfg, "rtt_poll_max_ms", 100, 1, 10000)
	if err != nil {
		return c, err
	}
	if hi < lo {
		return c, fmt.Errorf("rtt_poll_max_ms %d is below rtt_poll_min_ms %d", hi, lo)
	}
	c.Wait, c.PollMin, c.PollMax = time.Duration(w)*time.Millisecond, time.Duration(lo)*time.Millisecond, time.Duration(hi)*time.Millisecond
	return c, nil
}

// Ranges is the window a control block may live in.
func (c Config) Ranges() []rtt.Range { return []rtt.Range{c.Window} }

// Locate finds and validates the control block.
func Locate(ctx context.Context, m rtt.Memory, c Config) (*rtt.ControlBlock, error) {
	addr := c.Address
	if !c.HasAddress {
		var err error
		if addr, err = rtt.Find(ctx, m, c.Ranges(), 1024); err != nil {
			return nil, err
		}
	}
	return rtt.Parse(ctx, m, addr, c.Ranges())
}

// IsMiss reports a missing or invalid control block (the firmware may not
// have built it yet), as opposed to a failed memory access.
func IsMiss(err error) bool {
	return err != nil && (strings.Contains(err.Error(), rtt.ErrNotFound.Error()) || strings.Contains(err.Error(), rtt.ErrCorrupt.Error()))
}

// Err turns an RTT failure into a status error; one that already carries a
// status code is returned unchanged.
func Err(err error) error {
	if s, ok := status.FromError(err); ok && s.Code() != codes.Unknown {
		return err
	}
	return status.Error(codes.FailedPrecondition, "rtt: "+err.Error())
}

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func bufferInfo(bs []rtt.Buffer) []any {
	out := make([]any, 0, len(bs))
	for _, b := range bs {
		out = append(out, map[string]any{"index": float64(b.Index), "name": b.Name, "size": float64(b.Size), "storage": hex32(b.Storage), "flags": float64(b.Flags)})
	}
	return out
}

// Info implements DebugProbe.rtt_info: it reports the control block and its
// channels without consuming data.
func Info(ctx context.Context, m rtt.Memory, a map[string]any) (map[string]any, error) {
	c, err := ParseConfig(a)
	if err != nil {
		return nil, argErr(err)
	}
	cb, err := Locate(ctx, m, c)
	if err != nil {
		return nil, Err(err)
	}
	return map[string]any{"control_block": hex32(cb.Addr), "up": bufferInfo(cb.Up), "down": bufferInfo(cb.Down),
		"search": map[string]any{"start": hex32(c.Window.Start), "size": float64(c.Window.Size), "explicit_address": c.HasAddress}}, nil
}

// Write implements DebugProbe.rtt_write: it writes text to a down buffer;
// bytes that do not fit are reported, never overwritten.
func Write(ctx context.Context, m rtt.Memory, a map[string]any) (map[string]any, error) {
	c, err := ParseConfig(a)
	if err != nil {
		return nil, argErr(err)
	}
	s, _ := a["data"].(string)
	if s == "" {
		return nil, argErr(fmt.Errorf("data: want non-empty text"))
	}
	if len(s) > maxWrite {
		return nil, argErr(fmt.Errorf("data is %d bytes; at most %d per call", len(s), maxWrite))
	}
	cb, err := Locate(ctx, m, c)
	if err != nil {
		return nil, Err(err)
	}
	n, err := rtt.New(m, cb).WriteDown(ctx, c.Channel, []byte(s))
	if err != nil {
		return nil, Err(err)
	}
	return map[string]any{"control_block": hex32(cb.Addr), "channel": float64(c.Channel), "written": float64(n), "requested": float64(len(s)), "dropped": float64(len(s) - n)}, nil
}

// ClassifyLine maps an RTT text line to a log level.
func ClassifyLine(s string) (level, msg string) {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "error") || strings.Contains(l, "<err>"):
		return "error", s
	case strings.Contains(l, "warn"):
		return "warn", s
	case strings.Contains(l, "debug") || strings.Contains(l, "<dbg>"):
		return "debug", s
	}
	return "info", s
}

// Target is the device side of a capture.
type Target interface {
	// Access runs fn with the device locked and the target attached and
	// accessible; m is valid during fn only. The lock is held for this call
	// only, so other device calls interleave between polls.
	Access(ctx context.Context, fn func(ctx context.Context, m rtt.Memory) error) error
	// Fail classifies a failure. When gone is set the device has disappeared
	// and the plugin has already forgotten it.
	Fail(err error) (classified error, gone bool)
	// Release ends the capture's hold on the device (closing one the capture
	// opened itself). It is called once.
	Release()
}

// Capture polls one up buffer through target memory and emits its lines.
type Capture struct {
	t      Target
	cfg    Config
	source string
	cb     *rtt.ControlBlock
	clock  *pluginsdk.Clock
	sink   *pluginsdk.Sink
	wg     sync.WaitGroup
	lost   bool
	cancel context.CancelFunc
	once   sync.Once

	lines, bytes, polls, relocs, lostPolls uint64
	err                                    error
	res                                    *pluginv1.StopCaptureResponse
}

// Start waits up to cfg.Wait for the control block and starts polling; source
// is the Log.source of the lines. When Start fails, t has been released.
func Start(ctx context.Context, t Target, cfg Config, source string, clock *pluginsdk.Clock, sink *pluginsdk.Sink) (*Capture, *pluginsdk.CaptureStart, error) {
	cctx, cancel := context.WithCancel(ctx)
	c := &Capture{t: t, cfg: cfg, source: source, clock: clock, sink: sink, cancel: cancel}
	cb, err := c.locateWait(cctx)
	if err != nil {
		cancel()
		c.release()
		err, _ = t.Fail(err)
		return nil, nil, Err(err)
	}
	if cfg.Channel >= len(cb.Up) {
		cancel()
		c.release()
		return nil, nil, status.Errorf(codes.InvalidArgument, "rtt_channel %d: the target has %d up buffers", cfg.Channel, len(cb.Up))
	}
	c.cb = cb
	c.wg.Add(1)
	go c.run(cctx)
	attrs := map[string]string{"source": source, "channel": fmt.Sprint(cfg.Channel), "control_block": hex32(cb.Addr), "up_buffers": fmt.Sprint(len(cb.Up)), "down_buffers": fmt.Sprint(len(cb.Down))}
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: Stream, Kind: "log", Attributes: attrs}}}, nil
}

func (c *Capture) release() { c.once.Do(c.t.Release) }

func (c *Capture) locate(ctx context.Context) (*rtt.ControlBlock, error) {
	var cb *rtt.ControlBlock
	err := c.t.Access(ctx, func(ctx context.Context, m rtt.Memory) error {
		var err error
		cb, err = Locate(ctx, m, c.cfg)
		return err
	})
	return cb, err
}

// locateWait retries until the firmware has initialised its control block.
func (c *Capture) locateWait(ctx context.Context) (*rtt.ControlBlock, error) {
	deadline := time.Now().Add(c.cfg.Wait)
	for {
		cb, err := c.locate(ctx)
		if err == nil {
			return cb, nil
		}
		if !IsMiss(err) || time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (c *Capture) emit(evs []*pluginv1.Event) {
	if len(evs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.sink.Emit(ctx, evs...); err != nil && c.err == nil {
		c.err = err
	}
}

// poll reads the new bytes within one Access.
func (c *Capture) poll(ctx context.Context) ([]byte, error) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var data []byte
	err := c.t.Access(pctx, func(ctx context.Context, m rtt.Memory) error {
		// The firmware may re-initialise (or, after a reset, not yet have
		// built) its control block: revalidate it now and then instead of
		// trusting a stale descriptor copy. While it is invalid nothing is read.
		if c.polls%16 == 0 || c.lost {
			cb, err := rtt.Parse(ctx, m, c.cb.Addr, c.cfg.Ranges())
			switch {
			case err == nil:
				if c.lost || !sameBuffers(cb, c.cb) {
					c.relocs++
				}
				c.cb, c.lost = cb, false
			case IsMiss(err):
				c.lost = true
				c.lostPolls++
				return nil
			default:
				return err
			}
		}
		var err error
		data, err = rtt.New(m, c.cb).ReadUp(ctx, c.cfg.Channel)
		return err
	})
	return data, err
}

func sameBuffers(a, b *rtt.ControlBlock) bool {
	if len(a.Up) != len(b.Up) || len(a.Down) != len(b.Down) {
		return false
	}
	for i := range a.Up {
		if a.Up[i].Storage != b.Up[i].Storage || a.Up[i].Size != b.Up[i].Size {
			return false
		}
	}
	for i := range a.Down {
		if a.Down[i].Storage != b.Down[i].Storage || a.Down[i].Size != b.Down[i].Size {
			return false
		}
	}
	return true
}

func (c *Capture) run(ctx context.Context) {
	defer c.wg.Done()
	var (
		pending []byte
		bo      = rtt.Backoff{Min: c.cfg.PollMin, Max: c.cfg.PollMax}
		got     bool
		fails   int
	)
	line := func(raw string, at int64) *pluginv1.Event {
		c.lines++
		level, msg := ClassifyLine(strings.TrimRight(raw, "\r"))
		return &pluginv1.Event{TimestampNs: at, StreamId: Stream, Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{
			Level: level, Source: c.source, Message: msg, Attributes: map[string]string{"channel": fmt.Sprint(c.cfg.Channel)}}}}
	}
	defer func() {
		if len(pending) > 0 {
			c.emit([]*pluginv1.Event{line(string(pending), c.clock.Now())})
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(bo.Next(got)):
		}
		data, err := c.poll(ctx)
		at := c.clock.Now()
		c.polls++
		got = len(data) > 0
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			err, gone := c.t.Fail(err)
			if gone {
				c.err = err
				return
			}
			if fails++; fails > 5 {
				c.err = fmt.Errorf("RTT polling failed %d times in a row: %w", fails, err)
				return
			}
			// A target restart re-initialises the control block: look again.
			if cb, lerr := c.locate(ctx); lerr == nil {
				c.cb, c.relocs = cb, c.relocs+1
			}
			continue
		}
		fails = 0
		if !got {
			continue
		}
		c.bytes += uint64(len(data))
		pending = append(pending, data...)
		var batch []*pluginv1.Event
		for {
			i := strings.IndexByte(string(pending), '\n')
			if i < 0 {
				break
			}
			batch = append(batch, line(string(pending[:i]), at))
			pending = pending[i+1:]
		}
		if len(pending) > maxLineLen { // a stream without newlines must not grow unbounded
			batch = append(batch, line(string(pending), at))
			pending = nil
		}
		c.emit(batch)
	}
}

// Wait implements pluginsdk.Capture.
func (c *Capture) Wait() error {
	c.wg.Wait()
	c.cancel()
	c.release()
	c.res = &pluginv1.StopCaptureResponse{
		Stats: &pluginv1.CaptureStats{Events: c.lines},
		Clock: &pluginv1.ClockInfo{
			Source:          "host clock when the poll that read the bytes returned",
			ResolutionNs:    1_000_000,
			ErrorMinBounded: true,
			Note: fmt.Sprintf("the stamp is late by the unknown time between the target writing and the next poll (poll interval %s..%s plus USB), never early; "+
				"lines are not stamped by the target; %d lines, %d bytes, %d polls, control block re-read %d times, invalid for %d polls", c.cfg.PollMin, c.cfg.PollMax, c.lines, c.bytes, c.polls, c.relocs, c.lostPolls),
		},
	}
	return c.err
}

// Result implements pluginsdk.Capture.
func (c *Capture) Result() *pluginv1.StopCaptureResponse { return c.res }

func hex32(v uint32) string { return fmt.Sprintf("0x%08x", v) }

func optInt(a map[string]any, key string, def, lo, hi int) (int, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return def, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("%s: want a number, got %v", key, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < float64(lo) || f > float64(hi) {
		return 0, fmt.Errorf("%s %v: want an integer %d..%d", key, f, lo, hi)
	}
	return int(f), nil
}

// u32 reads a 32-bit unsigned value given as a number or a "0x.." string.
func u32(a map[string]any, key string) (uint32, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, fmt.Errorf("%s is required", key)
	}
	switch x := v.(type) {
	case float64:
		if x < 0 || x > math.MaxUint32 || x != math.Trunc(x) {
			return 0, fmt.Errorf("%s %v: want an integer 0..4294967295", key, x)
		}
		return uint32(x), nil
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(x), 0, 32)
		if err != nil {
			return 0, fmt.Errorf("%s %q: want a 32-bit number such as 0x20000000", key, x)
		}
		return uint32(n), nil
	}
	return 0, fmt.Errorf("%s: want a number or a 0x.. string, got %v", key, v)
}
