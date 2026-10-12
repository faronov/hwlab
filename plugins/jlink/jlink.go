// Package jlink is the hwlab plugin for SEGGER J-Link debug probes.
//
// Probe discovery needs no SEGGER software (USB sysfs). Debug operations and
// RTT use SEGGER's J-Link Commander (JLinkExe) as an external runtime
// dependency; the proprietary probe protocol is not reimplemented. Each
// capability is advertised independently: without the commander only
// discovery is offered. The core knows none of this: it sees the generic
// DebugProbe, MemoryAccess, FlashProgramming and RTT capabilities.
package jlink

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// maxMemBytes bounds one read; Commander allows at most 0x100000 items.
const maxMemBytes = 1 << 20

// maxWriteBytes bounds one write script.
const maxWriteBytes = 4096

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	sysfs string
	cmd   Commander

	mu       sync.Mutex
	cfg      map[string]map[string]any
	locks    map[string]*sync.Mutex
	rttPorts map[int]bool    // telnet ports reserved by running RTT captures
	rttHeld  map[string]bool // probes whose RTT Commander session is open
}

// New returns the plugin using the real J-Link Commander.
func New() *Plugin { return NewWith(DefaultSysfs, &ExecCommander{}) }

// NewWith injects the USB tree root and the commander (tests).
func NewWith(sysfs string, c Commander) *Plugin {
	return &Plugin{sysfs: sysfs, cmd: c, cfg: map[string]map[string]any{}, locks: map[string]*sync.Mutex{}, rttPorts: map[int]bool{}, rttHeld: map[string]bool{}}
}

func caps(attrs map[string]string, ids ...string) []*pluginv1.Capability {
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: attrs}
	}
	return out
}

func (p *Plugin) toolPath() (string, bool) {
	path, err := p.cmd.Available()
	return path, err == nil
}

func safe(capName, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: capName, Name: name, Description: desc, Danger: pluginv1.Danger_DANGER_SAFE, ArgsSchemaJson: schema}
}

func confirm(capName, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: capName, Name: name, Description: desc, Danger: pluginv1.Danger_DANGER_CONFIRM, ArgsSchemaJson: schema}
}

const targetProps = `"target":{"type":"string","description":"J-Link device name, e.g. NRF52840_XXAA"},"interface":{"type":"string","enum":["SWD","JTAG"]},"speed_khz":{"type":"number"}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	path, ok := p.toolPath()
	desc := "SEGGER J-Link Commander (JLinkExe) from the J-Link Software and Documentation Pack; needed for every operation except discovery"
	if ok {
		desc += " (found at " + path + ")"
	}
	return &pluginv1.PluginInfo{
		Id: "jlink", Name: "SEGGER J-Link debug probe", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: caps(nil, pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming, pluginv1.CapRTT, pluginv1.CapLiveRawStream),
		RuntimeRequirements: []*pluginv1.RuntimeRequirement{
			{Name: "JLinkExe", Kind: "binary", Optional: false, Description: desc, Satisfied: ok},
		},
		Methods: []*pluginv1.MethodSpec{
			safe(pluginv1.CapDebugProbe, "info", "probe firmware, hardware version and measured target voltage (VTref); needs no target", `{"type":"object"}`),
			safe(pluginv1.CapDebugProbe, "connect", "connect to the target and report the core; args: "+"target, interface, speed_khz", `{"type":"object","required":["target"],"properties":{`+targetProps+`}}`),
			confirm(pluginv1.CapDebugProbe, "halt", "halt the target CPU and confirm it with IsHalted; every call is its own commander session and the target runs again when it ends", `{"type":"object","properties":{`+targetProps+`}}`),
			confirm(pluginv1.CapDebugProbe, "resume", "resume the target CPU and confirm it is running", `{"type":"object","properties":{`+targetProps+`}}`),
			confirm(pluginv1.CapDebugProbe, "reset", "reset the target; resumes it unless run=false", `{"type":"object","properties":{`+targetProps+`,"run":{"type":"boolean"}}}`),
			confirm(pluginv1.CapDebugProbe, "read_registers", "halt the target and read the CPU registers", `{"type":"object","properties":{`+targetProps+`}}`),
			safe(pluginv1.CapMemoryAccess, "read", "read target memory; args: address, length (bytes, max 1 MiB). May halt the CPU briefly", `{"type":"object","required":["address","length"],"properties":{`+targetProps+`,"address":{},"length":{"type":"number"}}}`),
			confirm(pluginv1.CapMemoryAccess, "write", "write target memory; args: address, data (hex string, max 4096 bytes)", `{"type":"object","required":["address","data"],"properties":{`+targetProps+`,"address":{},"data":{"type":"string"}}}`),
			confirm(pluginv1.CapFlashProgramming, "program", "program a .hex/.srec/.bin file; .bin needs address; resets and runs afterwards unless run=false", `{"type":"object","required":["path"],"properties":{`+targetProps+`,"path":{"type":"string"},"address":{},"run":{"type":"boolean"}}}`),
		},
	}
}

func deviceCaps(probeTool bool) []*pluginv1.Capability {
	if probeTool {
		return caps(nil, pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming, pluginv1.CapRTT, pluginv1.CapLiveRawStream)
	}
	return caps(map[string]string{"operations": "discovery-only", "missing": "JLinkExe"}, pluginv1.CapDebugProbe)
}

// probes lists every attached probe. USB sysfs gives the serial numbers; the
// commander's ShowEmuList adds the model and nickname (and is the only source
// on hosts without sysfs). It never selects or connects to a probe.
func (p *Plugin) probes(ctx context.Context) []Probe {
	ps := scanSysfs(p.sysfs)
	if _, ok := p.toolPath(); ok {
		ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if out, err := p.cmd.Run(ectx, "", Target{}, []string{"ShowEmuList USB"}); err == nil {
			ps = mergeProbes(ps, parseEmuList(out))
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Serial < ps[j].Serial })
	return ps
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(ctx context.Context) ([]*pluginv1.Device, error) {
	_, tool := p.toolPath()
	var out []*pluginv1.Device
	for _, pr := range p.probes(ctx) {
		id := pr.Serial
		if id == "" {
			id = filepath.Base(pr.Path)
		}
		props := map[string]string{"vid": SEGGERVID, "pid": pr.PID, "product": pr.Product, "commander": fmt.Sprint(tool)}
		name := "SEGGER J-Link"
		switch {
		case pr.Model != "":
			name = pr.Model
			props["model"] = pr.Model
		case pr.Product != "":
			name = pr.Product
		}
		if pr.Nickname != "" {
			props["nickname"] = pr.Nickname
		}
		if pr.Path != "" {
			props["usb_device"] = filepath.Base(pr.Path)
		}
		out = append(out, &pluginv1.Device{Id: id, Name: name, Serial: pr.Serial, Transport: "usb",
			Capabilities: deviceCaps(tool), Properties: props})
	}
	return out, nil
}

func target(cfg map[string]any) (Target, error) {
	t := Target{}
	if v, ok := cfg["target"]; ok {
		s, ok := v.(string)
		if !ok {
			return t, errors.New("target must be a string (J-Link device name)")
		}
		t.Device = strings.TrimSpace(s)
	}
	if v, ok := cfg["interface"]; ok {
		s, _ := v.(string)
		switch strings.ToUpper(s) {
		case "SWD", "JTAG":
			t.Interface = strings.ToUpper(s)
		default:
			return t, fmt.Errorf("interface %v: want SWD or JTAG", v)
		}
	}
	if v, ok := cfg["speed_khz"]; ok {
		n, err := toInt(v)
		if err != nil || n < 1 || n > 50000 {
			return t, fmt.Errorf("speed_khz %v: want 1..50000", v)
		}
		t.SpeedKHz = n
	}
	return t, nil
}

func toInt(v any) (int, error) {
	switch x := v.(type) {
	case float64:
		return int(x), nil
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case string:
		return strconv.Atoi(x)
	}
	return 0, fmt.Errorf("not a number: %v", v)
}

// toAddr accepts a number or a hex/decimal string ("0x20000000").
func toAddr(v any) (uint64, error) {
	switch x := v.(type) {
	case float64:
		if x < 0 || x != float64(uint64(x)) {
			return 0, fmt.Errorf("invalid address %v", x)
		}
		return uint64(x), nil
	case string:
		return strconv.ParseUint(strings.TrimSpace(x), 0, 64)
	}
	return 0, fmt.Errorf("address must be a number or a string like \"0x20000000\"")
}

// OpenDevice implements pluginsdk.Plugin.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	if _, err := target(cfg); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if v, ok := cfg["rtt_port"]; ok {
		if n, err := toInt(v); err != nil || n < 1 || n > 65535 {
			return status.Errorf(codes.InvalidArgument, "rtt_port %v: want 1..65535", v)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg[id] = cfg
	return nil
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cfg, id)
	return nil
}

func (p *Plugin) lock(id string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.locks[id] == nil {
		p.locks[id] = &sync.Mutex{}
	}
	return p.locks[id]
}

// merged returns the device's OpenDevice config overridden by call args.
func (p *Plugin) merged(id string, args map[string]any) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := map[string]any{}
	for k, v := range p.cfg[id] {
		m[k] = v
	}
	for k, v := range args {
		m[k] = v
	}
	return m
}

func unavailable(err error) error { return status.Error(codes.FailedPrecondition, err.Error()) }

// run serialises access to one probe and checks the output for failures.
//
// Every call passes the device id as -USB <serial>: more than one probe may be
// attached, and without it the commander cannot choose. The serial number the
// commander reports is checked against the requested one, so a call can never
// silently run on another probe.
func (p *Plugin) run(ctx context.Context, id string, t Target, script []string) (string, error) {
	if _, err := p.cmd.Available(); err != nil {
		return "", unavailable(err)
	}
	if !validSerial(id) {
		return "", status.Errorf(codes.InvalidArgument, "device id %q is not a J-Link serial number", id)
	}
	l := p.lock(id)
	l.Lock()
	defer l.Unlock()
	out, err := p.cmd.Run(ctx, id, t, script)
	if err != nil {
		return out, status.Error(codes.Unavailable, err.Error())
	}
	if f := failure(out); f != "" {
		if strings.Contains(strings.ToLower(f), "cannot connect to the probe") {
			if o := p.otherRTTHolders(NormalizeSerial(id)); len(o) > 0 {
				f += fmt.Sprintf(" (an RTT Commander session is also open on probe %s: SEGGER's DLL lets other processes share only one probe held by a long-running session at a time, so run RTT on one probe, or set rtt: false in the config of the others)", strings.Join(o, ", "))
			}
		}
		return out, status.Errorf(codes.Unavailable, "J-Link: %s", f)
	}
	if got := outputSerial(out); got != "" && got != NormalizeSerial(id) {
		return out, status.Errorf(codes.Internal, "J-Link: requested probe %s but the commander answered from probe %s; refusing to continue", id, got)
	}
	return out, nil
}

// validSerial reports whether id looks like a J-Link serial number (decimal).
func validSerial(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (p *Plugin) needTarget(cfg map[string]any) (Target, error) {
	t, err := target(cfg)
	if err != nil {
		return t, status.Error(codes.InvalidArgument, err.Error())
	}
	if t.Device == "" {
		return t, status.Error(codes.InvalidArgument, `no target device: pass "target" (a J-Link device name such as NRF52840_XXAA) in the device config or the call args`)
	}
	return t, nil
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	id := req.GetDeviceId()
	args := p.merged(id, req.GetArgs().AsMap())
	var res map[string]any
	var err error
	switch req.GetCapability() + "." + req.GetMethod() {
	case pluginv1.CapDebugProbe + ".info":
		res, err = p.info(ctx, id)
	case pluginv1.CapDebugProbe + ".connect":
		res, err = p.simple(ctx, id, args, []string{"connect"}, true)
	case pluginv1.CapDebugProbe + ".halt":
		res, err = p.simple(ctx, id, args, []string{"h", "IsHalted"}, false, expect(true))
	case pluginv1.CapDebugProbe + ".resume":
		res, err = p.simple(ctx, id, args, []string{"g", "IsHalted"}, false, expect(false))
	case pluginv1.CapDebugProbe + ".reset":
		script, want := []string{"r"}, true
		if run, ok := args["run"].(bool); !ok || run {
			script, want = append(script, "g"), false
		}
		res, err = p.simple(ctx, id, args, append(script, "IsHalted"), false, expect(want))
	case pluginv1.CapDebugProbe + ".read_registers":
		res, err = p.registers(ctx, id, args)
	case pluginv1.CapMemoryAccess + ".read":
		res, err = p.readMem(ctx, id, args)
	case pluginv1.CapMemoryAccess + ".write":
		res, err = p.writeMem(ctx, id, args)
	case pluginv1.CapFlashProgramming + ".program":
		res, err = p.program(ctx, id, args)
	default:
		return nil, status.Errorf(codes.Unimplemented, "unsupported method %s.%s", req.GetCapability(), req.GetMethod())
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

func (p *Plugin) info(ctx context.Context, id string) (map[string]any, error) {
	// Commander connects lazily; "f" forces the probe connection and prints
	// the firmware/hardware block.
	out, err := p.run(ctx, id, Target{}, []string{"f", "ShowEmuList USB"})
	if err != nil {
		return nil, err
	}
	info := probeInfo(out)
	if e, ok := emuEntry(out, NormalizeSerial(id)); ok {
		if e.Model != "" {
			info["model"] = e.Model
		}
		if e.Nickname != "" {
			info["nickname"] = e.Nickname
		}
	}
	info["serial_requested"] = id
	info["output"] = tail(out, 12)
	return info, nil
}

// expect is the CPU state a call must leave, checked with IsHalted in the same
// commander session (the state does not outlive the session: see Info).
func expect(halted bool) *bool { return &halted }

func (p *Plugin) simple(ctx context.Context, id string, args map[string]any, script []string, wantTarget bool, wantHalted ...*bool) (map[string]any, error) {
	t, err := p.needTarget(args)
	if err != nil {
		return nil, err
	}
	out, err := p.run(ctx, id, t, script)
	if err != nil {
		return nil, err
	}
	res := targetInfo(out)
	res["target"] = t.Device
	res["probe_serial"] = NormalizeSerial(id)
	res["ok"] = true
	if halted, pc, ok := haltState(out); ok {
		res["halted"] = halted
		if pc != "" {
			res["pc"] = pc
		}
		if len(wantHalted) == 1 && wantHalted[0] != nil && *wantHalted[0] != halted {
			return nil, status.Errorf(codes.Unavailable, "J-Link: target halted=%v after the call, expected %v", halted, *wantHalted[0])
		}
	}
	if wantTarget && res["core"] == nil {
		res["note"] = "no core identification in the output; the connection could not be confirmed"
		res["ok"] = false
	}
	res["output"] = tail(out, 12)
	return res, nil
}

func (p *Plugin) registers(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	t, err := p.needTarget(args)
	if err != nil {
		return nil, err
	}
	out, err := p.run(ctx, id, t, []string{"h", "regs"})
	if err != nil {
		return nil, err
	}
	regs := parseRegs(out)
	if len(regs) == 0 {
		return nil, status.Error(codes.Internal, "no registers in the commander output")
	}
	m := map[string]any{}
	for k, v := range regs {
		m[k] = v
	}
	return map[string]any{"registers": m, "halted": true, "probe_serial": NormalizeSerial(id), "target": t.Device}, nil
}

func (p *Plugin) readMem(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	t, err := p.needTarget(args)
	if err != nil {
		return nil, err
	}
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	n, err := toInt(args["length"])
	if err != nil || n < 1 || n > maxMemBytes {
		return nil, status.Errorf(codes.InvalidArgument, "length %v: want 1..%d bytes", args["length"], maxMemBytes)
	}
	out, err := p.run(ctx, id, t, []string{fmt.Sprintf("mem8 0x%X 0x%X", addr, n)})
	if err != nil {
		return nil, err
	}
	data, err := parseMem(out, 1, n)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return map[string]any{"address": fmt.Sprintf("0x%X", addr), "length": float64(n), "hex": hex.EncodeToString(data),
		"probe_serial": NormalizeSerial(id), "target": t.Device}, nil
}

func (p *Plugin) writeMem(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	t, err := p.needTarget(args)
	if err != nil {
		return nil, err
	}
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s, _ := args["data"].(string)
	data, err := hex.DecodeString(strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(s), "0x"), " ", ""))
	if err != nil || len(data) == 0 || len(data) > maxWriteBytes {
		return nil, status.Errorf(codes.InvalidArgument, "data: want 1..%d bytes as a hex string", maxWriteBytes)
	}
	if _, err := p.run(ctx, id, t, writeScript(addr, data)); err != nil {
		return nil, err
	}
	return map[string]any{"address": fmt.Sprintf("0x%X", addr), "written": float64(len(data))}, nil
}

func (p *Plugin) program(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	t, err := p.needTarget(args)
	if err != nil {
		return nil, err
	}
	path, _ := args["path"].(string)
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "path is required")
	}
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return nil, status.Errorf(codes.InvalidArgument, "firmware file %q not found", path)
	}
	load := fmt.Sprintf("loadfile %q", path)
	if strings.EqualFold(filepath.Ext(path), ".bin") {
		a, err := toAddr(args["address"])
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, ".bin files need an address (e.g. \"0x0\")")
		}
		load = fmt.Sprintf("loadfile %q 0x%X", path, a)
	}
	script := []string{"h", load, "r"}
	if run, ok := args["run"].(bool); !ok || run {
		script = append(script, "g")
	}
	out, err := p.run(ctx, id, t, script)
	if err != nil {
		return nil, err
	}
	return map[string]any{"programmed": path, "output": tail(out, 12)}, nil
}

func rttConfigFrom(cfg map[string]any) (rttConfig, error) {
	rc := rttConfig{host: "127.0.0.1", port: DefaultRTTPort, launch: true, connectTimeout: 10 * time.Second}
	t, err := target(cfg)
	if err != nil {
		return rc, err
	}
	rc.target = t
	if v, ok := cfg["rtt"].(bool); ok && !v {
		rc.disabled, rc.launch = true, false
		return rc, nil
	}
	if v, ok := cfg["rtt_host"].(string); ok && v != "" {
		rc.host = v
	}
	if v, ok := cfg["rtt_port"]; ok {
		if rc.port, err = toInt(v); err != nil || rc.port < 1 || rc.port > 65535 {
			return rc, fmt.Errorf("rtt_port %v: want 1..65535", v)
		}
	}
	if v, ok := cfg["rtt_launch"].(bool); ok {
		rc.launch = v
	}
	if v, ok := cfg["rtt_address"]; ok {
		if rc.controlBlock, err = toAddr(v); err != nil || rc.controlBlock == 0 {
			return rc, fmt.Errorf("rtt_address %v: want the RTT control block address, e.g. \"0x20038000\"", v)
		}
	}
	if v, ok := cfg["rtt_connect_timeout_ms"]; ok {
		n, err := toInt(v)
		if err != nil || n < 100 {
			return rc, fmt.Errorf("rtt_connect_timeout_ms %v: want >= 100", v)
		}
		rc.connectTimeout = time.Duration(n) * time.Millisecond
	}
	if rc.launch && rc.target.Device == "" {
		return rc, errors.New(`rtt needs "target" (J-Link device name) to launch the commander, or rtt_launch: false when another J-Link program already serves RTT`)
	}
	if !rc.launch {
		if h, _, err := net.SplitHostPort(net.JoinHostPort(rc.host, "0")); err != nil || h == "" {
			return rc, fmt.Errorf("rtt_host %q is not valid", rc.host)
		}
	}
	return rc, nil
}

// StartCapture implements pluginsdk.Plugin: RTT terminal output as Log events.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	cfg := p.merged(req.DeviceID, req.Config)
	rc, err := rttConfigFrom(cfg)
	if err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if rc.launch {
		if _, err := p.cmd.Available(); err != nil {
			return nil, nil, unavailable(err)
		}
	}
	if rc.disabled {
		return &rttCapture{cfg: rc, serial: req.DeviceID, cmd: p.cmd, clock: req.Clock, sink: sink},
			&pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: rttStream, Kind: "log",
				Attributes: map[string]string{"source": LogSource, "channel": "0", "rtt": "disabled"}}}}, nil
	}
	if _, explicit := cfg["rtt_port"]; !explicit && rc.launch {
		// Each probe's commander serves RTT on its own telnet port; a fixed
		// default would make a second probe collide with the first.
		rc.port = p.reserveRTTPort(rc.host)
	}
	c := &rttCapture{cfg: rc, serial: req.DeviceID, cmd: p.cmd, clock: req.Clock, sink: sink}
	if rc.launch {
		serial, port, auto := NormalizeSerial(req.DeviceID), rc.port, !explicitPort(cfg)
		p.mu.Lock()
		p.rttHeld[serial] = true
		p.mu.Unlock()
		c.release = func() {
			p.mu.Lock()
			delete(p.rttHeld, serial)
			if auto {
				delete(p.rttPorts, port)
			}
			p.mu.Unlock()
		}
	}
	if err := c.begin(ctx); err != nil {
		c.shutdown()
		return nil, nil, status.Error(codes.Unavailable, err.Error())
	}
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: rttStream, Kind: "log",
		Attributes: map[string]string{"source": LogSource, "channel": "0", "port": strconv.Itoa(rc.port)}}}}, nil
}

func explicitPort(cfg map[string]any) bool { _, ok := cfg["rtt_port"]; return ok }

// otherRTTHolders lists the probes, other than serial, whose long-running RTT
// Commander session is open.
func (p *Plugin) otherRTTHolders(serial string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for s := range p.rttHeld {
		if s != serial {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// reserveRTTPort returns the first telnet port from DefaultRTTPort up that no
// running capture of this plugin reserved and that is free on host.
func (p *Plugin) reserveRTTPort(host string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port := DefaultRTTPort; port < DefaultRTTPort+64; port++ {
		if p.rttPorts[port] {
			continue
		}
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			continue
		}
		if err := l.Close(); err != nil {
			slog.Warn("release RTT port availability check", "err", err)
			continue
		}
		p.rttPorts[port] = true
		return port
	}
	return DefaultRTTPort
}

var _ pluginsdk.Plugin = (*Plugin)(nil)
