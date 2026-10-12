// Package stlinkplugin is the hwlab plugin for ST-Link debug probes.
//
// Layers, bottom to top, each in its own package:
//
//	internal/usb                 Linux usbfs, one claimed interface, no libusb, no cgo
//	plugins/stlink/stlink        ST-Link/V2 USB protocol (probe commands, status codes)
//	internal/cortexm             generic Arm Cortex-M debug (DHCSR/DFSR, registers, FPB, DWT)
//	plugins/stlink/stm32         STM32 identity profiles and the flash controller driver
//	this package                 DebugProbe / MemoryAccess / FlashProgramming capabilities
//
// The probe is selected by its USB serial number, never by bus or device
// number. Every probe is a separate device, any number can be present, and
// an ambiguous selection is an error that lists the candidates. Only the
// debug interface is claimed; no kernel driver is detached.
//
// Memory writes need allow_memory_write and are limited to SRAM; resets need
// allow_reset; flash methods exist only for a recognised STM32 and need
// allow_flash/allow_erase, confirmation tokens and a verified full backup.
// Option bytes, readout protection and OTP are never written.
package stlinkplugin

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

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/stlink/sim"
)

// VID is the STMicroelectronics USB vendor id.
const VID = 0x0483

// probePIDs are the ST-Link product ids this plugin lists. Listing a device
// is not a promise that its generation is supported: the protocol layer
// reads GET_VERSION and refuses what it cannot drive.
var probePIDs = map[uint16]string{
	0x3744: "ST-Link/V1", 0x3748: "ST-Link/V2", 0x374b: "ST-Link/V2-1", 0x3752: "ST-Link/V2-1 (no MSD)",
	0x374d: "ST-Link/V3 (loader)", 0x374e: "ST-Link/V3", 0x374f: "ST-Link/V3 (bridge)", 0x3753: "ST-Link/V3 (dual VCP)",
	0x3754: "ST-Link/V3 (no MSD)", 0x3755: "ST-Link/V3 (mini)", 0x3757: "ST-Link/V3 (mini, no MSD)",
}

// SimEnv selects SYNTHETIC probes: "1" or "2" (the number of probes).
const SimEnv = "HWLAB_STLINK_SIM"

// Entry is one probe the plugin can open.
type Entry struct {
	ID     string
	Dev    usb.Device
	Serial string // canonical serial, the stable identity
	Sim    *sim.Config
}

// Lister enumerates probes.
type Lister func() []Entry

// Opener claims one interface of a probe.
type Opener func(e Entry, iface int, opt usb.OpenOptions) (usb.Port, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener

	mu   sync.Mutex
	devs map[string]*session
	// last remembers how each persistently opened probe was configured and
	// what it was, so a session lost to an unplug can be rebuilt.
	last map[string]*known
}

// CanonicalSerial is the stable text form of a probe's USB serial. Genuine
// probes report ASCII hex, but some report raw binary bytes (one per UTF-16
// unit); those are shown as upper-case hex so they can be typed and compared.
func CanonicalSerial(raw string) string {
	b := make([]byte, 0, len(raw))
	for _, r := range raw {
		if r > 0xFF {
			return strings.ToUpper(fmt.Sprintf("%x", []byte(raw)))
		}
		b = append(b, byte(r))
	}
	printable := len(b) > 0
	for _, c := range b {
		if c <= 0x20 || c >= 0x7F {
			printable = false
		}
	}
	if printable {
		return string(b)
	}
	return strings.ToUpper(fmt.Sprintf("%x", b))
}

// simProbes builds the synthetic probes for HWLAB_STLINK_SIM=n.
func simProbes(n int) []Entry {
	var es []Entry
	for i := 0; i < n; i++ {
		serial := fmt.Sprintf("SIMSTLINK%04d", i+1)
		t := cortexm.NewSim()
		// A marker in SRAM makes the two simulated targets distinguishable.
		_ = t.Write32(context.Background(), 0x20000000, 0x51A00000+uint32(i+1))
		cfg := sim.NewConfig(serial, t)
		es = append(es, Entry{Dev: SimDevice(serial), Serial: serial, Sim: cfg})
	}
	return es
}

// SimDevice returns the descriptor a simulated probe presents.
func SimDevice(serial string) usb.Device {
	return usb.Device{
		VID: VID, PID: 0x3748, BCD: 0x0100, Manufacturer: "STMicroelectronics", Product: "STM32 STLink",
		Serial: serial, SerialRaw: serial, SysName: "sim-" + serial, Configuration: 1, SpeedMbps: 12,
		Interfaces: []usb.Interface{{Number: 0, Class: 0xff, Name: "ST Link", Endpoints: []usb.Endpoint{
			{Address: 0x02, Type: "Bulk", MaxPacket: 64}, {Address: 0x81, Type: "Bulk", MaxPacket: 64}, {Address: 0x83, Type: "Bulk", MaxPacket: 64}}}},
	}
}

// New returns the plugin for the host's probes.
func New() *Plugin {
	var simEntries []Entry
	if v := os.Getenv(SimEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 8 {
			n = 1
		}
		simEntries = simProbes(n)
	}
	list := func() []Entry {
		var es []Entry
		for _, d := range usb.Scan("/sys", func(vid, pid uint16) bool { _, ok := probePIDs[pid]; return vid == VID && ok }) {
			raw := d.SerialRaw
			if raw == "" {
				raw = d.Serial
			}
			es = append(es, Entry{Dev: d, Serial: CanonicalSerial(raw)})
		}
		for _, e := range simEntries {
			if e.Sim.Present() {
				es = append(es, e)
			}
		}
		return nameEntries(es)
	}
	return NewWith(list, defaultOpen)
}

// NewWith injects enumeration and interface opening (tests).
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*session{}, last: map[string]*known{}}
}

func defaultOpen(e Entry, iface int, opt usb.OpenOptions) (usb.Port, error) {
	if e.Sim != nil {
		return e.Sim.Port(), nil
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

// nameEntries assigns stable ids vid:pid:serial. A probe without a serial, or
// two probes that report the same serial, fall back to vid:pid@sysname, which
// is only stable while the device stays in the same port; such devices are
// flagged ambiguous so a session never silently follows the wrong one.
func nameEntries(es []Entry) []Entry {
	count := map[string]int{}
	base := func(e Entry) string {
		if e.Serial == "" {
			return ""
		}
		return fmt.Sprintf("%04x:%04x:%s", e.Dev.VID, e.Dev.PID, sanitize(e.Serial))
	}
	for _, e := range es {
		count[base(e)]++
	}
	for i := range es {
		b := base(es[i])
		if b == "" || count[b] > 1 {
			b = fmt.Sprintf("%04x:%04x@%s", es[i].Dev.VID, es[i].Dev.PID, es[i].Dev.SysName)
		}
		es[i].ID = b
	}
	sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID })
	return es
}

// debugInterface returns the vendor-class interface with the bulk
// OUT+IN pair that carries the ST-Link protocol.
func debugInterface(d usb.Device) (usb.Interface, bool) {
	for _, i := range d.Interfaces {
		if in, out, _ := i.BulkEndpoints(); i.Class == 0xff && in != 0 && out != 0 {
			return i, true
		}
	}
	return usb.Interface{}, false
}

func capAttrs() map[string]string {
	return map[string]string{
		"interface":       "SWD",
		"transport":       "stlink-usb",
		"target_protocol": "arm-adiv5-cortex-m",
		"cpu_debug":       "halt,resume,step,registers,breakpoints,watchpoints",
		"memory_access":   "read (any); write (SRAM only, allow_memory_write)",
		"reset":           "system,core,pin (allow_reset)",
		"flash":           "stm32-direct-controller: recognised STM32 only (allow_flash/allow_erase)",
	}
}

func deviceCaps() []*pluginv1.Capability {
	ids := []string{pluginv1.CapTargetIdentify, pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming}
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: capAttrs()}
	}
	return out
}

// ListDevices implements pluginsdk.Plugin. It reads sysfs only and claims
// nothing.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, e := range p.list() {
		d := &pluginv1.Device{Id: e.ID, Serial: e.Serial, Capabilities: deviceCaps()}
		props := map[string]string{
			"vid": fmt.Sprintf("%04x", e.Dev.VID), "pid": fmt.Sprintf("%04x", e.Dev.PID),
			"manufacturer": e.Dev.Manufacturer, "product": e.Dev.Product, "usb_serial_canonical": e.Serial,
			"configuration": strconv.Itoa(e.Dev.Configuration), "interfaces": strconv.Itoa(len(e.Dev.Interfaces)),
			"pid_family_hint": probePIDs[e.Dev.PID] + " (from the USB PID; the generation is only known after GET_VERSION)",
		}
		if e.Serial != e.Dev.Serial {
			props["usb_serial_raw_hex"] = fmt.Sprintf("%x", e.Dev.SerialRaw)
		}
		if di, ok := debugInterface(e.Dev); ok {
			in, o, mp := di.BulkEndpoints()
			props["debug_interface"] = strconv.Itoa(di.Number)
			props["debug_endpoints"] = fmt.Sprintf("OUT 0x%02x, IN 0x%02x, %d B", o, in, mp)
			props["debug_kernel_driver"] = di.Driver
		}
		var companions []string
		for _, i := range e.Dev.Interfaces {
			if i.Class == 0x02 || i.Class == 0x0a || i.Class == 0x08 {
				companions = append(companions, strconv.Itoa(i.Number))
				if i.TTY != "" {
					props["companion_tty"] = "/dev/" + i.TTY
				}
			}
		}
		if len(companions) > 0 {
			props["companion_interfaces"] = strings.Join(companions, ",")
		}
		if e.Sim != nil {
			d.Name, d.Transport = "Simulated ST-Link/V2 (SYNTHETIC)", "fake"
			props["synthetic"] = "true"
		} else {
			d.Name, d.Transport = strings.TrimSpace(e.Dev.Manufacturer+" "+e.Dev.Product), "usb"
			if d.Name == "" {
				d.Name = "ST-Link"
			}
			props["usb_address"] = fmt.Sprintf("%d:%d", e.Dev.Bus, e.Dev.Address)
			props["usb_device"] = e.Dev.SysName
		}
		d.Properties = props
		out = append(out, d)
	}
	return out, nil
}

func (p *Plugin) find(id string) (Entry, error) {
	var serials []string
	for _, e := range p.list() {
		if e.ID == id {
			return e, nil
		}
		serials = append(serials, e.Serial)
	}
	msg := fmt.Sprintf("ST-Link %q is not connected", id)
	if len(serials) > 0 {
		msg += " (connected probes: " + strings.Join(serials, ", ") + ")"
	}
	return Entry{}, status.Error(codes.NotFound, msg)
}

// OpenDevice implements pluginsdk.Plugin: it claims the debug interface only.
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
		if err := old.close(); err != nil {
			return wrap(err)
		}
	}
	s, err := openSession(ctx, e, o, p.open)
	if err != nil {
		return status.Errorf(openCode(err), "open %s: %v", id, transportErr(err))
	}
	s.persist = true
	p.mu.Lock()
	p.devs[id] = s
	p.last[id] = &known{opts: o}
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
	delete(p.last, id)
	p.mu.Unlock()
	if s != nil {
		if err := s.close(); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// drop forgets a session whose transport died, so the next call reopens.
func (p *Plugin) drop(id string, s *session) {
	p.mu.Lock()
	if p.devs[id] == s {
		delete(p.devs, id)
		if k := p.last[id]; k != nil && s.identity != nil {
			k.id = s.identity
		}
	}
	p.mu.Unlock()
	_ = s.close()
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
	k := p.last[id]
	p.mu.Unlock()
	if (s != nil || k != nil) && len(over) > 0 {
		return nil, nil, status.Error(codes.InvalidArgument, "device settings cannot change on an open device: close it and open it again with the new settings (use set_speed for the clock)")
	}
	if s != nil {
		return s, func() {}, nil
	}
	if k != nil {
		// A persistent probe whose session was lost: wait, bounded, for the
		// same USB serial to come back and rebuild.
		ns, _, err := p.reopen(ctx, id, k.opts, k.id)
		if err != nil {
			return nil, nil, err
		}
		ns.persist = true
		p.mu.Lock()
		p.devs[id] = ns
		p.mu.Unlock()
		return ns, func() {}, nil
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

// StartCapture implements pluginsdk.Plugin.
func (p *Plugin) StartCapture(context.Context, pluginsdk.CaptureRequest, *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	return nil, nil, status.Error(codes.Unimplemented, "stlink has no capture capability (SWO trace is not implemented)")
}
