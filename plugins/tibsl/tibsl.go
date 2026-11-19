// Package tibslplugin is the hwlab plugin for the TI CC13xx/CC26xx and
// CC2538 ROM serial bootloader (BSL) over a UART: CC2652P/R/P7/R7,
// CC1352P/R/P7, CC1312R, CC26x0/CC13x0 and CC2538. It speaks the ROM
// protocol natively (plugins/tibsl/bsl); cc2538-bsl and Python are not used.
//
// ENTRY. A port is listed in HWLAB_TIBSL_PORTS as
// name=/dev/ttyUSB0[@baud][:entry]; entry is "manual" (hold the BSL button
// while resetting), "dtr-rts" (Sonoff ZBDongle-P style: RTS drives RESET,
// DTR the backdoor pin DIO15, both lines changed in one ioctl) or "rts-dtr"
// (roles swapped). With a wired entry every connection resets the chip into
// the bootloader; it stays there until FlashProgramming.reset.
//
// SAFETY. Reads never write. Erase, program and restore are CONFIRM methods,
// need their own token and a verified full backup of the same chip (FCFG1
// IEEE, ICEPICK id, USER_ID, flash size). The flash sector holding the CCFG
// (CC26xx) or CCA (CC2538) is a protected region: changing it needs
// confirm_config="modify-ccfg", and a resulting configuration that would
// disable the bootloader or its backdoor, move the backdoor pin, lock the
// CPU debug port, disable chip/bank erase, newly disable a TAP or set sector
// write protection is refused unless ccfg_override="allow-bsl-lockout" (a
// bricked dongle can only be recovered with JTAG). Bank erase needs its own
// token too. MEMORY_WRITE and SET_CCFG are never sent.
//
// STATUS: see internal/status (entry "tibsl") and docs/hardware.md.
package tibslplugin

import (
	"context"
	"errors"
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
	"github.com/faronov/hwlab/plugins/tibsl/bsl"
	"github.com/faronov/hwlab/plugins/uart"
)

const (
	// PortsEnv lists BSL ports: name=/dev/ttyUSB0[@baud][:manual|dtr-rts|rts-dtr].
	PortsEnv = "HWLAB_TIBSL_PORTS"
	// SimEnv ("1") offers in-process SYNTHETIC chips.
	SimEnv = "HWLAB_TIBSL_SIM"
	// SimID is a simulated CC2652P dongle wired for dtr-rts entry.
	SimID = "sim-cc2652p"
	// Sim2538ID is a simulated CC2538 put into the bootloader by hand.
	Sim2538ID = "sim-cc2538"

	defaultBaud = 460800
)

// Entry is one BSL port the plugin can open.
type Entry struct {
	ID    string
	Port  string
	Baud  int
	Entry bsl.Entry
	Sim   *bsl.Sim
}

// Conn is an open serial line to the chip.
type Conn struct {
	Link  bsl.Link
	Lines bsl.Lines
	Close func() error
}

// Lister enumerates configured ports.
type Lister func() []Entry

// Opener opens the line of an entry.
type Opener func(e Entry, baud int) (*Conn, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener

	mu   sync.Mutex
	devs map[string]*device
}

// New returns the plugin for the host's configuration.
func New() *Plugin {
	var sims []Entry
	if os.Getenv(SimEnv) == "1" {
		s := bsl.NewSim()
		s.Wired = true
		s.PowerOn()
		sims = append(sims, Entry{ID: SimID, Entry: bsl.EntryDTRRTS, Baud: defaultBaud, Sim: s},
			Entry{ID: Sim2538ID, Entry: bsl.EntryManual, Baud: defaultBaud, Sim: bsl.NewSim2538()})
	}
	list := func() []Entry { return append(parsePorts(os.Getenv(PortsEnv)), sims...) }
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
		e := Entry{ID: name, Baud: defaultBaud, Entry: bsl.EntryManual}
		if i := strings.LastIndex(spec, ":"); i > 0 {
			if en, err := bsl.ParseEntry(spec[i+1:]); err == nil {
				e.Entry, spec = en, spec[:i]
			}
		}
		path, baud, hasBaud := strings.Cut(spec, "@")
		if path == "" {
			continue
		}
		e.Port = path
		if hasBaud {
			b, err := strconv.Atoi(baud)
			if err != nil || b < 9600 || b > 1_500_000 {
				continue
			}
			e.Baud = b
		}
		out = append(out, e)
	}
	return out
}

// openSettle lets a freshly opened port settle before the first command.
const openSettle = 30 * time.Millisecond

func defaultOpen(e Entry, baud int) (*Conn, error) {
	if e.Sim != nil {
		e.Sim.Reopen()
		return &Conn{Link: e.Sim, Close: func() error { return nil },
			Lines: func(dtr, rts bool) (bool, error) { return true, e.Sim.SetLines(dtr, rts) }}, nil
	}
	// Equal levels release RESET and the backdoor pin on the wired boards.
	off := false
	m := uart.DefaultMode()
	m.Baud = baud
	m.DTR, m.RTS = &off, &off
	p, err := uart.OpenPort(e.Port, m)
	if err != nil {
		return nil, err
	}
	time.Sleep(openSettle)
	if err := p.ResetInput(); err != nil {
		return nil, errors.Join(err, p.Close())
	}
	return &Conn{Link: p, Close: p.Close, Lines: func(dtr, rts bool) (bool, error) { return uart.SetModemLines(p, dtr, rts) }}, nil
}

func caps(attrs map[string]string) []*pluginv1.Capability {
	ids := []string{pluginv1.CapTargetIdentify, pluginv1.CapFlashProgramming, pluginv1.CapMemoryAccess}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: attrs}
	}
	return out
}

func deviceCaps() []*pluginv1.Capability {
	return caps(map[string]string{
		"interface": "TI ROM serial bootloader (UART)", "backend": "native-go-ti-bsl", "targets": "CC13x2/CC26x2 (P/R/x7), CC13x0/CC26x0, CC2538",
		"memory_access": "read only (MEMORY_WRITE is never sent)", "protected_region": "CCFG/CCA sector (confirm_config=modify-ccfg, lockout check)",
		"python_required": "no",
	})
}

// ListDevices implements pluginsdk.Plugin. It never opens a port.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, e := range p.list() {
		d := &pluginv1.Device{Id: e.ID, Capabilities: deviceCaps(), Transport: "serial"}
		props := map[string]string{"baud": strconv.Itoa(e.Baud), "bsl_entry": string(e.Entry), "backend": "native-go-ti-bsl", "probed": "false"}
		if e.Sim != nil {
			d.Name, d.Transport = "Simulated TI ROM bootloader (SYNTHETIC)", "fake"
			props["synthetic"], props["validated"] = "true", "false"
			props["family"] = e.Sim.Family.String()
		} else {
			d.Name = "TI CC13xx/CC26xx/CC2538 ROM serial bootloader"
			props["port"] = e.Port
		}
		d.Properties = props
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
	return Entry{}, status.Errorf(codes.NotFound, "tibsl port %q is not configured: list it in %s as name=/dev/ttyUSB0[@baud][:manual|dtr-rts|rts-dtr]", id, PortsEnv)
}

// options are the device-level settings.
type options struct {
	baud    int
	timeout time.Duration
	entry   bsl.Entry
}

var deviceKeys = []string{"baud", "timeout_ms", "bsl_entry"}

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
		if v < 9600 || v > 1_500_000 || v != float64(int(v)) {
			return o, fmt.Errorf("baud %v: want an integer 9600..1500000", v)
		}
		o.baud = int(v)
	}
	if v, ok, err := optNumber(cfg, "timeout_ms"); err != nil {
		return o, err
	} else if ok {
		if v < 100 || v > 60000 {
			return o, fmt.Errorf("timeout_ms %v: want 100..60000", v)
		}
		o.timeout = time.Duration(v) * time.Millisecond
	}
	if v, ok := cfg["bsl_entry"]; ok && v != nil {
		s, isStr := v.(string)
		if !isStr {
			return o, fmt.Errorf("bsl_entry: want a string, got %v", v)
		}
		en, err := bsl.ParseEntry(s)
		if err != nil {
			return o, err
		}
		o.entry = en
	}
	return o, nil
}

func defaultOptions(e Entry) options {
	b := e.Baud
	if b == 0 {
		b = defaultBaud
	}
	en := e.Entry
	if en == "" {
		en = bsl.EntryManual
	}
	return options{baud: b, timeout: 2 * time.Second, entry: en}
}

// device is a synchronized bootloader link.
type device struct {
	id    string
	entry Entry
	o     options

	mu       sync.Mutex
	cl       *bsl.Client
	conn     *Conn
	chip     *bsl.Chip
	atomic   bool
	resynced bool
	// left is set once the chip was reset out of the bootloader.
	left bool
}

func (p *Plugin) connect(ctx context.Context, e Entry, o options) (*device, error) {
	c, err := p.open(e, o.baud)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", e.ID, err)
	}
	fail := func(err error) (*device, error) {
		_ = c.Close()
		return nil, err
	}
	cl := bsl.New(c.Link)
	cl.Timeout = o.timeout
	cl.SetContext(ctx)
	defer cl.SetContext(context.Background())
	d := &device{id: e.ID, entry: e, o: o, cl: cl, conn: c}
	if o.entry.Wired() {
		if d.atomic, err = bsl.Enter(ctx, c.Lines, o.entry); err != nil {
			return fail(status.Errorf(codes.Unavailable, "%s: bootloader entry: %v", e.ID, err))
		}
	}
	d.resynced, err = cl.Connect(o.timeout)
	if o.entry.Wired() {
		if rerr := bsl.Release(c.Lines, o.entry); rerr != nil && err == nil {
			err = rerr
		}
	}
	if err != nil {
		hint := "hold the BSL button (DIO15 on a ZBDongle-P) while resetting the board, then retry; or configure the DTR/RTS entry (bsl_entry=dtr-rts or rts-dtr)"
		if o.entry.Wired() {
			hint = fmt.Sprintf("the %s line sequence did not start the bootloader: check the wiring (try the other role assignment), that the CCFG backdoor is enabled on that pin, and that nothing else holds the port", o.entry)
		}
		return fail(status.Errorf(codes.Unavailable, "%s: %v: %s", e.ID, err, hint))
	}
	if d.chip, err = bsl.Identify(cl); err != nil {
		return fail(status.Errorf(codes.Unavailable, "%s: identify: %v", e.ID, err))
	}
	return d, nil
}

func (d *device) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conn.Close()
}

// OpenDevice implements pluginsdk.Plugin.
func (p *Plugin) OpenDevice(ctx context.Context, id string, cfg map[string]any) error {
	e, err := p.find(id)
	if err != nil {
		return err
	}
	o, err := parseOptions(defaultOptions(e), cfg)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	d, err := p.connect(ctx, e, o)
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

// lease returns the open device, or connects for a single call.
func (p *Plugin) lease(ctx context.Context, id string, args map[string]any) (*device, func(), error) {
	over := map[string]any{}
	for _, k := range deviceKeys {
		if v, ok := args[k]; ok {
			over[k] = v
		}
	}
	p.mu.Lock()
	d := p.devs[id]
	p.mu.Unlock()
	if d != nil {
		if len(over) > 0 {
			return nil, nil, status.Errorf(codes.InvalidArgument, "%s is open: set %s when opening it, not per call", id, strings.Join(deviceKeys, ", "))
		}
		if d.left {
			return nil, nil, status.Errorf(codes.FailedPrecondition, "%s left the bootloader (reset into the application): close and reopen the device", id)
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
	d, err = p.connect(ctx, e, o)
	if err != nil {
		return nil, nil, err
	}
	return d, func() { _ = d.close() }, nil
}

// StartCapture implements pluginsdk.Plugin; a bootloader link has no stream.
func (p *Plugin) StartCapture(context.Context, pluginsdk.CaptureRequest, *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	return nil, nil, status.Error(codes.Unimplemented, "tibsl has no capture capability")
}
