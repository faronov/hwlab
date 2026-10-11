package modbusplugin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/modbus"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Stream ids of a polling capture.
const (
	StreamRequest     = "request"
	StreamResponse    = "response"
	StreamTransaction = "transaction"
)

const (
	minInterval = 10 * time.Millisecond
	maxPolls    = 32
	emitTimeout = 5 * time.Second
)

type poll struct {
	name     string
	req      *modbus.Request
	interval time.Duration
	as, wo   string
	next     time.Time
}

// parsePolls reads the polls setting. Only read functions are accepted: a
// capture repeats its requests until it ends, and repeating a write would be a
// state change nobody asked for.
func parsePolls(cfg map[string]any, d *device) ([]*poll, error) {
	if err := checkKeys(cfg); err != nil {
		return nil, err
	}
	l, err := listArg(cfg, "polls")
	if err != nil {
		return nil, errors.New(`a capture needs polls: [{function: read_holding_registers, address: 0, quantity: 2, interval_ms: 500}]`)
	}
	if len(l) > maxPolls {
		return nil, fmt.Errorf("polls: at most %d", maxPolls)
	}
	var out []*poll
	for i, e := range l {
		a, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("polls[%d]: want an object", i)
		}
		fn, err := strArg(a, "function")
		if err != nil {
			return nil, fmt.Errorf("polls[%d]: %w", i, err)
		}
		if fn == "" {
			fn = "read_holding_registers"
		}
		if fc, ok := modbus.FunctionByName(fn); !ok || modbus.IsWrite(fc) {
			return nil, fmt.Errorf("polls[%d]: function %q: only read_coils, read_discrete_inputs, read_holding_registers and read_input_registers can be polled", i, fn)
		}
		req, err := buildRequest(fn, a, d)
		if err != nil {
			return nil, fmt.Errorf("polls[%d]: %w", i, err)
		}
		def := 1000
		ms, err := intArg(a, "interval_ms", int(minInterval/time.Millisecond), 3_600_000, &def)
		if err != nil {
			return nil, fmt.Errorf("polls[%d]: %w", i, err)
		}
		name, err := strArg(a, "name")
		if err != nil {
			return nil, fmt.Errorf("polls[%d]: %w", i, err)
		}
		if name == "" {
			name = fmt.Sprintf("%s@%d", fn, req.Address)
		}
		as, _ := strArg(a, "as")
		wo, _ := strArg(a, "word_order")
		if as != "" || wo != "" {
			if req.Function != modbus.FCReadHoldingRegisters && req.Function != modbus.FCReadInputRegisters {
				return nil, fmt.Errorf("polls[%d]: as/word_order apply to register reads only", i)
			}
			if _, err := modbus.Interpret(make([]uint16, int(req.Quantity)), as, wo); err != nil {
				return nil, fmt.Errorf("polls[%d]: %w", i, err)
			}
		}
		out = append(out, &poll{name: name, req: req, interval: time.Duration(ms) * time.Millisecond, as: as, wo: wo})
	}
	return out, nil
}

type capture struct {
	dev     *device
	release func()
	req     pluginsdk.CaptureRequest
	polls   []*poll
	sink    *pluginsdk.Sink
	synth   bool
	done    chan struct{}

	mu       sync.Mutex
	err      error
	events   uint64
	txns     uint64
	failures uint64
	rttMax   time.Duration
}

func (c *capture) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

// StartCapture implements pluginsdk.Plugin.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	d, release, err := p.lease(req.DeviceID, req.Config)
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
		release()
		return nil, nil, e
	}
	polls, err := parsePolls(req.Config, d)
	if err != nil {
		return fail(argErr(err))
	}
	d.mu.Lock()
	if d.cap != nil {
		d.mu.Unlock()
		return fail(status.Errorf(codes.FailedPrecondition, "%s already has a running capture", req.DeviceID))
	}
	c := &capture{dev: d, release: release, req: req, polls: polls, sink: sink, synth: d.entry.Fake != nil, done: make(chan struct{})}
	d.cap = c
	d.mu.Unlock()

	go c.run(ctx)

	attrs := map[string]string{"transport": d.cl.T.Kind(), "endpoint": d.cl.T.Describe()}
	if c.synth {
		attrs["synthetic"] = "true"
	}
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{
		{StreamId: StreamRequest, Kind: "packet", Attributes: attrs},
		{StreamId: StreamResponse, Kind: "packet", Attributes: attrs},
		{StreamId: StreamTransaction, Kind: "decoded", Attributes: attrs},
	}}, nil
}

func (c *capture) run(ctx context.Context) {
	defer close(c.done)
	defer func() {
		c.dev.mu.Lock()
		if c.dev.cap == c {
			c.dev.cap = nil
		}
		c.dev.mu.Unlock()
		c.release()
	}()
	now := time.Now()
	for _, p := range c.polls {
		p.next = now
	}
	for ctx.Err() == nil {
		next := c.polls[0]
		for _, p := range c.polls[1:] {
			if p.next.Before(next.next) {
				next = p
			}
		}
		if wait := time.Until(next.next); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		c.once(ctx, next)
		next.next = next.next.Add(next.interval)
		if now := time.Now(); next.next.Before(now) {
			next.next = now.Add(next.interval) // do not burst to catch up
		}
	}
}

func (c *capture) emit(ctx context.Context, evs ...*pluginv1.Event) bool {
	ectx, cancel := context.WithTimeout(ctx, emitTimeout)
	defer cancel()
	if err := c.sink.Emit(ectx, evs...); err != nil {
		if ctx.Err() == nil {
			c.fail(fmt.Errorf("emit: %w", err))
		}
		return false
	}
	c.mu.Lock()
	c.events += uint64(len(evs))
	c.mu.Unlock()
	return true
}

func (c *capture) once(ctx context.Context, p *poll) {
	res, err := c.dev.cl.Do(ctx, p.req)
	if ctx.Err() != nil && err != nil {
		return // cut off by the end of the capture: not a bus event
	}
	proto := "modbus-" + c.dev.cl.T.Kind()
	attrs := map[string]string{"poll": p.name, "unit": fmt.Sprint(p.req.Unit), "function": modbus.FunctionName(p.req.Function)}
	pkt := func(stream string, at time.Time, data []byte, summary string) *pluginv1.Event {
		return &pluginv1.Event{TimestampNs: c.req.Clock.At(at), StreamId: stream, Payload: &pluginv1.Event_Packet{Packet: &pluginv1.Packet{
			Protocol: proto, Data: append([]byte(nil), data...), Summary: summary, Attributes: attrs}}}
	}

	fields := map[string]any{"poll": p.name, "unit": int(p.req.Unit), "function": modbus.FunctionName(p.req.Function), "request": p.req.Fields(), "attempts": 0}
	var evs []*pluginv1.Event
	var dur time.Duration
	state := "ok"
	summary := p.req.Summary()
	start := time.Now()
	if res != nil && res.Exchange != nil {
		ex := res.Exchange
		start = ex.Sent
		fields["attempts"] = res.Attempts
		evs = append(evs, pkt(StreamRequest, ex.Sent, ex.ReqADU, p.req.Summary()))
		if len(ex.RespADU) > 0 {
			dur = ex.Received.Sub(ex.Sent)
			rs := "response"
			if res.Response != nil {
				rs = res.Response.Summary()
			}
			evs = append(evs, pkt(StreamResponse, ex.Received, ex.RespADU, rs))
		}
	}
	if err == nil {
		if res.Response != nil {
			fields["response"] = res.Response.Fields()
			summary = res.Response.Summary()
			if (p.as != "" || p.wo != "") && len(res.Response.Registers) > 0 {
				if vals, ierr := modbus.Interpret(res.Response.Registers, p.as, p.wo); ierr == nil {
					fields["values"] = vals
				}
			}
		}
	} else {
		var ee *modbus.ExceptionError
		switch {
		case errors.As(err, &ee):
			state = "exception"
			if res != nil && res.Response != nil {
				fields["response"] = res.Response.Fields()
			}
		case errors.Is(err, modbus.ErrTimeout):
			state = "timeout"
		case errors.Is(err, modbus.ErrCRC):
			state = "crc_error"
		default:
			state = "error"
		}
		fields["error"] = err.Error()
		summary = fmt.Sprintf("%s: %s", p.req.Summary(), state)
	}
	fields["status"] = state
	fields["rtt_us"] = dur.Microseconds()
	sf, serr := structpb.NewStruct(fields)
	if serr != nil {
		c.fail(serr)
		return
	}
	evs = append(evs, &pluginv1.Event{TimestampNs: c.req.Clock.At(start), StreamId: StreamTransaction, Payload: &pluginv1.Event_Decoded{Decoded: &pluginv1.DecodedProtocolEvent{
		Protocol: "modbus", Summary: summary, Fields: sf, DurationNs: dur.Nanoseconds()}}})
	if !c.emit(ctx, evs...) {
		return
	}
	c.mu.Lock()
	c.txns++
	if state != "ok" {
		c.failures++
	}
	if dur > c.rttMax {
		c.rttMax = dur
	}
	c.mu.Unlock()
}

// Wait implements pluginsdk.Capture.
func (c *capture) Wait() error {
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Result implements pluginsdk.Capture.
func (c *capture) Result() *pluginv1.StopCaptureResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &pluginv1.StopCaptureResponse{
		Stats: &pluginv1.CaptureStats{Events: c.events},
		Clock: c.clockInfo(),
	}
}

// clockInfo is deliberately modest: every stamp is a host time. The request is
// stamped just before the write call and the response when its last byte was
// read, so a latency is an upper bound on the wire time, inflated by the OS and
// adapter buffering, which was not measured.
func (c *capture) clockInfo() *pluginv1.ClockInfo {
	ci := &pluginv1.ClockInfo{
		Source:       "host clock: request before the write call, response when its last byte was read",
		ResolutionNs: 1000,
	}
	if c.synth {
		ci.Source = "host clock (synthetic slave on an in-memory link: no real bus)"
		ci.Note = "SYNTHETIC; times only order the events"
		return ci
	}
	ci.Note = fmt.Sprintf("transactions seen: %d, failed: %d, slowest round trip %s; USB-serial adapters add latency (often 1-16 ms) that was not measured, so round trips are not wire times",
		c.txns, c.failures, c.rttMax.Round(time.Microsecond))
	return ci
}
