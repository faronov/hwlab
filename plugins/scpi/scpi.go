package scpiplugin

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/scpi"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/uart"
)

// Environment variables that tell the plugin where instruments are. LAN
// instruments cannot be enumerated safely (no VXI-11/mDNS discovery is done),
// and serial ports are never probed with *IDN?, which could upset a device that
// is not an instrument.
const (
	// HostsEnv lists instruments as comma-separated name=address pairs, e.g.
	// "scope=tcp://192.168.1.50,psu=serial:/dev/ttyUSB1@9600".
	HostsEnv = "HWLAB_SCPI_HOSTS"
	// FakeEnv ("rigol" or "siglent") offers an in-process SYNTHETIC oscilloscope
	// for demos and fixtures.
	FakeEnv = "HWLAB_SCPI_FAKE"
)

// Entry is one instrument the plugin can open.
type Entry struct {
	ID     string
	Addr   scpi.Addr
	Source string // "env", "usbtmc" or "fake"
	Fake   *scpi.Fake
}

// Lister enumerates instruments.
type Lister func() ([]Entry, error)

// Opener connects an entry.
type Opener func(e Entry, timeout time.Duration) (scpi.Transport, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener

	mu   sync.Mutex
	devs map[string]*device
}

type device struct {
	id    string
	entry Entry
	inst  *scpi.Instrument
	idn   scpi.IDN
	scope *scpi.Scope
	opts  options

	mu  sync.Mutex // guards cap
	cap *capture
}

// New returns the plugin for the host's instruments.
func New() *Plugin {
	var fake *scpi.Fake
	fakeID := ""
	if v := strings.ToLower(os.Getenv(FakeEnv)); v != "" {
		if p, ok := scpi.ProfileByID(v); ok && !p.Scopeless() {
			fake, fakeID = scpi.NewFake(p), "fake-"+p.ID
		}
	}
	list := func() ([]Entry, error) {
		es := parseHosts(os.Getenv(HostsEnv))
		es = append(es, usbtmcEntries()...)
		if fake != nil {
			es = append(es, Entry{ID: fakeID, Source: "fake", Fake: fake})
		}
		return es, nil
	}
	return NewWith(list, defaultOpen)
}

// NewWith injects instrument enumeration and connection.
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*device{}}
}

func defaultOpen(e Entry, timeout time.Duration) (scpi.Transport, error) {
	if e.Fake != nil {
		return e.Fake.Dial(), nil
	}
	switch e.Addr.Kind {
	case scpi.KindTCP:
		return scpi.DialTCP(e.Addr.Target, timeout)
	case scpi.KindUSBTMC:
		return scpi.OpenUSBTMC(e.Addr.Target)
	case scpi.KindSerial:
		port, err := uart.OpenPort(e.Addr.Target, uart.Mode{Baud: e.Addr.Baud, DataBits: 8, Parity: "none", StopBits: "1", Flow: uart.FlowNone})
		if err != nil {
			return nil, err
		}
		return &serialTransport{port: port, desc: e.Addr.String()}, nil
	}
	return nil, fmt.Errorf("unsupported address kind %q", e.Addr.Kind)
}

// parseHosts reads HostsEnv; malformed entries are skipped.
func parseHosts(v string) []Entry {
	var out []Entry
	for _, kv := range strings.Split(v, ",") {
		name, addr, ok := strings.Cut(strings.TrimSpace(kv), "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			continue
		}
		a, err := scpi.ParseAddr(addr)
		if err != nil {
			continue
		}
		out = append(out, Entry{ID: name, Addr: a, Source: "env"})
	}
	return out
}

// usbtmcEntries lists the Linux usbtmc device nodes.
func usbtmcEntries() []Entry {
	nodes, _ := filepath.Glob("/dev/usbtmc*")
	sort.Strings(nodes)
	out := make([]Entry, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, Entry{ID: filepath.Base(n), Addr: scpi.Addr{Kind: scpi.KindUSBTMC, Target: n}, Source: "usbtmc"})
	}
	return out
}

// serialTransport adapts a uart.Port.
type serialTransport struct {
	port uart.Port
	desc string
}

func (s *serialTransport) Write(p []byte) error {
	_, err := s.port.Write(p)
	return err
}

func (s *serialTransport) Read(p []byte, timeout time.Duration) (int, error) {
	if err := s.port.SetReadTimeout(timeout); err != nil {
		return 0, err
	}
	n, err := s.port.Read(p)
	if n == 0 && err == nil {
		return 0, scpi.ErrTimeout
	}
	if n == 0 && err == io.EOF {
		return 0, scpi.ErrTimeout
	}
	return n, err
}

func (s *serialTransport) Close() error     { return s.port.Close() }
func (s *serialTransport) Describe() string { return s.desc }

func caps() []*pluginv1.Capability {
	ids := []string{pluginv1.CapSCPI, pluginv1.CapOscilloscope, pluginv1.CapAnalogCapture, pluginv1.CapTimedCapture, pluginv1.CapOfflineCapture, pluginv1.CapArtifactExport}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1}
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
		transport := string(e.Addr.Kind)
		props := map[string]string{"source": e.Source}
		if e.Fake != nil {
			transport = "fake"
			props["synthetic"] = "true"
		} else {
			props["address"] = e.Addr.String()
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
	return Entry{}, status.Errorf(codes.NotFound, "instrument %q is not configured: list it in %s (name=tcp://host or name=serial:/dev/ttyUSB0@9600); /dev/usbtmc* nodes are found automatically", id, HostsEnv)
}

func (p *Plugin) device(id string) *device {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.devs[id]
}

// connect opens the transport, identifies the instrument and picks a profile.
func (p *Plugin) connect(e Entry, o options) (*device, error) {
	tr, err := p.open(e, o.timeout)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", e.ID, err)
	}
	inst := scpi.NewInstrument(tr)
	inst.Timeout = o.timeout
	idn, err := inst.Identify()
	if err != nil {
		_ = inst.Close()
		return nil, status.Errorf(codes.Unavailable, "%s did not answer *IDN? (%v): check the address, that it speaks SCPI, and the serial baud rate", e.ID, err)
	}
	prof := scpi.Detect(idn)
	if o.profile != "" {
		pr, ok := scpi.ProfileByID(o.profile)
		if !ok {
			_ = inst.Close()
			return nil, status.Errorf(codes.InvalidArgument, "profile %q: want rigol, siglent or generic", o.profile)
		}
		prof = pr
	}
	return &device{id: e.ID, entry: e, inst: inst, idn: idn, scope: scpi.NewScope(inst, prof, o.channels), opts: o}, nil
}

// OpenDevice implements pluginsdk.Plugin.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	e, err := p.find(id)
	if err != nil {
		return err
	}
	o, err := parseOptions(cfg)
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
		_ = old.inst.Close()
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
		return d.inst.Close()
	}
	return nil
}

// lease returns the open device, or opens one for the duration of a single
// call so that `hwlab invoke` works without a prior open.
func (p *Plugin) lease(id string, cfg map[string]any) (*device, func(), error) {
	if d := p.device(id); d != nil {
		return d, func() {}, nil
	}
	e, err := p.find(id)
	if err != nil {
		return nil, nil, err
	}
	o, err := parseOptions(cfg)
	if err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	d, err := p.connect(e, o)
	if err != nil {
		return nil, nil, err
	}
	return d, func() { _ = d.inst.Close() }, nil
}

// options are the device-level settings.
type options struct {
	timeout  time.Duration
	profile  string
	channels int
}

// deviceKeys and captureKeys are the settings of the plugin. The core hands the
// same configuration map to open and to capture, so each side tolerates the
// other's keys, but a key in neither set is a typo.
var (
	deviceKeys  = []string{"timeout_ms", "profile", "channel_count"}
	captureKeys = []string{"channels", "trigger", "trigger_timeout_s", "screenshot", "waveform"}
)

func knownKey(k string) bool {
	for _, set := range [][]string{deviceKeys, captureKeys} {
		for _, s := range set {
			if s == k {
				return true
			}
		}
	}
	return false
}

func checkKeys(cfg map[string]any) error {
	for k := range cfg {
		if !knownKey(k) {
			return fmt.Errorf("unknown setting %q (device: %s; capture: %s)", k, strings.Join(deviceKeys, ", "), strings.Join(captureKeys, ", "))
		}
	}
	return nil
}

func parseOptions(cfg map[string]any) (options, error) {
	o := options{timeout: scpi.DefaultTimeout}
	if err := checkKeys(cfg); err != nil {
		return o, err
	}
	if v, ok, err := optNumber(cfg, "timeout_ms"); err != nil {
		return o, err
	} else if ok {
		if v < 50 || v > 600000 {
			return o, fmt.Errorf("timeout_ms %v: want 50..600000", v)
		}
		o.timeout = time.Duration(v) * time.Millisecond
	}
	if v, ok, err := optNumber(cfg, "channel_count"); err != nil {
		return o, err
	} else if ok {
		if v < 1 || v > scpi.MaxChannels || v != float64(int(v)) {
			return o, fmt.Errorf("channel_count %v: want an integer 1..%d", v, scpi.MaxChannels)
		}
		o.channels = int(v)
	}
	if s, ok := cfg["profile"].(string); ok {
		o.profile = strings.ToLower(s)
	} else if _, present := cfg["profile"]; present {
		return o, fmt.Errorf("profile must be a string")
	}
	return o, nil
}
