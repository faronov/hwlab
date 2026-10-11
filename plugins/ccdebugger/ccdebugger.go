// Package ccdebugger is the hwlab plugin for the TI CC-Debugger with
// CC253x-class 8051 SoC targets.
//
// There are two backends behind one DebugProbe/MemoryAccess/FlashProgramming
// surface:
//
//   - native (primary, Linux): a pure-Go USB implementation of the debugger and
//     the CC2530 debug interface (package native): target state, reset-attach,
//     halt/resume/step, registers, XDATA/CODE/physical flash reads, verified RAM
//     writes and hardware breakpoints. It runs under an explicit access policy
//     with independent permissions, configured per device.
//   - cc-tool (optional fallback): the open-source cc-tool host programmer, used
//     for what the native backend does not implement: MAC and info-page reads,
//     whole-flash read, erase, program, reset-to-run, and SmartRF05 boards.
//
// There is no RTT, and the capability set says so.
package ccdebugger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/ccdebugger/native"
)

// Plugin implements pluginsdk.Plugin.
type Plugin struct {
	sysfs string
	tool  Tool
	open  Opener // nil: native backend unavailable

	mu    sync.Mutex
	cfg   map[string]map[string]any
	conf  map[string]devConfig
	sess  map[string]*native.Debugger
	locks map[string]*sync.Mutex
}

// New returns the plugin with the native backend (where supported) and the
// real cc-tool as fallback.
func New() *Plugin {
	var open Opener
	if native.Supported {
		open = native.Open
	}
	return NewWithNative(DefaultSysfs, &ExecTool{}, open)
}

// NewWith injects the USB tree root and the tool, without a native backend:
// the cc-tool-only plugin (tests).
func NewWith(sysfs string, t Tool) *Plugin { return NewWithNative(sysfs, t, nil) }

// NewWithNative injects the USB tree root, the cc-tool and the native opener.
func NewWithNative(sysfs string, t Tool, open Opener) *Plugin {
	return &Plugin{sysfs: sysfs, tool: t, open: open, cfg: map[string]map[string]any{}, conf: map[string]devConfig{},
		sess: map[string]*native.Debugger{}, locks: map[string]*sync.Mutex{}}
}

func caps(attrs map[string]string, ids ...string) []*pluginv1.Capability {
	out := make([]*pluginv1.Capability, len(ids))
	for i, id := range ids {
		out[i] = &pluginv1.Capability{Id: id, Version: 1, Attributes: attrs}
	}
	return out
}

func spec(d pluginv1.Danger, capName, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: capName, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
}

const (
	obj      = `{"type":"object"}`
	targetKV = `"target":{"type":"string","description":"cc-tool target name, e.g. CC2530; detected when omitted"}`
)

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	path, err := p.tool.Available()
	desc := "optional cc-tool host programmer (https://github.com/dashesy/cc-tool): MAC/info-page/whole-flash read, erase, program, reset-to-run and SmartRF05 boards"
	if err == nil {
		desc += " (found at " + path + ")"
	}
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	pcaps := []*pluginv1.Capability{{Id: pluginv1.CapDebugProbe, Version: 1}, {Id: pluginv1.CapFlashProgramming, Version: 1}}
	methods := []*pluginv1.MethodSpec{
		spec(safe, pluginv1.CapDebugProbe, "info", "identify the debugger (native: adapter state, target id, session policy; backend=cc-tool: chip id, revision, flash/RAM size)", `{"type":"object","properties":{`+targetKV+`}}`),
		spec(safe, pluginv1.CapDebugProbe, "read_mac", "read the primary and secondary IEEE (MAC) addresses (native: info page + flash, see read_ieee; or cc-tool)", obj),
		spec(confirm, pluginv1.CapDebugProbe, "reset", "reset the target chip and let it run (cc-tool)", obj),
		spec(safe, pluginv1.CapFlashProgramming, "read", "read flash into a new file; args: path, overwrite (default false). Native backend (allow_memory_access): also address, length for a range, atomic file write. backend=cc-tool: whole flash, fast (default true). Non-destructive", `{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"overwrite":{"type":"boolean"},"fast":{"type":"boolean"}}}`),
		spec(safe, pluginv1.CapFlashProgramming, "read_info_page", "read the target's 2 KB information page into a new file; args: path, overwrite", `{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"overwrite":{"type":"boolean"}}}`),
		spec(confirm, pluginv1.CapFlashProgramming, "erase", "erase the whole target flash, lock bits and secondary IEEE (destroys the firmware). Native: needs allow_chip_erase and confirm=\"erase-entire-chip\"; does not resume the target; not yet verified on real hardware", `{"type":"object","properties":{"confirm":{"type":"string"}}}`),
		spec(confirm, pluginv1.CapFlashProgramming, "program", "program a .hex/.bin file. Native (allow_flash_program, not yet verified on real hardware): validates against the identified chip, erases only needed pages, programs and verifies; args: path, base (.bin), mode (preserve-device-config default|preserve-mac|full-chip-image), chip_erase (full-chip-image only), verify (readback default|crc|both|none), confirm=\"program-flash\", confirm_config=\"write-device-config\" if the lock page is touched. backend=cc-tool: args path, verify (crc|read), preserve_mac, reset, fast", `{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"base":{},"mode":{"type":"string","enum":["preserve-device-config","preserve-mac","full-chip-image"]},"chip_erase":{"type":"boolean"},"verify":{"type":"string","enum":["crc","read","readback","both","none"]},"confirm":{"type":"string"},"confirm_config":{"type":"string"},"preserve_mac":{"type":"boolean"},"reset":{"type":"boolean"},"fast":{"type":"boolean"}}}`),
	}
	var dangerous []string
	if p.open != nil {
		pcaps = append(pcaps, &pluginv1.Capability{Id: pluginv1.CapMemoryAccess, Version: 1})
		dangerous = dangerousKeys
		const sess = " Needs an open session with a debug-session access policy and the matching allow_* permission."
		memArgs := `"space":{"type":"string","enum":["xdata","code","flash"]},"address":{},"length":{"type":"number"},"path":{"type":"string"},"overwrite":{"type":"boolean"}`
		methods = append(methods,
			spec(confirm, pluginv1.CapDebugProbe, "attach", "reset the target into debug halt and establish the debug session (access=reset-debug-session, allow_target_reset). Interrupts the running firmware.", obj),
			spec(safe, pluginv1.CapDebugProbe, "status", "read the target's debug status and configuration."+sess, obj),
			spec(confirm, pluginv1.CapDebugProbe, "reset_halt", "reset the target into debug halt (allow_target_reset)", obj),
			spec(confirm, pluginv1.CapDebugProbe, "halt", "halt the CPU (allow_cpu_control)", obj),
			spec(confirm, pluginv1.CapDebugProbe, "resume", "resume the halted CPU (allow_cpu_control)", obj),
			spec(confirm, pluginv1.CapDebugProbe, "step", "execute one instruction (allow_cpu_control)", obj),
			spec(confirm, pluginv1.CapDebugProbe, "enable_dma", "clear DMA_PAUSE once, right after this session's reset and before resuming (allow_dma_enable)", obj),
			spec(safe, pluginv1.CapDebugProbe, "read_registers", "snapshot PC, bank, A, B, SP, PSW, DPTRs and R0-R7 of the halted CPU, twice, and verify they agree (allow_memory_access)", obj),
			spec(safe, pluginv1.CapDebugProbe, "read_sfr", "read one reviewed core SFR of the halted CPU (allow_memory_access); args: address", `{"type":"object","required":["address"],"properties":{"address":{}}}`),
			spec(confirm, pluginv1.CapDebugProbe, "set_breakpoint", "configure one of 4 hardware breakpoints at an unbanked CODE address (allow_breakpoints); args: slot 0-3, address, enabled", `{"type":"object","required":["slot","address"],"properties":{"slot":{"type":"number"},"address":{},"enabled":{"type":"boolean"}}}`),
			spec(safe, pluginv1.CapDebugProbe, "wait_halted", "poll until the CPU halts, e.g. at a breakpoint; args: timeout_ms (default 5000)", `{"type":"object","properties":{"timeout_ms":{"type":"number"}}}`),
			spec(safe, pluginv1.CapMemoryAccess, "read", "read target memory of the halted CPU (allow_memory_access); args: space (xdata <0x2000, code <0x8000, flash = physical CODE <0x3E800), address, length (max 1024 inline, or a path for a file); the CPU's registers are restored and verified", `{"type":"object","required":["address","length"],"properties":{`+memArgs+`}}`),
			spec(safe, pluginv1.CapDebugProbe, "chip_info", "identify the target from its hardware chip-information registers: chip, revision, flash/RAM size, page size, bank count, USB, lock state. A configured target name is never trusted. Needs an attached halted target (allow_memory_access)", obj),
			spec(safe, pluginv1.CapDebugProbe, "read_lock", "read and decode the 16-byte lock-bit structure at the end of flash (allow_memory_access)", obj),
			spec(safe, pluginv1.CapDebugProbe, "read_ieee", "read the primary (info page) and secondary (flash) IEEE addresses with their sources (allow_memory_access)", obj),
			spec(safe, pluginv1.CapFlashProgramming, "verify", "compare a .hex/.bin image with the physical flash without writing (allow_memory_access); args: path, base (.bin only), mode (readback default|crc|both)", `{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"base":{},"mode":{"type":"string","enum":["readback","crc","both"]}}}`),
			spec(confirm, pluginv1.CapFlashProgramming, "erase_pages", "native: erase flash pages and blank-check them (allow_flash_erase). The lock-bit page and lock-bit-protected pages are refused. args: pages[] or first_page+count, confirm=\"erase-flash-pages\". Not yet verified on real hardware", `{"type":"object","required":["confirm"],"properties":{"pages":{"type":"array","items":{"type":"number"}},"first_page":{},"count":{},"confirm":{"type":"string"}}}`),
			spec(confirm, pluginv1.CapFlashProgramming, "write_config", "native, VERY DANGEROUS: rewrite the secondary IEEE address and/or lock bits (allow_config_write). Saves the lock-bit page to backup_path first, then erases and programs it and verifies. args: secondary_ieee (MSB first), lock (16 hex bytes), backup_path, confirm=\"write-device-config\", confirm_lock=\"lock-debug-interface\" when the result debug-locks the target. Not yet verified on real hardware", `{"type":"object","required":["backup_path","confirm"],"properties":{"secondary_ieee":{"type":"string"},"lock":{"type":"string"},"backup_path":{"type":"string"},"verify":{"type":"string","enum":["readback","crc","both","none"]},"confirm":{"type":"string"},"confirm_lock":{"type":"string"}}}`),
			spec(confirm, pluginv1.CapMemoryAccess, "write", "write RAM (XDATA below 0x1E00) with per-byte readback (allow_memory_access + allow_memory_write); flash and peripherals are never written; args: address, data (hex, max 1024 bytes)", `{"type":"object","required":["address","data"],"properties":{"space":{"type":"string","enum":["xdata"]},"address":{},"data":{"type":"string"}}}`),
		)
	}
	return &pluginv1.PluginInfo{
		Id: "ccdebugger", Name: "TI CC-Debugger", Vendor: "hwlab", Version: "0.2.0",
		Capabilities: pcaps, DangerousConfigKeys: dangerous,
		RuntimeRequirements: []*pluginv1.RuntimeRequirement{
			{Name: "cc-tool", Kind: "binary", Optional: p.open != nil, Description: desc, Satisfied: err == nil},
		},
		Methods: methods,
	}
}

func (p *Plugin) tooling() (path string, ok bool) {
	path, err := p.tool.Available()
	return path, err == nil
}

// ListDevices implements pluginsdk.Plugin.
func (p *Plugin) ListDevices(context.Context) ([]*pluginv1.Device, error) {
	_, tool := p.tooling()
	all := scanSysfs(p.sysfs)
	var out []*pluginv1.Device
	for _, d := range all {
		nat := p.open != nil && d.PID == "16a2"
		var c []*pluginv1.Capability
		switch {
		case nat:
			c = caps(map[string]string{"backend": "native", "memory_access": "xdata,code,flash(read)", "flash_programming": "native", "flash_programming_status": "implemented, simulator-tested; erase/program not yet verified on real hardware", "cc_tool_fallback": fmt.Sprint(tool), "rtt": "no"},
				pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming)
		case tool:
			c = caps(map[string]string{"backend": "cc-tool", "memory_access": "flash-only", "rtt": "no"}, pluginv1.CapDebugProbe, pluginv1.CapFlashProgramming)
		default:
			c = caps(map[string]string{"operations": "discovery-only", "missing": "cc-tool"}, pluginv1.CapDebugProbe)
		}
		out = append(out, &pluginv1.Device{
			Id: deviceID(d, all), Name: d.Product, Serial: d.Serial, Transport: "usb", Capabilities: c,
			Properties: map[string]string{"vid": TIVID, "pid": d.PID, "usb_address": d.Address, "cc_tool": fmt.Sprint(tool), "native": fmt.Sprint(nat)},
		})
	}
	return out, nil
}

// OpenDevice implements pluginsdk.Plugin. The native USB session is opened
// lazily by the first native operation.
func (p *Plugin) OpenDevice(_ context.Context, id string, cfg map[string]any) error {
	dc, err := parseConfig(cfg)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg[id] = cfg
	p.conf[id] = dc
	return nil
}

// CloseDevice implements pluginsdk.Plugin.
func (p *Plugin) CloseDevice(_ context.Context, id string) error {
	p.dropSession(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cfg, id)
	delete(p.conf, id)
	return nil
}

// StartCapture implements pluginsdk.Plugin; a programmer produces no stream.
func (p *Plugin) StartCapture(context.Context, pluginsdk.CaptureRequest, *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	return nil, nil, status.Error(codes.Unimplemented, "ccdebugger has no capture capability")
}

func (p *Plugin) lock(id string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.locks[id] == nil {
		p.locks[id] = &sync.Mutex{}
	}
	return p.locks[id]
}

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

// run resolves the debugger's current USB address, serialises access and
// runs cc-tool. -d is always passed so several debuggers cannot be confused.
func (p *Plugin) run(ctx context.Context, id string, args map[string]any, extra ...string) (string, error) {
	if _, err := p.tool.Available(); err != nil {
		return "", status.Error(codes.FailedPrecondition, err.Error())
	}
	all := scanSysfs(p.sysfs)
	var addr string
	for _, d := range all {
		if deviceID(d, all) == id {
			addr = d.Address
		}
	}
	if addr == "" {
		return "", status.Errorf(codes.NotFound, "CC-Debugger %q is not connected", id)
	}
	argv := []string{"-d", addr}
	if t, _ := args["target"].(string); t != "" {
		argv = append(argv, "-n", t)
	}
	if fast, _ := args["fast"].(bool); fast {
		argv = append(argv, "-f")
	}
	argv = append(argv, extra...)
	l := p.lock(id)
	l.Lock()
	defer l.Unlock()
	// cc-tool needs USB interface 0, which an open native session holds. The
	// native session is dropped, and any debug session it had is gone.
	p.dropSession(id)
	out, err := p.tool.Run(ctx, argv)
	if err != nil {
		if f := failure(out); f != "" {
			return out, status.Errorf(codes.Unavailable, "cc-tool: %s", f)
		}
		return out, status.Error(codes.Unavailable, err.Error())
	}
	if f := failure(out); f != "" {
		return out, status.Errorf(codes.Unavailable, "cc-tool: %s", f)
	}
	return out, nil
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	id := req.GetDeviceId()
	args := p.merged(id, req.GetArgs().AsMap())
	var res map[string]any
	var err error
	key := req.GetCapability() + "." + req.GetMethod()
	if nat, _ := p.useNative(id); nat && p.open != nil && nativeFlashMethods[key] {
		l := p.lock(id)
		l.Lock()
		res, err = p.invokeNativeFlash(id, req.GetCapability(), req.GetMethod(), args)
		l.Unlock()
		return finish(res, err)
	}
	switch key {
	case pluginv1.CapDebugProbe + ".info":
		res, err = p.info(ctx, id, args)
	case pluginv1.CapDebugProbe + ".read_mac":
		res, err = p.readMAC(ctx, id, args)
	case pluginv1.CapDebugProbe + ".reset":
		res, err = p.simple(ctx, id, args, "--reset")
	case pluginv1.CapFlashProgramming + ".read":
		res, err = p.readFile(ctx, id, args, "-r")
	case pluginv1.CapFlashProgramming + ".read_info_page":
		res, err = p.readFile(ctx, id, args, "-i")
	case pluginv1.CapFlashProgramming + ".erase":
		res, err = p.simple(ctx, id, args, "-e")
	case pluginv1.CapFlashProgramming + ".program":
		res, err = p.program(ctx, id, args)
	default:
		res, err = p.invokeNative(id, req.GetCapability(), req.GetMethod(), args)
	}
	return finish(res, err)
}

func finish(res map[string]any, err error) (*pluginv1.InvokeResponse, error) {
	if err != nil {
		return nil, err
	}
	st, err := structpb.NewStruct(res)
	if err != nil {
		return nil, err
	}
	return &pluginv1.InvokeResponse{Result: st}, nil
}

// invokeNative serves the native-backend methods under the device lock.
func (p *Plugin) invokeNative(id, capName, method string, args map[string]any) (map[string]any, error) {
	if p.open == nil {
		return nil, status.Errorf(codes.Unimplemented, "unsupported method %s.%s", capName, method)
	}
	switch capName + "." + method {
	case pluginv1.CapDebugProbe + ".attach", pluginv1.CapDebugProbe + ".status", pluginv1.CapDebugProbe + ".reset_halt",
		pluginv1.CapDebugProbe + ".halt", pluginv1.CapDebugProbe + ".resume", pluginv1.CapDebugProbe + ".step",
		pluginv1.CapDebugProbe + ".enable_dma", pluginv1.CapDebugProbe + ".read_registers", pluginv1.CapDebugProbe + ".read_sfr",
		pluginv1.CapDebugProbe + ".set_breakpoint", pluginv1.CapDebugProbe + ".wait_halted",
		pluginv1.CapMemoryAccess + ".read", pluginv1.CapMemoryAccess + ".write":
	default:
		return nil, status.Errorf(codes.Unimplemented, "unsupported method %s.%s", capName, method)
	}
	l := p.lock(id)
	l.Lock()
	defer l.Unlock()
	switch capName + "." + method {
	case pluginv1.CapDebugProbe + ".attach":
		return p.nativeAttach(id)
	case pluginv1.CapDebugProbe + ".status":
		return p.nativeStatus(id)
	case pluginv1.CapDebugProbe + ".read_registers":
		return p.nativeRegisters(id)
	case pluginv1.CapDebugProbe + ".read_sfr":
		return p.nativeSFR(id, args)
	case pluginv1.CapDebugProbe + ".set_breakpoint":
		return p.nativeBreakpoint(id, args)
	case pluginv1.CapDebugProbe + ".wait_halted":
		return p.nativeWait(id, args)
	case pluginv1.CapMemoryAccess + ".read":
		return p.nativeMemRead(id, args)
	case pluginv1.CapMemoryAccess + ".write":
		return p.nativeMemWrite(id, args)
	}
	return p.nativeControl(id, method)
}

func (p *Plugin) info(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	if nat, err := p.useNative(id); err != nil {
		return nil, err
	} else if nat {
		l := p.lock(id)
		l.Lock()
		defer l.Unlock()
		return p.nativeInfo(id)
	}
	out, err := p.run(ctx, id, args, "-t")
	if err != nil {
		return nil, err
	}
	in := parseInfo(out)
	if in.Programmer == "" {
		return nil, status.Errorf(codes.Unavailable, "cc-tool gave no programmer information: %s", strings.TrimSpace(out))
	}
	toAny := func(m map[string]string) map[string]any {
		r := map[string]any{}
		for k, v := range m {
			r[k] = v
		}
		return r
	}
	return map[string]any{
		"programmer": in.Programmer, "target": in.Target,
		"debugger": toAny(in.Device), "chip": toAny(in.TargetInfo),
		"capabilities": []any{"info", "read_mac", "reset", "flash_read", "flash_erase", "flash_program"},
		"unsupported":  []any{"memory_read", "memory_write", "registers", "halt", "resume", "rtt"},
	}, nil
}

func (p *Plugin) readMAC(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	out, err := p.run(ctx, id, args, "-a")
	if err != nil {
		return nil, err
	}
	pri, sec, ok := parseMAC(out)
	if !ok {
		return nil, status.Errorf(codes.Unavailable, "cannot parse MAC addresses from: %s", strings.TrimSpace(out))
	}
	return map[string]any{"primary": pri, "secondary": sec}, nil
}

func (p *Plugin) simple(ctx context.Context, id string, args map[string]any, flag string) (map[string]any, error) {
	out, err := p.run(ctx, id, args, flag)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "output": tailLines(out, 8)}, nil
}

var progressRe = regexp.MustCompile(`Progress:\s*\d+%\s*`)

func tailLines(s string, n int) []any {
	var lines []string
	for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if t := strings.TrimSpace(progressRe.ReplaceAllString(l, "")); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]any, len(lines))
	for i, l := range lines {
		out[i] = l
	}
	return out
}

func pathArg(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", status.Error(codes.InvalidArgument, "path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", status.Error(codes.InvalidArgument, err.Error())
	}
	return abs, nil
}

func withFast(args map[string]any, v bool) map[string]any {
	m := map[string]any{}
	for k, x := range args {
		m[k] = x
	}
	m["fast"] = v
	return m
}

func (p *Plugin) readFile(ctx context.Context, id string, args map[string]any, flag string) (map[string]any, error) {
	path, err := pathArg(args)
	if err != nil {
		return nil, err
	}
	if ow, _ := args["overwrite"].(bool); !ow {
		if _, err := os.Stat(path); err == nil {
			return nil, status.Errorf(codes.AlreadyExists, "%s exists; pass overwrite=true to replace it", path)
		}
	}
	if _, ok := args["fast"]; !ok {
		args = withFast(args, true)
	}
	if _, err := p.run(ctx, id, args, flag, path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cc-tool reported success but %s cannot be read: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return map[string]any{"path": path, "bytes": float64(len(b)), "sha256": hex.EncodeToString(sum[:])}, nil
}

func (p *Plugin) program(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	path, err := pathArg(args)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return nil, status.Errorf(codes.InvalidArgument, "firmware file %s not found", path)
	}
	method := "c"
	if v, ok := args["verify"].(string); ok {
		switch v {
		case "crc":
		case "read":
			method = "r"
		default:
			return nil, status.Errorf(codes.InvalidArgument, "verify %q: want crc or read", v)
		}
	}
	argv := []string{"-e", "-w", path, "-v", method}
	if pm, ok := args["preserve_mac"].(bool); !ok || pm {
		argv = append(argv, "-p")
	}
	if rs, ok := args["reset"].(bool); !ok || rs {
		argv = append(argv, "--reset")
	}
	out, err := p.run(ctx, id, args, argv...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "path": path, "verify": method, "output": tailLines(out, 8)}, nil
}
