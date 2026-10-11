// Package cmsisdapplugin is the hwlab plugin for CMSIS-DAP debug probes
// (DAPLink, Raspberry Pi Debug Probe, BL702-DAPLink, ...). It drives the probe
// natively over USB (v2 bulk through usbfs on internal/usb, v1 HID through
// hidraw) and speaks SWD to a Cortex-M target through the shared internal/adiv5
// and internal/cortexm layers; JTAG is available for chain scans through the
// shared internal/jtag driver. It claims only the DAP interface, so the CDC
// UART of a composite probe stays with the kernel. The plugin advertises the
// generic DebugProbe and MemoryAccess capabilities only: vendor flash
// programming, RTT and SWO are not implemented, so they are not offered.
//
// STATUS: the v2 bulk transport and the probe commands were validated on a
// real BL702-DAPLink (CherryUSB) with NO target attached; target operations
// are validated against a SYNTHETIC simulator (plugins/cmsisdap/dap.Sim) until
// a target is wired. docs/notes/phase22-cmsisdap-results.md has the evidence.
package cmsisdapplugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx"
	"github.com/faronov/hwlab/internal/ti/cc23xx/service"
	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/cmsisdap/dap"
)

// SimEnv ("1") offers an in-process SYNTHETIC probe with a simulated Cortex-M
// target, for demos, fixtures and CI.
const SimEnv = "HWLAB_CMSISDAP_SIM"

// SimID is the id of the simulated probe.
const SimID = "sim-cmsisdap"

// Entry is one probe the plugin can open.
type Entry struct {
	ID  string
	USB *dap.UsbDevice
	Sim *dap.Sim
}

// Lister enumerates probes.
type Lister func() []Entry

// Opener opens the DAP transport of an entry.
type Opener func(Entry) (dap.Transport, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener

	mu   sync.Mutex
	devs map[string]*device
}

// New returns the plugin for the host's probes.
func New() *Plugin {
	var sim *dap.Sim
	if os.Getenv(SimEnv) == "1" {
		sim = dap.NewSim()
	}
	list := func() []Entry {
		var es []Entry
		for _, d := range dap.Scan("/sys") {
			d := d
			es = append(es, Entry{ID: d.ID(), USB: &d})
		}
		if sim != nil {
			es = append(es, Entry{ID: SimID, Sim: sim})
		}
		return es
	}
	return NewWith(list, defaultOpen)
}

// NewWith injects probe enumeration and transport opening (tests).
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*device{}}
}

func defaultOpen(e Entry) (dap.Transport, error) {
	if e.Sim != nil {
		return e.Sim, nil
	}
	return dap.OpenUSB(*e.USB)
}

func caps(attrs map[string]string) []*pluginv1.Capability {
	ids := []string{pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: attrs}
	}
	return out
}

func deviceCaps() []*pluginv1.Capability {
	return caps(map[string]string{"interface": "SWD", "jtag": "chain-scan-only", "flash_programming": "CC23xx-only", "rtt": "no", "swo": "no"})
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, e := range p.list() {
		d := &pluginv1.Device{Id: e.ID, Capabilities: deviceCaps()}
		switch {
		case e.Sim != nil:
			d.Name, d.Transport = "Simulated CMSIS-DAP (SYNTHETIC)", "fake"
			d.Properties = map[string]string{"synthetic": "true", "validated": "false"}
		default:
			u := e.USB
			d.Name, d.Serial, d.Transport = u.Product, u.Serial, "usb"
			if d.Name == "" {
				d.Name = "CMSIS-DAP probe"
			}
			ver := "v2 bulk"
			if u.Version == dap.V1 {
				ver = "v1 HID"
			}
			d.Properties = map[string]string{
				"vid": u.VID, "pid": u.PID, "manufacturer": u.Manufacturer, "transport_version": ver,
				"usb_address": fmt.Sprintf("%d:%d", u.Bus, u.Address), "validated": "false",
			}
			if u.Version == dap.V1 && u.HIDRaw == "" {
				d.Properties["warning"] = "no hidraw node found"
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func (p *Plugin) find(id string) (Entry, error) {
	for _, e := range p.list() {
		if e.ID == id {
			return e, nil
		}
	}
	return Entry{}, status.Errorf(codes.NotFound, "CMSIS-DAP probe %q is not connected", id)
}

// options are the device-level settings.
type options struct {
	clockHz uint32
	ap      uint8
	timeout time.Duration
	// cc holds the CC23xx flash permissions: a device setting made when the
	// device is opened; a call cannot grant it.
	cc service.Config
}

var (
	callKeys   = []string{"clock_hz", "ap", "timeout_ms"}
	deviceKeys = append(append([]string{}, callKeys...), service.ConfigKeys...)
)

func parseOptions(cfg map[string]any) (options, error) {
	o := options{clockHz: 1_000_000, timeout: 2 * time.Second}
	for k := range cfg {
		known := false
		for _, s := range deviceKeys {
			known = known || s == k
		}
		if !known {
			return o, fmt.Errorf("unknown setting %q (want %s)", k, strings.Join(deviceKeys, ", "))
		}
	}
	if v, ok, err := optNumber(cfg, "clock_hz"); err != nil {
		return o, err
	} else if ok {
		if v < 1000 || v > 50_000_000 || v != float64(uint32(v)) {
			return o, fmt.Errorf("clock_hz %v: want an integer 1000..50000000", v)
		}
		o.clockHz = uint32(v)
	}
	if v, ok, err := optNumber(cfg, "ap"); err != nil {
		return o, err
	} else if ok {
		if v < 0 || v > 255 || v != float64(uint8(v)) {
			return o, fmt.Errorf("ap %v: want an integer 0..255", v)
		}
		o.ap = uint8(v)
	}
	if v, ok, err := optNumber(cfg, "timeout_ms"); err != nil {
		return o, err
	} else if ok {
		if v < 50 || v > 60000 {
			return o, fmt.Errorf("timeout_ms %v: want 50..60000", v)
		}
		o.timeout = time.Duration(v) * time.Millisecond
	}
	cc, err := service.ParseConfig(cfg)
	if err != nil {
		return o, err
	}
	o.cc = cc
	return o, nil
}

// capClock lowers the SWD clock to at most hz for a SACI exchange and returns
// the function that puts the attachment's clock back.
func (d *device) capClock(hz uint32) (func(), error) {
	want := d.attached.clockHz
	if want <= hz {
		return func() {}, nil
	}
	if err := d.probe.SetClock(hz); err != nil {
		return nil, err
	}
	return func() { _ = d.probe.SetClock(want) }, nil
}

// device is an open probe, possibly attached to a target.
type device struct {
	id    string
	entry Entry
	o     options
	// transient devices live for one call (no OpenDevice); they keep no
	// breakpoint or watchpoint state.
	transient bool

	mu    sync.Mutex // serialises operations
	probe *dap.Probe
	tgt   *adiv5.Target
	core  *cortexm.Core
	// cc is the CC23xx session when the attached target is one.
	cc       *cc23xx.Session
	attached options

	trig triggers
}

func (p *Plugin) connect(e Entry, o options) (*device, error) {
	tr, err := p.open(e)
	if err != nil {
		if errors.Is(err, usb.ErrBusy) || errors.Is(err, usb.ErrDisconnected) || errors.Is(err, usb.ErrPermission) {
			return nil, classify(err)
		}
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", e.ID, err)
	}
	pr, err := dap.NewProbe(tr)
	if err != nil {
		_ = tr.Close()
		if errors.Is(err, usb.ErrDisconnected) {
			return nil, classify(err)
		}
		return nil, status.Errorf(codes.Unavailable, "%s is not answering as a CMSIS-DAP probe: %v", e.ID, err)
	}
	pr.SetTimeout(o.timeout)
	return &device{id: e.ID, entry: e, o: o, probe: pr}, nil
}

func (d *device) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closeLocked(true)
}

// closeLocked releases the probe. With restore set, breakpoints and
// watchpoints installed through this device are removed first, best effort.
func (d *device) closeLocked(restore bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	if d.tgt != nil {
		if restore {
			err = d.trig.restoreAll(ctx, d.core)
		}
		err = errors.Join(err, d.probe.Disconnect())
		d.tgt, d.core, d.cc = nil, nil, nil
	}
	return classifyClose(errors.Join(err, d.probe.Close()))
}

// detach drops the target attachment; the probe stays open.
func (d *device) detach() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if d.tgt != nil {
		if err := d.trig.restoreAll(ctx, d.core); err != nil {
			return err
		}
		if err := d.probe.Disconnect(); err != nil {
			return err
		}
		d.tgt, d.core, d.cc = nil, nil, nil
	}
	return nil
}

// attach connects to the target over SWD, reusing the attachment when the
// options are unchanged. It never halts, resets or writes the target.
func (d *device) attach(o options) error {
	if d.tgt != nil && d.attached == o {
		return nil
	}
	if err := d.detach(); err != nil {
		return err
	}
	d.probe.SetTimeout(o.timeout)
	err := dap.AttachLink(d.probe, dap.AttachOptions{ClockHz: o.clockHz, AP: o.ap})
	var t *adiv5.Target
	var core *cortexm.Core
	var cc *cc23xx.Session
	if err == nil {
		t, core, cc, err = service.Attach(context.Background(), d.probe, o.ap, d.pulseNRESETLow)
		if err != nil {
			_ = d.probe.Disconnect()
		}
	}
	if err != nil {
		if c := classify(err); IsDeviceGone(c) || IsBusy(c) {
			return c
		}
		return status.Errorf(codes.FailedPrecondition, "attach to the target: %v", err)
	}
	d.tgt, d.core, d.cc, d.attached = t, core, cc, o
	if cc != nil {
		cc.CapClock = d.capClock
	}
	d.trig = triggers{}
	return nil
}

// OpenDevice implements pluginsdk.Plugin. It opens the probe only; the target
// is attached by the first target operation.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	e, err := p.find(id)
	if err != nil {
		return err
	}
	o, err := parseOptions(cfg)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	// A re-open first releases this plugin's own handle: the interface can be
	// claimed only once, so connecting first would report BUSY against
	// ourselves.
	p.mu.Lock()
	old := p.devs[id]
	delete(p.devs, id)
	p.mu.Unlock()
	if old != nil {
		if err := old.close(); err != nil {
			return err
		}
	}
	d, err := p.connect(e, o)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.devs[id] = d
	p.mu.Unlock()
	return nil
}

// drop forgets an open device whose probe is gone so that the next call
// re-opens it by id (the id carries the USB serial).
func (p *Plugin) drop(d *device) {
	p.mu.Lock()
	if p.devs[d.id] == d {
		delete(p.devs, d.id)
	}
	p.mu.Unlock()
	d.mu.Lock()
	_ = d.closeLocked(false)
	d.mu.Unlock()
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	d := p.devs[id]
	delete(p.devs, id)
	p.mu.Unlock()
	if d != nil {
		return d.close()
	}
	return nil
}

// lease returns the open device, or opens one for a single call so that
// `hwlab invoke` works without a prior open. A target left halted by an
// earlier call stays halted: detaching does not resume it.
func (p *Plugin) lease(id string, args map[string]any) (*device, options, func(), error) {
	o, err := parseOptions(pick(args, callKeys))
	if err != nil {
		return nil, o, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	p.mu.Lock()
	d := p.devs[id]
	p.mu.Unlock()
	if d != nil {
		base := d.o
		oo, err := parseOptionsOver(base, pick(args, callKeys))
		if err != nil {
			return nil, o, nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return d, oo, func() {}, nil
	}
	e, err := p.find(id)
	if err != nil {
		return nil, o, nil, err
	}
	d, err = p.connect(e, o)
	if err != nil {
		return nil, o, nil, err
	}
	d.transient = true
	return d, o, func() { _ = d.close() }, nil
}

func pick(a map[string]any, keys []string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := a[k]; ok {
			out[k] = v
		}
	}
	return out
}

// parseOptionsOver applies per-call overrides to the device's options.
func parseOptionsOver(base options, over map[string]any) (options, error) {
	o, err := parseOptions(over)
	if err != nil {
		return o, err
	}
	if _, ok := over["clock_hz"]; !ok {
		o.clockHz = base.clockHz
	}
	if _, ok := over["ap"]; !ok {
		o.ap = base.ap
	}
	if _, ok := over["timeout_ms"]; !ok {
		o.timeout = base.timeout
	}
	o.cc = base.cc
	return o, nil
}

// StartCapture implements pluginsdk.Plugin; a debug probe produces no stream.
func (p *Plugin) StartCapture(context.Context, pluginsdk.CaptureRequest, *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	return nil, nil, status.Error(codes.Unimplemented, "cmsisdap has no capture capability (RTT and SWO are not implemented)")
}

func classifyClose(err error) error {
	if err == nil {
		return nil
	}
	return classify(err)
}
