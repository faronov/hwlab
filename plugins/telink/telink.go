// Package telinkplugin is the hwlab plugin for Telink TLSR825x targets behind a
// SWire (SWS) programmer. It drives the programmer natively over a serial port
// (see plugins/telink/sws) and offers the generic DebugProbe, MemoryAccess and
// FlashProgramming capabilities. No Python tool, probe-rs or other backend is
// needed.
//
// SAFETY. Flash erase and program are bounded by a per-board layout
// (sws.Layout): the factory EUI-64, configuration and ADC calibration sectors
// are never modified, and the product NV journals only on an explicit
// allow_journals. Chip erase is not offered. Programming clears the flash
// block-protection bits (a persistent change); the result reports it.
//
// Layers: serial transport (plugins/uart) -> SWire programmer protocol
// (plugins/telink/sws) -> chip-family drivers with flash geometry and
// protection maps (plugins/telink/target). Flash modification fails closed
// until the connected board's protection map is verified (target.Protection).
//
// STATUS: the read-only path (programmer info, target identify, memory read,
// flash read, halt, resume) is validated on a real TLSR825x board behind a
// CH340 programmer; see docs/notes/phase14-telink-real-hardware-results.md.
// Erase, program, step, breakpoints and memory write are only tested against a
// SYNTHETIC simulator (sws.Sim, enabled by HWLAB_TELINK_SIM=1 only).
//
// Programmers come from HWLAB_TELINK_PORTS (name=/dev/ttyUSB0[@baud]) and
// from USB-UART adapters matched by VID/PID. Enumeration never writes to a
// port; candidates are marked unprobed until opened.
package telinkplugin

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/telink/sws"
	"github.com/faronov/hwlab/plugins/telink/target"
	"github.com/faronov/hwlab/plugins/uart"
)

const (
	// PortsEnv lists programmers as comma-separated name=/dev/ttyUSB0[@baud].
	PortsEnv = "HWLAB_TELINK_PORTS"
	// SimEnv ("1") offers an in-process SYNTHETIC programmer and target.
	SimEnv = "HWLAB_TELINK_SIM"
	// SimID is the id of the simulated programmer.
	SimID = "sim-telink"
)

// Entry is one programmer the plugin can open.
type Entry struct {
	ID   string
	Port string
	Baud int
	Sim  *sws.Sim
	// Candidate marks a USB-UART adapter found by VID/PID. Nothing has been
	// written to it: whether a Telink programmer is behind it is unknown until
	// the device is opened.
	Candidate bool
	USB       string // vid:pid
	Product   string
}

// Lister enumerates programmers.
type Lister func() []Entry

// Opener opens the serial link of an entry. The returned func closes it.
type Opener func(e Entry, baud int) (sws.Link, func() error, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener

	mu   sync.Mutex
	devs map[string]*device
}

// New returns the plugin for the host's configuration.
func New() *Plugin {
	var sim *sws.Sim
	if os.Getenv(SimEnv) == "1" {
		sim = sws.NewSim()
	}
	list := func() []Entry {
		es := parsePorts(os.Getenv(PortsEnv))
		es = append(es, candidates(es, uart.ListPorts)...)
		if sim != nil {
			es = append(es, Entry{ID: SimID, Sim: sim})
		}
		return es
	}
	return NewWith(list, defaultOpen)
}

// NewWith injects enumeration and opening (tests).
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*device{}}
}

// parsePorts reads PortsEnv; malformed entries are skipped.
func parsePorts(s string) []Entry {
	var out []Entry
	for _, item := range strings.Split(s, ",") {
		name, spec, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || name == "" || spec == "" {
			continue
		}
		path, baud, hasBaud := strings.Cut(spec, "@")
		e := Entry{ID: name, Port: path, Baud: sws.DefaultBaud}
		if hasBaud {
			b, err := strconv.Atoi(baud)
			if err != nil || b < 9600 || b > 4_000_000 {
				continue
			}
			e.Baud = b
		}
		if path == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// programmerAdapters are the USB-UART bridges used by the pvvx USB-UART/TB-0x
// SWire programmers (CH340, PL2303) and common equivalents.
var programmerAdapters = map[string]bool{
	"1a86:7523": true, "1a86:7522": true, "067b:2303": true, "067b:23a3": true,
	"10c4:ea60": true, "0403:6001": true, "0403:6015": true,
}

// candidates lists serial ports whose USB id matches a known programmer
// adapter. It only enumerates; it never opens a port or writes to it. Ports
// already listed explicitly are skipped.
func candidates(have []Entry, enum func() ([]uart.Info, error)) []Entry {
	infos, err := enum()
	if err != nil {
		return nil
	}
	used := map[string]bool{}
	for _, e := range have {
		used[e.Port] = true
	}
	var out []Entry
	for _, i := range infos {
		id := strings.ToLower(i.VID + ":" + i.PID)
		if !i.USB || !programmerAdapters[id] || used[i.Path] {
			continue
		}
		out = append(out, Entry{ID: i.ID(), Port: i.Path, Baud: sws.DefaultBaud, Candidate: true, USB: id, Product: i.Product})
	}
	return out
}

func defaultOpen(e Entry, baud int) (sws.Link, func() error, error) {
	if e.Sim != nil {
		return e.Sim, func() error { return nil }, nil
	}
	off := false
	m := uart.DefaultMode()
	m.Baud = baud
	m.DTR, m.RTS = &off, &off
	p, err := uart.OpenPort(e.Port, m)
	if err != nil {
		return nil, nil, err
	}
	return p, p.Close, nil
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
	return caps(map[string]string{
		"interface": "SWS", "backend": "native-go-swire", "target_drivers": "TLSR825x", "breakpoints": "1", "rtt": "no",
		"chip_erase": "not-offered", "flash_erase_program": "disabled until the board's protection map is verified",
		"factory_flash": "protected", "python_required": "no", "probe_rs_required": "no",
	})
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, e := range p.list() {
		d := &pluginv1.Device{Id: e.ID, Capabilities: deviceCaps()}
		if e.Sim != nil {
			d.Name, d.Transport = "Simulated Telink SWire programmer + TLSR8258 (SYNTHETIC)", "fake"
			d.Properties = map[string]string{"synthetic": "true", "validated": "false"}
		} else {
			d.Name, d.Transport = "Telink SWire programmer", "serial"
			d.Properties = map[string]string{"port": e.Port, "baud": strconv.Itoa(e.Baud), "backend": "native-go-swire"}
			if e.Candidate {
				d.Name = "USB-UART adapter, possible Telink SWire programmer (unprobed)"
				d.Properties["candidate"] = "true"
				d.Properties["usb"] = e.USB
				d.Properties["product"] = e.Product
				d.Properties["probed"] = "false"
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
	return Entry{}, status.Errorf(codes.NotFound, "telink programmer %q is not configured: list it in %s (name=/dev/ttyUSB0[@baud])", id, PortsEnv)
}

// options are the device-level settings.
type options struct {
	baud    int
	swdiv   int // 0: leave the programmer's setting
	timeout time.Duration
	layout  string // target.AutoLayout or a named board layout
}

var deviceKeys = []string{"baud", "swdiv", "timeout_ms", "layout"}

func parseOptions(base options, cfg map[string]any) (options, error) {
	o := base
	for k := range cfg {
		known := false
		for _, s := range deviceKeys {
			known = known || s == k
		}
		if !known {
			return o, fmt.Errorf("unknown setting %q (want %s)", k, strings.Join(deviceKeys, ", "))
		}
	}
	if v, ok, err := optNumber(cfg, "baud"); err != nil {
		return o, err
	} else if ok {
		if v < 9600 || v > 4_000_000 || v != float64(int(v)) {
			return o, fmt.Errorf("baud %v: want an integer 9600..4000000", v)
		}
		o.baud = int(v)
	}
	if v, ok, err := optNumber(cfg, "swdiv"); err != nil {
		return o, err
	} else if ok {
		if v < 1 || v > 255 || v != float64(int(v)) {
			return o, fmt.Errorf("swdiv %v: want an integer 1..255", v)
		}
		o.swdiv = int(v)
	}
	if v, ok, err := optNumber(cfg, "timeout_ms"); err != nil {
		return o, err
	} else if ok {
		if v < 20 || v > 60000 {
			return o, fmt.Errorf("timeout_ms %v: want 20..60000", v)
		}
		o.timeout = time.Duration(v) * time.Millisecond
	}
	if v, ok := cfg["layout"]; ok && v != nil {
		s, isStr := v.(string)
		if !isStr || !target.ValidLayout(s) {
			return o, fmt.Errorf("layout %v: want one of %s", v, strings.Join(target.AllLayoutNames(), ", "))
		}
		o.layout = s
	}
	return o, nil
}

func defaultOptions(e Entry) options {
	b := e.Baud
	if b == 0 {
		b = sws.DefaultBaud
	}
	return options{baud: b, timeout: 300 * time.Millisecond, layout: target.AutoLayout}
}

// device is an open programmer link.
type device struct {
	id    string
	entry Entry
	o     options

	mu     sync.Mutex // serialises operations
	prog   *sws.Programmer
	closer func() error
}

func (p *Plugin) connect(e Entry, o options) (*device, error) {
	link, closer, err := p.open(e, o.baud)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", e.ID, err)
	}
	pr := sws.New(link)
	pr.SetTimeout(o.timeout)
	d := &device{id: e.ID, entry: e, o: o, prog: pr, closer: closer}
	if _, err := pr.Version(); err != nil {
		_ = closer()
		return nil, status.Errorf(codes.Unavailable, "%s does not answer as a Telink SWire programmer at %d baud: %v", e.ID, o.baud, err)
	}
	if o.swdiv > 0 {
		v, _ := pr.Cached()
		if int(v.SWDiv) != o.swdiv {
			if err := pr.Configure(byte(o.swdiv), v.AddrLen, v.SWBuf); err != nil {
				_ = closer()
				return nil, status.Errorf(codes.Unavailable, "set swdiv %d: %v", o.swdiv, err)
			}
		}
	}
	return d, nil
}

func (d *device) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closer()
}

// OpenDevice implements pluginsdk.Plugin.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	e, err := p.find(id)
	if err != nil {
		return err
	}
	o, err := parseOptions(defaultOptions(e), cfg)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	d, err := p.connect(e, o)
	if err != nil {
		return err
	}
	p.mu.Lock()
	old := p.devs[id]
	p.devs[id] = d
	p.mu.Unlock()
	if old != nil {
		_ = old.close()
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
		return d.close()
	}
	return nil
}

// lease returns the open device, or opens one for a single call so that
// `hwlab invoke` works without a prior open. The programmer keeps the target's
// SWire state between calls; only the serial line is reopened.
func (p *Plugin) lease(id string, args map[string]any) (*device, func(), error) {
	over := pick(args, deviceKeys)
	p.mu.Lock()
	d := p.devs[id]
	p.mu.Unlock()
	if d != nil {
		if len(over) > 0 {
			return nil, nil, status.Errorf(codes.InvalidArgument, "%s is open: set %s when opening it, not per call", id, strings.Join(deviceKeys, ", "))
		}
		return d, func() {}, nil
	}
	e, err := p.find(id)
	if err != nil {
		return nil, nil, err
	}
	o, err := parseOptions(defaultOptions(e), over)
	if err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	d, err = p.connect(e, o)
	if err != nil {
		return nil, nil, err
	}
	return d, func() { _ = d.close() }, nil
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

// StartCapture implements pluginsdk.Plugin; a SWire programmer produces no stream.
func (p *Plugin) StartCapture(context.Context, pluginsdk.CaptureRequest, *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	return nil, nil, status.Error(codes.Unimplemented, "telink has no capture capability (SWS printf logging is not implemented)")
}
