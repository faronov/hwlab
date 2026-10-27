// Package espusbjtag is the hwlab plugin for the Espressif USB Serial/JTAG
// peripheral (ESP32-C3/S3/C6/H2, VID 303a PID 1001).
//
// Layers, bottom to top, each in its own package:
//
//	internal/usb            Linux usbfs or macOS IOKit, one claimed interface, no libusb, no cgo
//	espusb                  Espressif vendor protocol -> jtag.Port
//	internal/jtag           generic IEEE 1149.1 TAP driver and chain scan
//	internal/riscv          RISC-V debug transport + debug module (spec 0.13)
//	this package            DebugProbe / MemoryAccess / JTAG capabilities
//
// Only the vendor-class JTAG interface is claimed. The two CDC-ACM interfaces
// of the same device stay with the kernel and belong to the uart plugin; this
// plugin never opens the tty.
//
// Scope: halt/resume/step, registers, system-bus memory read and SRAM write,
// hardware breakpoints, per-hart selection, hart/system/SoC reset with USB
// re-enumeration handling, and (ESP-specific, in package esp) physical flash
// read, backup, erase, program, verify and restore. Everything that changes
// the target needs an explicit allow_* setting and, for flash, a confirmation
// token and a verified full backup. eFuses are never written.
package espusbjtag

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/riscv/rvsim"
	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
	"github.com/faronov/hwlab/plugins/espusbjtag/sim"
)

// USB identity of the Espressif USB Serial/JTAG unit.
const (
	VID = 0x303a
	PID = 0x1001
)

// SimEnv selects a SYNTHETIC device ("1").
const SimEnv = "HWLAB_ESPJTAG_SIM"

// SimID is the id of the simulated device.
const SimID = "sim-espusbjtag"

// Entry is one device the plugin can open.
type Entry struct {
	ID  string
	Dev usb.Device
	Sim *sim.Config
}

// Lister enumerates devices.
type Lister func() []Entry

// Opener claims one interface of a device.
type Opener func(e Entry, iface int, opt usb.OpenOptions) (usb.Port, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener

	mu   sync.Mutex
	devs map[string]*session
	// last remembers how each persistently opened device was configured and
	// what it was, so a session lost to an unplug or reset can be rebuilt.
	last map[string]*known
}

// SimDevice returns the descriptor the simulated device presents.
func SimDevice() usb.Device {
	return usb.Device{
		Bus: 0, Address: 0, VID: VID, PID: PID, BCD: 0x0101,
		Manufacturer: "Espressif", Product: "USB JTAG/serial debug unit", Serial: "SIM:00:00:00:00:01",
		SysName: "sim", Configuration: 1, SpeedMbps: 12,
		Interfaces: []usb.Interface{
			{Number: 0, Class: 0x02, Driver: "cdc_acm", Endpoints: []usb.Endpoint{{Address: 0x82, Type: "Interrupt", MaxPacket: 16}}},
			{Number: 1, Class: 0x0a, Driver: "cdc_acm", Endpoints: []usb.Endpoint{{Address: 0x01, Type: "Bulk", MaxPacket: 64}, {Address: 0x81, Type: "Bulk", MaxPacket: 64}}},
			{Number: 2, Class: 0xff, Endpoints: []usb.Endpoint{{Address: 0x02, Type: "Bulk", MaxPacket: 64}, {Address: 0x83, Type: "Bulk", MaxPacket: 64}}},
		},
	}
}

// SimConfig is the synthetic target: a two-hart core (hart 1 unavailable), SRAM and flash.
func SimConfig() sim.Config {
	c, _ := SimConfigFlash()
	return c
}

// SimConfigFlash is SimConfig plus a handle on the simulated SPI flash, so
// tests can inspect and fault-inject it. Everything in it is SYNTHETIC: a
// 1 MiB flash whose first 256 bytes read 00 01 02 ..., 64 KiB of HP SRAM, the
// ROM chip-detect word, unprotected eFuses and idle watchdogs.
func SimConfigFlash() (sim.Config, *esp.SimFlash) {
	fl := esp.NewSimFlash(256 << 10)
	for i := 0; i < 256; i++ {
		fl.Mem[i] = byte(i)
	}
	sram := make([]byte, 64<<10)
	for i := 0; i < 256; i++ {
		sram[i] = byte(i)
	}
	magic := binary.LittleEndian.AppendUint32(nil, esp.ChipMagicC6)
	regs := func(base uint32) rvsim.Device {
		m := map[uint32]uint32{}
		return rvsim.Device{Base: base, Size: 0x100, Read: func(o uint32) uint32 { return m[o] }, Write: func(o, v uint32) { m[o] = v }}
	}
	return sim.Config{
		Target: rvsim.Config{
			Harts: 2, Unavailable: map[int]bool{1: true}, Triggers: 4,
			Memory: []rvsim.Region{
				{Base: esp.HPSRAMBase, Data: sram},
				{Base: esp.ChipMagicAddr, Data: magic},
				{Base: esp.FlashMapBase, Data: fl.Mem},
			},
			Devices: []rvsim.Device{fl.Device(), regs(0x600B0800), regs(0x60008000), regs(0x60009000), regs(0x600B1C00)},
		},
	}, fl
}

// New returns the plugin for the host's devices.
func New() *Plugin {
	var simCfg *sim.Config
	if os.Getenv(SimEnv) == "1" {
		c := SimConfig()
		c.Shared = rvsim.New(c.Target)
		simCfg = &c
	}
	list := func() []Entry {
		var es []Entry
		for _, d := range usb.Scan("/sys", func(vid, pid uint16) bool { return vid == VID && pid == PID }) {
			es = append(es, Entry{Dev: d})
		}
		if simCfg != nil {
			es = append(es, Entry{ID: SimID, Dev: SimDevice(), Sim: simCfg})
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
		return sim.New(*e.Sim), nil
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

// nameEntries assigns stable ids: vid:pid:serial, or vid:pid@sysname without a
// serial or when two devices share one.
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

// jtagInterface returns the vendor-class interface with a bulk IN and OUT
// endpoint, which is the one carrying the Espressif JTAG protocol.
func jtagInterface(d usb.Device) (usb.Interface, bool) {
	for _, i := range d.Interfaces {
		if in, out, _ := i.BulkEndpoints(); i.Class == 0xff && in != 0 && out != 0 {
			return i, true
		}
	}
	return usb.Interface{}, false
}

func capAttrs() map[string]string {
	return map[string]string{
		"interface":       "JTAG",
		"transport":       "espressif-usb-jtag",
		"target_protocol": "riscv-debug-0.13",
		"cpu_debug":       "halt,resume,step,registers-read,breakpoints,multi-hart",
		"memory_access":   "read (system bus), write (SRAM only, allow_memory_write)",
		"reset":           "hart,system,soc (allow_reset)",
		"flash":           "esp-native-spi1: info,read,backup,verify,erase,program,restore (allow_flash/allow_erase)",
	}
}

func deviceCaps() []*pluginv1.Capability {
	ids := []string{pluginv1.CapJTAGTransport, pluginv1.CapJTAGScan, pluginv1.CapTargetIdentify, pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming}
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
		d := &pluginv1.Device{Id: e.ID, Serial: e.Dev.Serial, Capabilities: deviceCaps()}
		props := map[string]string{
			"vid": fmt.Sprintf("%04x", e.Dev.VID), "pid": fmt.Sprintf("%04x", e.Dev.PID),
			"manufacturer": e.Dev.Manufacturer, "product": e.Dev.Product,
			"configuration": strconv.Itoa(e.Dev.Configuration), "interfaces": strconv.Itoa(len(e.Dev.Interfaces)),
			"chip": "unknown (the USB descriptors do not name the chip)",
		}
		if ji, ok := jtagInterface(e.Dev); ok {
			in, o, mp := ji.BulkEndpoints()
			props["jtag_interface"] = strconv.Itoa(ji.Number)
			props["jtag_endpoints"] = fmt.Sprintf("OUT 0x%02x, IN 0x%02x, %d B", o, in, mp)
			props["jtag_kernel_driver"] = ji.Driver
		}
		for _, i := range e.Dev.Interfaces {
			if i.Class == 0x02 || i.TTY != "" {
				if i.TTY != "" {
					props["companion_cdc_tty"] = "/dev/" + i.TTY
				}
				if i.Driver != "" {
					props["companion_cdc_driver"] = i.Driver
				}
				props["companion_cdc_interfaces"] = "0,1"
			}
		}
		if e.Sim != nil {
			d.Name, d.Transport = "Simulated Espressif USB Serial/JTAG (SYNTHETIC)", "fake"
			props["synthetic"] = "true"
		} else {
			d.Name, d.Transport = strings.TrimSpace(e.Dev.Manufacturer+" "+e.Dev.Product), "usb"
			if d.Name == "" {
				d.Name = "Espressif USB Serial/JTAG"
			}
			props["usb_address"] = fmt.Sprintf("%d:%d", e.Dev.Bus, e.Dev.Address)
			props["usb_device"] = e.Dev.SysName // matches the uart plugin's usb_device for the CDC tty
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
	return Entry{}, status.Errorf(codes.NotFound, "Espressif USB Serial/JTAG device %q is not connected", id)
}

// OpenDevice implements pluginsdk.Plugin: it claims the JTAG interface only.
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
		return s.close()
	}
	return nil
}

func (p *Plugin) isCurrent(id string, s *session) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.devs[id] == s
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
	if s != nil || k != nil {
		// "hart" is also a per-call argument that selects the hart for that
		// call only; it is not a change of the open device's settings.
		delete(over, "hart")
	}
	if s != nil {
		if len(over) > 0 {
			return nil, nil, status.Error(codes.InvalidArgument, "device settings cannot change on an open device: close it and open it again with the new settings")
		}
		return s, func() {}, nil
	}
	if k != nil {
		// A persistent device whose session was lost (unplug, reset): wait,
		// bounded, for the same USB serial to come back and rebuild.
		if len(over) > 0 {
			return nil, nil, status.Error(codes.InvalidArgument, "device settings cannot change on an open device: close it and open it again with the new settings")
		}
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
	return nil, nil, status.Error(codes.Unimplemented, "espusbjtag has no capture capability")
}
