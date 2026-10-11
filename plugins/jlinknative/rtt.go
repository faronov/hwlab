package jlinknativeplugin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/rtt"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// RTTLogSource is the Log.source of native RTT lines in the timeline.
const RTTLogSource = "jlink-native-rtt"

const (
	rttStream     = "rtt"
	rttMaxLineLen = 64 << 10
	// The default search window is the first 32 KiB of SRAM. A read past the
	// end of RAM faults, so the window is never widened silently; set
	// rtt_search_size for a larger part.
	defaultSearchStart = 0x20000000
	defaultSearchSize  = 0x8000
)

// rttMem is target memory of one device. Every call must hold d.mu with the
// target attached.
type rttMem struct{ d *device }

func (m rttMem) Read(_ context.Context, a uint32, n int) ([]byte, error) {
	return m.d.tgt.ReadMem(a, n)
}
func (m rttMem) Write(_ context.Context, a uint32, b []byte) error { return m.d.tgt.WriteMem(a, b) }

type rttConfig struct {
	address     uint32
	hasAddress  bool
	window      rtt.Range
	channel     int
	wait        time.Duration
	pollMin     time.Duration
	pollMax     time.Duration
	callOptions map[string]any
}

func parseRTTConfig(cfg map[string]any) (rttConfig, error) {
	c := rttConfig{window: rtt.Range{Start: defaultSearchStart, Size: defaultSearchSize}}
	var err error
	if _, ok := cfg["rtt_address"]; ok {
		if c.address, err = u32(cfg, "rtt_address"); err != nil {
			return c, err
		}
		c.hasAddress = true
	}
	if _, ok := cfg["rtt_search_start"]; ok {
		if c.window.Start, err = u32(cfg, "rtt_search_start"); err != nil {
			return c, err
		}
	}
	if _, ok := cfg["rtt_search_size"]; ok {
		if c.window.Size, err = u32(cfg, "rtt_search_size"); err != nil {
			return c, err
		}
		if c.window.Size < 64 || uint64(c.window.Start)+uint64(c.window.Size) > 1<<32 {
			return c, fmt.Errorf("rtt_search_size %d: want 64 and a window inside the 32-bit address space", c.window.Size)
		}
	}
	if c.channel, err = optInt(cfg, "rtt_channel", 0, 0, 31); err != nil {
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
	c.wait, c.pollMin, c.pollMax = time.Duration(w)*time.Millisecond, time.Duration(lo)*time.Millisecond, time.Duration(hi)*time.Millisecond
	c.callOptions = pick(cfg, callKeys)
	return c, nil
}

func (c rttConfig) ranges() []rtt.Range { return []rtt.Range{c.window} }

// keepAttachment prefers the options of an existing attachment: re-attaching
// with different ones would drop the breakpoints and watchpoints of the
// device, and RTT polling must never do that.
func (d *device) keepAttachment(o options) options {
	if d.tgt != nil {
		return d.attached
	}
	return o
}

// locate finds and validates the control block. The caller holds d.mu.
func (d *device) locate(ctx context.Context, c rttConfig, o options) (*rtt.ControlBlock, error) {
	if err := d.attach(ctx, d.keepAttachment(o)); err != nil {
		return nil, err
	}
	d.link.SetContext(ctx)
	m := rttMem{d}
	addr := c.address
	if !c.hasAddress {
		var err error
		if addr, err = rtt.Find(ctx, m, c.ranges(), 1024); err != nil {
			return nil, err
		}
	}
	return rtt.Parse(ctx, m, addr, c.ranges())
}

// locateWait retries until the firmware has initialised its control block.
func (d *device) locateWait(ctx context.Context, c rttConfig, o options) (*rtt.ControlBlock, error) {
	deadline := time.Now().Add(c.wait)
	for {
		d.mu.Lock()
		cb, err := d.locate(ctx, c, o)
		d.mu.Unlock()
		if err == nil {
			return cb, nil
		}
		if !errorsIsRTTMiss(err) || time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func errorsIsRTTMiss(err error) bool {
	return strings.Contains(err.Error(), rtt.ErrNotFound.Error()) || strings.Contains(err.Error(), rtt.ErrCorrupt.Error())
}

func bufferInfo(bs []rtt.Buffer) []any {
	out := make([]any, 0, len(bs))
	for _, b := range bs {
		out = append(out, map[string]any{"index": float64(b.Index), "name": b.Name, "size": float64(b.Size), "storage": hex32(b.Storage), "flags": float64(b.Flags)})
	}
	return out
}

// rttInfo reports the control block and its channels without consuming data.
func (d *device) rttInfo(ctx context.Context, a map[string]any, o options) (map[string]any, error) {
	c, err := parseRTTConfig(a)
	if err != nil {
		return nil, argErr(err)
	}
	cb, err := d.locate(ctx, c, o)
	if err != nil {
		return nil, rttErr(err)
	}
	return map[string]any{"control_block": hex32(cb.Addr), "up": bufferInfo(cb.Up), "down": bufferInfo(cb.Down),
		"search": map[string]any{"start": hex32(c.window.Start), "size": float64(c.window.Size), "explicit_address": c.hasAddress}}, nil
}

// rttWrite writes text to a down buffer; bytes that do not fit are reported,
// never overwritten.
func (d *device) rttWrite(ctx context.Context, a map[string]any, o options) (map[string]any, error) {
	c, err := parseRTTConfig(a)
	if err != nil {
		return nil, argErr(err)
	}
	s, _ := a["data"].(string)
	if s == "" {
		return nil, argErr(fmt.Errorf("data: want non-empty text"))
	}
	if len(s) > 4096 {
		return nil, argErr(fmt.Errorf("data is %d bytes; at most 4096 per call", len(s)))
	}
	cb, err := d.locate(ctx, c, o)
	if err != nil {
		return nil, rttErr(err)
	}
	n, err := rtt.New(rttMem{d}, cb).WriteDown(ctx, c.channel, []byte(s))
	if err != nil {
		return nil, rttErr(err)
	}
	return map[string]any{"control_block": hex32(cb.Addr), "channel": float64(c.channel), "written": float64(n), "requested": float64(len(s)), "dropped": float64(len(s) - n)}, nil
}

func rttErr(err error) error {
	if s, ok := status.FromError(err); ok && s.Code() != codes.Unknown {
		return err
	}
	return status.Error(codes.FailedPrecondition, "rtt: "+err.Error())
}

// rttCapture polls one up buffer through target memory and emits its lines.
type rttCapture struct {
	p      *Plugin
	d      *device
	cfg    rttConfig
	o      options
	cb     *rtt.ControlBlock
	clock  *pluginsdk.Clock
	sink   *pluginsdk.Sink
	wg     sync.WaitGroup
	owned  bool
	lost   bool
	cancel context.CancelFunc

	lines, bytes, polls, relocs, lostPolls uint64
	err                                    error
	res                                    *pluginv1.StopCaptureResponse
}

// StartCapture implements pluginsdk.Plugin: RTT up-buffer output as Log events.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	cfg, err := parseRTTConfig(req.Config)
	if err != nil {
		return nil, nil, argErr(err)
	}
	o, err := parseOptions(cfg.callOptions)
	if err != nil {
		return nil, nil, argErr(err)
	}
	p.mu.Lock()
	d := p.devs[req.DeviceID]
	p.mu.Unlock()
	owned := false
	if d == nil {
		e, err := p.find(req.DeviceID)
		if err != nil {
			return nil, nil, err
		}
		if d, err = p.connect(e, o); err != nil {
			return nil, nil, err
		}
		p.mu.Lock()
		p.devs[d.id] = d
		p.mu.Unlock()
		owned = true
	} else if oo, err := parseOptionsOver(d.o, cfg.callOptions); err == nil {
		o = oo
	}
	cctx, cancel := context.WithCancel(ctx)
	cb, err := d.locateWait(cctx, cfg, o)
	c := &rttCapture{p: p, d: d, cfg: cfg, o: o, cb: cb, clock: req.Clock, sink: sink, owned: owned, cancel: cancel}
	if err != nil {
		cancel()
		c.release()
		err = classify(err)
		if IsDeviceGone(err) {
			p.drop(d)
		}
		return nil, nil, rttErr(err)
	}
	if cfg.channel >= len(cb.Up) {
		cancel()
		c.release()
		return nil, nil, status.Errorf(codes.InvalidArgument, "rtt_channel %d: the target has %d up buffers", cfg.channel, len(cb.Up))
	}
	c.wg.Add(1)
	go c.run(cctx)
	attrs := map[string]string{"source": RTTLogSource, "channel": fmt.Sprint(cfg.channel), "control_block": hex32(cb.Addr), "up_buffers": fmt.Sprint(len(cb.Up)), "down_buffers": fmt.Sprint(len(cb.Down))}
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: rttStream, Kind: "log", Attributes: attrs}}}, nil
}

// release closes a device this capture opened itself.
func (c *rttCapture) release() {
	if !c.owned {
		return
	}
	c.owned = false
	c.p.mu.Lock()
	if c.p.devs[c.d.id] == c.d {
		delete(c.p.devs, c.d.id)
	}
	c.p.mu.Unlock()
	_ = c.d.close()
}

func (c *rttCapture) emit(evs []*pluginv1.Event) {
	if len(evs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.sink.Emit(ctx, evs...); err != nil && c.err == nil {
		c.err = err
	}
}

// poll reads the new bytes. The device lock is held for one poll only, so
// other calls interleave between polls.
func (c *rttCapture) poll(ctx context.Context) ([]byte, error) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	if err := c.d.attach(pctx, c.d.keepAttachment(c.o)); err != nil {
		return nil, err
	}
	c.d.link.SetContext(pctx)
	// The firmware may re-initialise (or, after a reset, not yet have built)
	// its control block: revalidate it now and then instead of trusting a
	// stale descriptor copy. While it is invalid nothing is read.
	if c.polls%16 == 0 || c.lost {
		cb, err := rtt.Parse(pctx, rttMem{c.d}, c.cb.Addr, c.cfg.ranges())
		switch {
		case err == nil:
			if c.lost || !sameBuffers(cb, c.cb) {
				c.relocs++
			}
			c.cb, c.lost = cb, false
		case errorsIsRTTMiss(err):
			c.lost = true
			c.lostPolls++
			return nil, nil
		default:
			return nil, err
		}
	}
	return rtt.New(rttMem{c.d}, c.cb).ReadUp(pctx, c.cfg.channel)
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

func (c *rttCapture) run(ctx context.Context) {
	defer c.wg.Done()
	var (
		pending []byte
		bo      = rtt.Backoff{Min: c.cfg.pollMin, Max: c.cfg.pollMax}
		got     bool
		fails   int
	)
	line := func(raw string, at int64) *pluginv1.Event {
		c.lines++
		level, msg := classifyRTTLine(strings.TrimRight(raw, "\r"))
		return &pluginv1.Event{TimestampNs: at, StreamId: rttStream, Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{
			Level: level, Source: RTTLogSource, Message: msg, Attributes: map[string]string{"channel": fmt.Sprint(c.cfg.channel)}}}}
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
			err = classify(err)
			if IsDeviceGone(err) {
				c.err = err
				c.p.drop(c.d)
				return
			}
			if fails++; fails > 5 {
				c.err = fmt.Errorf("RTT polling failed %d times in a row: %w", fails, err)
				return
			}
			// A target restart re-initialises the control block: look again.
			c.d.mu.Lock()
			cb, lerr := c.d.locate(ctx, c.cfg, c.o)
			c.d.mu.Unlock()
			if lerr == nil {
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
		if len(pending) > rttMaxLineLen { // a stream without newlines must not grow unbounded
			batch = append(batch, line(string(pending), at))
			pending = nil
		}
		c.emit(batch)
	}
}

func classifyRTTLine(s string) (level, msg string) {
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

// Wait implements pluginsdk.Capture.
func (c *rttCapture) Wait() error {
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
				"lines are not stamped by the target; %d lines, %d bytes, %d polls, control block re-read %d times, invalid for %d polls", c.cfg.pollMin, c.cfg.pollMax, c.lines, c.bytes, c.polls, c.relocs, c.lostPolls),
		},
	}
	return c.err
}

// Result implements pluginsdk.Capture.
func (c *rttCapture) Result() *pluginv1.StopCaptureResponse { return c.res }
