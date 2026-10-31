// Package wstk is the hwlab plugin for Silicon Labs Wireless Starter Kit
// mainboards (BRD4001A and relatives). It talks to the kit natively, without
// Simplicity Commander or silink:
//
//   - over USB, through the kit's on-board J-Link, using EMUCOM channels of
//     the J-Link protocol (plugins/jlinknative/usb): kit identification and
//     the Advanced Energy Monitor (AEM) current/voltage stream;
//   - over Ethernet, through the kit's TCP services: the read-only admin
//     console (port 4902) for identification and low-rate AEM polling, and
//     the Debug Channel (port 4905) for Packet Trace (PTI) capture.
//
// The protocols live in the protocol subpackage; see
// docs/adr/0037-wstk-native.md for how they were established and what is
// and is not verified.
package wstk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	jusb "github.com/faronov/hwlab/plugins/jlinknative/usb"
	"github.com/faronov/hwlab/plugins/wstk/protocol"
)

// Environment.
const (
	// HostsEnv lists kits reachable over Ethernet: "name=host,..." or bare
	// hosts (the host then names the device).
	HostsEnv = "HWLAB_WSTK_HOSTS"
	// SimEnv ("1") adds a SYNTHETIC kit (wstk:SIM) backed by protocol.SimKit.
	SimEnv = "HWLAB_WSTK_SIM"
	// ListIdentifyEnv ("0") stops device listing from reading the board
	// list of a USB kit it has not identified yet.
	ListIdentifyEnv = "HWLAB_WSTK_LIST_IDENTIFY"
)

// TCP ports of the kit's network services.
const (
	AdminPort = 4902
	DCHPort   = 4905
)

const (
	usbPrefix = "wstk:"
	netPrefix = "wstk-net:"
	simSerial = "SIM"

	kitTimeout = 2 * time.Second
	// listIdentify bounds the J-Link hold of an identification made while
	// listing devices (two small kit commands, ~80 ms on a BRD4001A);
	// listRetry spaces attempts that failed (probe busy, kit absent).
	listIdentify = time.Second
	listRetry    = time.Minute
)

// USBKit is a kit found on USB. Nothing is opened to find it.
type USBKit struct {
	Serial       string // J-Link USB serial, leading zeros stripped
	Manufacturer string
	Product      string
	Info         jusb.Info
}

// KitConn is an open EMUCOM link to a kit.
type KitConn interface {
	protocol.Link
	Close() error
}

// Env injects the host side: USB discovery and opening, network hosts and
// dialing. Tests replace it.
type Env struct {
	ScanUSB func() []USBKit
	OpenUSB func(USBKit) (KitConn, error)
	Hosts   func() map[string]string // device name -> host
	Dial    func(ctx context.Context, addr string) (net.Conn, error)
	// VCOM finds the kit's virtual COM port path from its serial.
	VCOM func(serial string) string
	// IdentifyOnList lets ListDevices identify a USB kit it has no board
	// information for.
	IdentifyOnList bool
}

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	env Env

	mu   sync.Mutex
	open map[string]map[string]any // opened device id -> config
	busy map[string]bool           // device ids with a running capture
	meta map[string]*kitMeta       // identification cache by device id
	// tried is when listing last tried to identify a kit.
	tried map[string]time.Time
}

type kitMeta struct {
	info   protocol.KitInfo
	boards []protocol.Board
	admin  map[string]string // net kits: raw admin answers
}

// New returns the plugin for the host's kits.
func New() *Plugin {
	var sim *protocol.SimKit
	if os.Getenv(SimEnv) == "1" {
		sim = protocol.NewSimKit()
	}
	return NewWith(DefaultEnv(sim))
}

// DefaultEnv is the real host environment, plus a simulated kit when sim is
// not nil.
func DefaultEnv(sim *protocol.SimKit) Env {
	return Env{
		ScanUSB: func() []USBKit {
			kits := ScanUSB(jusb.Scan("/sys"))
			if sim != nil {
				kits = append(kits, USBKit{Serial: simSerial, Manufacturer: "hwlab", Product: "SYNTHETIC WSTK"})
			}
			return kits
		},
		OpenUSB: func(k USBKit) (KitConn, error) {
			if k.Serial == simSerial && sim != nil {
				return nopCloser{sim}, nil
			}
			return jusb.Open(k.Info)
		},
		Hosts: func() map[string]string { return ParseHosts(os.Getenv(HostsEnv)) },
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
		VCOM:           findVCOM,
		IdentifyOnList: os.Getenv(ListIdentifyEnv) != "0",
	}
}

type nopCloser struct{ protocol.Link }

func (nopCloser) Close() error { return nil }

// NewWith returns a plugin using env.
func NewWith(env Env) *Plugin {
	return &Plugin{env: env, open: map[string]map[string]any{}, busy: map[string]bool{}, meta: map[string]*kitMeta{},
		tried: map[string]time.Time{}}
}

// ScanUSB keeps the J-Link probes that are Silicon Labs kits: SEGGER's VID
// with a Silicon Labs or Energy Micro manufacturer string (the kit's
// on-board "J-Link Pro OB" / "J-Link EnergyMicro").
func ScanUSB(infos []jusb.Info) []USBKit {
	var out []USBKit
	for _, i := range infos {
		m := strings.ToLower(i.Device.Manufacturer + " " + i.Device.Product)
		if !strings.Contains(m, "silicon lab") && !strings.Contains(m, "energy micro") && !strings.Contains(m, "energymicro") {
			continue
		}
		out = append(out, USBKit{Serial: i.Serial, Manufacturer: i.Device.Manufacturer, Product: i.Device.Product, Info: i})
	}
	return out
}

// ParseHosts parses HostsEnv.
func ParseHosts(s string) map[string]string {
	out := map[string]string{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		name, host, ok := strings.Cut(f, "=")
		if !ok {
			host = name
		}
		name, host = strings.TrimSpace(name), strings.TrimSpace(host)
		if name != "" && host != "" {
			out[name] = host
		}
	}
	return out
}

func findVCOM(serial string) string {
	for _, pat := range []string{"/dev/cu.usbmodem*" + serial + "*", "/dev/serial/by-id/*" + serial + "*"} {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			sort.Strings(m)
			return m[0]
		}
	}
	return ""
}

func caps(ids ...string) []*pluginv1.Capability {
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1}
	}
	return out
}

func usbCaps() []*pluginv1.Capability {
	return caps(pluginv1.CapPowerMeasurement, pluginv1.CapVoltageMeasurement, pluginv1.CapTimedCapture, pluginv1.CapLiveRawStream)
}

func netCaps() []*pluginv1.Capability {
	return caps(pluginv1.CapPowerMeasurement, pluginv1.CapVoltageMeasurement, pluginv1.CapPacketCapture,
		pluginv1.CapTimedCapture, pluginv1.CapLiveRawStream, pluginv1.CapArtifactExport)
}

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	return &pluginv1.PluginInfo{
		Id: "wstk", Name: "Silicon Labs Wireless Starter Kit (native AEM / PTI)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: netCaps(),
		Methods: []*pluginv1.MethodSpec{
			{Capability: pluginv1.CapPowerMeasurement, Name: "info", Danger: pluginv1.Danger_DANGER_SAFE,
				Description: "read kit firmware, mainboard and radio board ids (USB: kit control channel; Ethernet: admin 'sys ver'/'boardid list')"},
			{Capability: pluginv1.CapPowerMeasurement, Name: "read_current", Danger: pluginv1.Danger_DANGER_SAFE,
				Description: "one AEM reading: USB averages one 25.6 ms buffer (mean/min/max mA, V); Ethernet reads the admin console"},
			{Capability: pluginv1.CapPowerMeasurement, Name: "admin_read", Danger: pluginv1.Danger_DANGER_SAFE,
				Description: "Ethernet only: run one read-only admin console command (args: command, one of " + strings.Join(protocol.AdminCommands(), ", ") + ")"},
			{Capability: pluginv1.CapPacketCapture, Name: "pti_status", Danger: pluginv1.Danger_DANGER_SAFE,
				Description: "Ethernet only: admin 'pti info' (PTI mode and packet counters)"},
		},
	}
}

// ListDevices implements pluginsdk.Plugin. The first listing of a USB kit
// reads its firmware and board list (two kit commands, the J-Link held for
// well under a second); after that the cache answers and the probe is not
// opened. A probe held by a debugger is skipped and tried again at most once
// a minute; ListIdentifyEnv=0 turns this off.
func (p *Plugin) ListDevices(ctx context.Context) ([]*pluginv1.Device, error) {
	var out []*pluginv1.Device
	for _, k := range p.env.ScanUSB() {
		id := usbPrefix + k.Serial
		p.identifyOnList(ctx, id, k)
		props := map[string]string{
			"transport": "usb (J-Link EMUCOM)", "jlink_serial": k.Serial, "usb_product": k.Product, "usb_manufacturer": k.Manufacturer,
			// The same probe as seen by the J-Link plugins (hwlab device ids).
			"jlink_devices": "jlink/" + k.Serial + ", jlinknative/jlink-native:" + k.Serial,
		}
		if p.env.VCOM != nil {
			if v := p.env.VCOM(k.Serial); v != "" {
				props["vcom"] = v
				if real, err := filepath.EvalSymlinks(v); err == nil {
					v = real // /dev/serial/by-id/... -> /dev/ttyACMn
				}
				props["uart_device"] = "uart/" + filepath.Base(v)
			}
		}
		out = append(out, &pluginv1.Device{Id: id, Name: p.deviceName(id), Serial: k.Serial, Transport: "usb",
			Capabilities: usbCaps(), Properties: p.metaProps(id, props)})
	}
	hosts := p.env.Hosts()
	names := make([]string, 0, len(hosts))
	for n := range hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		id := netPrefix + n
		props := map[string]string{"transport": "tcp", "host": hosts[n],
			"admin_port": fmt.Sprint(AdminPort), "dch_port": fmt.Sprint(DCHPort)}
		out = append(out, &pluginv1.Device{Id: id, Name: p.deviceName(id), Transport: "tcp",
			Capabilities: netCaps(), Properties: p.metaProps(id, props)})
	}
	return out, nil
}

func (p *Plugin) identifyOnList(ctx context.Context, id string, k USBKit) {
	if !p.env.IdentifyOnList {
		return
	}
	p.mu.Lock()
	skip := p.meta[id] != nil || p.busy[id] || time.Since(p.tried[id]) < listRetry
	if !skip {
		p.tried[id] = time.Now()
	}
	p.mu.Unlock()
	if skip {
		return
	}
	c, err := p.env.OpenUSB(k)
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, listIdentify)
	defer cancel()
	if m, err := identify(ctx, &protocol.Emucom{L: c}); err == nil {
		p.setMeta(id, m)
	}
}

func (p *Plugin) deviceName(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	name := "Silicon Labs Wireless Starter Kit"
	if m := p.meta[id]; m != nil && len(m.boards) > 0 {
		var parts []string
		for _, b := range m.boards {
			parts = append(parts, b.Part)
		}
		name += " (" + strings.Join(parts, " + ") + ")"
	}
	return name
}

func (p *Plugin) metaProps(id string, props map[string]string) map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.meta[id]
	if m == nil {
		props["boards"] = "unknown until info or a capture has run"
		return props
	}
	for k, v := range metaResult(m) {
		props[k] = fmt.Sprint(v)
	}
	return props
}

func metaResult(m *kitMeta) map[string]any {
	res := map[string]any{}
	if m.info.Firmware != "" {
		res["firmware"] = m.info.Firmware
		res["firmware_build"] = float64(m.info.Build)
		res["mainboard_serial"] = fmt.Sprint(m.info.MainboardSerial)
	}
	var ids []string
	for i, b := range m.boards {
		ids = append(ids, b.ID())
		pre := fmt.Sprintf("board%d_", i)
		res[pre+"id"] = b.ID()
		res[pre+"serial"] = fmt.Sprint(b.Serial)
		res[pre+"date"] = b.Date
		if b.Name != "" {
			res[pre+"name"] = b.Name
		}
	}
	if len(ids) > 0 {
		res["boards"] = strings.Join(ids, ", ")
	}
	for k, v := range m.admin {
		res["admin."+k] = v
	}
	return res
}

func (p *Plugin) setMeta(id string, m *kitMeta) {
	p.mu.Lock()
	p.meta[id] = m
	p.mu.Unlock()
}

// OpenDevice implements pluginsdk.Plugin. It only validates and records
// the configuration: the J-Link is opened per call and per capture and
// released afterwards, so a debugger can share the probe between them.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	if _, err := p.resolve(id); err != nil {
		return err
	}
	if _, err := parseOptions(cfg, strings.HasPrefix(id, netPrefix)); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.open[id]; ok {
		return fmt.Errorf("device %q already open", id)
	}
	p.open[id] = cfg
	return nil
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.open, id)
	return nil
}

// target is a resolved device id.
type target struct {
	id   string
	usb  *USBKit
	host string // net kits
}

func (p *Plugin) resolve(id string) (target, error) {
	switch {
	case strings.HasPrefix(id, usbPrefix):
		want := jusb.NormalizeSerial(strings.TrimPrefix(id, usbPrefix))
		for _, k := range p.env.ScanUSB() {
			if k.Serial == want {
				k := k
				return target{id: id, usb: &k}, nil
			}
		}
		return target{}, status.Errorf(codes.NotFound, "wstk %q not found on USB (is the kit plugged in?)", id)
	case strings.HasPrefix(id, netPrefix):
		if h, ok := p.env.Hosts()[strings.TrimPrefix(id, netPrefix)]; ok {
			return target{id: id, host: h}, nil
		}
		return target{}, status.Errorf(codes.NotFound, "wstk %q is not listed in %s", id, HostsEnv)
	}
	return target{}, status.Errorf(codes.NotFound, "unknown wstk device %q", id)
}

// openKit opens the kit's J-Link. A probe held by another program (a
// debugger, Simplicity Commander) is reported as unavailable, not retried.
func (p *Plugin) openKit(t target) (*protocol.Emucom, func(), error) {
	c, err := p.env.OpenUSB(*t.usb)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "open the kit's J-Link %s: %v (another debugger or tool may hold it; it is opened exclusively)", t.usb.Serial, err)
	}
	return &protocol.Emucom{L: c}, func() { _ = c.Close() }, nil
}

// identify reads firmware and board records over the kit control channel.
func identify(ctx context.Context, e *protocol.Emucom) (*kitMeta, error) {
	r, err := e.KitCommand(ctx, protocol.KitCmdInfo, 1, nil, kitTimeout)
	if err != nil {
		return nil, fmt.Errorf("kit info: %w", err)
	}
	info, err := protocol.ParseKitInfo(r)
	if err != nil {
		return nil, err
	}
	r, err = e.KitCommand(ctx, protocol.KitCmdBoards, 1, nil, kitTimeout)
	if err != nil {
		return nil, fmt.Errorf("board list: %w", err)
	}
	boards, err := protocol.ParseBoards(r)
	if err != nil {
		return nil, err
	}
	return &kitMeta{info: info, boards: boards}, nil
}

func (p *Plugin) markBusy(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy[id] {
		return status.Errorf(codes.FailedPrecondition, "a capture is running on %s", id)
	}
	p.busy[id] = true
	return nil
}

func (p *Plugin) clearBusy(id string) {
	p.mu.Lock()
	delete(p.busy, id)
	p.mu.Unlock()
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	t, err := p.resolve(req.GetDeviceId())
	if err != nil {
		return nil, err
	}
	args := req.GetArgs().AsMap()
	var res map[string]any
	switch m := req.GetCapability() + "." + req.GetMethod(); {
	case t.usb != nil:
		if err := p.markBusy(t.id); err != nil {
			return nil, err
		}
		defer p.clearBusy(t.id)
		res, err = p.invokeUSB(ctx, t, m)
	default:
		res, err = p.invokeNet(ctx, t, m, args)
	}
	if err != nil {
		return nil, err
	}
	st, err := structpb.NewStruct(res)
	if err != nil {
		return nil, err
	}
	return &pluginv1.InvokeResponse{Result: st}, nil
}

func (p *Plugin) invokeUSB(ctx context.Context, t target, method string) (map[string]any, error) {
	switch method {
	case pluginv1.CapPowerMeasurement + ".info", pluginv1.CapPowerMeasurement + ".read_current":
	case pluginv1.CapPowerMeasurement + ".admin_read", pluginv1.CapPacketCapture + ".pti_status":
		return nil, status.Errorf(codes.Unimplemented, "%s needs the kit's admin console, which hwlab reaches over Ethernet only (set %s)", method, HostsEnv)
	default:
		return nil, status.Errorf(codes.Unimplemented, "unsupported method %s", method)
	}
	e, release, err := p.openKit(t)
	if err != nil {
		return nil, err
	}
	defer release()
	if method == pluginv1.CapPowerMeasurement+".info" {
		m, err := identify(ctx, e)
		if err != nil {
			return nil, err
		}
		p.setMeta(t.id, m)
		return metaResult(m), nil
	}
	b, err := readOneBuffer(ctx, e)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"voltage_v": float64(b.VoltageV), "samples": float64(len(b.CurrentmA)),
		"sample_rate_hz": float64(b.SampleRate), "source": "AEM buffer over USB"}
	for k, v := range currentStats(b.CurrentmA) {
		res[k] = v
	}
	return res, nil
}

func currentStats(mA []float32) map[string]any {
	if len(mA) == 0 {
		return map[string]any{}
	}
	lo, hi, sum := mA[0], mA[0], 0.0
	for _, v := range mA {
		lo, hi = min(lo, v), max(hi, v)
		sum += float64(v)
	}
	return map[string]any{"current_mean_ma": sum / float64(len(mA)), "current_min_ma": float64(lo), "current_max_ma": float64(hi)}
}

var errNoData = errors.New("the kit sent no AEM data")
