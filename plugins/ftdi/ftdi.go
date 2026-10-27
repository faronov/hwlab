// Package ftdiplugin is the hwlab plugin for FTDI MPSSE JTAG adapters, with
// the Sipeed RV-Debugger-BL702 (an FT2232D emulator) as a built-in profile.
// It talks to the adapter natively: Linux usbfs or macOS IOKit, the FTDI control/bulk
// protocol, an MPSSE command engine and a JTAG TAP driver. There is no
// libusb, no cgo, no OpenOCD and no Python.
//
// Scope: FTDI/MPSSE/JTAG is a probe transport. The plugin advertises only
// JTAGTransport, JTAGScan and TargetIdentify; it does not claim DebugProbe,
// MemoryAccess, FlashProgramming, halt, resume or registers, because no CPU
// debug protocol sits above the TAP yet.
//
// Only the adapter's JTAG interface is claimed. A companion UART interface
// stays with the kernel serial driver and is served by the uart plugin.
package ftdiplugin

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/ftdi/ftdi"
	"github.com/faronov/hwlab/plugins/ftdi/profiles"
	"github.com/faronov/hwlab/plugins/ftdi/sim"
)

// SimEnv selects a SYNTHETIC adapter: "1" has one TAP, "empty" has no target
// (TDO floating high, like an unconnected Sipeed adapter), "chain" has three.
const SimEnv = "HWLAB_FTDI_SIM"

// SimID is the id of the simulated adapter.
const SimID = "sim-ftdi"

// Entry is one adapter the plugin can open.
type Entry struct {
	ID  string
	Dev usb.Device
	Sim *sim.Config
}

// Lister enumerates adapters.
type Lister func() []Entry

// Opener claims one interface of an adapter.
type Opener func(e Entry, iface int, opt usb.OpenOptions) (usb.Port, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener

	mu   sync.Mutex
	devs map[string]*session
}

// New returns the plugin for the host's adapters.
func New() *Plugin {
	var simCfg *sim.Config
	switch os.Getenv(SimEnv) {
	case "1":
		c := sim.Config{Sipeed: true, FloatHigh: true, Chain: []sim.TAP{{IDCode: 0x1BA01477, IRLen: 4}}}
		simCfg = &c
	case "chain":
		c := sim.Config{Sipeed: true, FloatHigh: true, Chain: []sim.TAP{{IDCode: 0x1BA01477, IRLen: 4}, {IRLen: 5}, {IDCode: 0x0362D093, IRLen: 6}}}
		simCfg = &c
	case "empty":
		c := sim.Config{Sipeed: true, FloatHigh: true}
		simCfg = &c
	}
	list := func() []Entry {
		var es []Entry
		for _, d := range usb.Scan("/sys", ftdi.IsKnown) {
			es = append(es, Entry{Dev: d})
		}
		if simCfg != nil {
			es = append(es, Entry{ID: SimID, Dev: sim.USBDevice(0), Sim: simCfg})
		}
		return nameEntries(es)
	}
	return NewWith(list, defaultOpen)
}

// NewWith injects enumeration and interface opening (tests).
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*session{}}
}

func defaultOpen(e Entry, iface int, opt usb.OpenOptions) (usb.Port, error) {
	if e.Sim != nil {
		cfg := *e.Sim
		cfg.Channel = iface
		if opt.DetachDriver != "" {
			cfg.DetachedDriver = opt.DetachDriver
		}
		return sim.New(cfg), nil
	}
	return usb.Open(e.Dev, iface, opt)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, s)
}

// nameEntries assigns stable ids: vid:pid:serial, or vid:pid@sysname without
// a serial or when two adapters share one.
func nameEntries(es []Entry) []Entry {
	count := map[string]int{}
	base := func(e Entry) string {
		if e.Dev.Serial == "" {
			return ""
		}
		return fmt.Sprintf("%04x:%04x:%s", e.Dev.VID, e.Dev.PID, sanitize(e.Dev.Serial))
	}
	for _, e := range es {
		if e.ID == "" {
			count[base(e)]++
		}
	}
	for i := range es {
		if es[i].ID != "" {
			continue
		}
		b := base(es[i])
		if b == "" || count[b] > 1 {
			b = fmt.Sprintf("%04x:%04x@%s", es[i].Dev.VID, es[i].Dev.PID, es[i].Dev.SysName)
		}
		es[i].ID = b
	}
	sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID })
	return es
}

func capAttrs(prof profiles.Profile) map[string]string {
	return map[string]string{
		"interface":      "JTAG",
		"transport":      "ftdi-mpsse",
		"profile":        prof.Name,
		"implementation": string(prof.Implementation),
		"cpu_debug":      "no",
		"memory_access":  "no",
		"flash":          "no",
	}
}

func deviceCaps(prof profiles.Profile) []*pluginv1.Capability {
	ids := []string{pluginv1.CapJTAGTransport, pluginv1.CapJTAGScan, pluginv1.CapTargetIdentify}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: capAttrs(prof)}
	}
	return out
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, e := range p.list() {
		prof := profiles.Match(e.Dev.VID, e.Dev.PID, e.Dev.Manufacturer, e.Dev.Product)
		d := &pluginv1.Device{Id: e.ID, Serial: e.Dev.Serial, Capabilities: deviceCaps(prof)}
		props := map[string]string{
			"vid": fmt.Sprintf("%04x", e.Dev.VID), "pid": fmt.Sprintf("%04x", e.Dev.PID),
			"manufacturer": e.Dev.Manufacturer, "product": e.Dev.Product,
			"bcd_device": fmt.Sprintf("0x%04x", e.Dev.BCD), "chip": ftdi.ChipFromBCD(e.Dev.BCD).String(),
			"configuration": strconv.Itoa(e.Dev.Configuration), "interfaces": strconv.Itoa(len(e.Dev.Interfaces)),
			"profile": prof.Name, "implementation": string(prof.Implementation),
			"jtag_interface": strconv.Itoa(prof.JTAGInterface), "jtag_channel": channelName(prof.JTAGInterface),
			"validated": "false",
		}
		if ji, ok := e.Dev.Iface(prof.JTAGInterface); ok {
			props["jtag_kernel_driver"] = ji.Driver
		}
		if prof.UARTInterface >= 0 {
			if ui, ok := e.Dev.Iface(prof.UARTInterface); ok {
				props["companion_uart_interface"] = strconv.Itoa(ui.Number)
				props["companion_uart_channel"] = channelName(ui.Number)
				if ui.TTY != "" {
					props["companion_uart_tty"] = "/dev/" + ui.TTY
				}
				props["companion_uart_driver"] = ui.Driver
			}
		}
		if e.Sim != nil {
			d.Name, d.Transport = "Simulated FTDI MPSSE adapter (SYNTHETIC)", "fake"
			props["synthetic"] = "true"
		} else {
			d.Name, d.Transport = strings.TrimSpace(e.Dev.Manufacturer+" "+e.Dev.Product), "usb"
			if d.Name == "" {
				d.Name = "FTDI MPSSE adapter"
			}
			props["usb_address"] = fmt.Sprintf("%d:%d", e.Dev.Bus, e.Dev.Address)
			props["usb_device"] = e.Dev.SysName // matches the uart plugin's usb_device for the companion tty
		}
		d.Properties = props
		out = append(out, d)
	}
	return out, nil
}

func channelName(i int) string {
	if i >= 0 && i < 4 {
		return string(rune('A' + i))
	}
	return strconv.Itoa(i)
}

func (p *Plugin) find(id string) (Entry, error) {
	for _, e := range p.list() {
		if e.ID == id {
			return e, nil
		}
	}
	return Entry{}, status.Errorf(codes.NotFound, "FTDI adapter %q is not connected", id)
}

var deviceKeys = []string{"profile", "interface", "clock_hz", "detach_kernel_driver", "latency_ms", "timeout_ms"}

func optNumber(a map[string]any, key string) (float64, bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok || f != f {
		return 0, false, fmt.Errorf("%s: want a number, got %v", key, v)
	}
	return f, true, nil
}

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
	if v, ok := cfg["profile"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return o, fmt.Errorf("profile: want a string, got %v", v)
		}
		o.profile = s
	}
	if v, ok := cfg["detach_kernel_driver"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return o, fmt.Errorf("detach_kernel_driver: want true or false, got %v", v)
		}
		o.detach = b
	}
	if v, ok, err := optNumber(cfg, "interface"); err != nil {
		return o, err
	} else if ok {
		if v < 0 || v > 3 || v != float64(int(v)) {
			return o, fmt.Errorf("interface %v: want 0..3", v)
		}
		o.iface = int(v)
	}
	if v, ok, err := optNumber(cfg, "clock_hz"); err != nil {
		return o, err
	} else if ok {
		if v < 100 || v > 30_000_000 || v != float64(uint32(v)) {
			return o, fmt.Errorf("clock_hz %v: want an integer 100..30000000", v)
		}
		o.clockHz = uint32(v)
	}
	if v, ok, err := optNumber(cfg, "latency_ms"); err != nil {
		return o, err
	} else if ok {
		if v < 1 || v > 255 || v != float64(int(v)) {
			return o, fmt.Errorf("latency_ms %v: want 1..255", v)
		}
		o.latencyMs = int(v)
	}
	if v, ok, err := optNumber(cfg, "timeout_ms"); err != nil {
		return o, err
	} else if ok {
		if v < 50 || v > 60000 {
			return o, fmt.Errorf("timeout_ms %v: want 50..60000", v)
		}
		o.timeout = durationMs(v)
	}
	return o, nil
}

// OpenDevice implements pluginsdk.Plugin: it claims the JTAG interface and
// brings the adapter into MPSSE mode.
func (p *Plugin) OpenDevice(ctx context.Context, id string, cfg map[string]any) error {
	e, err := p.find(id)
	if err != nil {
		return err
	}
	o, err := parseOptions(defaultOptions(), cfg)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	p.mu.Lock()
	old := p.devs[id]
	delete(p.devs, id)
	p.mu.Unlock()
	if old != nil {
		_ = old.close() // release the interface before claiming it again
	}
	s, err := openSession(ctx, e, o, p.open)
	if err != nil {
		return status.Errorf(openCode(err), "open %s: %v", id, transportErr(err))
	}
	p.mu.Lock()
	p.devs[id] = s
	p.mu.Unlock()
	return nil
}

func openCode(err error) codes.Code {
	switch {
	case isErr(err, errInvalid):
		return codes.InvalidArgument
	case isErr(err, usb.ErrPermission):
		return codes.PermissionDenied
	case isErr(err, usb.ErrBusy):
		return codes.FailedPrecondition
	}
	return codes.Unavailable
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	s := p.devs[id]
	delete(p.devs, id)
	p.mu.Unlock()
	if s != nil {
		return s.close()
	}
	return nil
}

// lease returns the open session, or opens one for a single call so that
// `hwlab invoke` works without a prior open.
func (p *Plugin) lease(ctx context.Context, id string, args map[string]any) (*session, func(), error) {
	over := map[string]any{}
	for _, k := range deviceKeys {
		if v, ok := args[k]; ok {
			over[k] = v
		}
	}
	p.mu.Lock()
	s := p.devs[id]
	p.mu.Unlock()
	if s != nil {
		if len(over) > 0 {
			return nil, nil, status.Error(codes.InvalidArgument, "device settings cannot change on an open device: close it and open it again with the new settings")
		}
		return s, func() {}, nil
	}
	o, err := parseOptions(defaultOptions(), over)
	if err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	e, err := p.find(id)
	if err != nil {
		return nil, nil, err
	}
	s, err = openSession(ctx, e, o, p.open)
	if err != nil {
		return nil, nil, status.Errorf(openCode(err), "open %s: %v", id, transportErr(err))
	}
	return s, func() { _ = s.close() }, nil
}

// StartCapture implements pluginsdk.Plugin; a JTAG transport produces no stream.
func (p *Plugin) StartCapture(context.Context, pluginsdk.CaptureRequest, *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	return nil, nil, status.Error(codes.Unimplemented, "ftdi has no capture capability")
}
