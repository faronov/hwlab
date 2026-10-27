package cmsisdapplugin

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/rtt"
	"github.com/faronov/hwlab/internal/rtt/rttplugin"
	"github.com/faronov/hwlab/internal/ti/cc23xx/service"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// RTTLogSource is the Log.source of RTT lines in the timeline.
const RTTLogSource = "cmsisdap-rtt"

// streamKey optionally names the capture stream; only "rtt" exists.
const streamKey = "stream"

var errClosed = status.Error(codes.FailedPrecondition, "the device was closed")

// rttMem is target memory of one device. Every call must hold d.mu with the
// target attached and, on a CC23xx, the AHB-AP open.
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

// StartCapture implements pluginsdk.Plugin: an RTT capture, up-buffer output
// as Log events on stream "rtt". It uses the open device, or opens one for
// the capture's lifetime. The device lock is held for one poll at a time, so
// debug calls interleave with the capture.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	if v, ok := req.Config[streamKey]; ok && v != "rtt" {
		return nil, nil, status.Errorf(codes.InvalidArgument, "stream %v: cmsisdap records only stream=rtt", v)
	}
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
	c, start, err := rttplugin.Start(ctx, &rttTarget{p: p, d: d, o: o, owned: owned}, cfg, RTTLogSource, req.Clock, sink)
	if err != nil {
		return nil, nil, err
	}
	return c, start, nil
}
