// Package xds110plugin is the hwlab plugin for TI XDS110 debug probes (the
// stand-alone LP-XDS110ET, LaunchPad on-board XDS110 and the XDS110 firmware
// family). It speaks the XDS110 vendor protocol natively over USB (usbfs on
// internal/usb) and reaches a Cortex-M target through the shared internal/adiv5
// and internal/cortexm layers; JTAG chain scans use the shared internal/jtag
// driver. It claims only the vendor debug interface, so the CDC UART ports,
// the CMSIS-DAP HID interface and the second vendor interface stay with the
// kernel. The plugin advertises the generic DebugProbe and MemoryAccess
// capabilities only: flash programming and EnergyTrace are not offered.
//
// STATUS: the XDS110 frame protocol, version query, clock encodings and JTAG
// commands were exercised on a real LP-XDS110ET (hardware id 0x23, firmware
// 3.0.0.22) with NO target connected. Target operations are validated against a
// SYNTHETIC simulator (plugins/xds110/xds.Sim) until a target is wired to the
// probe's P7 connector. docs/notes/phase24-xds110-results.md has the evidence.
package xds110plugin

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
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

// SimEnv ("1") offers an in-process SYNTHETIC probe with a simulated Cortex-M
// target, for demos, fixtures and CI.
const SimEnv = "HWLAB_XDS110_SIM"

// SimID is the id of the simulated probe.
const SimID = "sim-xds110"

// Entry is one probe the plugin can open.
type Entry struct {
	ID  string
	USB *usb.Device
	Sim *xds.Sim
}

// Lister enumerates probes.
type Lister func() []Entry

// Opener opens the XDS110 transport of an entry.
type Opener func(Entry) (xds.Transport, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener
	// openData opens the EnergyTrace stream interface.
	openData DataOpener
	mu       sync.Mutex
	devs     map[string]*device
}

// DeviceID is the stable identity of a USB probe: VID:PID:serial. The bus and
// device numbers change on every re-enumeration and are never used. A probe
// without a serial falls back to its sysfs port name.
func DeviceID(d usb.Device) string {
	if d.Serial != "" {
		return fmt.Sprintf("%04x:%04x:%s", d.VID, d.PID, d.Serial)
	}
	return fmt.Sprintf("%04x:%04x@%s", d.VID, d.PID, d.SysName)
}

// New returns the plugin for the host's probes.
func New() *Plugin {
	var sim *xds.Sim
	if os.Getenv(SimEnv) == "1" {
		sim = xds.NewSim()
	}
	list := func() []Entry {
		var es []Entry
		for _, d := range usb.Scan("/sys", xds.Match) {
			d := d
			es = append(es, Entry{ID: DeviceID(d), USB: &d})
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
	return &Plugin{list: l, open: o, openData: defaultOpenData, devs: map[string]*device{}}
}

func defaultOpen(e Entry) (xds.Transport, error) {
	if e.Sim != nil {
		return e.Sim, nil
	}
	return xds.OpenUSB(*e.USB)
}

func caps(attrs map[string]string) []*pluginv1.Capability {
	ids := []string{pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: attrs}
	}
	return out
}

func powerCap() *pluginv1.Capability {
	return &pluginv1.Capability{Id: pluginv1.CapPowerMeasurement, Version: 1, Attributes: etAttributes()}
}

func deviceCaps(e Entry) []*pluginv1.Capability {
	attrs := map[string]string{"interface": "SWD", "jtag": "chain-scan-only", "flash_programming": "CC23xx-only", "rtt": "no", "swo": "no", "energytrace": "no"}
	out := caps(attrs)
	if supportsEnergyTrace(e) {
		attrs["energytrace"] = "PROTOCOL_PARTIAL"
		out = append(out, powerCap(),
			&pluginv1.Capability{Id: pluginv1.CapTimedCapture, Version: 1, Attributes: etAttributes()},
			&pluginv1.Capability{Id: pluginv1.CapLiveRawStream, Version: 1, Attributes: etAttributes()})
	}
	return out
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, e := range p.list() {
		d := &pluginv1.Device{Id: e.ID, Capabilities: deviceCaps(e)}
		switch {
		case e.Sim != nil:
			d.Name, d.Transport = "Simulated XDS110 (SYNTHETIC)", "fake"
			d.Properties = map[string]string{"synthetic": "true", "validated": "false"}
		default:
			u := e.USB
			d.Name, d.Serial, d.Transport = u.Product, u.Serial, "usb"
			if d.Name == "" {
				d.Name = "XDS110 probe"
			}
			d.Properties = map[string]string{
				"vid": fmt.Sprintf("%04x", u.VID), "pid": fmt.Sprintf("%04x", u.PID), "manufacturer": u.Manufacturer,
				"usb_address": fmt.Sprintf("%d:%d", u.Bus, u.Address), "validated": "probe-only",
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
	return Entry{}, status.Errorf(codes.NotFound, "XDS110 probe %q is not connected", id)
}

// options are the device-level settings.
type options struct {
	clockHz uint32
	ap      uint8
	timeout time.Duration
	// allowPower permits the target-supply method at all. It is a device
	// setting made when the device is opened; a call cannot grant it.
	allowPower bool
	// cc holds the CC23xx flash permissions, also granted only at open.
	cc service.Config
}

var (
	callKeys   = []string{"clock_hz", "ap", "timeout_ms"}
	deviceKeys = append(append(append([]string{}, callKeys...), "allow_target_power"), service.ConfigKeys...)
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
		if v < 100_000 || v > 14_000_000 || v != float64(uint32(v)) {
			return o, fmt.Errorf("clock_hz %v: want an integer 100000..14000000", v)
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
	if v, ok, err := optBool(cfg, "allow_target_power"); err != nil {
		return o, err
	} else if ok {
		o.allowPower = v
	}
	cc, err := service.ParseConfig(cfg)
	if err != nil {
		return o, err
	}
	o.cc = cc
	return o, nil
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
	probe *xds.Probe
	tgt   *adiv5.Target
	core  *cortexm.Core
	// cc is the CC23xx session when the attached target is one.
	cc       *cc23xx.Session
	attached options
	// supplyOn records a target supply this device enabled, so closing the
	// device can switch it off again.
	supplyOn bool

	trig triggers
	// cap is the running EnergyTrace capture, guarded by mu.
	cap *etCapture
}

func (p *Plugin) connect(e Entry, o options) (*device, error) {
	tr, err := p.open(e)
	if err != nil {
		if errors.Is(err, usb.ErrBusy) || errors.Is(err, usb.ErrDisconnected) || errors.Is(err, usb.ErrPermission) {
			return nil, classify(err)
		}
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", e.ID, err)
	}
	pr := xds.NewProbe(tr)
	pr.SetTimeout(o.timeout)
	if _, err := pr.Connect(); err != nil {
		_ = tr.Close()
		if errors.Is(err, usb.ErrDisconnected) {
			return nil, classify(err)
		}
		return nil, status.Errorf(codes.Unavailable, "%s is not answering as an XDS110 probe: %v", e.ID, err)
	}
	return &device{id: e.ID, entry: e, o: o, probe: pr}, nil
}

func (d *device) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closeLocked(true)
}

// closeLocked releases the probe. With restore set, breakpoints and
// watchpoints installed through this device are removed first and a target
// supply this device switched on is switched off, best effort.
func (d *device) closeLocked(restore bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	if d.tgt != nil {
		if restore {
			err = d.trig.restoreAll(ctx, d.core)
		}
		err = errors.Join(err, d.probe.SWDDisconnect())
		d.tgt, d.core, d.cc = nil, nil, nil
	}
	if restore {
		if d.supplyOn {
			err = errors.Join(err, d.probe.SetSupply(0, false))
			d.supplyOn = false
		}
		err = errors.Join(err, d.probe.Disconnect())
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
		if err := d.probe.SWDDisconnect(); err != nil {
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
	if _, err := d.probe.SetClock(o.clockHz / 1000); err != nil {
		return d.attachErr(err)
	}
	t, core, cc, err := service.Attach(context.Background(), xds.NewSWDLink(d.probe), o.ap, d.pulseSRST)
	if err != nil {
		_ = d.probe.SWDDisconnect()
		return d.attachErr(err)
	}
	d.tgt, d.core, d.cc, d.attached = t, core, cc, o
	if cc != nil {
		cc.CapClock = d.capClock
	}
	d.trig = triggers{}
	return nil
}

// capClock lowers the SWD clock to at most hz for a SACI exchange and returns
// the function that puts the attachment's clock back.
func (d *device) capClock(hz uint32) (func(), error) {
	want := d.attached.clockHz
	if want <= hz {
		return func() {}, nil
	}
	if _, err := d.probe.SetClock(hz / 1000); err != nil {
		return nil, err
	}
	return func() { _, _ = d.probe.SetClock(want / 1000) }, nil
}

// pulseSRST drives nRESET low for low and releases it.
func (d *device) pulseSRST(ctx context.Context, low time.Duration) error {
	if err := d.probe.SetSRST(false); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-time.After(low):
	}
	return d.probe.SetSRST(true)
}

func (d *device) attachErr(err error) error {
	if c := classify(err); IsDeviceGone(c) || IsBusy(c) {
		return c
	}
	return status.Errorf(codes.FailedPrecondition, "attach to the target over SWD: %v (is a target connected to the probe's debug connector?)", err)
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
		old.stopCapture()
		_ = old.close()
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
		d.stopCapture()
		return d.close()
	}
	return nil
}

// stopCapture ends a running EnergyTrace capture and waits until the probe is
// released again. It must be called without holding d.mu.
func (d *device) stopCapture() {
	d.mu.Lock()
	c := d.cap
	d.mu.Unlock()
	if c != nil {
		c.stop()
	}
}

// lease returns the open device, or opens one for a single call so that
// `hwlab invoke` works without a prior open. A target left halted by an
// earlier call stays halted: detaching does not resume it. A one-shot device
// never has allow_target_power: only an explicit OpenDevice can grant it.
func (p *Plugin) lease(id string, args map[string]any) (*device, options, func(), error) {
	o, err := parseOptions(pick(args, callKeys))
	if err != nil {
		return nil, o, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	p.mu.Lock()
	d := p.devs[id]
	p.mu.Unlock()
	if d != nil {
		oo, err := parseOptionsOver(d.o, pick(args, callKeys))
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
	o.allowPower = base.allowPower
	o.cc = base.cc
	return o, nil
}

func classifyClose(err error) error {
	if err == nil {
		return nil
	}
	return classify(err)
}
