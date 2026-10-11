package stlinkplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

const opTimeout = 30 * time.Second

func spec(c string, d pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: c, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
}

const devProps = `"transport":{"type":"string","enum":["swd","jtag"],"description":"debug transport, default swd (jtag is implemented but not verified on real hardware)"},` +
	`"clock_khz":{"type":"number","description":"requested SWD clock in kHz, default 1800; the probe quantises it and the real value is reported"},` +
	`"timeout_ms":{"type":"number"},` +
	`"reconnect_timeout_ms":{"type":"number","description":"how long a lost probe is waited for (rediscovery by USB serial), default 8000"},` +
	`"allow_memory_write":{"type":"boolean","description":"DANGEROUS config: permit SRAM and core-register writes"},` +
	`"allow_reset":{"type":"boolean","description":"DANGEROUS config: permit core/system/pin resets"},` +
	`"allow_flash":{"type":"boolean","description":"DANGEROUS config: permit flash programming/restore (recognised STM32 only)"},` +
	`"allow_erase":{"type":"boolean","description":"DANGEROUS config: permit flash erase"},` +
	`"allow_chip_erase":{"type":"boolean","description":"DANGEROUS config: permit whole-flash erase (stronger than allow_erase)"}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	I, D, M, F := pluginv1.CapTargetIdentify, pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming
	obj := func(props string) string { return `{"type":"object","properties":{` + props + `}}` }
	regProps := devProps + `,"keep_halted":{"type":"boolean","description":"leave the core halted if this call had to halt it; default false (it is resumed)"},"crosscheck":{"type":"boolean","description":"also read pc through DCRSR and compare it with the native register read"}`
	readArgs := `{"type":"object","required":["address","length"],"properties":{` + devProps +
		`,"address":{"description":"number or string like \"0x20000000\""},"length":{"type":"number","description":"bytes, 1..4096 inline"},` +
		`"allow_peripheral":{"type":"boolean","description":"permit reads of peripherals (0x40000000-0x5FFFFFFF) and the private peripheral bus, where a read can have side effects; default false"},` +
		`"path":{"type":"string","description":"write the bytes to this new file (up to 16 MiB) instead of returning hex"},"overwrite":{"type":"boolean"}}}`
	writeArgs := `{"type":"object","required":["address","data"],"properties":{` + devProps + `,"address":{},"data":{"type":"string","description":"hex, 1..4096 bytes"}}}`
	writeRegArgs := `{"type":"object","required":["name","value"],"properties":{` + devProps + `,"name":{"type":"string"},"value":{}}}`
	stepArgs := obj(devProps + `,"count":{"type":"number","description":"1..1000"},"mask_interrupts":{"type":"boolean","description":"set C_MASKINTS while stepping; default false"}`)
	bpArgs := `{"type":"object","required":["address"],"properties":{` + devProps + `,"address":{},"run_wait_ms":{"type":"number","description":"resume and wait this long for the breakpoint to hit (one-shot flow)"}}}`
	clrArgs := obj(devProps + `,"address":{},"all":{"type":"boolean"}`)
	wpArgs := `{"type":"object","required":["address"],"properties":{` + devProps + `,"address":{},"kind":{"type":"string","enum":["read","write","access"]},"mask_bits":{"type":"number","description":"ignored low address bits (0..15), default 2 = 4 bytes"}}}`
	resetArgs := obj(devProps + `,"kind":{"type":"string","enum":["system","core","pin"]},"halt":{"type":"boolean"},"confirm":{"type":"string","description":"pin reset needs \"reset-pin\""}`)
	flashReadArgs := `{"type":"object","required":["address","length"],"properties":{` + devProps + `,"address":{},"length":{"type":"number"},"path":{"type":"string"},"overwrite":{"type":"boolean"}}}`
	flashImgArgs := `{"type":"object","required":["path","address"],"properties":{` + devProps + `,"path":{"type":"string"},"address":{}}}`
	eraseArgs := `{"type":"object","required":["mode","confirm","backup"],"properties":{` + devProps + `,"mode":{"type":"string","enum":["region","chip"]},"address":{},"length":{"type":"number"},"confirm":{"type":"string"},"backup":{"type":"string"}}}`
	progArgs := `{"type":"object","required":["path","address","confirm","backup"],"properties":{` + devProps + `,"path":{"type":"string"},"address":{},"confirm":{"type":"string"},"backup":{"type":"string"},"reset":{"type":"string","enum":["run","halt","none"]}}}`
	restoreArgs := `{"type":"object","required":["path","confirm"],"properties":{` + devProps + `,"path":{"type":"string"},"confirm":{"type":"string"},"reset":{"type":"string","enum":["run","halt","none"]}}}`
	return &pluginv1.PluginInfo{
		Id: "stlink", Name: "ST-Link (native USB, SWD/JTAG)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities:        deviceCaps(),
		DangerousConfigKeys: append([]string(nil), permKeys...),
		Methods: []*pluginv1.MethodSpec{
			spec(D, safe, "info", "probe, USB interface and (when open) transport facts; claims nothing when the device is not open", obj(devProps)),
			spec(I, safe, "identify", "probe identity (serial, firmware), ADIv5 debug port, Arm Cortex-M core (CPUID) and a vendor hint read from vendor registers; the vendor is never inferred from the core", obj(devProps)),
			spec(D, safe, "chip_info", "same as TargetIdentify.identify, plus the breakpoint (FPB) and watchpoint (DWT) comparator counts the core reports and the run state", obj(devProps)),
			spec(D, confirm, "halt", "halt the core and confirm it in DHCSR; it stays halted until resume", obj(devProps)),
			spec(D, confirm, "resume", "resume the core; if a breakpoint sits at the current PC it is stepped over first", obj(devProps)),
			spec(D, safe, "read_registers", "read r0-r12, sp, lr, pc, xpsr, msp, psp and the special registers; halts a running core and resumes it again unless keep_halted", obj(regProps)),
			spec(D, confirm, "write_register", "write one core register of a halted core (needs allow_memory_write); the old value is returned and the write is read back", writeRegArgs),
			spec(M, safe, "read", "read target memory through the MEM-AP without halting the core; max 4096 bytes inline or up to 16 MiB to a new file with path (SHA-256 returned). Peripherals and the private peripheral bus need allow_peripheral", readArgs),
			spec(M, confirm, "write", "write SRAM (0x20000000-0x21FFFFFF; needs allow_memory_write): read back and compared, previous bytes returned, restored if the read-back differs. Flash, peripherals and the private peripheral bus are refused", writeArgs),
			spec(D, confirm, "step", "single-step the halted core count times (DHCSR.C_STEP; default 1, max 1000); each step is confirmed by halt, and C_STEP is cleared afterwards. PC deltas are reported, not assumed", stepArgs),
			spec(D, safe, "harts", "report the cores behind this debug port", obj(devProps)),
			spec(D, confirm, "wait_halt", "wait for the core to halt (wait_ms, default 2000) and report the cause; says whether an installed breakpoint/watchpoint was hit", obj(devProps+`,"wait_ms":{"type":"number"}`)),
			spec(D, confirm, "set_breakpoint", "install a hardware breakpoint (FPB comparator) at an address; the slot count is what the core reports. run_wait_ms>0 resumes and waits for the hit", bpArgs),
			spec(D, confirm, "clear_breakpoint", "remove a breakpoint installed by this session (address) or all=true; the original FPB state is restored", clrArgs),
			spec(D, safe, "list_breakpoints", "enumerate the FPB comparators", obj(devProps)),
			spec(D, confirm, "set_watchpoint", "install a DWT data watchpoint (read/write/access) on an address; the comparator count is what the core reports", wpArgs),
			spec(D, confirm, "clear_watchpoint", "remove a watchpoint installed by this session (address) or all=true; DWT and DEMCR are restored", clrArgs),
			spec(D, safe, "list_watchpoints", "enumerate the DWT comparators", obj(devProps)),
			spec(D, confirm, "reset", "reset: kind=system (AIRCR.SYSRESETREQ, default), core (VECTRESET) or pin (NRST; needs confirm=\"reset-pin\" and is only claimed when a reset is observed). Needs allow_reset; every reset must be observed (S_RESET_ST or debug-enable loss), otherwise it is an error. halt=true stops at the reset vector", resetArgs),
			spec(D, confirm, "set_speed", "change the SWD clock of the open probe (khz); proven by a read, the old clock is restored on failure", `{"type":"object","required":["khz"],"properties":{`+devProps+`,"khz":{"type":"number"}}}`),
			spec(D, safe, "reconnect", "drop the session and wait (bounded by reconnect_timeout_ms) for the probe with the same USB serial, reopen it and verify the DP IDCODE, CPUID and vendor evidence equal the earlier ones", obj(devProps)),
			spec(F, safe, "info", "flash identity and geometry; for a target that is not a recognised STM32 it reports UNSUPPORTED_BY_ATTACHED_HW with the evidence and touches nothing", obj(devProps)),
			spec(F, safe, "read", "read flash (memory mapped on STM32): max 4096 bytes inline, or any length up to 16 MiB to a new file with path (+overwrite); SHA-256 returned", flashReadArgs),
			spec(F, safe, "verify", "compare a file with flash at address without writing; any difference is an error", flashImgArgs),
			spec(F, confirm, "backup", "read the WHOLE flash twice, require identical SHA-256, store it (mode 0600) with a manifest in dir (default $HWLAB_HOME/artifacts/stlink-flash); never modifies flash. The artifact stays local", obj(devProps+`,"dir":{"type":"string"}`)),
			spec(F, confirm, "erase", "erase flash: mode=region (address,length on sector boundaries; confirm=\"erase-flash-region\") or chip (\"erase-entire-chip\", needs allow_chip_erase). Both need allow_flash+allow_erase and backup=<verified full backup .bin>. There is no implicit mode. Recognised programmable STM32 (F4) only", eraseArgs),
			spec(F, confirm, "program", "program an image at address, changing only the covering sectors (bytes outside the image are preserved), erasing only when a bit must be set, read back and compared; needs allow_flash, confirm=\"program-flash\", backup=<verified full backup>; reset=run|halt|none after success (needs allow_reset). Option bytes and RDP are never written", progArgs),
			spec(F, confirm, "restore", "restore a verified full backup (path): changes only differing sectors, then compares the whole-flash SHA-256 with the backup; needs allow_flash+allow_erase and confirm=\"restore-flash\"", restoreArgs),
		},
	}
}

// UnsupportedByHW prefixes the message of a call that cannot work because the
// attached core does not implement the resource (no watchpoint comparators,
// no VECTRESET). It is a fact about the target, not a gap in hwlab.
const UnsupportedByHW = "UNSUPPORTED_BY_ATTACHED_HW"

// IsUnsupportedByHW classifies an error returned by the plugin.
func IsUnsupportedByHW(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.FailedPrecondition && strings.HasPrefix(st.Message(), UnsupportedByHW)
}

func wrap(err error) error {
	if primary := cortexm.PrimaryError(err); primary != err {
		st := status.Convert(wrap(primary))
		return status.Error(st.Code(), st.Message()+strings.TrimPrefix(err.Error(), primary.Error()))
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, usb.ErrDisconnected):
		return goneErr("%v", transportErr(err))
	case errors.Is(err, cortexm.ErrNotHalted):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, cortexm.ErrAbsent):
		return status.Error(codes.FailedPrecondition, UnsupportedByHW+": "+err.Error())
	case errors.Is(err, cortexm.ErrTimeout):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.FailedPrecondition, transportErr(err).Error())
}

var methods = map[string]bool{}

func init() {
	for _, m := range (*Plugin)(nil).Info().Methods {
		methods[m.Capability+"."+m.Name] = true
	}
}

// opTimeoutFor gives long flash operations room (the probe does ~100 KB/s).
func opTimeoutFor(method string) time.Duration {
	if strings.HasPrefix(method, "FlashProgramming.") {
		return 20 * time.Minute
	}
	return opTimeout
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	method := req.GetCapability() + "." + req.GetMethod()
	if !methods[method] {
		return nil, status.Errorf(codes.Unimplemented, "method %s", method)
	}
	id := req.GetDeviceId()
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	if strings.HasSuffix(method, ".info") && method != "FlashProgramming.info" {
		if res, ok, err := p.staticInfo(id, args); err != nil {
			return nil, err
		} else if ok {
			return respond(res)
		}
	}
	switch method {
	case "MemoryAccess.read":
		if _, err := parseRead(args); err != nil {
			return nil, err
		}
	case "MemoryAccess.write":
		if _, err := parseWrite(args); err != nil {
			return nil, err
		}
	}
	if method == "DebugProbe.reconnect" {
		res, err := p.reconnect(ctx, id, args)
		if err != nil {
			return nil, wrap(err)
		}
		return respond(res)
	}
	s, release, err := p.lease(ctx, id, args)
	if err != nil {
		return nil, wrap(err)
	}
	defer release()
	res, err := p.run(ctx, s, method, args)
	if err != nil {
		if errors.Is(err, usb.ErrDisconnected) {
			p.drop(id, s)
		}
		return nil, wrap(err)
	}
	return respond(res)
}

func (p *Plugin) run(ctx context.Context, s *session, method string, args map[string]any) (res map[string]any, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		if err != nil && s.core != nil && pluginsdk.DebugCleanupNeeded(err) {
			err = cortexm.WithCleanupError(err, s.dropTriggers(ctx))
		}
	}()
	if s.closed {
		return nil, status.Error(codes.FailedPrecondition, "device is closed")
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeoutFor(method))
	defer cancel()
	res, err = s.dispatch(cctx, method, args)
	if err == nil && s.persist && s.identity == nil {
		// What this probe and target are, so that a later reconnect can prove
		// it is the same pair.
		if got, e := s.captureIdentity(cctx); e == nil {
			s.identity = &got
		}
	}
	return res, err
}

func (s *session) dispatch(ctx context.Context, method string, args map[string]any) (map[string]any, error) {
	switch method {
	case "DebugProbe.info":
		return s.facts(), nil
	case "TargetIdentify.identify", "DebugProbe.chip_info":
		return s.chipInfo(ctx)
	case "DebugProbe.halt":
		return s.halt(ctx)
	case "DebugProbe.resume":
		return s.resume(ctx)
	case "DebugProbe.read_registers":
		return s.registers(ctx, args)
	case "DebugProbe.write_register":
		return s.writeRegister(ctx, args)
	case "MemoryAccess.read":
		return s.readMem(ctx, args)
	case "MemoryAccess.write":
		return s.writeMem(ctx, args)
	case "DebugProbe.step":
		return s.step(ctx, args)
	case "DebugProbe.harts":
		return s.harts(ctx)
	case "DebugProbe.wait_halt":
		return s.waitHalt(ctx, args)
	case "DebugProbe.set_breakpoint":
		return s.setBreakpoint(ctx, args)
	case "DebugProbe.clear_breakpoint":
		return s.clearBreakpoint(ctx, args)
	case "DebugProbe.list_breakpoints":
		return s.listBreakpoints(ctx)
	case "DebugProbe.set_watchpoint":
		return s.setWatchpoint(ctx, args)
	case "DebugProbe.clear_watchpoint":
		return s.clearWatchpoint(ctx, args)
	case "DebugProbe.list_watchpoints":
		return s.listWatchpoints(ctx)
	case "DebugProbe.reset":
		return s.reset(ctx, args)
	case "DebugProbe.set_speed":
		return s.setSpeed(ctx, args)
	case "FlashProgramming.info":
		return s.flashInfo(ctx, args)
	case "FlashProgramming.read":
		return s.flashRead(ctx, args)
	case "FlashProgramming.verify":
		return s.flashVerify(ctx, args)
	case "FlashProgramming.backup":
		return s.flashBackup(ctx, args)
	case "FlashProgramming.erase":
		return s.flashErase(ctx, args)
	case "FlashProgramming.program":
		return s.flashProgram(ctx, args)
	case "FlashProgramming.restore":
		return s.flashRestore(ctx, args)
	}
	return nil, status.Errorf(codes.Unimplemented, "method %s", method)
}

func respond(res map[string]any) (*pluginv1.InvokeResponse, error) {
	st, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: st}, nil
}

func usbFacts(e Entry) map[string]any {
	d := e.Dev
	res := map[string]any{
		"vid": fmt.Sprintf("%04x", d.VID), "pid": fmt.Sprintf("%04x", d.PID),
		"manufacturer": d.Manufacturer, "product": d.Product, "serial": e.Serial,
		"configuration": float64(d.Configuration), "interface_count": float64(len(d.Interfaces)),
		"target_protocol": "Arm ADIv5 (SWD or JTAG) to a Cortex-M core",
		"cpu_debug":       []any{"halt", "resume", "step", "read_registers", "breakpoints", "watchpoints"},
		"memory_access":   "read (MEM-AP); write: SRAM only, needs allow_memory_write",
		"reset":           "system, core, pin (needs allow_reset)",
		"flash":           "STM32-specific direct controller driver; only a positively recognised STM32 (needs allow_flash/allow_erase, confirmation tokens, a verified backup)",
	}
	var ifs []any
	for _, i := range d.Interfaces {
		m := map[string]any{"number": float64(i.Number), "class": fmt.Sprintf("0x%02x", i.Class), "driver": i.Driver, "name": i.Name}
		var eps []any
		for _, ep := range i.Endpoints {
			eps = append(eps, fmt.Sprintf("0x%02x %s %dB", ep.Address, ep.Type, ep.MaxPacket))
		}
		m["endpoints"] = eps
		if i.Class == 0xff {
			m["role"] = "debug (claimed by this plugin)"
		}
		ifs = append(ifs, m)
	}
	res["interfaces"] = ifs
	if i, ok := debugInterface(d); ok {
		res["debug_interface"] = float64(i.Number)
	}
	if e.Sim != nil {
		res["synthetic"] = true
	}
	return res
}

// staticInfo answers info for a device that is not open without claiming
// anything.
func (p *Plugin) staticInfo(id string, args map[string]any) (map[string]any, bool, error) {
	p.mu.Lock()
	open := p.devs[id] != nil
	p.mu.Unlock()
	if open {
		return nil, false, nil
	}
	e, err := p.find(id)
	if err != nil {
		return nil, false, err
	}
	if _, err := parseOptions(defaultOptions(), args); err != nil {
		return nil, false, status.Error(codes.InvalidArgument, err.Error())
	}
	res := usbFacts(e)
	res["open"] = false
	res["note"] = "device not open: nothing was claimed; call identify or chip_info to talk to the target"
	return res, true, nil
}
