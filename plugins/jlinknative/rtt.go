package jlinknativeplugin

import (
	"context"

	"github.com/faronov/hwlab/internal/rtt"
	"github.com/faronov/hwlab/internal/rtt/rttplugin"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// RTTLogSource is the Log.source of native RTT lines in the timeline.
const RTTLogSource = "jlink-native-rtt"

// rttMem is target memory of one device. Every call must hold d.mu with the
// target attached.
type rttMem struct{ d *device }

func (m rttMem) Read(_ context.Context, a uint32, n int) ([]byte, error) {
	return m.d.tgt.ReadMem(a, n)
}
func (m rttMem) Write(_ context.Context, a uint32, b []byte) error { return m.d.tgt.WriteMem(a, b) }

// keepAttachment prefers the options of an existing attachment: re-attaching
// with different ones would drop the breakpoints and watchpoints of the
// device, and RTT polling must never do that.
func (d *device) keepAttachment(o options) options {
	if d.tgt != nil {
		return d.attached
	}
	return o
}

// rttAccess attaches (keeping an existing attachment) and runs fn on the
// target memory. The caller holds d.mu.
func (d *device) rttAccess(ctx context.Context, o options, fn func(context.Context, rtt.Memory) error) error {
	if err := d.attach(ctx, d.keepAttachment(o)); err != nil {
		return err
	}
	d.link.SetContext(ctx)
	return fn(ctx, rttMem{d})
}

// rttInfo reports the control block and its channels without consuming data.
func (d *device) rttInfo(ctx context.Context, a map[string]any, o options) (res map[string]any, err error) {
	err = d.rttAccess(ctx, o, func(ctx context.Context, m rtt.Memory) error {
		res, err = rttplugin.Info(ctx, m, a)
		return err
	})
	return res, err
}

// rttWrite writes text to a down buffer; bytes that do not fit are reported,
// never overwritten.
func (d *device) rttWrite(ctx context.Context, a map[string]any, o options) (res map[string]any, err error) {
	err = d.rttAccess(ctx, o, func(ctx context.Context, m rtt.Memory) error {
		res, err = rttplugin.Write(ctx, m, a)
		return err
	})
	return res, err
}

// rttTarget is the device side of an RTT capture.
type rttTarget struct {
	p     *Plugin
	d     *device
	o     options
	owned bool
}

func (t *rttTarget) Access(ctx context.Context, fn func(context.Context, rtt.Memory) error) error {
	t.d.mu.Lock()
	defer t.d.mu.Unlock()
	return t.d.rttAccess(ctx, t.o, fn)
}

func (t *rttTarget) Fail(err error) (error, bool) {
	err = classify(err)
	if IsDeviceGone(err) {
		t.p.drop(t.d)
		return err, true
	}
	return err, false
}

// Release closes a device the capture opened itself.
func (t *rttTarget) Release() {
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

// StartCapture implements pluginsdk.Plugin: RTT up-buffer output as Log events.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	cfg, err := rttplugin.ParseConfig(req.Config)
	if err != nil {
		return nil, nil, argErr(err)
	}
	callOptions := pick(req.Config, callKeys)
	o, err := parseOptions(callOptions)
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
	} else if oo, err := parseOptionsOver(d.o, callOptions); err == nil {
		o = oo
	}
	c, start, err := rttplugin.Start(ctx, &rttTarget{p: p, d: d, o: o, owned: owned}, cfg, RTTLogSource, req.Clock, sink)
	if err != nil {
		return nil, nil, err
	}
	return c, start, nil
}
