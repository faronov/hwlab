// Package modbusplugin is the hwlab Modbus master. The protocol lives in
// internal/modbus; RTU runs on the generic uart plugin's port code, so there is
// no second serial implementation here.
package modbusplugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/modbus"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/uart"
)

// Environment variables that tell the plugin where Modbus devices are. A bus is
// never scanned: probing unit ids on a serial line can disturb other devices.
const (
	// HostsEnv lists devices as comma-separated name=address pairs, e.g.
	// "plc=tcp://192.168.1.20:502,meter=rtu:/dev/ttyUSB0@9600:8E1".
	HostsEnv = "HWLAB_MODBUS_HOSTS"
	// FakeEnv set to anything but "" or "0" offers an in-process SYNTHETIC
	// Modbus RTU slave for demos and fixtures.
	FakeEnv = "HWLAB_MODBUS_FAKE"
)

// FakeID is the id of the synthetic device.
const FakeID = "fake-modbus"

// Addr is where a device is.
type Addr struct {
	Kind   string // "tcp" or "rtu"
	Target string // host:port or serial device path
	Mode   uart.Mode
}

func (a Addr) String() string {
	if a.Kind == "tcp" {
		return "tcp://" + a.Target
	}
	return fmt.Sprintf("rtu:%s@%d:%d%s%s", a.Target, a.Mode.Baud, a.Mode.DataBits, strings.ToUpper(a.Mode.Parity[:1]), a.Mode.StopBits)
}

// ParseAddr reads tcp://host[:port] (port 502 by default) or
// rtu:<path>[@baud[:8E1]] (19200 8E1 by default, the Modbus serial-line
// default).
func ParseAddr(s string) (Addr, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "tcp://"):
		host := strings.TrimPrefix(s, "tcp://")
		if host == "" {
			return Addr{}, fmt.Errorf("%q: missing host", s)
		}
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(strings.Trim(host, "[]"), "502")
		}
		return Addr{Kind: "tcp", Target: host}, nil
	case strings.HasPrefix(s, "rtu:"):
		rest := strings.TrimPrefix(s, "rtu:")
		path, spec, _ := strings.Cut(rest, "@")
		if path == "" {
			return Addr{}, fmt.Errorf("%q: missing serial device", s)
		}
		m := uart.Mode{Baud: 19200, DataBits: 8, Parity: "even", StopBits: "1", Flow: uart.FlowNone}
		if spec != "" {
			baud, frame, hasFrame := strings.Cut(spec, ":")
			b, err := strconv.Atoi(baud)
			if err != nil || b < 300 || b > 4_000_000 {
				return Addr{}, fmt.Errorf("%q: baud %q", s, baud)
			}
			m.Baud = b
			if hasFrame {
				if len(frame) != 3 || frame[0] != '7' && frame[0] != '8' {
					return Addr{}, fmt.Errorf("%q: frame %q: want like 8N1, 8E1, 8O1 or 8N2", s, frame)
				}
				m.DataBits = int(frame[0] - '0')
				switch strings.ToUpper(frame[1:2]) {
				case "N":
					m.Parity = "none"
				case "E":
					m.Parity = "even"
				case "O":
					m.Parity = "odd"
				default:
					return Addr{}, fmt.Errorf("%q: parity %q: want N, E or O", s, frame[1:2])
				}
				if frame[2] != '1' && frame[2] != '2' {
					return Addr{}, fmt.Errorf("%q: stop bits %q: want 1 or 2", s, frame[2:])
				}
				m.StopBits = frame[2:]
			}
		}
		return Addr{Kind: "rtu", Target: path, Mode: m}, nil
	}
	return Addr{}, fmt.Errorf("%q: want tcp://host[:port] or rtu:/dev/ttyUSB0[@baud[:8E1]]", s)
}

// Entry is one device the plugin can open.
type Entry struct {
	ID     string
	Addr   Addr
	Source string // "env" or "fake"
	Fake   *Fake
}

// Lister enumerates devices.
type Lister func() ([]Entry, error)

// Opener connects an entry.
type Opener func(e Entry, timeout time.Duration) (modbus.Transport, error)

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
	cl    *modbus.Client
	opts  options

	mu  sync.Mutex // guards cap
	cap *capture
}

// New returns the plugin for the host's configuration.
func New() *Plugin {
	var fake *Fake
	if v := os.Getenv(FakeEnv); v != "" && v != "0" {
		fake = NewFake()
	}
	list := func() ([]Entry, error) {
		es := parseHosts(os.Getenv(HostsEnv))
		if fake != nil {
			es = append(es, Entry{ID: FakeID, Source: "fake", Fake: fake})
		}
		return es, nil
	}
	return NewWith(list, defaultOpen)
}

// NewWith injects device enumeration and connection.
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*device{}}
}

func defaultOpen(e Entry, timeout time.Duration) (modbus.Transport, error) {
	if e.Fake != nil {
		return e.Fake.Dial(), nil
	}
	switch e.Addr.Kind {
	case "tcp":
		return modbus.DialTCP(e.Addr.Target, timeout)
	case "rtu":
		port, err := uart.OpenPort(e.Addr.Target, e.Addr.Mode)
		if err != nil {
			return nil, err
		}
		t := modbus.NewRTU(port, e.Addr.Mode.Baud, e.Addr.String())
		t.Timeout = timeout
		return t, nil
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
		a, err := ParseAddr(addr)
		if err != nil {
			continue
		}
		out = append(out, Entry{ID: name, Addr: a, Source: "env"})
	}
	return out
}

func caps() []*pluginv1.Capability {
	ids := []string{pluginv1.CapModbus, pluginv1.CapProtocolDecode, pluginv1.CapPacketCapture, pluginv1.CapTimedCapture}
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
		transport := e.Addr.Kind
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
	return Entry{}, status.Errorf(codes.NotFound, "modbus device %q is not configured: list it in %s (name=tcp://host:502 or name=rtu:/dev/ttyUSB0@9600:8E1)", id, HostsEnv)
}

func (p *Plugin) device(id string) *device {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.devs[id]
}

func (p *Plugin) connect(e Entry, o options) (*device, error) {
	tr, err := p.open(e, o.timeout)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", e.ID, err)
	}
	switch t := tr.(type) {
	case *modbus.RTU:
		t.Timeout = o.timeout
	case *modbus.TCP:
		t.Timeout = o.timeout
	case *fakeTransport:
		t.Timeout = o.timeout
	}
	return &device{id: e.ID, entry: e, cl: &modbus.Client{T: tr, Retries: o.retries}, opts: o}, nil
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
		_ = old.cl.T.Close()
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
		return d.cl.T.Close()
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
	return d, func() { _ = d.cl.T.Close() }, nil
}

// options are the device-level settings.
type options struct {
	unit    byte
	timeout time.Duration
	retries int
}

// The core hands the same configuration map to open and to capture, so each
// side tolerates the other's keys, but a key in neither set is a typo.
var (
	deviceKeys  = []string{"unit", "timeout_ms", "retries"}
	captureKeys = []string{"polls"}
)

func checkKeys(cfg map[string]any) error {
	for k := range cfg {
		known := false
		for _, set := range [][]string{deviceKeys, captureKeys} {
			for _, s := range set {
				known = known || s == k
			}
		}
		if !known {
			return fmt.Errorf("unknown setting %q (device: %s; capture: %s)", k, strings.Join(deviceKeys, ", "), strings.Join(captureKeys, ", "))
		}
	}
	return nil
}

func parseOptions(cfg map[string]any) (options, error) {
	o := options{unit: 1, timeout: time.Second}
	if err := checkKeys(cfg); err != nil {
		return o, err
	}
	if v, ok, err := optNumber(cfg, "unit"); err != nil {
		return o, err
	} else if ok {
		if v < 0 || v > 255 || v != float64(int(v)) {
			return o, fmt.Errorf("unit %v: want an integer 0..255", v)
		}
		o.unit = byte(v)
	}
	if v, ok, err := optNumber(cfg, "timeout_ms"); err != nil {
		return o, err
	} else if ok {
		if v < 20 || v > 60000 {
			return o, fmt.Errorf("timeout_ms %v: want 20..60000", v)
		}
		o.timeout = time.Duration(v) * time.Millisecond
	}
	if v, ok, err := optNumber(cfg, "retries"); err != nil {
		return o, err
	} else if ok {
		if v < 0 || v > 5 || v != float64(int(v)) {
			return o, fmt.Errorf("retries %v: want an integer 0..5 (applies to reads only)", v)
		}
		o.retries = int(v)
	}
	return o, nil
}
