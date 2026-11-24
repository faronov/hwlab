// Package espromplugin is the hwlab plugin for the Espressif serial
// bootloader: the ROM download mode of ESP32-series chips plus the
// esp-flasher-stub RAM loader (vendored, Apache-2.0 OR MIT), spoken natively
// (plugins/esprom/rom); esptool and Python are not used.
//
// TARGETS. Three kinds of serial port reach a chip's ROM:
//
//	esp32_uart  a USB-UART bridge (CP210x, CH34x, FTDI) wired with the classic
//	            auto-reset circuit (RTS->EN, DTR->GPIO0), e.g. a SONOFF
//	            Dongle Max (ESP32 + CP2102N)
//	h2_bridge   CDC 0 of the zigbee-rs-hub S3 firmware (tbr-bridge/tbr-hub
//	            bridge mode, USB 303a:4002): the S3 maps DTR/RTS to the H2's
//	            EN/BOOT like the classic circuit and follows the host line
//	            coding. CDC 1 (the bridge console) is never offered.
//	usj         the chip's own USB-Serial/JTAG CDC port (303a:1001: ESP32-S3,
//	            C3, C6, H2); its reset/boot lines follow the TRM's USJ table,
//	            not the classic circuit. This is also where the chip reset
//	            without buttons ("usj_reset") lives: FlashProgramming.reset
//	            mode=run on a usj device is the TRM's chip-reset line sequence
//	            and needs no working firmware and no ROM.
//
// DISCOVERY is passive (IORegistry/sysfs only, no port opened, no line
// touched), with stable ids from the USB identity like tibsl. Aliases of one
// port (macOS cu.usbserial-* and the SiLabs VCP cu.SLAB_USBtoUART*) are one
// device. The chip is identified only by an operation; the last identity is
// cached ($HWLAB_HOME/cache/esprom-identity.json).
//
// SESSION. The port is opened when a device is opened or a call leases it,
// but the ROM is entered lazily: only operations that need it reset the chip
// into download mode, sync at 115200, identify, upload the stub, switch the
// baud rate and attach the flash. FlashProgramming.reset only drives the
// lines. The chip stays in the loader until reset.
//
// SAFETY. Reads never write. Erase, program and restore are CONFIRM methods
// with their own tokens, allow_flash/allow_erase(/allow_chip_erase) device
// settings and a verified full backup of the same chip (MAC, JEDEC id, size)
// whose bytes still match the region; the bootloader slot, the partition
// table and every non-app partition (nvs, phy_init, otadata, coredump, any
// custom data such as hubdata) need confirm_protected; without a valid
// partition table the whole flash is protected. Non-ESP images need
// raw=true. Every modifying call and reset is journaled. eFuses are never
// written; flash with encryption or secure boot enabled is read-only.
package espromplugin

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

	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/esprom/rom"
	"github.com/faronov/hwlab/plugins/uart"
)

const (
	// PortsEnv lists ports discovery cannot find: name=/dev/tty[@baud][:classic|usj|none].
	PortsEnv = "HWLAB_ESPROM_PORTS"
	// SimEnv ("1") offers in-process SYNTHETIC chips.
	SimEnv = "HWLAB_ESPROM_SIM"

	// romBaud is the rate the ROM loaders start at.
	romBaud = 115200
)

// Kind is the kind of port a chip is reached through.
type Kind string

const (
	KindUART   Kind = "esp32_uart"
	KindBridge Kind = "h2_bridge"
	KindUSJ    Kind = "usj"
)

func (k Kind) defaultEntry() rom.Entry {
	if k == KindUSJ {
		return rom.EntryUSJ
	}
	return rom.EntryClassic
}

// defaultBaud is the link rate after the stub runs (0: none, USB).
func (k Kind) defaultBaud() int {
	switch k {
	case KindUSJ:
		return 0
	case KindBridge:
		return 460800
	}
	return 921600
}

// Entry is one port the plugin can open.
type Entry struct {
	ID        string
	Port      string
	Kind      Kind
	Entry     rom.Entry
	Baud      int
	Sim       *rom.Sim
	Candidate bool
	USB       string
	Serial    string
	Product   string
	Location  string
	AltPorts  []string
	// Console is the bridge's console port (never opened).
	Console    string
	Configured bool
}

// Conn is an open serial line.
type Conn struct {
	Link    rom.Link
	Lines   rom.Lines
	SetBaud func(int) error
	Close   func() error
}

// Lister enumerates the ports.
type Lister func() []Entry

// Opener opens the line of an entry at the ROM rate with DTR/RTS released.
type Opener func(e Entry) (*Conn, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list  Lister
	open  Opener
	cache identityCache
	jmu   sync.Mutex

	mu   sync.Mutex
	devs map[string]*device
}

// New returns the plugin for the host's ports.
func New() *Plugin {
	var sims []Entry
	if os.Getenv(SimEnv) == "1" {
		sims = SimEntries()
	}
	list := func() []Entry {
		explicit := parsePorts(os.Getenv(PortsEnv))
		used := map[string]bool{}
		for _, e := range explicit {
			used[e.Port] = true
		}
		var found []Entry
		for _, e := range discover(usb.SerialPorts(usb.DefaultRoot, nil)) {
			if !used[e.Port] {
				found = append(found, e)
			}
		}
		cfg, _ := loadFileConfig()
		return append(cfg.apply(append(explicit, found...)), sims...)
	}
	return NewWith(list, defaultOpen)
}

// NewWith injects enumeration and opening (tests).
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*device{}}
}

func parsePorts(s string) []Entry {
	var out []Entry
	for _, item := range strings.Split(s, ",") {
		name, spec, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || name == "" || spec == "" {
			continue
		}
		e := Entry{ID: name, Kind: KindUART, Entry: rom.EntryClassic}
		if i := strings.LastIndex(spec, ":"); i > 0 {
			if en, err := rom.ParseEntry(spec[i+1:]); err == nil {
				e.Entry, spec = en, spec[:i]
				if en == rom.EntryUSJ {
					e.Kind = KindUSJ
				}
			}
		}
		path, baud, hasBaud := strings.Cut(spec, "@")
		if path == "" {
			continue
		}
		e.Port, e.Baud = path, e.Kind.defaultBaud()
		if hasBaud {
			b, err := strconv.Atoi(baud)
			if err != nil || !validBaud(b) {
				continue
			}
			e.Baud = b
		}
		out = append(out, e)
	}
	return out
}

func validBaud(b int) bool { return b == 0 || (b >= 9600 && b <= 3_000_000) }

func defaultOpen(e Entry) (*Conn, error) {
	if e.Sim != nil {
		e.Sim.Reopen()
		_ = e.Sim.SetBaud(romBaud)
		return &Conn{Link: e.Sim, Lines: e.Sim.SetLines, SetBaud: e.Sim.SetBaud, Close: func() error { return nil }}, nil
	}
	// Both lines released: the classic circuit and the bridge keep EN and
	// the strap released, the USJ only clears its download flag.
	off := false
	m := uart.DefaultMode()
	m.Baud = romBaud
	m.DTR, m.RTS = &off, &off
	p, err := uart.OpenPort(e.Port, m)
	if err != nil {
		return nil, err
	}
	time.Sleep(30 * time.Millisecond)
	if err := p.ResetInput(); err != nil {
		return nil, errors.Join(err, p.Close())
	}
	setBaud := func(b int) error {
		mm := uart.DefaultMode()
		mm.Baud = b
		return p.SetMode(mm)
	}
	return &Conn{Link: p, Close: p.Close, SetBaud: setBaud, Lines: func(dtr, rts bool) (bool, error) { return uart.SetModemLines(p, dtr, rts) }}, nil
}

func deviceCaps() []*pluginv1.Capability {
	attrs := map[string]string{
		"interface": "Espressif serial bootloader (ROM download mode + esp-flasher-stub)", "backend": "native-go-esp-rom",
		"targets":          "ESP32 (USB-UART, classic auto-reset), ESP32-H2 behind the zigbee-rs-hub S3 bridge, ESP32-S3/C6/H2 USB-Serial/JTAG",
		"reset":            "classic DTR/RTS or USB-Serial/JTAG line sequence: run (application) or bootloader (download mode)",
		"protected_region": "bootloader slot, partition table, every non-app partition (confirm_protected); whole flash without a valid table",
		"python_required":  "no",
	}
	ids := []string{pluginv1.CapTargetIdentify, pluginv1.CapFlashProgramming}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: attrs}
	}
	return out
}

var kindNames = map[Kind]string{
	KindUART:   "USB-UART bridge, possible ESP ROM bootloader (unprobed)",
	KindBridge: "ESP32-H2 behind the S3 bridge (CDC 0, unprobed)",
	KindUSJ:    "Espressif USB-Serial/JTAG CDC port (unprobed)",
}

// ListDevices implements pluginsdk.Plugin. It never opens a port.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	known := p.cache.load()
	ti := tibslIdentities()
	for _, e := range p.list() {
		d := &pluginv1.Device{Id: e.ID, Capabilities: deviceCaps(), Transport: "serial"}
		props := map[string]string{"target_kind": string(e.Kind), "entry": string(e.Entry), "baud": strconv.Itoa(e.Baud),
			"backend": "native-go-esp-rom", "probed": "false"}
		if e.Sim != nil {
			d.Name, d.Transport = "Simulated "+e.Sim.Chip.Name+" serial bootloader (SYNTHETIC)", "fake"
			props["synthetic"], props["validated"] = "true", "false"
		} else {
			d.Name = kindNames[e.Kind]
			props["port"] = e.Port
			if len(e.AltPorts) > 0 {
				props["alt_ports"] = strings.Join(e.AltPorts, ",")
			}
			if e.Candidate {
				props["candidate"] = "true"
				props["usb"], props["usb_serial"], props["product"], props["usb_location"] = e.USB, e.Serial, e.Product, e.Location
				if !genericSerial(e.Serial) {
					d.Serial = e.Serial
				}
			}
			if e.Kind == KindUSJ && e.Serial != "" {
				props["espusbjtag_device"] = "espusbjtag/303a:1001:" + sanitize(e.Serial)
			}
			if e.Console != "" {
				props["bridge_console_port"] = e.Console + " (S3 console, never opened)"
			}
			if e.Configured {
				props["configured"] = ConfigFile()
			}
		}
		if t, ok := ti[e.ID]; ok {
			props["tibsl_identified_chip"] = t
		}
		if id, ok := known[e.ID]; ok {
			if e.Sim == nil {
				d.Name = id.Chip + " via the ESP serial bootloader"
			}
			props["chip"], props["mac"], props["chip_revision"] = id.Chip, id.MAC, id.Revision
			props["flash_bytes"], props["flash_jedec"] = strconv.Itoa(id.FlashBytes), id.JEDEC
			props["identified_utc"], props["identified_port"] = id.SeenUTC, id.Port
			props["identity_source"] = "cache of the last bootloader connection (listing does not touch the port)"
			d.Serial = strings.ReplaceAll(id.MAC, ":", "")
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
	return Entry{}, status.Errorf(codes.NotFound, "esprom device %q is not present: plug the board in (CP210x/CH34x/FTDI bridges, the S3 bridge's CDC 0 and USB-Serial/JTAG ports are discovered), or list the port in %s as name=/dev/tty[@baud][:classic|usj|none]", id, PortsEnv)
}

// options are the device-level settings.
type options struct {
	baud           int
	timeout        time.Duration
	entry          rom.Entry
	stub           bool
	allowOtherChip bool
	perm           perms
}

type perms struct{ flash, erase, chipErase, reset bool }

var deviceKeys = []string{"baud", "timeout_ms", "entry", "stub", "allow_other_chip", "allow_flash", "allow_erase", "allow_chip_erase", "allow_reset"}

// permKeys widen what a session may do; all are dangerous configuration.
var permKeys = []string{"allow_flash", "allow_erase", "allow_chip_erase", "allow_reset"}

func defaultOptions(e Entry) options {
	en := e.Entry
	if en == "" {
		en = e.Kind.defaultEntry()
	}
	return options{baud: e.Baud, timeout: 3 * time.Second, entry: en, stub: true}
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
	if v, ok, err := optNumber(cfg, "baud"); err != nil {
		return o, err
	} else if ok {
		if v != float64(int(v)) || !validBaud(int(v)) {
			return o, fmt.Errorf("baud %v: want 0 (stay at %d) or an integer 9600..3000000", v, romBaud)
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
	if v, ok := cfg["entry"]; ok && v != nil {
		s, isStr := v.(string)
		if !isStr {
			return o, fmt.Errorf("entry: want a string, got %v", v)
		}
		en, err := rom.ParseEntry(s)
		if err != nil {
			return o, err
		}
		o.entry = en
	}
	bools := map[string]*bool{"stub": &o.stub, "allow_other_chip": &o.allowOtherChip, "allow_flash": &o.perm.flash,
		"allow_erase": &o.perm.erase, "allow_chip_erase": &o.perm.chipErase, "allow_reset": &o.perm.reset}
	for k, dst := range bools {
		if _, ok := cfg[k]; !ok {
			continue
		}
		b, err := optBool(cfg, k)
		if err != nil {
			return o, err
		}
		*dst = b
	}
	return o, nil
}

// OpenDevice implements pluginsdk.Plugin: it opens the port without touching
// the lines; the ROM is entered by the first operation that needs it.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	e, err := p.find(id)
	if err != nil {
		return err
	}
	o, err := parseOptions(defaultOptions(e), cfg)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	d, err := p.newDevice(e, o)
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

// lease returns the open device, or opens one for a single call.
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
	d, err = p.newDevice(e, o)
	if err != nil {
		return nil, nil, err
	}
	return d, func() { _ = d.close() }, nil
}

// StartCapture implements pluginsdk.Plugin; a bootloader link has no stream.
func (p *Plugin) StartCapture(context.Context, pluginsdk.CaptureRequest, *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	return nil, nil, status.Error(codes.Unimplemented, "esprom has no capture capability")
}
