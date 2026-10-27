package xds110plugin

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/rtt"
	"github.com/faronov/hwlab/internal/rtt/rttplugin"
	"github.com/faronov/hwlab/internal/ti/cc23xx/service"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// RTTLogSource is the Log.source of RTT lines in the timeline.
const RTTLogSource = "xds110-rtt"

// streamKey selects what StartCapture records: "rtt" or "energytrace".
const streamKey = "stream"

var errClosed = status.Error(codes.FailedPrecondition, "the device was closed")

// rttMem is target memory of one device. Every call must hold d.mu with the
// target attached and, on a CC23xx, the AHB-AP open.
type rttMem struct{ d *device }

func (m rttMem) Read(_ context.Context, a uint32, n int) ([]byte, error) {
	return m.d.tgt.ReadMem(a, n)
}
func (m rttMem) Write(_ context.Context, a uint32, b []byte) error { return m.d.tgt.WriteMem(a, b) }

// captureStream applies the StartCapture dispatch rule: stream=rtt, or any
// rtt_* key without a stream, records RTT; otherwise (stream absent or
// "energytrace") the EnergyTrace stream, as before RTT existed.
func captureStream(cfg map[string]any) (string, error) {
	v, ok := cfg[streamKey]
	if !ok {
		if rttplugin.HasKeys(cfg) {
			return "rtt", nil
		}
		return "energytrace", nil
	}
	switch v {
	case "rtt":
		return "rtt", nil
	case "energytrace":
		if rttplugin.HasKeys(cfg) {
			return "", fmt.Errorf("rtt_* settings need stream=rtt, not stream=energytrace")
		}
		return "energytrace", nil
	}
	return "", fmt.Errorf("stream %v: want rtt or energytrace", v)
}

// keepAttachment prefers the options of an existing attachment: re-attaching
// with different ones would drop the breakpoints and watchpoints of the
// device, and RTT polling must never do that.
func (d *device) keepAttachment(o options) options {
	if d.tgt != nil {
		return d.attached
	}
	return o
}

// rttTarget is the device side of an RTT capture. Each access attaches and,
// on a CC23xx, leaves SACI exactly as MemoryAccess.read does, so a capture
// follows the target through a reset.
type rttTarget struct {
	p     *Plugin
	d     *device
	o     options
	owned bool
}

func (t *rttTarget) Access(ctx context.Context, fn func(context.Context, rtt.Memory) error) error {
	d := t.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errClosed
	}
	if d.cap != nil {
		return status.Error(codes.FailedPrecondition, "an EnergyTrace capture is running on this probe")
	}
	if err := d.attach(d.keepAttachment(t.o)); err != nil {
		return err
	}
	if d.cc != nil {
		if err := service.EnsureAHB(ctx, d.cc, d.o.cc.KeepSACI); err != nil {
			return err
		}
	}
	err := fn(ctx, rttMem{d})
	if err != nil && d.cc != nil && !rttplugin.IsMiss(err) {
		err = service.Annotate(ctx, d.cc, err)
	}
	return err
}

func (t *rttTarget) Fail(err error) (error, bool) {
	if errors.Is(err, errClosed) {
		return err, true
	}
	err = classify(err)
	if IsDeviceGone(err) {
		t.p.drop(t.d)
		return err, true
	}
	return err, false
}

// Release ends the capture's hold and closes a device it opened itself.
func (t *rttTarget) Release() {
	t.d.mu.Lock()
	t.d.rtt--
	t.d.mu.Unlock()
	if !t.owned {
		return
	}
	t.owned = false
	t.p.mu.Lock()
	if t.p.devs[t.d.id] == t.d {
		delete(t.p.devs, t.d.id)
	}
	t.p.mu.Unlock()
	_ = t.d.close()
}

// startRTT starts an RTT capture: up-buffer output as Log events. It uses the
// open device, or opens one for the capture's lifetime. The device lock is
// held for one poll at a time, so debug calls interleave with the capture.
func (p *Plugin) startRTT(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	cfg, err := rttplugin.ParseConfig(req.Config)
	if err != nil {
		return nil, nil, argErr(err)
	}
	rest := rttplugin.Without(req.Config, streamKey)
	o, err := parseOptions(rest)
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
		// Like a one-shot call, a device opened by the capture never has
		// allow_target_power: only an explicit OpenDevice grants it.
		o.allowPower = false
		if d, err = p.connect(e, o); err != nil {
			return nil, nil, err
		}
		p.mu.Lock()
		p.devs[d.id] = d
		p.mu.Unlock()
		owned = true
	} else if oo, err := parseOptionsOver(d.o, pick(rest, callKeys)); err == nil {
		o = oo
	}
	d.mu.Lock()
	if d.cap != nil {
		d.mu.Unlock()
		return nil, nil, status.Error(codes.FailedPrecondition, "an EnergyTrace capture is running on this probe: stop it before starting an RTT capture")
	}
	d.rtt++
	d.mu.Unlock()
	c, start, err := rttplugin.Start(ctx, &rttTarget{p: p, d: d, o: o, owned: owned}, cfg, RTTLogSource, req.Clock, sink)
	if err != nil {
		return nil, nil, err
	}
	return c, start, nil
}
