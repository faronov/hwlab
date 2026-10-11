// Package picoscopeplugin is a PicoScope-class oscilloscope plugin: block
// acquisition on a few channels at high rate, with input ranges, a timebase, a
// trigger, waveform selection and measurements. It is written against the
// backend.Backend seam, so it never touches a vendor library.
//
// STATUS: contract and synthetic backend only. No PicoScope was available, no
// real driver exists in this tree, and nothing here has run on real hardware.
// Streaming mode (continuous acquisition) and rapid block mode are not
// implemented. Devices come from HWLAB_PICOSCOPE_FAKE=1 (SYNTHETIC) only.
//
// Large waveforms never travel through RPC results: methods return statistics,
// measurements and a bounded min/max envelope; the samples go to the session
// as analog events and to a CSV artifact.
package picoscopeplugin

import (
	"context"
	"fmt"
	"os"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/picoscope/backend"
)

// FakeEnv offers the SYNTHETIC scope as picoscope/fake-ps.
const FakeEnv = "HWLAB_PICOSCOPE_FAKE"

// FakeID is the device id of the synthetic scope.
const FakeID = "fake-ps"

// Entry is one instrument the plugin can open.
type Entry struct {
	ID        string
	Model     string
	Synthetic bool
	Open      func() (backend.Backend, error)
}

// Lister enumerates instruments.
type Lister func() ([]Entry, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister

	mu   sync.Mutex
	devs map[string]*device
}

// New returns the plugin for this host: today only the synthetic scope.
func New() *Plugin {
	return NewWith(func() ([]Entry, error) {
		var es []Entry
		if os.Getenv(FakeEnv) != "" {
			es = append(es, Entry{ID: FakeID, Model: "SYNTHETIC-PS4000", Synthetic: true,
				Open: func() (backend.Backend, error) { return backend.NewFake(backend.FakeSerial), nil }})
		}
		return es, nil
	})
}

// NewWith injects instrument enumeration.
func NewWith(l Lister) *Plugin { return &Plugin{list: l, devs: map[string]*device{}} }

func caps() []*pluginv1.Capability {
	ids := []string{pluginv1.CapOscilloscope, pluginv1.CapAnalogCapture, pluginv1.CapTimedCapture, pluginv1.CapArtifactExport}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: map[string]string{"mode": "block", "streaming": "not-implemented"}}
	}
	return out
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	es, err := p.list()
	if err != nil {
		return nil, err
	}
	out := make([]*pluginv1.Device, 0, len(es))
	for _, e := range es {
		props := map[string]string{"model": e.Model, "validated": "false"}
		transport := "usb"
		if e.Synthetic {
			props["synthetic"] = "true"
			transport = "fake"
		}
		out = append(out, &pluginv1.Device{Id: e.ID, Name: e.ID, Transport: transport, Capabilities: caps(), Properties: props})
	}
	return out, nil
}

func (p *Plugin) find(id string) (Entry, error) {
	es, err := p.list()
	if err != nil {
		return Entry{}, status.Error(codes.Unavailable, err.Error())
	}
	for _, e := range es {
		if e.ID == id {
			return e, nil
		}
	}
	return Entry{}, status.Errorf(codes.NotFound, "scope %q is not available: no PicoScope driver is built in; set %s=1 for the synthetic scope", id, FakeEnv)
}

func (p *Plugin) device(id string) *device {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.devs[id]
}

func (p *Plugin) connect(e Entry, cfg map[string]any) (*device, error) {
	b, err := e.Open()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", e.ID, err)
	}
	info := b.Info()
	s, err := parseSettings(defaultSettings(info), cfg, allKeys)
	if err != nil {
		_ = b.Close()
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if _, err := b.Plan(s.request()); err != nil {
		_ = b.Close()
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &device{id: e.ID, entry: e, b: b, info: info, set: s}, nil
}

// OpenDevice implements pluginsdk.Plugin.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	e, err := p.find(id)
	if err != nil {
		return err
	}
	d, err := p.connect(e, cfg)
	if err != nil {
		return err
	}
	p.mu.Lock()
	old := p.devs[id]
	p.devs[id] = d
	p.mu.Unlock()
	if old != nil {
		_ = old.b.Close()
	}
	return nil
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	d := p.devs[id]
	delete(p.devs, id)
	p.mu.Unlock()
	if d != nil {
		return d.b.Close()
	}
	return nil
}

// lease returns the open device, or opens a transient one for a single call.
// A transient device starts from the defaults (plus cfg), so settings do not
// outlive the call.
func (p *Plugin) lease(id string, cfg map[string]any) (*device, bool, func(), error) {
	if d := p.device(id); d != nil {
		return d, false, func() {}, nil
	}
	e, err := p.find(id)
	if err != nil {
		return nil, false, nil, err
	}
	d, err := p.connect(e, cfg)
	if err != nil {
		return nil, false, nil, err
	}
	return d, true, func() { _ = d.b.Close() }, nil
}

// device is one opened scope.
type device struct {
	id    string
	entry Entry
	b     backend.Backend
	info  backend.Info

	mu   sync.Mutex // guards everything below and serialises backend calls
	set  settings
	last *acquisition
	busy bool // a capture is running
}

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func errf(c codes.Code, f string, a ...any) error { return status.Error(c, fmt.Sprintf(f, a...)) }
