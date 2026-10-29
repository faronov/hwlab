// Package bflbplugin is the hwlab plugin for Bouffalo Lab BL702 chips in
// boot-ROM ISP mode. It flashes them natively over the chip's own USB port
// (the ROM enumerates as a CDC device, VID:PID ffff:ffff, while BOOT is held
// at power-up) or a UART, speaking the ROM protocol and the command set of
// the vendor eflash_loader helper (see plugins/bflb/isp). No Python tool is
// needed; the helper binary is NOT shipped: name it in HWLAB_BFLB_EFLASH_LOADER
// (bflb-mcu-tool's chips/bl702/eflash_loader/eflash_loader_32m.bin).
//
// SAFETY. Erase, program and restore require a verified full backup of the
// connected board (FlashProgramming.backup, bound to the efuse MAC, flash
// JEDEC id and size). Chip erase is not offered. Loading the helper changes
// only RAM; a reset (or replug without BOOT) leaves ISP mode.
//
// STATUS: see internal/status (entry "bflb") and
// docs/notes/bflb-bl702-usb-isp-results.md for what ran on hardware.
package bflbplugin

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
	"github.com/faronov/hwlab/plugins/bflb/isp"
	"github.com/faronov/hwlab/plugins/uart"
)

const (
	// PortsEnv lists ISP ports as comma-separated name=/dev/ttyUSB0[@baud].
	PortsEnv = "HWLAB_BFLB_PORTS"
	// HelperEnv is the path of the vendor eflash_loader RAM image.
	HelperEnv = "HWLAB_BFLB_EFLASH_LOADER"
	// SimEnv ("1") offers an in-process SYNTHETIC chip.
	SimEnv = "HWLAB_BFLB_SIM"
	// SimID is the id of the simulated chip.
	SimID = "sim-bl702"

	defaultBaud = 115200
	// romUSB is the USB id of the BL702 boot ROM's CDC device.
	romUSB = "ffff:ffff"
)

// Entry is one ISP port the plugin can open.
type Entry struct {
	ID   string
	Port string
	Baud int
	Sim  *isp.Sim
	// USBCDC marks the ROM's own USB device: no auto-baud, a short sync.
	USBCDC bool
	// Candidate marks a port found by VID:PID; nothing was written to it.
	Candidate bool
	USB       string
	Product   string
}

// Lister enumerates ISP ports.
type Lister func() []Entry

// Opener opens the link of an entry. The returned func closes it.
type Opener func(e Entry, baud int) (isp.Link, func() error, error)

// HelperLoader returns the eflash_loader image for an entry.
type HelperLoader func(e Entry) ([]byte, string, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list   Lister
	open   Opener
	helper HelperLoader

	mu   sync.Mutex
	devs map[string]*device
}

// New returns the plugin for the host's configuration.
func New() *Plugin {
	var sim *isp.Sim
	if os.Getenv(SimEnv) == "1" {
		sim = isp.NewSim()
	}
	list := func() []Entry {
		es := parsePorts(os.Getenv(PortsEnv), uart.ListPorts)
		es = append(es, candidates(es, uart.ListPorts)...)
		if sim != nil {
			es = append(es, Entry{ID: SimID, Sim: sim, USBCDC: true})
		}
		return es
	}
	return NewWith(list, defaultOpen, defaultHelper)
}

// NewWith injects enumeration, opening and the helper source (tests).
func NewWith(l Lister, o Opener, h HelperLoader) *Plugin {
	return &Plugin{list: l, open: o, helper: h, devs: map[string]*device{}}
}

func usbID(i uart.Info) string { return strings.ToLower(i.VID + ":" + i.PID) }

// parsePorts reads PortsEnv; malformed entries are skipped. A listed port
// that is the ROM's USB device gets the USB CDC handshake.
func parsePorts(s string, enum func() ([]uart.Info, error)) []Entry {
	var out []Entry
	cdc := map[string]bool{}
	if strings.TrimSpace(s) != "" {
		if infos, err := enum(); err == nil {
			for _, i := range infos {
				cdc[i.Path] = i.USB && usbID(i) == romUSB
			}
		}
	}
	for _, item := range strings.Split(s, ",") {
		name, spec, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || name == "" || spec == "" {
			continue
		}
		path, baud, hasBaud := strings.Cut(spec, "@")
		if path == "" {
			continue
		}
		e := Entry{ID: name, Port: path, Baud: defaultBaud, USBCDC: cdc[path]}
		if hasBaud {
			b, err := strconv.Atoi(baud)
			if err != nil || b < 9600 || b > 4_000_000 {
				continue
			}
			e.Baud = b
		}
		out = append(out, e)
	}
	return out
}

// candidates lists the BL702 ROM's USB CDC ports. It only enumerates; it never
// opens or writes a port. Ports listed explicitly are skipped.
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
		if !i.USB || usbID(i) != romUSB || used[i.Path] {
			continue
		}
		out = append(out, Entry{ID: i.ID(), Port: i.Path, Baud: defaultBaud, USBCDC: true, Candidate: true, USB: romUSB, Product: i.Product})
	}
	return out
}

// openSettle lets a freshly opened port settle before the first command.
const openSettle = 30 * time.Millisecond

func defaultOpen(e Entry, baud int) (isp.Link, func() error, error) {
	if e.Sim != nil {
		e.Sim.Reopen()
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
	time.Sleep(openSettle)
	if err := p.ResetInput(); err != nil {
		return nil, nil, errors.Join(err, p.Close())
	}
	return p, p.Close, nil
}

func defaultHelper(e Entry) ([]byte, string, error) {
	if e.Sim != nil {
		return isp.SimHelperImage(), "synthetic", nil
	}
	path := os.Getenv(HelperEnv)
	if path == "" {
		return nil, "", status.Errorf(codes.FailedPrecondition, "the flash helper is not configured: set %s to the vendor eflash_loader image (bflb-mcu-tool: chips/bl702/eflash_loader/eflash_loader_32m.bin); hwlab does not ship it", HelperEnv)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, "", status.Errorf(codes.FailedPrecondition, "%s: %v", HelperEnv, err)
	}
	if st.Size() > 256<<10 {
		return nil, "", status.Errorf(codes.FailedPrecondition, "%s=%s is %d bytes: not a RAM helper image", HelperEnv, path, st.Size())
	}
	b, err := os.ReadFile(path)
	return b, path, err
}

func caps(attrs map[string]string) []*pluginv1.Capability {
	ids := []string{pluginv1.CapTargetIdentify, pluginv1.CapFlashProgramming}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: attrs}
	}
	return out
}

func deviceCaps() []*pluginv1.Capability {
	return caps(map[string]string{
		"interface": "boot-ROM ISP (USB CDC or UART)", "backend": "native-go-bflb-isp", "targets": "BL702",
		"chip_erase": "not-offered", "flash_helper": "vendor eflash_loader via " + HelperEnv, "python_required": "no",
	})
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, e := range p.list() {
		d := &pluginv1.Device{Id: e.ID, Capabilities: deviceCaps()}
		if e.Sim != nil {
			d.Name, d.Transport = "Simulated BL702 in ISP mode (SYNTHETIC)", "fake"
			d.Properties = map[string]string{"synthetic": "true", "validated": "false"}
		} else {
			d.Name, d.Transport = "Bouffalo BL702 boot-ROM ISP", "serial"
			d.Properties = map[string]string{"port": e.Port, "baud": strconv.Itoa(e.Baud), "usb_cdc": strconv.FormatBool(e.USBCDC), "backend": "native-go-bflb-isp"}
			if e.Candidate {
				d.Name = "BL702 boot-ROM USB device (unprobed)"
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
	return Entry{}, status.Errorf(codes.NotFound, "bflb port %q is not present: hold BOOT while plugging the BL702's USB in, or list a UART in %s (name=/dev/ttyUSB0[@baud])", id, PortsEnv)
}

// options are the device-level settings.
type options struct {
	baud    int
	sync    int
	timeout time.Duration
}

var deviceKeys = []string{"baud", "sync_bytes", "timeout_ms"}

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
	if v, ok, err := optNumber(cfg, "sync_bytes"); err != nil {
		return o, err
	} else if ok {
		if v < 4 || v > 2048 || v != float64(int(v)) {
			return o, fmt.Errorf("sync_bytes %v: want an integer 4..2048", v)
		}
		o.sync = int(v)
	}
	if v, ok, err := optNumber(cfg, "timeout_ms"); err != nil {
		return o, err
	} else if ok {
		if v < 100 || v > 60000 {
			return o, fmt.Errorf("timeout_ms %v: want 100..60000", v)
		}
		o.timeout = time.Duration(v) * time.Millisecond
	}
	return o, nil
}

func defaultOptions(e Entry) options {
	b := e.Baud
	if b == 0 {
		b = defaultBaud
	}
	return options{baud: b, timeout: 2 * time.Second}
}

// device is an open ISP link.
type device struct {
	id    string
	entry Entry
	o     options

	mu     sync.Mutex
	cl     *isp.Client
	closer func() error
	// boot is the latest get_boot_info answer; rom the ROM's own, if seen.
	boot isp.BootInfo
	rom  *isp.BootInfo
	// helperPath is set once this session loaded the helper.
	helperPath string
	helper     HelperLoader
}

func (p *Plugin) connect(e Entry, o options) (*device, error) {
	link, closer, err := p.open(e, o.baud)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", e.ID, err)
	}
	cl := isp.New(link)
	cl.Timeout = o.timeout
	switch {
	case o.sync > 0:
		cl.SyncBytes = o.sync
	case e.USBCDC:
		cl.SyncBytes = isp.DefaultSyncBytes
	default:
		cl.SyncBytes = isp.SyncBytesFor(o.baud)
	}
	if e.USBCDC {
		cl.WriteChunk = isp.USBWriteChunk
	}
	d := &device{id: e.ID, entry: e, o: o, cl: cl, closer: closer, helper: p.helper}
	cl.Reopen = func() (isp.Link, error) {
		_ = d.closer()
		d.closer = func() error { return nil }
		l, c, err := p.open(e, o.baud)
		if err != nil {
			return nil, err
		}
		d.closer = c
		return l, nil
	}
	bi, err := cl.Connect()
	if err != nil {
		_ = d.closer()
		return nil, status.Errorf(codes.Unavailable, "%s: %v", e.ID, err)
	}
	d.setBoot(bi)
	return d, nil
}

func (d *device) setBoot(bi isp.BootInfo) {
	d.boot = bi
	if !bi.HelperRunning() {
		r := bi
		d.rom = &r
	}
}

// ensureHelper loads and starts the eflash_loader if the ROM still answers.
func (d *device) ensureHelper() error {
	if d.boot.HelperRunning() {
		return nil
	}
	b, path, err := d.helper(d.entry)
	if err != nil {
		return err
	}
	img, err := isp.ParseImage(b)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "helper %s: %v", path, err)
	}
	bi, err := d.cl.LoadHelper(img, d.boot)
	if err != nil {
		return err
	}
	d.boot, d.helperPath = bi, path
	return nil
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

// lease returns the open device, or opens one for a single call. The chip
// keeps the loaded helper between calls; reopening the port only returns it
// to handshake state.
func (p *Plugin) lease(id string, args map[string]any) (*device, func(), error) {
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

// StartCapture implements pluginsdk.Plugin; an ISP link produces no stream.
func (p *Plugin) StartCapture(context.Context, pluginsdk.CaptureRequest, *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	return nil, nil, status.Error(codes.Unimplemented, "bflb has no capture capability")
}
