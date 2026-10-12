package espusbjtag

import (
	"context"

	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/faronov/hwlab/internal/ownedtemp"
	"math"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

const (
	opTimeout = 30 * time.Second
	// maxShiftBits bounds one raw IR/DR scan.
	maxShiftBits = 8192
	// maxReadBytes bounds one memory read: every word costs a few DMI scans
	// (about 2.5 ms each on the USB-JTAG unit).
	maxReadBytes = 4096
	// maxReadFile bounds a memory read written to a file.
	maxReadFile = 4 << 20
	// Espressif peripheral register space. A system-bus read there can have
	// side effects (FIFO pops, interrupt clears), so it needs an opt-in.
	periphLo, periphHi = 0x60000000, 0x6FFFFFFF
)

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func spec(c string, d pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: c, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
}

const devProps = `"interface":{"type":"number","description":"USB interface number of the vendor JTAG interface; default is auto-detected (2 on ESP32-C6). The CDC interfaces are never claimed"},` +
	`"clock_khz":{"type":"number","description":"requested TCK in kHz, default 4000"},` +
	`"timeout_ms":{"type":"number"},` +
	`"hart":{"type":"number","description":"hart index in the debug module, default 0; may also be given per call to address another hart"},` +
	`"reconnect_timeout_ms":{"type":"number","description":"how long a lost device is waited for (rediscovery by USB serial), default 8000"},` +
	`"allow_memory_write":{"type":"boolean","description":"DANGEROUS config: permit SRAM writes"},` +
	`"allow_reset":{"type":"boolean","description":"DANGEROUS config: permit hart/system/SoC resets"},` +
	`"allow_flash":{"type":"boolean","description":"DANGEROUS config: permit flash programming/restore"},` +
	`"allow_erase":{"type":"boolean","description":"DANGEROUS config: permit flash erase"},` +
	`"allow_chip_erase":{"type":"boolean","description":"DANGEROUS config: permit whole-chip erase (stronger than allow_erase)"}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	T, S, I := pluginv1.CapJTAGTransport, pluginv1.CapJTAGScan, pluginv1.CapTargetIdentify
	D, M, F := pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming
	obj := func(props string) string { return `{"type":"object","properties":{` + props + `}}` }
	shiftArgs := `{"type":"object","required":["bits"],"properties":{` + devProps +
		`,"bits":{"type":"number","description":"number of bits, 1..8192"},` +
		`"data":{"type":"string","description":"hex, LSB-first byte order; default all ones"},` +
		`"capture":{"type":"boolean","description":"return the bits shifted out, default true"}}}`
	scanProps := devProps + `,"max_devices":{"type":"number","description":"bound on chain length, default 8"},` +
		`"measure_ir":{"type":"boolean","description":"measure total IR length (loads BYPASS), default true"}`
	regProps := devProps + `,"keep_halted":{"type":"boolean","description":"leave the hart halted if this call had to halt it; default false (it is resumed)"}`
	readArgs := `{"type":"object","required":["address","length"],"properties":{` + devProps +
		`,"address":{"description":"number or string like \"0x42000000\""},"length":{"type":"number","description":"bytes, 1..4096 inline"},` +
		`"allow_peripheral":{"type":"boolean","description":"permit reads in 0x60000000-0x6FFFFFFF, where a read can have side effects; default false"},"path":{"type":"string","description":"write the bytes to this new file (up to 4 MiB) instead of returning hex"},"overwrite":{"type":"boolean"}}}`
	hartP := `"hart":{"type":"number"}`
	writeArgs := `{"type":"object","required":["address","data"],"properties":{` + devProps + `,"address":{},"data":{"type":"string","description":"hex, 1..4096 bytes"}}}`
	stepArgs := obj(devProps + `,"count":{"type":"number","description":"1..1000"}`)
	bpArgs := `{"type":"object","required":["address"],"properties":{` + devProps + `,"address":{},"run_wait_ms":{"type":"number","description":"resume and wait this long for the breakpoint to hit (one-shot flow)"}}}`
	clrArgs := obj(devProps + `,"address":{},"all":{"type":"boolean"}`)
	resetArgs := obj(devProps + `,"kind":{"type":"string","enum":["hart","system","soc"]},"halt":{"type":"boolean"},"confirm":{"type":"string"}`)
	flashReadArgs := `{"type":"object","required":["address","length"],"properties":{` + devProps + `,"address":{},"length":{"type":"number"},"path":{"type":"string"},"overwrite":{"type":"boolean"}}}`
	flashImgArgs := `{"type":"object","required":["path","address"],"properties":{` + devProps + `,"path":{"type":"string"},"address":{}}}`
	eraseArgs := `{"type":"object","required":["mode","confirm","backup"],"properties":{` + devProps + `,"mode":{"type":"string","enum":["region","app","chip"]},"address":{},"length":{"type":"number"},"partition":{"type":"string"},"confirm":{"type":"string"},"confirm_protected":{"type":"string"},"backup":{"type":"string"}}}`
	progArgs := `{"type":"object","required":["path","address","confirm","backup"],"properties":{` + devProps + `,"path":{"type":"string"},"address":{},"confirm":{"type":"string"},"confirm_protected":{"type":"string"},"backup":{"type":"string"},"reset":{"type":"string","enum":["run","halt","none"]}}}`
	restoreArgs := `{"type":"object","required":["path","confirm"],"properties":{` + devProps + `,"path":{"type":"string"},"confirm":{"type":"string"},"confirm_protected":{"type":"string"},"reset":{"type":"string","enum":["run","halt","none"]},"allow_other_device":{"type":"boolean"}}}`
	_ = hartP
	return &pluginv1.PluginInfo{
		Id: "espusbjtag", Name: "Espressif USB Serial/JTAG (native)", Vendor: "hwlab", Version: "0.2.0",
		Capabilities:        deviceCaps(),
		DangerousConfigKeys: append([]string(nil), permKeys...),
		Methods: []*pluginv1.MethodSpec{
			spec(T, safe, "info", "device, USB interface mapping and (when open) the transport facts; claims nothing when the device is not open", obj(devProps)),
			spec(T, safe, "tap_reset", "force the TAP to Test-Logic-Reset (five TCK cycles with TMS high); touches no target register", obj(devProps)),
			spec(T, confirm, "shift_ir", "shift bits through the instruction register and return what came out; loads an instruction into the TAP", shiftArgs),
			spec(T, confirm, "shift_dr", "shift bits through the selected data register and return what came out; may change target state", shiftArgs),
			spec(S, safe, "scan", "reset the TAP and enumerate the chain from the IDCODE/BYPASS stream; repeatable, writes nothing to the target", obj(scanProps)),
			spec(I, safe, "identify", "IDCODE of the chain with decoded version/part/manufacturer and a chip hint; JTAG identification only, no CPU or memory access", obj(scanProps)),
			spec(D, safe, "info", "same as JTAGTransport.info", obj(devProps)),
			spec(D, safe, "chip_info", "RISC-V debug transport and debug module identification (spec version, harts, program buffer, system bus) and the hart state; does not halt anything", obj(devProps)),
			spec(D, confirm, "halt", "halt the selected hart and confirm it in dmstatus; it stays halted (debug module active) until resume", obj(devProps)),
			spec(D, confirm, "resume", "resume the selected hart and wait for the acknowledge", obj(devProps)),
			spec(D, confirm, "read_registers", "read x0-x31, pc (dpc) and the identification CSRs of the selected hart; halts a running hart and resumes it again unless keep_halted", obj(regProps)),
			spec(M, safe, "read", "read target memory through the RISC-V system bus (no halt); 32-bit accesses, max 4096 bytes inline or up to 4 MiB to a new file with path (SHA-256 returned). The Espressif peripheral window 0x60000000-0x6FFFFFFF needs allow_peripheral", readArgs),
			spec(M, confirm, "write", "write SRAM through the system bus (needs allow_memory_write): 8/16/32-bit aware (read-modify-write), read back and compared, previous bytes returned. ROM, the flash-mapped window, peripherals and unmapped space are refused; use the flash methods for flash", writeArgs),
			spec(D, confirm, "step", "single-step the halted hart count times (dcsr.step; default 1, max 1000); each step is confirmed by halt and dcsr.cause=step, the step bit is restored", stepArgs),
			spec(D, safe, "harts", "per-hart report (availability, run state, XLEN, hartinfo, abstract-command and register access, SBA); halts and resumes nothing", obj(devProps)),
			spec(D, confirm, "set_breakpoint", "install a hardware execution breakpoint (RISC-V trigger) at an address; the free slot is discovered, never assumed. run_wait_ms>0 resumes and waits for the hit", bpArgs),
			spec(D, confirm, "clear_breakpoint", "remove a breakpoint installed by this session (address) or all=true; the slot's previous trigger state is restored", clrArgs),
			spec(D, safe, "list_breakpoints", "enumerate the hart's trigger slots (tselect/tdata1/tdata2) and the breakpoints this session installed", obj(devProps)),
			spec(D, confirm, "wait_halt", "wait for the hart to halt (wait_ms, default 2000) and report the cause; says whether an installed breakpoint was hit", obj(devProps+`,"wait_ms":{"type":"number"}`)),
			spec(D, confirm, "reset", "reset: kind=hart (hartreset), system (ndmreset, default) or soc (RTC watchdog full chip reset; the USB device re-enumerates and the session is rebuilt by USB serial and identity-checked). Needs allow_reset; soc needs confirm=\"reset-soc\". halt=true stops at the first instruction", resetArgs),
			spec(D, safe, "reconnect", "drop the session and wait (bounded by reconnect_timeout_ms) for the device with the same USB serial, reopen interface and rebuild TAP/DTM/DM, and verify IDCODE and ROM chip evidence equal the earlier ones", obj(devProps)),
			spec(F, confirm, "info", "flash identity and geometry (JEDEC, SFDP size, status), eFuse security state (read-only), partition table, protected ranges; halts the CPU and quiesces watchdogs for the call and restores them", obj(devProps)),
			spec(F, confirm, "read", "physical flash read through the SPI1 controller (not the CPU window): address, length; up to 4096 bytes inline, or any length to a new file with path (+overwrite); SHA-256 returned", flashReadArgs),
			spec(F, confirm, "verify", "compare a file with physical flash at address without writing; any difference is an error", flashImgArgs),
			spec(F, confirm, "backup", "read the WHOLE flash twice, require identical SHA-256, store it (mode 0600) with a manifest in dir (default $HWLAB_HOME/artifacts/esp-flash); never modifies flash. The artifact stays local", obj(devProps+`,"dir":{"type":"string"}`)),
			spec(F, confirm, "erase", "erase flash: mode=region (address,length, 4 KiB aligned; confirm=\"erase-flash-region\"), app (partition label; \"erase-app-partition\") or chip (\"erase-entire-chip\", needs allow_chip_erase). All need allow_flash+allow_erase, backup=<verified full backup .bin>; protected ranges need confirm_protected=\"modify-protected-region\". There is no implicit mode", eraseArgs),
			spec(F, confirm, "program", "program an image at address, touching only the 4 KiB sectors that change (edge bytes preserved), read back and compared; needs allow_flash, confirm=\"program-flash\", backup=<verified full backup>; ESP images (0xE9) are validated first (truncated/corrupt refused); reset=run|halt|none after success (needs allow_reset)", progArgs),
			spec(F, confirm, "restore", "restore a verified full backup (path): changes only differing sectors, then compares the whole-flash SHA-256 with the backup; needs allow_flash+allow_erase, confirm=\"restore-flash\", confirm_protected=\"modify-protected-region\"", restoreArgs),
		},
	}
}

func wrap(err error) error {
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, usb.ErrDisconnected):
		return goneErr("%v", transportErr(err))
	case errors.Is(err, riscv.ErrHaltTimeout):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.FailedPrecondition, transportErr(err).Error())
}

var methods = map[string]bool{
	"JTAGTransport.info": true, "JTAGTransport.tap_reset": true, "JTAGTransport.shift_ir": true, "JTAGTransport.shift_dr": true,
	"JTAGScan.scan": true, "TargetIdentify.identify": true,
	"DebugProbe.info": true, "DebugProbe.chip_info": true, "DebugProbe.halt": true, "DebugProbe.resume": true,
	"DebugProbe.read_registers": true, "MemoryAccess.read": true, "MemoryAccess.write": true,
	"DebugProbe.step": true, "DebugProbe.harts": true, "DebugProbe.set_breakpoint": true, "DebugProbe.clear_breakpoint": true,
	"DebugProbe.list_breakpoints": true, "DebugProbe.wait_halt": true, "DebugProbe.reset": true, "DebugProbe.reconnect": true,
	"FlashProgramming.info": true, "FlashProgramming.read": true, "FlashProgramming.verify": true, "FlashProgramming.backup": true,
	"FlashProgramming.erase": true, "FlashProgramming.program": true, "FlashProgramming.restore": true,
}

// opTimeoutFor gives long flash operations room: a full-flash read runs at
// about 70 KB/s.
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
	if strings.HasSuffix(method, ".info") {
		if res, ok, err := p.staticInfo(id, args); err != nil {
			return nil, err
		} else if ok {
			return respond(res)
		}
	}
	if method == "MemoryAccess.read" {
		if _, err := parseRead(args); err != nil {
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
	var res map[string]any
	if kind, _ := args["kind"].(string); method == "DebugProbe.reset" && kind == "soc" {
		sctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		res, err = p.soc(sctx, id, s, p.isCurrent(id, s), args)
	} else {
		res, err = p.run(ctx, s, method, args)
	}
	if err != nil {
		if errors.Is(err, usb.ErrDisconnected) {
			p.drop(id, s)
		}
		return nil, wrap(err)
	}
	return respond(res)
}

func (p *Plugin) run(ctx context.Context, s *session, method string, args map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.FailedPrecondition, "device is closed")
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeoutFor(method))
	defer cancel()
	res, err := s.dispatch(cctx, method, args)
	if err == nil && s.persist && s.dm != nil && s.identity == nil {
		// Remember what this device is so that a later reconnect can prove it
		// is the same one.
		if got, e := s.captureIdentity(cctx); e == nil {
			s.identity = &got
		}
	}
	return res, err
}

func (s *session) dispatch(cctx context.Context, method string, args map[string]any) (map[string]any, error) {
	switch method {
	case "JTAGTransport.info", "DebugProbe.info":
		return s.facts(), nil
	case "JTAGTransport.tap_reset":
		return s.tapReset(cctx)
	case "JTAGTransport.shift_ir":
		return s.shift(cctx, args, true)
	case "JTAGTransport.shift_dr":
		return s.shift(cctx, args, false)
	case "JTAGScan.scan":
		return s.scan(cctx, args)
	case "TargetIdentify.identify":
		return s.identify(cctx, args)
	case "DebugProbe.chip_info":
		return s.chipInfo(cctx, args)
	case "DebugProbe.halt":
		return s.halt(cctx, args)
	case "DebugProbe.resume":
		return s.resume(cctx, args)
	case "MemoryAccess.write":
		return s.writeMem(cctx, args)
	case "DebugProbe.step":
		return s.step(cctx, args)
	case "DebugProbe.harts":
		return s.harts(cctx, args)
	case "DebugProbe.set_breakpoint":
		return s.setBreakpoint(cctx, args)
	case "DebugProbe.clear_breakpoint":
		return s.clearBreakpoint(cctx, args)
	case "DebugProbe.list_breakpoints":
		return s.listBreakpoints(cctx, args)
	case "DebugProbe.wait_halt":
		return s.waitHalt(cctx, args)
	case "DebugProbe.reset":
		return s.reset(cctx, args)
	case "FlashProgramming.info":
		return s.flashInfo(cctx, args)
	case "FlashProgramming.read":
		return s.flashRead(cctx, args)
	case "FlashProgramming.verify":
		return s.flashVerify(cctx, args)
	case "FlashProgramming.backup":
		return s.flashBackup(cctx, args)
	case "FlashProgramming.erase":
		return s.flashErase(cctx, args)
	case "FlashProgramming.program":
		return s.flashProgram(cctx, args)
	case "FlashProgramming.restore":
		return s.flashRestore(cctx, args)
	case "DebugProbe.read_registers":
		return s.registers(cctx, args)
	case "MemoryAccess.read":
		return s.readMem(cctx, args)
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

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

func usbFacts(e Entry) map[string]any {
	d := e.Dev
	res := map[string]any{
		"vid": fmt.Sprintf("%04x", d.VID), "pid": fmt.Sprintf("%04x", d.PID),
		"manufacturer": d.Manufacturer, "product": d.Product, "serial": d.Serial,
		"configuration": float64(d.Configuration), "interface_count": float64(len(d.Interfaces)),
		"target_protocol": "RISC-V Debug Module 0.13 over JTAG",
		"cpu_debug":       []any{"halt", "resume", "step", "read_registers", "breakpoints", "harts"}, "memory_access": "read (system bus); write: SRAM only, needs allow_memory_write",
		"reset": "hart, system, soc (needs allow_reset)", "flash": "ESP-specific native SPI1 driver (needs allow_flash/allow_erase, confirmation tokens, a verified backup)",
	}
	var ifs []any
	for _, i := range d.Interfaces {
		m := map[string]any{"number": float64(i.Number), "class": fmt.Sprintf("0x%02x", i.Class), "driver": i.Driver}
		if i.TTY != "" {
			m["tty"] = "/dev/" + i.TTY
		}
		var eps []any
		for _, ep := range i.Endpoints {
			eps = append(eps, fmt.Sprintf("0x%02x %s %dB", ep.Address, ep.Type, ep.MaxPacket))
		}
		m["endpoints"] = eps
		switch i.Class {
		case 0xff:
			m["role"] = "jtag (claimed by this plugin)"
		case 0x02, 0x0a:
			m["role"] = "cdc-acm serial (never claimed; use the uart plugin)"
		}
		ifs = append(ifs, m)
	}
	res["interfaces"] = ifs
	if ji, ok := jtagInterface(d); ok {
		res["jtag_interface"] = float64(ji.Number)
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
	res["note"] = "device not open: nothing was claimed; call scan, identify or chip_info to talk to the target"
	return res, true, nil
}

func (s *session) facts() map[string]any {
	res := usbFacts(s.entry)
	res["open"] = true
	res["jtag_interface"] = float64(s.iface)
	c := s.probe.Caps()
	res["transport"] = map[string]any{
		"proto_version": float64(c.ProtoVersion), "apb_khz": float64(c.APBKHz), "base_khz": float64(c.BaseKHz),
		"divider": float64(s.probe.Divisor()), "divider_min": float64(c.DivMin), "divider_max": float64(c.DivMax),
		"tck_khz": float64(s.probe.ClockKHz()), "hart": float64(s.curHart()),
	}
	return res
}

// mark adds the evidence labels every target answer carries.
func (s *session) mark(res map[string]any, real bool) map[string]any {
	res["target_verified"] = real && s.entry.Sim == nil
	if s.entry.Sim != nil {
		res["synthetic"] = true
	}
	res["usb_serial"] = s.entry.Dev.Serial
	res["device"] = s.entry.ID
	return res
}

// jtagTouched is called after raw JTAG traffic: the DTM must reselect its
// instruction.
func (s *session) jtagTouched(ctx context.Context) error {
	if s.dtm != nil {
		return s.dtm.Reset(ctx)
	}
	return nil
}

func (s *session) tapReset(ctx context.Context) (map[string]any, error) {
	if err := s.drv.Reset(ctx); err != nil {
		return nil, err
	}
	if err := s.jtagTouched(ctx); err != nil {
		return nil, err
	}
	return s.mark(map[string]any{"state": s.drv.State().String(), "ok": true}, false), nil
}

func (s *session) shift(ctx context.Context, args map[string]any, ir bool) (map[string]any, error) {
	nf, ok, err := optNumber(args, "bits")
	if err != nil {
		return nil, argErr(err)
	}
	if !ok || nf < 1 || nf > maxShiftBits || nf != math.Trunc(nf) {
		return nil, argErr(fmt.Errorf("bits: want an integer 1..%d", maxShiftBits))
	}
	n := int(nf)
	data := jtag.Ones(n)
	if v, ok := args["data"]; ok && v != nil {
		str, isStr := v.(string)
		if !isStr {
			return nil, argErr(errors.New("data: want a hex string"))
		}
		b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(str), "0x"))
		if err != nil {
			return nil, argErr(fmt.Errorf("data: %w", err))
		}
		if len(b) != (n+7)/8 {
			return nil, argErr(fmt.Errorf("data: %d bits need %d bytes, got %d", n, (n+7)/8, len(b)))
		}
		data = b
	}
	capture := true
	if v, ok := args["capture"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, argErr(errors.New("capture: want true or false"))
		}
		capture = b
	}
	var out []byte
	if ir {
		out, err = s.drv.ShiftIR(ctx, data, n, capture)
	} else {
		out, err = s.drv.ShiftDR(ctx, data, n, capture)
	}
	if err != nil {
		return nil, err
	}
	res := map[string]any{"bits": float64(n), "state": s.drv.State().String(), "register": map[bool]string{true: "IR", false: "DR"}[ir]}
	if capture {
		res["out"] = hex.EncodeToString(out)
	}
	if err := s.jtagTouched(ctx); err != nil {
		return nil, err
	}
	return s.mark(res, false), nil
}

func scanOpts(args map[string]any) (jtag.ScanOptions, error) {
	o := jtag.ScanOptions{MaxDevices: 8, MeasureIR: true}
	if v, ok, err := optNumber(args, "max_devices"); err != nil {
		return o, argErr(err)
	} else if ok {
		if v < 1 || v > 64 || v != math.Trunc(v) {
			return o, argErr(fmt.Errorf("max_devices %v: want 1..64", v))
		}
		o.MaxDevices = int(v)
	}
	if v, ok := args["measure_ir"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return o, argErr(errors.New("measure_ir: want true or false"))
		}
		o.MeasureIR = b
	}
	return o, nil
}

func (s *session) rescan(ctx context.Context, args map[string]any) (jtag.Chain, error) {
	opt, err := scanOpts(args)
	if err != nil {
		return jtag.Chain{}, err
	}
	c, err := jtag.ScanChain(ctx, s.drv, opt)
	if err != nil {
		return c, err
	}
	if opt.MeasureIR {
		s.chain = &c
	}
	return c, s.jtagTouched(ctx)
}

func (s *session) scan(ctx context.Context, args map[string]any) (map[string]any, error) {
	c, err := s.rescan(ctx, args)
	if err != nil {
		return nil, err
	}
	res, err := toMap(c)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if res["devices"] == nil {
		res["devices"] = []any{}
	}
	res["count"] = float64(len(c.Devices))
	res["tck_khz"] = float64(s.probe.ClockKHz())
	return s.mark(res, c.Status == jtag.StatusOK), nil
}

// chipHint reports what the IDCODE alone supports. It is evidence, not
// identification: other Espressif RISC-V parts may share a JTAG IDCODE, and
// the USB descriptors never name the chip. The ROM chip-detect word read
// through the debug module (see FlashProgramming.info) is the stronger check.
func chipHint(id *jtag.IDCode) map[string]any {
	if id == nil {
		return nil
	}
	if id.Raw == 0x0000dc25 {
		return map[string]any{
			"observed": "IDCODE 0x0000dc25", "consistent_with": "ESP32-C6 (observed on a board known to carry one)",
			"basis":      "an IDCODE match is evidence, not proof; the chip is not identified from the IDCODE alone",
			"confidence": "idcode-only",
		}
	}
	return map[string]any{"observed": fmt.Sprintf("IDCODE 0x%08x", id.Raw), "consistent_with": "unknown", "basis": "this IDCODE is not in the built-in table"}
}

func (s *session) identify(ctx context.Context, args map[string]any) (map[string]any, error) {
	c, err := s.rescan(ctx, args)
	if err != nil {
		return nil, err
	}
	var targets []any
	var first *jtag.IDCode
	for _, d := range c.Devices {
		t := map[string]any{"index": float64(d.Index)}
		if d.Bypass || d.IDCode == nil {
			t["bypass"] = true
		} else {
			id := d.IDCode
			if first == nil {
				first = id
			}
			t["idcode"] = fmt.Sprintf("0x%08x", id.Raw)
			t["version"] = float64(id.Version)
			t["part"] = fmt.Sprintf("0x%04x", id.Part)
			t["manufacturer"] = fmt.Sprintf("0x%03x", id.Manufacturer)
			if id.ManufacturerName != "" {
				t["manufacturer_name"] = id.ManufacturerName
			}
		}
		if d.IRLength > 0 {
			t["ir_length"] = float64(d.IRLength)
		}
		targets = append(targets, t)
	}
	res := map[string]any{
		"status": c.Status, "count": float64(len(c.Devices)), "targets": targets,
		"basis": "JTAG IDCODE only; no CPU, memory or flash access",
	}
	if len(targets) == 0 {
		res["targets"] = []any{}
	}
	if h := chipHint(first); h != nil {
		res["chip_hint"] = h
	}
	if len(c.Notes) > 0 {
		var ns []any
		for _, n := range c.Notes {
			ns = append(ns, n)
		}
		res["notes"] = ns
	}
	return s.mark(res, c.Status == jtag.StatusOK), nil
}

func (s *session) chipInfo(ctx context.Context, args map[string]any) (map[string]any, error) {
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	info, err := dm.Identify(ctx, 8)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"basis": "JTAG IDCODE and RISC-V debug module registers; nothing was halted"}
	if c := s.chain; c != nil && len(c.Devices) == 1 && c.Devices[0].IDCode != nil {
		id := c.Devices[0].IDCode
		res["idcode"] = fmt.Sprintf("0x%08x", id.Raw)
		res["ir_length"] = float64(c.TotalIR)
		if h := chipHint(id); h != nil {
			res["chip_hint"] = h
		}
	}
	dtm, err := toMap(info.DTM)
	if err != nil {
		return nil, err
	}
	res["dtm"] = dtm
	res["debug_spec"] = info.Status.VersionName()
	res["authenticated"] = info.Status.Authenticated
	res["harts"] = float64(info.Harts)
	res["hart"] = float64(dm.Hart())
	res["hart_state"] = string(info.Status.State)
	res["has_reset_halt_req"] = info.Status.HasResetHaltReq
	res["progbuf_words"] = float64(info.ProgBufSize)
	res["data_registers"] = float64(info.DataCount)
	res["sba"] = map[string]any{"version": float64(info.SBVersion), "address_bits": float64(info.SBASize), "access32": info.SBA32}
	res["debug_module_activated_by_this_session"] = info.ActivatedByUs
	return s.mark(res, true), nil
}

func (s *session) stateResult(ctx context.Context, dm *riscv.DM, st riscv.Status) map[string]any {
	res := map[string]any{"hart": float64(dm.Hart()), "state": string(st.State), "halted": st.State == riscv.HartHalted, "dmstatus": fmt.Sprintf("0x%08x", st.Raw)}
	if st.State == riscv.HartHalted {
		if pc, err := dm.ReadReg(ctx, riscv.CSRDPC); err == nil {
			res["pc"] = fmt.Sprintf("0x%08x", pc)
		}
	}
	return res
}

func (s *session) halt(ctx context.Context, args map[string]any) (map[string]any, error) {
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	st, err := dm.Halt(ctx)
	if err != nil {
		return nil, err
	}
	res := s.stateResult(ctx, dm, st)
	res["note"] = "the hart stays halted, with the debug module active, until DebugProbe.resume"
	return s.mark(res, true), nil
}

func (s *session) resume(ctx context.Context, args map[string]any) (map[string]any, error) {
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	before, err := dm.Status(ctx)
	if err != nil {
		return nil, err
	}
	res := map[string]any{}
	st := before
	if before.State == riscv.HartHalted {
		// Only the acknowledge is awaited: with an armed trigger the hart may
		// already have halted again, which is a result, not an error.
		if err = dm.ResumeNoWait(ctx); err != nil {
			return nil, err
		}
		if st, err = dm.Status(ctx); err != nil {
			return nil, err
		}
		res["resumed"] = true
		if st.State == riscv.HartHalted {
			res["halted_again"] = true
			if hi, herr := dm.HaltedInfo(ctx); herr == nil {
				res["halted_cause"], res["halted_cause_name"], res["halted_pc"] = float64(hi.Cause), hi.CauseStr, fmt.Sprintf("0x%08x", hi.PC)
			}
		}
	} else {
		res["resumed"] = false
		res["note"] = "the hart was not halted; nothing to resume"
	}
	for k, v := range s.stateResult(ctx, dm, st) {
		res[k] = v
	}
	return s.mark(res, true), nil
}

var gprNames = [32]string{"zero", "ra", "sp", "gp", "tp", "t0", "t1", "t2", "s0", "s1", "a0", "a1", "a2", "a3", "a4", "a5",
	"a6", "a7", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9", "s10", "s11", "t3", "t4", "t5", "t6"}

func (s *session) registers(ctx context.Context, args map[string]any) (res map[string]any, err error) {
	keep := false
	if v, ok := args["keep_halted"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, argErr(errors.New("keep_halted: want true or false"))
		}
		keep = b
	}
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	st, err := dm.Status(ctx)
	if err != nil {
		return nil, err
	}
	wasRunning := st.State == riscv.HartRunning
	if st.State != riscv.HartHalted {
		if !wasRunning {
			return nil, fmt.Errorf("hart %d is %s and cannot be halted", dm.Hart(), st.State)
		}
		if _, err := dm.Halt(ctx); err != nil {
			return nil, err
		}
	}
	defer func() {
		// Restore the run state even when a read failed.
		if wasRunning && !keep {
			if _, rerr := dm.Resume(context.WithoutCancel(ctx)); rerr != nil && err == nil {
				err = fmt.Errorf("registers were read but the hart did not resume: %w", rerr)
			} else if rerr == nil && res != nil {
				res["resumed"] = true
			}
		}
	}()
	regs := map[string]any{}
	for n := 0; n < 32; n++ {
		v, err := dm.ReadReg(ctx, riscv.GPR(n))
		if err != nil {
			return nil, fmt.Errorf("read x%d: %w", n, err)
		}
		regs[fmt.Sprintf("x%d", n)] = fmt.Sprintf("0x%08x", v)
		regs[gprNames[n]] = fmt.Sprintf("0x%08x", v)
	}
	csrs := []struct {
		name string
		num  riscv.RegNum
	}{{"pc", riscv.CSRDPC}, {"dcsr", riscv.CSRDCSR}, {"misa", riscv.CSRMISA}, {"mvendorid", riscv.CSRMVendorID},
		{"marchid", riscv.CSRMArchID}, {"mimpid", riscv.CSRMImpID}, {"mhartid", riscv.CSRMHartID}}
	for _, c := range csrs {
		v, err := dm.ReadReg(ctx, c.num)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", c.name, err)
		}
		regs[c.name] = fmt.Sprintf("0x%08x", v)
	}
	res = map[string]any{
		"registers": regs, "hart": float64(dm.Hart()), "was_running": wasRunning, "halted": !wasRunning || keep,
		"basis": "abstract Access Register commands on a halted hart; read-only",
	}
	return s.mark(res, true), nil
}

func toAddr(v any) (uint32, error) {
	var u uint64
	switch x := v.(type) {
	case float64:
		if x < 0 || x != math.Trunc(x) {
			return 0, fmt.Errorf("invalid address %v", x)
		}
		u = uint64(x)
	case string:
		var err error
		if u, err = strconv.ParseUint(strings.TrimSpace(x), 0, 64); err != nil {
			return 0, err
		}
	default:
		return 0, errors.New(`address must be a number or a string like "0x42000000"`)
	}
	if u > math.MaxUint32 {
		return 0, fmt.Errorf("address 0x%x does not fit the 32-bit address space", u)
	}
	return uint32(u), nil
}

type readReq struct {
	addr uint32
	n    int
	path string
}

// parseRead validates a read before any device is opened.
func parseRead(args map[string]any) (readReq, error) {
	addr, err := toAddr(args["address"])
	if err != nil {
		return readReq{}, argErr(err)
	}
	nf, ok, err := optNumber(args, "length")
	if err != nil {
		return readReq{}, argErr(err)
	}
	limit := maxReadBytes
	path, err := strArg(args, "path")
	if err != nil {
		return readReq{}, err
	}
	if path != "" {
		limit = maxReadFile
	}
	if !ok || nf < 1 || nf > float64(limit) || nf != math.Trunc(nf) {
		if path == "" {
			return readReq{}, argErr(fmt.Errorf("length: want an integer 1..%d bytes inline (pass path=<new file> for up to %d bytes)", maxReadBytes, maxReadFile))
		}
		return readReq{}, argErr(fmt.Errorf("length: want an integer 1..%d bytes", limit))
	}
	n := int(nf)
	if uint64(addr)+uint64(n) > 1<<32 {
		return readReq{}, argErr(errors.New("the range wraps past the end of the 32-bit address space"))
	}
	allow := false
	if v, ok := args["allow_peripheral"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return readReq{}, argErr(errors.New("allow_peripheral: want true or false"))
		}
		allow = b
	}
	if !allow && uint64(addr) <= periphHi && uint64(addr)+uint64(n)-1 >= periphLo {
		return readReq{}, argErr(fmt.Errorf("0x%08x..0x%08x touches the peripheral window 0x%08x-0x%08x where a read can have side effects: pass allow_peripheral=true to read it anyway",
			addr, uint64(addr)+uint64(n)-1, uint32(periphLo), uint32(periphHi)))
	}
	return readReq{addr, n, path}, nil
}

func (s *session) readMem(ctx context.Context, args map[string]any) (map[string]any, error) {
	rq, err := parseRead(args)
	if err != nil {
		return nil, err
	}
	addr, n := rq.addr, rq.n
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	data, err := dm.ReadMem(ctx, addr, n)
	if err != nil {
		return nil, err
	}
	res := map[string]any{
		"address": fmt.Sprintf("0x%08x", addr), "length": float64(n),
		"via": "system bus access, 32-bit, hart not halted by this call", "elapsed_ms": float64(time.Since(start).Milliseconds()),
		"sha256": sum(data),
	}
	if rq.path == "" {
		res["hex"] = hex.EncodeToString(data)
	} else {
		overwrite, err := boolArg(args, "overwrite", false)
		if err != nil {
			return nil, err
		}
		if err := ownedtemp.WriteFile(rq.path, data, overwrite); err != nil {
			return nil, argErr(err)
		}
		res["path"] = rq.path
	}
	return s.mark(res, true), nil
}
