// Package stm32dfuplugin is the hwlab plugin for the STM32 system-memory
// (ROM) bootloader in USB DFU mode (ST DfuSe, VID:PID 0483:df11). It is
// written for, and only programs, the STM32WB5x (e.g. the P-NUCLEO-WB55
// USB dongle MB1293C held in the bootloader with BOOT0): a board without a
// debugger can be inspected, backed up and programmed through its own USB.
//
// Layers:
//
//	internal/usb                     Linux usbfs or macOS IOKit control transfers, no libusb, no cgo
//	plugins/stm32dfu/dfu             DFU 1.1 + DfuSe (descriptors, state machine, the download guard)
//	this package                     identity, the STM32WB secure boundary, flash and FUS methods
//
// SAFETY (see docs/adr/0040-stm32-dfuse-rom-bootloader.md):
//   - The secure boundary (SFSA, CPU2's FUS and wireless stack) is read from
//     the option bytes (FLASH_SFR mirror at 0x1FFF8070, complement checked)
//     and cross-checked with the flash alternate setting. If it is not
//     determined unambiguously every flash method is refused.
//   - Page erase and data downloads are confined by the dfu guard to the
//     flash alternate setting and [0x08000000, SFSA*4K). Mass erase, read
//     unprotect and any download to the option bytes or another alternate
//     setting are never sent.
//   - Erase/program/restore need allow_flash (+allow_erase), the stlink
//     confirmation tokens and a verified backup taken under the same SFSA.
//   - Leaving DFU needs allow_reset and confirm="leave-dfu".
package stm32dfuplugin

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
	"github.com/faronov/hwlab/plugins/stm32dfu/sim"
)

// USB ids of the STM32 ROM DFU bootloader.
const (
	VID = 0x0483
	PID = 0xdf11
)

// SimEnv selects SYNTHETIC devices: "1".."3" devices with SFSA 0xCB, 0xF4
// and 0xE1 in turn.
const SimEnv = "HWLAB_STM32DFU_SIM"

// Entry is one bootloader the plugin can open.
type Entry struct {
	ID     string
	Dev    usb.Device
	Serial string
	Sim    *sim.Device
}

// Lister enumerates devices.
type Lister func() []Entry

// Opener claims the DFU interface of a device.
type Opener func(e Entry, iface int, opt usb.OpenOptions) (usb.Port, error)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	list Lister
	open Opener

	mu   sync.Mutex
	devs map[string]*session
}

// SimSFSAs are the secure boundaries of the synthetic devices.
var SimSFSAs = []uint8{0xCB, 0xF4, 0xE1}

func simDevices(n int) []Entry {
	var es []Entry
	for i := 0; i < n; i++ {
		serial := fmt.Sprintf("SIMDFU%04d", i+1)
		d := sim.New(sim.Config{Serial: serial, SFSA: SimSFSAs[i%len(SimSFSAs)], FUSVersion: sim.FUS053})
		es = append(es, Entry{Dev: d.USBDevice(), Serial: serial, Sim: d})
	}
	return es
}

// New returns the plugin for the host's devices.
func New() *Plugin {
	var simEntries []Entry
	if v := os.Getenv(SimEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 8 {
			n = 1
		}
		simEntries = simDevices(n)
	}
	list := func() []Entry {
		var es []Entry
		for _, d := range usb.Scan("/sys", func(vid, pid uint16) bool { return vid == VID && pid == PID }) {
			es = append(es, Entry{Dev: d, Serial: canonicalSerial(d)})
		}
		for _, e := range simEntries {
			if !e.Sim.Gone() {
				es = append(es, e)
			}
		}
		return nameEntries(es)
	}
	return NewWith(list, defaultOpen)
}

// NewWith injects enumeration and opening (tests).
func NewWith(l Lister, o Opener) *Plugin {
	return &Plugin{list: l, open: o, devs: map[string]*session{}}
}

func defaultOpen(e Entry, iface int, opt usb.OpenOptions) (usb.Port, error) {
	if e.Sim != nil {
		return e.Sim.Port(), nil
	}
	return usb.Open(e.Dev, iface, opt)
}

func canonicalSerial(d usb.Device) string {
	s := strings.TrimSpace(d.Serial)
	if s == "" {
		s = strings.TrimSpace(d.SerialRaw)
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7E {
			return strings.ToUpper(fmt.Sprintf("%x", []byte(s)))
		}
	}
	return s
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

// nameEntries assigns the id <serial>. A device without a serial, or two
// that report the same serial, fall back to <serial>@<sysname> ("dfu@..."
// without a serial), which is only stable while the device stays in the
// same port.
func nameEntries(es []Entry) []Entry {
	count := map[string]int{}
	for _, e := range es {
		count[sanitize(e.Serial)]++
	}
	for i := range es {
		b := sanitize(es[i].Serial)
		if b == "" {
			es[i].ID = "dfu@" + sanitize(es[i].Dev.SysName)
		} else if count[b] > 1 {
			es[i].ID = b + "@" + sanitize(es[i].Dev.SysName)
		} else {
			es[i].ID = b
		}
	}
	sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID })
	return es
}

// dfuInterface returns the DFU interface (class 0xFE).
func dfuInterface(d usb.Device) (usb.Interface, bool) {
	for _, i := range d.Interfaces {
		if i.Class == 0xfe {
			return i, true
		}
	}
	if len(d.Interfaces) == 0 {
		// Discovery without interface details: the ROM uses interface 0.
		return usb.Interface{Number: 0, Class: 0xfe}, true
	}
	return usb.Interface{}, false
}

func capAttrs() map[string]string {
	return map[string]string{
		"interface":       "USB DFU 1.1 + ST DfuSe (system-memory bootloader)",
		"transport":       "usb-control",
		"target_protocol": "stm32-dfuse-rom",
		"memory_access":   "read only (DfuSe UPLOAD); never at or above the STM32WB secure start",
		"flash":           "STM32WB5x user flash below SFSA only: page erase/program (allow_flash/allow_erase, tokens, verified backup)",
		"reset":           "leave DFU (allow_reset, confirm=leave-dfu)",
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

// ListDevices implements pluginsdk.Plugin. It claims nothing.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, e := range p.list() {
		d := &pluginv1.Device{Id: e.ID, Serial: e.Serial, Capabilities: deviceCaps()}
		props := map[string]string{
			"vid": fmt.Sprintf("%04x", e.Dev.VID), "pid": fmt.Sprintf("%04x", e.Dev.PID),
			"manufacturer": e.Dev.Manufacturer, "product": e.Dev.Product, "usb_serial": e.Serial,
			"bootloader_version": bcd(e.Dev.BCD), "bcd_device": fmt.Sprintf("0x%04x", e.Dev.BCD),
		}
		if i, ok := dfuInterface(e.Dev); ok {
			props["dfu_interface"] = strconv.Itoa(i.Number)
			if i.Name != "" {
				props["alt0_name"] = i.Name
			}
		}
		if e.Sim != nil {
			d.Name, d.Transport = "Simulated STM32WB55 ROM DFU (SYNTHETIC)", "fake"
			props["synthetic"] = "true"
		} else {
			d.Name, d.Transport = strings.TrimSpace(e.Dev.Manufacturer+" "+e.Dev.Product), "usb"
			if d.Name == "" {
				d.Name = "STM32 ROM DFU"
			}
			props["usb_address"] = fmt.Sprintf("%d:%d", e.Dev.Bus, e.Dev.Address)
			props["usb_device"] = e.Dev.SysName
		}
		d.Properties = props
		out = append(out, d)
	}
	return out, nil
}

func bcd(v uint16) string { return fmt.Sprintf("%x.%x", v>>8, v&0xFF) }

func (p *Plugin) find(id string) (Entry, error) {
	var ids []string
	for _, e := range p.list() {
		if e.ID == id {
			return e, nil
		}
		ids = append(ids, e.ID)
	}
	msg := fmt.Sprintf("STM32 DFU device %q is not connected", id)
	if len(ids) > 0 {
		msg += " (connected: " + strings.Join(ids, ", ") + ")"
	}
	return Entry{}, status.Error(codes.NotFound, msg)
}

// OpenDevice implements pluginsdk.Plugin: it claims the DFU interface.
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
		_ = old.close()
	}
	s, err := openSession(ctx, e, o, p.open)
	if err != nil {
		return status.Errorf(openCode(err), "open %s: %v", id, err)
	}
	s.persist = true
	p.mu.Lock()
	p.devs[id] = s
	p.mu.Unlock()
	return nil
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	s := p.devs[id]
	delete(p.devs, id)
	p.mu.Unlock()
	if s != nil {
		return wrap(s.close())
	}
	return nil
}

func (p *Plugin) drop(id string, s *session) {
	p.mu.Lock()
	if p.devs[id] == s {
		delete(p.devs, id)
	}
	p.mu.Unlock()
	_ = s.close()
}

// lease returns the open session, or opens one for a single call.
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
		return nil, nil, status.Errorf(openCode(err), "open %s: %v", id, err)
	}
	return s, func() { _ = s.close() }, nil
}

// StartCapture implements pluginsdk.Plugin.
func (p *Plugin) StartCapture(context.Context, pluginsdk.CaptureRequest, *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	return nil, nil, status.Error(codes.Unimplemented, "stm32dfu has no capture capability")
}
