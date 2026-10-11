// Package jlinknativeplugin is the hwlab plugin for the NATIVE J-Link backend
// (jlink-native): a pure-Go driver for SEGGER J-Link and SAM-ICE class probes
// over Linux usbfs. It runs no SEGGER process or library, no libusb and no
// CGO, and it never falls back to the SEGGER backend (jlink-segger,
// plugins/jlink): selecting one or the other is the caller's explicit choice.
//
// Layering (the probe only supplies the wire):
//
//	usbfs -> internal/usb -> plugins/jlinknative/usb (framing, errors)
//	      -> plugins/jlinknative/protocol (probe commands)
//	      -> plugins/jlinknative/swd (SWD bit sequences, adiv5.Link)
//	      -> internal/adiv5 (DP/AP) -> internal/cortexm (core debug)
//
// No halt, register, step or breakpoint logic lives here or in the transport.
// Flash programming is not offered: it belongs to target-family layers.
//
// STATUS: see docs/notes/phase-native-jlink-results.md for what was run on
// real hardware (one SAM-ICE class probe on an EFR32MG1) and what was not.
package jlinknativeplugin

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
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
	"github.com/faronov/hwlab/plugins/jlinknative/protocol"
	"github.com/faronov/hwlab/plugins/jlinknative/sim"
	"github.com/faronov/hwlab/plugins/jlinknative/swd"
	jusb "github.com/faronov/hwlab/plugins/jlinknative/usb"
)

// SimEnv ("1") offers an in-process SYNTHETIC probe with a simulated Cortex-M.
const SimEnv = "HWLAB_JLINKNATIVE_SIM"

// SimID is the id of the simulated probe.
const SimID = "jlink-native:SIM00000001"

// Entry is one probe the plugin can open. The id carries the USB serial, never
// a bus or device address.
type Entry struct {
	ID  string
	USB *jusb.Info
	Sim *sim.Sim
}

// Lister enumerates probes.
type Lister func() []Entry

// Opener opens the transport of an entry. For USB probes it must open by the
// entry's serial.
type Opener func(Entry) (jusb.Transport, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener

	mu   sync.Mutex
	devs map[string]*device
}

// New returns the plugin for the host's probes.
func New() *Plugin {
	var s *sim.Sim
	if os.Getenv(SimEnv) == "1" {
		s = sim.New()
	}
	list := func() []Entry {
		var es []Entry
		for _, i := range jusb.Scan("/sys") {
			i := i
			es = append(es, Entry{ID: i.ID(), USB: &i})
		}
		if s != nil {
			es = append(es, Entry{ID: SimID, Sim: s})
		}
		return es
	}
	return NewWith(list, defaultOpen)
}

// NewWith injects probe enumeration and opening (tests).
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*device{}}
}

func defaultOpen(e Entry) (jusb.Transport, error) {
	if e.Sim != nil {
		return e.Sim, nil
	}
	return jusb.OpenSerial("/sys", e.USB.Serial)
}

func caps(attrs map[string]string) []*pluginv1.Capability {
	ids := []string{pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapRTT, pluginv1.CapLiveRawStream}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: attrs}
	}
	return out
}

func deviceCaps() []*pluginv1.Capability {
	return caps(map[string]string{"backend": "jlink-native", "interface": "SWD", "jtag": "implemented-untested", "flash_programming": "no", "rtt": "target-memory", "swo": "no"})
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, e := range p.list() {
		d := &pluginv1.Device{Id: e.ID, Capabilities: deviceCaps()}
		if e.Sim != nil {
			d.Name, d.Transport = "Simulated J-Link (SYNTHETIC)", "fake"
			d.Properties = map[string]string{"synthetic": "true", "validated": "false", "backend": "jlink-native"}
		} else {
			u := e.USB
			d.Name, d.Serial, d.Transport = "SEGGER J-Link class probe", u.Serial, "usb"
			d.Properties = map[string]string{
				"vid": fmt.Sprintf("%04x", jusb.VID), "interface": fmt.Sprint(u.Interface),
				"endpoint_out": fmt.Sprintf("0x%02x", u.Out), "endpoint_in": fmt.Sprintf("0x%02x", u.In), "max_packet": fmt.Sprint(u.MaxPacket),
				"backend": "jlink-native", "validated": "false",
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
	return Entry{}, status.Errorf(codes.NotFound, "J-Link probe %q is not connected", id)
}

// options are the device-level settings.
type options struct {
	clockHz uint32
	ap      uint8
	timeout time.Duration
}

var callKeys = []string{"clock_hz", "ap", "timeout_ms"}

func parseOptions(cfg map[string]any) (options, error) {
	o := options{clockHz: 1_000_000, timeout: 2 * time.Second}
	for k := range cfg {
		known := false
		for _, s := range callKeys {
			known = known || s == k
		}
		if !known {
			return o, fmt.Errorf("unknown setting %q (want %s)", k, strings.Join(callKeys, ", "))
		}
	}
	if v, ok, err := optNumber(cfg, "clock_hz"); err != nil {
		return o, err
	} else if ok {
		if v < 1000 || v > 65534000 || v != float64(uint32(v)) || uint32(v)%1000 != 0 {
			return o, fmt.Errorf("clock_hz %v: want a whole number of kHz, 1000..65534000 (the probe takes kHz)", v)
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
	return o, nil
}

// timeouter is implemented by transports with a settable I/O timeout.
type timeouter interface{ SetTimeout(time.Duration) }

// device is an open probe, possibly attached to a target.
type device struct {
	id    string
	entry Entry
	o     options
	// transient devices live for one call (no OpenDevice); they keep no
	// breakpoint or watchpoint state.
	transient bool

	mu     sync.Mutex // serialises operations
	tr     jusb.Transport
	client *protocol.Client
	link   *swd.Link
	tgt    *adiv5.Target
	core   *cortexm.Core
	// ident is what the probe reported at open; kept for diagnostics.
	version  protocol.VersionInfo
	attached options

	trig triggers
}

func (p *Plugin) connect(e Entry, o options) (*device, error) {
	tr, err := p.open(e)
	if err != nil {
		if c := classify(err); IsDeviceGone(c) || IsBusy(c) || status.Code(c) == codes.PermissionDenied {
			return nil, c
		}
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", e.ID, err)
	}
	if t, ok := tr.(timeouter); ok {
		t.SetTimeout(o.timeout)
	}
	if c, ok := tr.(*jusb.Conn); ok && e.USB != nil && c.Info().Serial != e.USB.Serial {
		_ = tr.Close()
		return nil, classify(jlerr.New(jlerr.WrongProbe, "open", fmt.Errorf("asked for serial %s, opened %s", e.USB.Serial, c.Info().Serial)))
	}
	cl := protocol.New(tr)
	v, err := cl.Version(context.Background())
	if err != nil {
		_ = tr.Close()
		return nil, classify(err)
	}
	if v.Bootloader {
		_ = tr.Close()
		return nil, classify(jlerr.New(jlerr.ProbeBootloader, "open", fmt.Errorf("the probe reports %q: it runs its bootloader, not the command set; replug it", v.Raw)))
	}
	return &device{id: e.ID, entry: e, o: o, tr: tr, client: cl, version: v}, nil
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
		d.tgt, d.core, d.link = nil, nil, nil
	}
	return classifyClose(errors.Join(err, d.tr.Close()))
}

// detach drops the target attachment; the probe stays open.
func (d *device) detach() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if d.tgt != nil {
		if err := d.trig.restoreAll(ctx, d.core); err != nil {
			return err
		}
		d.tgt, d.core, d.link = nil, nil, nil
	}
	return nil
}

// attach connects to the target over SWD, reusing the attachment when the
// options are unchanged. It never halts, resets or writes the target. The
// interface is selected every time: the probe falls back to JTAG when it is
// re-opened.
func (d *device) attach(ctx context.Context, o options) error {
	if d.tgt != nil && d.attached == o {
		return nil
	}
	if err := d.detach(); err != nil {
		return err
	}
	if t, ok := d.tr.(timeouter); ok {
		t.SetTimeout(o.timeout)
	}
	if _, err := d.client.SelectInterface(ctx, protocol.IfSWD); err != nil {
		return d.attachErr(err)
	}
	if err := d.setClock(ctx, o.clockHz); err != nil {
		return d.attachErr(err)
	}
	l := swd.New(d.client, d.client)
	l.SetContext(ctx)
	t, err := adiv5.Connect(l, o.ap)
	if err != nil {
		return d.attachErr(err)
	}
	d.link, d.tgt, d.core, d.attached = l, t, cortexm.New(adiv5.Bus{T: t}), o
	d.trig = triggers{}
	return nil
}

func (d *device) attachErr(err error) error {
	if c := classify(err); IsDeviceGone(c) || IsBusy(c) || status.Code(c) == codes.DeadlineExceeded {
		return c
	}
	return status.Errorf(codes.FailedPrecondition, "attach to the target: %v", classify(err).Error())
}

// setClock programs the SWD clock; the rate must be inside the probe's range.
func (d *device) setClock(ctx context.Context, hz uint32) error {
	sp, err := d.client.Speeds(ctx)
	if err != nil {
		return err
	}
	khz := hz / 1000
	if max := sp.MaxKHz(); max != 0 && khz > max {
		return jlerr.New(jlerr.UnsupportedProbeCommand, "set speed", fmt.Errorf("%d kHz is above the probe's maximum of %d kHz", khz, max))
	}
	return d.client.SetSpeed(ctx, uint16(khz))
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
	// claimed once, so connecting first would report BUSY against ourselves.
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

// drop forgets an open device whose probe is gone, so that the next call
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
	return o, nil
}

func classifyClose(err error) error {
	if err == nil {
		return nil
	}
	return classify(err)
}
