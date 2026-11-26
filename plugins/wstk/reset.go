package wstk

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	jproto "github.com/faronov/hwlab/plugins/jlinknative/protocol"
	"github.com/faronov/hwlab/plugins/wstk/protocol"
)

// Target reset through the kit's J-Link.
//
// The kit's J-Link USB interface can be claimed by one process at a time, and
// a USB AEM capture keeps it for its whole duration, so no debugger (jlink,
// jlinknative, Simplicity Commander) can reach the target meanwhile. The reset
// is therefore offered here, on the same USB connection: the J-Link command
// set is request/reply, and EMUCOM reads of the AEM loop and the probe's
// nRESET commands are serialised per operation, never interleaved inside one.
// A reset during a capture therefore needs no second process, does not stop
// the AEM stream, and shows on the capture's timeline.
//
// Only the nRESET line ("hardware" reset) is driven: no SWD attach, so the
// target is not left with debug power-up requests or C_DEBUGEN set that would
// change the very current being measured.
const (
	resetKindHardware = "hardware"
	defaultHoldMs     = 20
	maxHoldMs         = 1000
	// evidenceWait bounds the wait for AEM samples covering the reset.
	evidenceWait = 2 * time.Second
	// recentSpan is how much AEM history a capture keeps for reset evidence.
	recentSpan = 3 * time.Second
	// resetSearch is how long after the release the drop is looked for.
	resetSearch = int64(250 * time.Millisecond)
)

var resetSchema = `{"type":"object","properties":{` +
	`"kind":{"enum":["hardware"],"description":"hardware (default): pulse the kit J-Link's nRESET line"},` +
	`"hold_ms":{"type":"number","description":"how long nRESET is held low, 1..1000 ms (default 20)"}}}`

func resetSpec() *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: pluginv1.CapDebugProbe, Name: "reset", Danger: pluginv1.Danger_DANGER_CONFIRM,
		ArgsSchemaJson: resetSchema,
		Description: "USB kits: reset the radio board by pulsing the kit J-Link's nRESET line (hold_ms, default 20). Works while an AEM " +
			"capture of this kit runs, through the capture's own J-Link connection (the probe is exclusive to one process): the AEM " +
			"stream continues, the reset is marked on the timeline and checked against the current (reset_observed). " +
			"Without a capture (and on kits without an AEM, such as Explorer Kits) the kit's J-Link is opened briefly and the reset " +
			"cannot be observed. No SWD attach is made."}
}

func parseReset(args map[string]any) (int, error) {
	if k, ok := args["kind"]; ok && k != nil && k != resetKindHardware {
		return 0, fmt.Errorf("kind %v: only %q (the kit J-Link's nRESET line) is offered by wstk; for an AIRCR system reset use the "+
			"jlink or jlinknative plugin when no AEM capture holds the probe", k, resetKindHardware)
	}
	hold := defaultHoldMs
	if v, ok := args["hold_ms"]; ok && v != nil {
		n, ok := toInt(v)
		if !ok || n < 1 || n > maxHoldMs {
			return 0, fmt.Errorf("hold_ms must be an integer in [1,%d]", maxHoldMs)
		}
		hold = n
	}
	return hold, nil
}

// pulse holds nRESET low for hold and releases it. lock serialises each
// command with other users of the link. The release is sent even when ctx
// ends or the assert failed, and retried: a target must not stay in reset.
// asserted is taken after the assert command completed and releasing before
// the release is sent, so nRESET was low for all of [asserted, releasing].
func pulse(ctx context.Context, l protocol.Link, lock func() func(), hold time.Duration) (asserted, releasing time.Time, err error) {
	cmd := func(ctx context.Context, c byte) error {
		unlock := lock()
		defer unlock()
		_, err := l.Exchange(ctx, []byte{c}, 0)
		return err
	}
	err = cmd(ctx, jproto.CmdResetLow)
	asserted = time.Now()
	if err == nil {
		_ = sleepCtx(ctx, hold)
	}
	releasing = time.Now()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var rerr error
	for range 3 {
		if rerr = cmd(rctx, jproto.CmdResetHigh); rerr == nil {
			break
		}
	}
	if err != nil {
		return asserted, releasing, fmt.Errorf("assert nRESET: %w", err)
	}
	if rerr != nil {
		return asserted, releasing, fmt.Errorf("release nRESET (the target may still be held in reset; replug the kit or press its reset button): %w", rerr)
	}
	return asserted, releasing, nil
}

func (p *Plugin) resetUSB(ctx context.Context, t target, args map[string]any) (map[string]any, error) {
	holdMs, err := parseReset(args)
	if err != nil {
		return nil, invalidArg(err)
	}
	hold := time.Duration(holdMs) * time.Millisecond
	if c := p.runningAEM(t.id); c != nil && c.use() {
		defer c.done1()
		return c.reset(ctx, hold)
	}
	if err := p.markBusy(t.id); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%v; a reset during a capture must go to the plugin process running it "+
			"(an experiment step, or device_invoke on the same MCP server)", err)
	}
	defer p.clearBusy(t.id)
	e, release, err := p.openKit(t)
	if err != nil {
		return nil, err
	}
	defer release()
	_, _, err = pulse(ctx, e.L, func() func() { return func() {} }, hold)
	if err != nil {
		return nil, err
	}
	note := "nRESET was pulsed but not observed: no AEM capture of this kit was running. During a capture the reset is " +
		"checked against the measured current."
	if aem, _ := p.aemOf(t.id); aem == aemNo {
		note = "nRESET was pulsed but not observed: this kit has no AEM to see it."
	}
	return map[string]any{"kind": resetKindHardware, "hold_ms": float64(holdMs), "during_capture": false, "reset_observed": false,
		"note": note}, nil
}

// sampleSpan is one emitted AEM batch kept for reset evidence.
type sampleSpan struct {
	startNs int64
	mA      []float32
}

func (c *aemCapture) remember(startNs int64, mA []float32) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	c.recent = append(c.recent, sampleSpan{startNs, mA})
	for len(c.recent) > 0 && startNs-c.recent[0].startNs > int64(recentSpan) {
		c.recent = c.recent[1:]
	}
}

func windowStats(spans []sampleSpan, from, to int64) (mean, lo float64, n int) {
	sum := 0.0
	for _, s := range spans {
		for i, v := range s.mA {
			ts := s.startNs + int64(i)*samplePeriodNs
			if ts < from || ts >= to {
				continue
			}
			if n == 0 || float64(v) < lo {
				lo = float64(v)
			}
			sum += float64(v)
			n++
		}
	}
	if n > 0 {
		mean = sum / float64(n)
	}
	return mean, lo, n
}

func (c *aemCapture) coveredUntil() int64 {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if len(c.recent) == 0 {
		return -1 << 62
	}
	l := c.recent[len(c.recent)-1]
	return l.startNs + int64(len(l.mA))*samplePeriodNs
}

// resetEvidence is what the AEM current shows around a reset.
type resetEvidence struct {
	BeforemA, DuringmA, MinmA float64
	NBefore, NDuring          int
	Observed                  bool
	DropNs                    int64 // session time of the first low sample
	// RecoveryNs is how long after the drop the current was back above
	// half its level before the reset: the hold plus the start of the boot
	// (0: not within the search window).
	RecoveryNs int64
}

// findReset looks for the reset in the AEM current: while a Cortex-M device
// is held in reset its supply current falls well below that of running
// firmware. The edge is searched from the assert time on rather than at it,
// because the AEM samples are not stamped exactly (they were measured late by
// some tens of milliseconds on a BRD4001A); a drop must last at least half
// the hold (up to 5 ms) to count. A device that was already asleep shows no
// clear drop, which is reported as not observed rather than guessed.
func findReset(spans []sampleSpan, aNs, rNs int64, hold time.Duration, searchNs int64) resetEvidence {
	var ev resetEvidence
	ev.BeforemA, _, ev.NBefore = windowStats(spans, aNs-int64(200*time.Millisecond), aNs-int64(2*time.Millisecond))
	if ev.NBefore < 10 || ev.BeforemA < 0.1 {
		return ev
	}
	thr := 0.5 * ev.BeforemA
	need := int(min(hold/2, 5*time.Millisecond) / samplePeriodNs)
	need = max(need, 3)
	type sample struct {
		ts int64
		v  float64
	}
	var xs []sample
	for _, s := range spans {
		for i, v := range s.mA {
			if ts := s.startNs + int64(i)*samplePeriodNs; ts >= aNs-int64(2*time.Millisecond) && ts < rNs+searchNs {
				xs = append(xs, sample{ts, float64(v)})
			}
		}
	}
	run := 0
	for i, x := range xs {
		if x.v >= thr {
			run = 0
			continue
		}
		run++
		if run < need {
			continue
		}
		start := i - run + 1
		ev.Observed, ev.DropNs = true, xs[start].ts
		end := xs[start].ts + int64(hold)
		sum := 0.0
		for j := start; j < len(xs); j++ {
			if xs[j].v >= thr {
				ev.RecoveryNs = xs[j].ts - ev.DropNs
				break
			}
			if xs[j].ts < end {
				if ev.NDuring == 0 || xs[j].v < ev.MinmA {
					ev.MinmA = xs[j].v
				}
				sum += xs[j].v
				ev.NDuring++
			}
		}
		ev.DuringmA = sum / float64(ev.NDuring)
		return ev
	}
	return ev
}

// use registers a user of the capture's connection; false once the capture
// has ended (the connection is about to be released).
func (c *aemCapture) use() bool {
	c.umu.Lock()
	defer c.umu.Unlock()
	if c.ended {
		return false
	}
	c.users.Add(1)
	return true
}

func (c *aemCapture) done1() { c.users.Done() }

// endUse refuses new users and waits for the current ones, so the connection
// is never released under a reset in progress.
func (c *aemCapture) endUse() {
	c.umu.Lock()
	c.ended = true
	c.umu.Unlock()
	c.users.Wait()
}

func (c *aemCapture) lockIO() func() {
	c.io.Lock()
	return c.io.Unlock
}

func (c *aemCapture) reset(ctx context.Context, hold time.Duration) (map[string]any, error) {
	asserted, releasing, err := pulse(ctx, c.e.L, c.lockIO, hold)
	aNs, rNs := c.clock.At(asserted), c.clock.At(releasing)
	holdMs := float64(hold / time.Millisecond)
	c.emitMarker(aNs, "target_reset", fmt.Sprintf("wstk: nRESET asserted (hold %v)", hold))
	c.emitMarker(rNs, "target_reset_release", "wstk: nRESET released")
	if err != nil {
		return nil, err
	}
	res := map[string]any{"kind": resetKindHardware, "hold_ms": holdMs, "during_capture": true,
		"asserted_ns": float64(aNs), "released_ns": float64(rNs)}
	// Wait for the samples that cover the hold, the AEM lag and the
	// recovery, then look for the drop.
	deadline := time.Now().Add(evidenceWait)
	for c.coveredUntil() < rNs+resetSearch && time.Now().Before(deadline) {
		if sleepCtx(ctx, 10*time.Millisecond) != nil {
			break
		}
	}
	c.rmu.Lock()
	ev := findReset(c.recent, aNs, rNs, hold, resetSearch)
	c.rmu.Unlock()
	res["reset_observed"] = ev.Observed
	res["current_before_ma"], res["samples_before"] = ev.BeforemA, float64(ev.NBefore)
	if ev.Observed {
		res["current_during_ma"], res["current_min_during_ma"], res["samples_during"] = ev.DuringmA, ev.MinmA, float64(ev.NDuring)
		res["drop_ns"] = float64(ev.DropNs)
		// How much later the drop is stamped than the assert command
		// returned: the AEM stamping lag (the pin fell during the command, so
		// this is at most about a millisecond short).
		res["aem_lag_ms"] = float64(ev.DropNs-aNs) / 1e6
		if ev.RecoveryNs > 0 {
			res["recovery_ms"] = float64(ev.RecoveryNs) / 1e6
		}
	} else {
		res["note"] = "no drop of the AEM current followed the reset: nRESET may not reach the target, or the target already drew too little current to tell"
	}
	return res, nil
}

func (c *aemCapture) emitMarker(ts int64, name, desc string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if c.sink.Emit(ctx, &pluginv1.Event{TimestampNs: ts, StreamId: powerStream,
		Payload: &pluginv1.Event_Marker{Marker: &pluginv1.Marker{Name: name, Description: desc}}}) == nil {
		c.events.Add(1)
	}
}

// runningAEM returns the USB AEM capture of a kit, or nil.
func (p *Plugin) runningAEM(id string) *aemCapture {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.aem[id]
}

func (p *Plugin) dropAEM(id string, c *aemCapture) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.aem[id] == c {
		delete(p.aem, id)
	}
}
