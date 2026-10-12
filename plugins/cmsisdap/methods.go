package cmsisdapplugin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx"
	"github.com/faronov/hwlab/internal/ti/cc23xx/service"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

const (
	// maxReadBytes bounds one memory read.
	maxReadBytes = 64 << 10
	// inlineBytes is the most a read returns as hex; more returns a preview
	// and the sha256 only.
	inlineBytes = 4096
	// maxWriteBytes bounds one memory write.
	maxWriteBytes = 4096
	previewBytes  = 64
	maxStepCount  = 1000
)

func spec(c string, d pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: c, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
}

const optProps = `"clock_hz":{"type":"number","description":"SWD clock, default 1000000"},"ap":{"type":"number","description":"MEM-AP index, default 0"},"timeout_ms":{"type":"number","description":"per-command probe timeout"}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	D, M := pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess
	obj := func(props string) string { return `{"type":"object","properties":{` + props + `}}` }
	req := func(required, props string) string {
		return `{"type":"object","required":[` + required + `],"properties":{` + props + `}}`
	}
	clr := obj(optProps + `,"address":{},"all":{"type":"boolean"}`)
	info := &pluginv1.PluginInfo{
		Id: "cmsisdap", Name: "CMSIS-DAP debug probe (SWD, Cortex-M)", Vendor: "hwlab", Version: "0.2.0",
		Capabilities:        caps(map[string]string{"validated": "probe-only"}),
		DangerousConfigKeys: service.DangerousKeys,
		Methods: []*pluginv1.MethodSpec{
			spec(D, safe, "info", "probe identification, DAP_Info strings, decoded capability bitmap, packet size/count; does not touch the target", `{"type":"object"}`),
			spec(D, safe, "pins", "read the DAP_SWJ_Pins input levels without driving anything. Levels show what the probe sees, not that a target is wired", `{"type":"object"}`),
			spec(D, confirm, "clock_probe", "ask the probe to accept each SWD clock (rates_hz, default 100k/400k/1M/2M/4M) and time a 256-bit SWJ sequence; reports acceptance and measured timing, never a precision claim. Needs no target", obj(`"rates_hz":{"type":"array","items":{"type":"number"}}`)),
			spec(D, confirm, "jtag_scan", "DAP_Connect(JTAG), reset the TAPs and enumerate the chain with the shared JTAG driver; reports no-device when TDO floats high. Drives TCK/TMS/TDI", obj(`"clock_hz":{"type":"number"},"max_devices":{"type":"number"}`)),
			spec(D, safe, "connect", "attach over SWD and identify the debug port and Cortex-M core; does not halt, reset or write the target", obj(optProps)),
			spec(D, safe, "ap_scan", "list the access ports (IDR/BASE/CFG) and the first level of the CoreSight ROM table behind the selected MEM-AP; read-only", obj(optProps)),
			spec(D, confirm, "disconnect", "release the SWD attachment (breakpoints and watchpoints installed by this device are restored first); the device stays open and target power is unchanged, so another backend can use the same probe", obj(optProps)),
			spec(D, safe, "status", "run state of the core (halted, sleeping, lockup) and the PC when halted", obj(optProps)),
			spec(D, confirm, "halt", "halt the core", obj(optProps)),
			spec(D, confirm, "resume", "let the core run; a breakpoint at the current PC is stepped over first", obj(optProps)),
			spec(D, confirm, "step", "single-step a halted core count times (default 1, max 1000), each step confirmed by halt; returns the PCs",
				obj(optProps+`,"count":{"type":"number"},"mask_interrupts":{"type":"boolean"}`)),
			spec(D, confirm, "wait_halt", "wait (wait_ms, default 2000) for the core to halt and report the cause; says whether one of this session's breakpoints or a watchpoint was hit", obj(optProps+`,"wait_ms":{"type":"number"}`)),
			spec(D, confirm, "reset", "reset the target. kind: sysreset (AIRCR.SYSRESETREQ, default), core (VECTRESET), dap_reset (the probe's DAP_ResetTarget) or hardware (nRESET pin). Every kind requires observed evidence of a reset; hardware and dap_reset fail honestly when nRESET is not wired. halt=true stops at the reset vector",
				obj(optProps+`,"kind":{"enum":["sysreset","core","dap_reset","hardware"]},"halt":{"type":"boolean"}`)),
			spec(D, confirm, "read_registers", "read r0-r12, sp, lr, pc, xpsr, msp, psp and the packed special register (control/faultmask/basepri/primask); needs a halted core, or halt=true to halt it first",
				obj(optProps+`,"halt":{"type":"boolean"}`)),
			spec(D, confirm, "write_register", "write one core register of a halted core; read back and compared", req(`"name","value"`, optProps+`,"name":{"type":"string"},"value":{}`)),
			spec(D, confirm, "set_breakpoint", "install an FPB hardware breakpoint (needs an open device); the slot count is what the core reports. run_wait_ms>0 resumes and waits for the hit", obj(optProps+`,"address":{},"run_wait_ms":{"type":"number"}`)),
			spec(D, confirm, "clear_breakpoint", "remove a breakpoint installed by this device (address) or all=true; the original FPB state is restored", clr),
			spec(D, safe, "list_breakpoints", "enumerate the FPB comparators", obj(optProps)),
			spec(D, confirm, "set_watchpoint", "install a DWT data watchpoint (kind read|write|access, mask_bits) on an address (needs an open device)", obj(optProps+`,"address":{},"kind":{"enum":["read","write","access"]},"mask_bits":{"type":"number"}`)),
			spec(D, confirm, "clear_watchpoint", "remove a watchpoint installed by this device (address) or all=true; DWT and DEMCR are restored", clr),
			spec(D, safe, "list_watchpoints", "enumerate the DWT comparators", obj(optProps)),
			spec(M, safe, "read", "read target memory; args: address, length (bytes, max 64 KiB; more than 4 KiB returns a preview and sha256 only). Peripheral registers can have read side effects",
				req(`"address","length"`, optProps+`,"address":{},"length":{"type":"number"}`)),
			spec(M, confirm, "write", "write target memory; args: address, data (hex string, max 4096 bytes); verifies by reading back unless verify=false. Flash is not writable this way",
				req(`"address","data"`, optProps+`,"address":{},"data":{"type":"string"},"verify":{"type":"boolean"}`)),
		},
	}
	info.Methods = append(info.Methods, service.Specs(optProps)...)
	return info
}

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func optNumber(a map[string]any, key string) (float64, bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false, fmt.Errorf("%s: want a number, got %v", key, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, fmt.Errorf("%s must be finite", key)
	}
	return f, true, nil
}

func optInt(a map[string]any, key string, def, lo, hi int) (int, error) {
	f, ok, err := optNumber(a, key)
	if err != nil {
		return 0, argErr(err)
	}
	if !ok {
		return def, nil
	}
	if f != math.Trunc(f) || f < float64(lo) || f > float64(hi) {
		return 0, argErr(fmt.Errorf("%s %v: want an integer %d..%d", key, f, lo, hi))
	}
	return int(f), nil
}

// u32 reads a 32-bit unsigned value given as a number or a "0x.." string.
func u32(a map[string]any, key string) (uint32, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, fmt.Errorf("%s is required", key)
	}
	switch x := v.(type) {
	case float64:
		if x < 0 || x > math.MaxUint32 || x != math.Trunc(x) {
			return 0, fmt.Errorf("%s %v: want an integer 0..4294967295", key, x)
		}
		return uint32(x), nil
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(x), 0, 32)
		if err != nil {
			return 0, fmt.Errorf("%s %q: want a 32-bit number such as 0x20000000", key, x)
		}
		return uint32(n), nil
	}
	return 0, fmt.Errorf("%s: want a number or a 0x.. string, got %v", key, v)
}

func optBool(a map[string]any, key string) (val, set bool, err error) {
	v, ok := a[key]
	if !ok || v == nil {
		return false, false, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, false, fmt.Errorf("%s: want true or false, got %v", key, v)
	}
	return b, true, nil
}

func hex32(v uint32) string { return fmt.Sprintf("0x%08x", v) }

// needsOpen methods keep state that only survives on an open device.
var needsOpen = map[string]bool{"DebugProbe.set_breakpoint": true, "DebugProbe.set_watchpoint": true}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	if c := req.GetCapability(); c != pluginv1.CapDebugProbe && c != pluginv1.CapMemoryAccess && c != pluginv1.CapFlashProgramming {
		return nil, status.Errorf(codes.Unimplemented, "capability %q", c)
	}
	method := req.GetCapability() + "." + req.GetMethod()
	if !p.knows(method) {
		return nil, status.Errorf(codes.Unimplemented, "method %s", method)
	}
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	d, o, release, err := p.lease(req.GetDeviceId(), args)
	if err != nil {
		return nil, err
	}
	defer release()
	if needsOpen[method] && d.transient {
		return nil, status.Errorf(codes.FailedPrecondition, "%s keeps hardware state and needs an open device (OpenDevice first): a one-shot call would leave it behind", method)
	}
	res, err := p.run(ctx, d, o, method, args)
	if err != nil {
		err = classify(err)
		if IsDeviceGone(err) {
			p.drop(d)
		}
		return nil, err
	}
	s, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: s}, nil
}

func (p *Plugin) knows(method string) bool {
	for _, m := range p.Info().Methods {
		if m.Capability+"."+m.Name == method {
			return true
		}
	}
	return false
}

func (p *Plugin) run(ctx context.Context, d *device, o options, method string, args map[string]any) (res map[string]any, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	timeout := 60 * time.Second
	if strings.HasPrefix(method, pluginv1.CapFlashProgramming+".") {
		timeout = service.FlashTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		if err != nil && d.core != nil && pluginsdk.DebugCleanupNeeded(err) {
			err = cortexm.WithCleanupError(err, d.trig.restoreAll(ctx, d.core))
		}
	}()
	switch method {
	case "DebugProbe.info":
		return d.info()
	case "DebugProbe.pins":
		return d.pins()
	case "DebugProbe.clock_probe":
		return d.clockProbe(args)
	case "DebugProbe.jtag_scan":
		return d.jtagScan(ctx, o, args)
	case "DebugProbe.disconnect":
		if err := d.detach(); err != nil {
			return nil, err
		}
		return map[string]any{"disconnected": true}, nil
	}
	err = d.attach(o)
	if err == nil && d.cc != nil {
		if res, handled, cerr := d.runCC23xx(ctx, method, args); handled {
			return res, cerr
		}
	}
	if method == "DebugProbe.reset" && args["kind"] == "hardware" {
		if err == nil {
			_, err = d.core.Status(ctx)
		}
		if err != nil {
			return d.recoverByNRESET(o, err)
		}
	}
	if err != nil {
		return nil, err
	}
	if capab, name, _ := strings.Cut(method, "."); service.Handles(capab, name) {
		return nil, service.Err(&cc23xx.Error{Code: cc23xx.CodeNotSupportedByPart, Msg: method + " needs a TI CC23xx target; the attached target is not one"})
	}
	if d.cc != nil {
		if err := service.EnsureAHB(ctx, d.cc, d.o.cc.KeepSACI); err != nil {
			return nil, err
		}
	}
	res, err = d.coreMethod(ctx, method, args)
	if err != nil && d.cc != nil {
		err = service.Annotate(ctx, d.cc, err)
	}
	return res, err
}

// runCC23xx routes the CC23xx-only methods and the CC23xx flavour of reset.
func (d *device) runCC23xx(ctx context.Context, method string, args map[string]any) (map[string]any, bool, error) {
	capab, name, _ := strings.Cut(method, ".")
	if method == "DebugProbe.reset" {
		kind, _ := args["kind"].(string)
		if kind == "" {
			kind = "sysreset"
		}
		if kind == "dap_reset" {
			return nil, true, service.Err(&cc23xx.Error{Code: cc23xx.CodeNotSupportedByPart, Msg: "the probe does not implement DAP_ResetTarget; use hardware (nRESET) or sysreset"})
		}
		halt, _, err := optBool(args, "halt")
		if err != nil {
			return nil, true, argErr(err)
		}
		res, err := service.Reset(ctx, d.cc, kind, halt)
		d.trig = triggers{}
		return res, true, err
	}
	if !service.Handles(capab, name) {
		return nil, false, nil
	}
	if service.NeedsAHB(capab, name) {
		if err := service.EnsureAHB(ctx, d.cc, d.o.cc.KeepSACI); err != nil {
			return nil, true, err
		}
	}
	env := service.Env{S: d.cc, Perm: d.o.cc.Perms, Serial: d.entry.ID}
	res, err := env.Invoke(ctx, capab, name, args)
	d.trig = triggers{}
	return res, true, err
}

func (d *device) coreMethod(ctx context.Context, method string, args map[string]any) (map[string]any, error) {
	switch method {
	case "DebugProbe.connect":
		return d.connectInfo(ctx)
	case "DebugProbe.ap_scan":
		return d.apScan()
	case "DebugProbe.status":
		return d.status(ctx)
	case "DebugProbe.halt":
		return d.halt(ctx)
	case "DebugProbe.resume":
		return d.resume(ctx)
	case "DebugProbe.step":
		return d.step(ctx, args)
	case "DebugProbe.wait_halt":
		return d.waitHalt(ctx, args)
	case "DebugProbe.reset":
		return d.reset(ctx, args)
	case "DebugProbe.read_registers":
		return d.readRegisters(ctx, args)
	case "DebugProbe.write_register":
		return d.writeRegister(ctx, args)
	case "DebugProbe.set_breakpoint":
		return d.setBreakpoint(ctx, args)
	case "DebugProbe.clear_breakpoint":
		return d.clearBreakpoint(ctx, args)
	case "DebugProbe.list_breakpoints":
		return d.listBreakpoints(ctx)
	case "DebugProbe.set_watchpoint":
		return d.setWatchpoint(ctx, args)
	case "DebugProbe.clear_watchpoint":
		return d.clearWatchpoint(ctx, args)
	case "DebugProbe.list_watchpoints":
		return d.listWatchpoints(ctx)
	case "MemoryAccess.read":
		return d.readMemory(args)
	case "MemoryAccess.write":
		return d.writeMemory(args)
	}
	return nil, status.Errorf(codes.Unimplemented, "method %s", method)
}
