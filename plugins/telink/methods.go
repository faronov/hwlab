package telinkplugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/telink/sws"
	"github.com/faronov/hwlab/plugins/telink/target"
)

const (
	maxReadBytes  = 64 << 10
	inlineBytes   = 4096
	maxWriteBytes = 4096
	previewBytes  = 64
	maxImageBytes = 512 << 10
	maxSteps      = 1000

	swireTop = 0x1000000

	defaultResetMs    = 500 // the reset+activate timing the zigbee-rs lab flow uses (-t 500 -a 200)
	defaultActivateMs = 200
	releaseWait       = 70 * time.Millisecond // target init time after releasing RST
)

func spec(c string, d pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: c, Name: name, Description: desc, Danger: d, ArgsSchemaJson: schema}
}

const optProps = `"baud":{"type":"number","description":"programmer UART speed, default 230400"},"swdiv":{"type":"number","description":"SWire clock divisor, programmer default kept unless set"},"timeout_ms":{"type":"number"}`

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	D, M, F := pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming
	obj := func(props string) string { return `{"type":"object","properties":{` + props + `}}` }
	timing := `"reset_ms":{"type":"number","description":"RST held low, default 500"},"activate_ms":{"type":"number","description":"SWire activation window, default 200"}`
	return &pluginv1.PluginInfo{
		Id: "telink", Name: "Telink SWire programmer (TLSR825x)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: caps(map[string]string{"validated": "false"}),
		Methods: []*pluginv1.MethodSpec{
			spec(D, safe, "info", "identify the programmer (firmware, chip family, SWire settings); does not touch the target", obj(optProps)),
			spec(D, safe, "connect", "read the target chip id and flash JEDEC id if the target already answers over SWire; never resets or activates it (use attach for that)", obj(optProps)),
			spec(D, confirm, "attach", "take control of the target: mode hard (default) pulses RST then activates SWire; mode soft reboots through SWire then activates. Interrupts the running firmware; halt=true leaves the CPU stalled",
				obj(optProps+`,"mode":{"enum":["hard","soft"]},`+timing+`,"halt":{"type":"boolean"}`)),
			spec(D, safe, "status", "CPU control byte, PC, whether the PC is moving; reports unreachable instead of failing when the target does not answer", obj(optProps)),
			spec(D, confirm, "halt", "capture the CPU: mode stall (default, control 0x06) or stop (0x05)", obj(optProps+`,"mode":{"enum":["stall","stop"]}`)),
			spec(D, confirm, "resume", "let a captured CPU run (control 0x08)", obj(optProps)),
			spec(D, confirm, "step", "execute count instructions (default 1, max 1000) of a captured CPU", obj(optProps+`,"count":{"type":"number"}`)),
			spec(D, confirm, "reset", "reset the target and let it run: kind hard (default, pulses RST) or soft (SWire reboot register); SWire must be re-activated afterwards with attach",
				obj(optProps+`,"kind":{"enum":["hard","soft"]},"reset_ms":{"type":"number"}`)),
			spec(D, safe, "read_registers", "read r0-r15 (r11 fp, r12 ip, r13 sp, r14 lr, r15 pc) and psr; meaningful only while the CPU is captured", obj(optProps)),
			spec(D, confirm, "set_breakpoint", "arm the single hardware breakpoint at an address and let the CPU run; wait_ms>0 waits for the PC to reach it", `{"type":"object","required":["address"],"properties":{`+optProps+`,"address":{},"wait_ms":{"type":"number"}}}`),
			spec(D, confirm, "clear_breakpoint", "disarm the hardware breakpoint", obj(optProps)),
			spec(M, safe, "read", "read target memory over SWire (peripheral registers from 0x800000, SRAM from 0x840000); args: address, length (max 64 KiB; over 4 KiB returns a preview and sha256). Register reads can have side effects",
				`{"type":"object","required":["address","length"],"properties":{`+optProps+`,"address":{},"length":{"type":"number"}}}`),
			spec(M, confirm, "write", "write target SRAM (0x840000-0x84ffff); other addresses (peripheral registers, the CPU debug block) need allow_registers=true. Max 4096 bytes, verified by readback for SRAM",
				`{"type":"object","required":["address","data"],"properties":{`+optProps+`,"address":{},"data":{"type":"string"},"verify":{"type":"boolean"},"allow_registers":{"type":"boolean"}}}`),
			spec(F, safe, "read", "read flash; args: address, length (max 64 KiB inline; with path, writes up to the whole flash to a new file). Also lists the layout regions touched",
				`{"type":"object","required":["address","length"],"properties":{`+optProps+`,"address":{},"length":{"type":"number"},"path":{"type":"string"}}}`),
			spec(F, safe, "verify", "compare flash with a file or hex data without changing it; args: address, path or data", `{"type":"object","required":["address"],"properties":{`+optProps+`,"address":{},"path":{"type":"string"},"data":{"type":"string"}}}`),
			spec(F, confirm, "erase", "erase the 4 KiB sectors covering a range. Refuses factory data (EUI-64, config, ADC calibration) always and the NV journals unless allow_journals. Clears flash block protection first",
				`{"type":"object","required":["address","length"],"properties":{`+optProps+`,"address":{},"length":{"type":"number"},"allow_journals":{"type":"boolean"}}}`),
			spec(F, confirm, "program", "erase the covering sectors, write an image, and read it back. address must be 4 KiB aligned; image from path (.bin) or hex data. Same region rules as erase; clears flash block protection (persistent)",
				`{"type":"object","required":["address"],"properties":{`+optProps+`,"address":{},"path":{"type":"string"},"data":{"type":"string"},"allow_journals":{"type":"boolean"}}}`),
		},
	}
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

func optBool(a map[string]any, key string) (bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return false, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, fmt.Errorf("%s: want true or false, got %v", key, v)
	}
	return b, nil
}

// addr24 reads a SWire/flash address given as a number or a "0x.." string.
func addr24(a map[string]any, key string) (uint32, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, fmt.Errorf("%s is required", key)
	}
	var n uint64
	switch x := v.(type) {
	case float64:
		if x < 0 || x != math.Trunc(x) || x >= swireTop {
			return 0, fmt.Errorf("%s %v: want an integer 0..0xffffff", key, x)
		}
		n = uint64(x)
	case string:
		var err error
		if n, err = strconv.ParseUint(strings.TrimSpace(x), 0, 32); err != nil || n >= swireTop {
			return 0, fmt.Errorf("%s %q: want a number 0..0xffffff such as 0x840000", key, x)
		}
	default:
		return 0, fmt.Errorf("%s: want a number or a 0x.. string, got %v", key, v)
	}
	return uint32(n), nil
}

func lengthArg(a map[string]any, max int) (int, error) {
	n, ok, err := optNumber(a, "length")
	if err != nil {
		return 0, err
	}
	if !ok || n < 1 || n != math.Trunc(n) || n > float64(max) {
		return 0, fmt.Errorf("length %v: want an integer 1..%d", n, max)
	}
	return int(n), nil
}

func msArg(a map[string]any, key string, def, min, max int) (int, error) {
	v, ok, err := optNumber(a, key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return def, nil
	}
	if v < float64(min) || v > float64(max) || v != math.Trunc(v) {
		return 0, fmt.Errorf("%s %v: want an integer %d..%d", key, v, min, max)
	}
	return int(v), nil
}

func hex24(v uint32) string { return fmt.Sprintf("0x%06x", v) }
func hex32(v uint32) string { return fmt.Sprintf("0x%08x", v) }

func hexData(a map[string]any, key string, max int) ([]byte, error) {
	s, _ := a[key].(string)
	s = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(s), " ", ""), "0x")
	data, err := hex.DecodeString(s)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("%s: want a non-empty hex string", key)
	}
	if len(data) > max {
		return nil, fmt.Errorf("%s is %d bytes; at most %d", key, len(data), max)
	}
	return data, nil
}

func (d *device) image(a map[string]any) ([]byte, error) {
	path, _ := a["path"].(string)
	_, hasData := a["data"]
	switch {
	case path != "" && hasData:
		return nil, errors.New("give either path or data, not both")
	case path != "":
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if st.Size() == 0 || st.Size() > maxImageBytes {
			return nil, fmt.Errorf("%s is %d bytes; want 1..%d", path, st.Size(), maxImageBytes)
		}
		return os.ReadFile(path)
	case hasData:
		return hexData(a, "data", maxWriteBytes)
	}
	return nil, errors.New("path or data is required")
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	method := req.GetCapability() + "." + req.GetMethod()
	switch method {
	case "DebugProbe.info", "DebugProbe.connect", "DebugProbe.attach", "DebugProbe.status", "DebugProbe.halt", "DebugProbe.resume",
		"DebugProbe.step", "DebugProbe.reset", "DebugProbe.read_registers", "DebugProbe.set_breakpoint", "DebugProbe.clear_breakpoint",
		"MemoryAccess.read", "MemoryAccess.write",
		"FlashProgramming.read", "FlashProgramming.verify", "FlashProgramming.erase", "FlashProgramming.program":
	default:
		return nil, status.Errorf(codes.Unimplemented, "method %s", method)
	}
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	d, release, err := p.lease(req.GetDeviceId(), args)
	if err != nil {
		return nil, err
	}
	defer release()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prog.SetContext(ctx)
	defer d.prog.SetContext(context.Background())

	var res map[string]any
	switch method {
	case "DebugProbe.info":
		res, err = d.info()
	case "DebugProbe.connect":
		res, err = d.connectInfo()
	case "DebugProbe.attach":
		res, err = d.attach(args)
	case "DebugProbe.status":
		res = d.status()
	case "DebugProbe.halt":
		res, err = d.halt(args)
	case "DebugProbe.resume":
		res, err = d.resume()
	case "DebugProbe.step":
		res, err = d.step(args)
	case "DebugProbe.reset":
		res, err = d.reset(args)
	case "DebugProbe.read_registers":
		res, err = d.readRegisters()
	case "DebugProbe.set_breakpoint":
		res, err = d.setBreakpoint(args)
	case "DebugProbe.clear_breakpoint":
		if err = d.prog.ClearBreakpoint(); err == nil {
			res = map[string]any{"breakpoint": "cleared"}
		}
	case "MemoryAccess.read":
		res, err = d.readMemory(args)
	case "MemoryAccess.write":
		res, err = d.writeMemory(args)
	case "FlashProgramming.read":
		res, err = d.flashRead(args)
	case "FlashProgramming.verify":
		res, err = d.flashVerify(args)
	case "FlashProgramming.erase":
		res, err = d.flashErase(args)
	case "FlashProgramming.program":
		res, err = d.flashProgram(args)
	}
	if err != nil {
		return nil, wrap(err)
	}
	if d.entry.Sim != nil {
		res["synthetic"] = true
	}
	s, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: s}, nil
}

// wrap keeps gRPC statuses; every other error is the programmer's or the
// target's answer to a request that cannot proceed in the current state.
func wrap(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

func (d *device) info() (map[string]any, error) {
	v, ok := d.prog.Cached()
	if !ok {
		var err error
		if v, err = d.prog.Version(); err != nil {
			return nil, err
		}
	}
	res := map[string]any{
		"programmer_chip": v.Chip, "programmer_chip_id": fmt.Sprintf("0x%04x", v.ChipID),
		"firmware": fmt.Sprintf("0x%04x", v.Firmware), "swdiv": float64(v.SWDiv), "addr_len": float64(v.AddrLen),
		"swbuf": hex.EncodeToString(v.SWBuf[:]), "power_pin": v.Power, "swire_mbit_s": v.BitRateMb,
		"baud": float64(d.o.baud), "layout": d.o.layout, "validated": false, "backend": "native-go-swire",
	}
	if d.entry.Sim == nil {
		res["validated"] = "read-only operations only (info, connect, memory/flash read, halt, resume); erase, program, step, breakpoints and memory write are unvalidated on hardware"
	}
	return res, nil
}

func (d *device) connectInfo() (map[string]any, error) {
	res, err := d.info()
	if err != nil {
		return nil, err
	}
	jedec, jerr := d.prog.JEDEC()
	if jerr == nil {
		res["flash_jedec"] = fmt.Sprintf("0x%06x", jedec)
		res["flash_kib"] = float64((uint32(1) << (jedec & 0xff)) >> 10)
		res["flash_size_code_plausible"] = target.FlashSize(jedec) != 0
		if uid, err := d.prog.FlashUID(); err == nil {
			res["flash_uid"] = hex.EncodeToString(uid)
		} else {
			res["flash_uid_error"] = err.Error()
		}
	} else {
		res["flash_error"] = jerr.Error()
	}
	rev, id, err := d.prog.ReadChipID()
	if err != nil {
		res["target"] = "not answering over SWire"
		res["target_error"] = err.Error()
		res["hint"] = "run attach to reset the target and activate SWire"
		return res, nil
	}
	res["target"] = "answering"
	res["target_chip_id"] = fmt.Sprintf("0x%04x", id)
	res["target_chip"] = sws.ChipName(id)
	res["target_revision"] = fmt.Sprintf("0x%02x", rev)
	ident := target.Identify(id, rev, jedec)
	res["target_family"], res["target_variant"] = ident.Family, ident.Variant
	if f := target.Lookup(id); f != nil {
		res["target_variant_note"] = f.VariantNote
	}
	prot, perr := d.protection()
	res["capabilities"] = capabilityState(prot, perr)
	if pc, err := d.prog.PC(); err == nil {
		res["pc"] = hex32(pc)
	}
	return res, nil
}

func (d *device) attach(a map[string]any) (map[string]any, error) {
	mode := "hard"
	if v, ok := a["mode"]; ok && v != nil {
		if s, isStr := v.(string); !isStr || (s != "hard" && s != "soft") {
			return nil, argErr(fmt.Errorf("mode %v: want hard or soft", v))
		}
		mode = a["mode"].(string)
	}
	resetMs, err := msArg(a, "reset_ms", defaultResetMs, 5, 5000)
	if err != nil {
		return nil, argErr(err)
	}
	actMs, err := msArg(a, "activate_ms", defaultActivateMs, 10, 5000)
	if err != nil {
		return nil, argErr(err)
	}
	halt, err := optBool(a, "halt")
	if err != nil {
		return nil, argErr(err)
	}
	if mode == "hard" {
		if err := d.prog.SetReset(false); err != nil {
			return nil, fmt.Errorf("hold RST low: %w", err)
		}
		time.Sleep(time.Duration(resetMs) * time.Millisecond)
	} else if err := d.prog.SoftReboot(); err != nil {
		return nil, fmt.Errorf("soft reboot (the target must already answer over SWire; use mode hard otherwise): %w", err)
	}
	pc, err := d.prog.Activate(actMs)
	if err != nil {
		return nil, fmt.Errorf("activate: %w", err)
	}
	res := map[string]any{"mode": mode, "activation_pc": hex32(pc), "reset_ms": float64(resetMs), "activate_ms": float64(actMs)}
	rev, id, err := d.prog.ReadChipID()
	if err != nil {
		return nil, fmt.Errorf("target activated but does not report a chip id: %w", err)
	}
	res["target_chip_id"], res["target_chip"], res["target_revision"] = fmt.Sprintf("0x%04x", id), sws.ChipName(id), fmt.Sprintf("0x%02x", rev)
	if halt {
		if err := d.prog.Stall(); err != nil {
			return nil, err
		}
	}
	return d.merge(res), nil
}

func (d *device) merge(res map[string]any) map[string]any {
	for k, v := range d.status() {
		res[k] = v
	}
	return res
}

// captured reports whether the control byte says the CPU is halted by us.
func captured(ctl byte) bool { return ctl == 0x05 || ctl == 0x06 }

func (d *device) status() map[string]any {
	ctl, err := d.prog.CPUControl()
	if err != nil {
		return map[string]any{
			"reachable": false, "error": err.Error(), "halted": false,
			"hint": "the target does not answer over SWire: it may be asleep, not activated or powered off; run attach",
		}
	}
	res := map[string]any{"reachable": true, "cpu_control": fmt.Sprintf("0x%02x", ctl), "halted": captured(ctl)}
	pc1, err := d.prog.PC()
	if err != nil {
		res["pc_error"] = err.Error()
		return res
	}
	pc2, err := d.prog.PC()
	res["pc"] = hex32(pc1)
	if err == nil {
		res["pc_stable"] = pc1 == pc2
	}
	return res
}

func (d *device) halt(a map[string]any) (map[string]any, error) {
	mode := "stall"
	if v, ok := a["mode"]; ok && v != nil {
		if s, isStr := v.(string); !isStr || (s != "stall" && s != "stop") {
			return nil, argErr(fmt.Errorf("mode %v: want stall or stop", v))
		}
		mode = a["mode"].(string)
	}
	var err error
	if mode == "stop" {
		err = d.prog.Stop()
	} else {
		err = d.prog.Stall()
	}
	if err != nil {
		return nil, err
	}
	return d.merge(map[string]any{"mode": mode}), nil
}

func (d *device) resume() (map[string]any, error) {
	if err := d.prog.Go(); err != nil {
		return nil, err
	}
	return d.status(), nil
}

func (d *device) requireCaptured() error {
	ctl, err := d.prog.CPUControl()
	if err != nil {
		return err
	}
	if !captured(ctl) {
		return fmt.Errorf("the CPU is not halted (control 0x%02x): halt it first", ctl)
	}
	return nil
}

func (d *device) step(a map[string]any) (map[string]any, error) {
	n, err := msArg(a, "count", 1, 1, maxSteps)
	if err != nil {
		return nil, argErr(err)
	}
	if err := d.requireCaptured(); err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		if err := d.prog.StepOne(); err != nil {
			return nil, fmt.Errorf("step %d of %d: %w", i+1, n, err)
		}
	}
	return d.merge(map[string]any{"steps": float64(n)}), nil
}

func (d *device) reset(a map[string]any) (map[string]any, error) {
	kind := "hard"
	if v, ok := a["kind"]; ok && v != nil {
		if s, isStr := v.(string); !isStr || (s != "hard" && s != "soft") {
			return nil, argErr(fmt.Errorf("kind %v: want hard or soft", v))
		}
		kind = a["kind"].(string)
	}
	resetMs, err := msArg(a, "reset_ms", defaultResetMs, 5, 5000)
	if err != nil {
		return nil, argErr(err)
	}
	if kind == "soft" {
		if err := d.prog.SoftReboot(); err != nil {
			return nil, fmt.Errorf("soft reboot: %w", err)
		}
		return map[string]any{"kind": kind, "note": "the target restarted; SWire is inactive until attach"}, nil
	}
	if err := d.prog.SetReset(false); err != nil {
		return nil, fmt.Errorf("hold RST low: %w", err)
	}
	time.Sleep(time.Duration(resetMs) * time.Millisecond)
	if err := d.prog.SetReset(true); err != nil {
		return nil, fmt.Errorf("release RST: %w", err)
	}
	time.Sleep(releaseWait)
	return map[string]any{"kind": kind, "reset_ms": float64(resetMs), "note": "the target is running; SWire is inactive until attach"}, nil
}

func (d *device) readRegisters() (map[string]any, error) {
	ctl, err := d.prog.CPUControl()
	if err != nil {
		return nil, err
	}
	r, err := d.prog.ReadRegisters()
	if err != nil {
		return nil, err
	}
	names := [16]string{"r0", "r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8", "r9", "r10", "fp", "ip", "sp", "lr", "pc"}
	regs := map[string]any{"psr": hex32(r.PSR)}
	for i, n := range names {
		regs[n] = hex32(r.R[i])
	}
	res := map[string]any{"registers": regs, "halted": captured(ctl), "pc": hex32(r.R[15])}
	if !captured(ctl) {
		res["warning"] = "the CPU is running; the register block is an arbitrary instant"
	}
	return res, nil
}

func (d *device) setBreakpoint(a map[string]any) (map[string]any, error) {
	addr, err := addr24(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	wait, err := msArg(a, "wait_ms", 0, 0, 30000)
	if err != nil {
		return nil, argErr(err)
	}
	if err := d.prog.SetBreakpoint(addr); err != nil {
		return nil, err
	}
	res := map[string]any{"breakpoint": hex24(addr)}
	if wait > 0 {
		pc, hit, err := d.prog.WaitPC(addr, time.Duration(wait)*time.Millisecond)
		if err != nil {
			return nil, err
		}
		res["hit"], res["pc"] = hit, hex32(pc)
	}
	return res, nil
}

func readResult(addr uint32, data []byte) map[string]any {
	sum := sha256.Sum256(data)
	res := map[string]any{"address": hex24(addr), "length": float64(len(data)), "sha256": hex.EncodeToString(sum[:])}
	if len(data) <= inlineBytes {
		res["hex"] = hex.EncodeToString(data)
	} else {
		res["hex_preview"] = hex.EncodeToString(data[:previewBytes])
		res["truncated"] = true
		res["note"] = fmt.Sprintf("only the first %d bytes are shown; reads over %d bytes return a sha256", previewBytes, inlineBytes)
	}
	return res
}

func (d *device) readMemory(a map[string]any) (map[string]any, error) {
	addr, err := addr24(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	n, err := lengthArg(a, maxReadBytes)
	if err != nil {
		return nil, argErr(err)
	}
	if uint64(addr)+uint64(n) > swireTop {
		return nil, argErr(fmt.Errorf("0x%x+%d runs past the 24-bit SWire address space", addr, n))
	}
	data, err := d.prog.SWRead(addr, n)
	if err != nil {
		return nil, err
	}
	return readResult(addr, data), nil
}

func (d *device) writeMemory(a map[string]any) (map[string]any, error) {
	addr, err := addr24(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	data, err := hexData(a, "data", maxWriteBytes)
	if err != nil {
		return nil, argErr(err)
	}
	allowRegs, err := optBool(a, "allow_registers")
	if err != nil {
		return nil, argErr(err)
	}
	sramStart, sramEnd, err := d.sramWindow()
	if err != nil {
		return nil, err
	}
	inSRAM := addr >= sramStart && uint64(addr)+uint64(len(data)) <= uint64(sramEnd)
	if !inSRAM && !allowRegs {
		return nil, status.Errorf(codes.PermissionDenied, "0x%06x+%d is outside SRAM (0x%06x-0x%06x); writes to peripheral registers and the CPU debug block need allow_registers=true", addr, len(data), sramStart, sramEnd-1)
	}
	verify := inSRAM
	if v, ok := a["verify"]; ok && v != nil {
		if verify, err = optBool(a, "verify"); err != nil {
			return nil, argErr(err)
		}
	}
	if err := d.prog.SWWrite(addr, data); err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex24(addr), "length": float64(len(data)), "verified": false}
	if verify {
		back, err := d.prog.SWRead(addr, len(data))
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(back, data) {
			return nil, status.Errorf(codes.DataLoss, "write verify failed at 0x%06x: read back %s", addr, hex.EncodeToString(back))
		}
		res["verified"] = true
	}
	return res, nil
}

// regionsTouched names the regions of the verified layout that a flash range
// overlaps; with no verified map it says so instead of guessing.
func (d *device) regionsTouched(prot target.Protection, addr uint32, n int) []any {
	if !prot.Verified {
		return []any{"unverified: no flash protection map for this target"}
	}
	var out []any
	for _, r := range prot.Layout.Regions {
		if r.Start < addr+uint32(n) && r.End > addr {
			out = append(out, r.Name+" ("+string(r.Class)+")")
		}
	}
	return out
}

func (d *device) flashRead(a map[string]any) (map[string]any, error) {
	addr, err := addr24(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	path, _ := a["path"].(string)
	size, _, err := d.flashBytes()
	if err != nil {
		return nil, err
	}
	max := maxReadBytes
	if path != "" {
		max = int(size)
	}
	n, err := lengthArg(a, max)
	if err != nil {
		return nil, argErr(err)
	}
	if uint64(addr)+uint64(n) > uint64(size) {
		return nil, argErr(fmt.Errorf("0x%x+%d is past the end of the %d KiB flash", addr, n, size>>10))
	}
	data, err := d.prog.FlashRead(addr, n)
	if err != nil {
		return nil, err
	}
	res := readResult(addr, data)
	prot, _ := d.protection()
	res["regions"] = d.regionsTouched(prot, addr, n)
	if path != "" {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return nil, argErr(fmt.Errorf("path: %w (existing files are never overwritten)", err))
		}
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, werr
		}
		res["path"] = path
		delete(res, "hex")
	}
	return res, nil
}

func (d *device) flashVerify(a map[string]any) (map[string]any, error) {
	addr, err := addr24(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	img, err := d.image(a)
	if err != nil {
		return nil, argErr(err)
	}
	size, _, err := d.flashBytes()
	if err != nil {
		return nil, err
	}
	if uint64(addr)+uint64(len(img)) > uint64(size) {
		return nil, argErr(fmt.Errorf("image does not fit: 0x%x+%d is past the end of the flash", addr, len(img)))
	}
	got, err := d.prog.FlashRead(addr, len(img))
	if err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex24(addr), "length": float64(len(img)), "match": bytes.Equal(got, img)}
	for i := range img {
		if got[i] != img[i] {
			res["first_difference"] = hex24(addr + uint32(i))
			break
		}
	}
	return res, nil
}

func (d *device) policy(a map[string]any) (sws.Policy, error) {
	j, err := optBool(a, "allow_journals")
	return sws.Policy{Journals: j}, err
}

func reportMap(r sws.FlashReport) map[string]any {
	return map[string]any{
		"erased_from": hex24(r.Start), "erased_to": hex24(r.End), "sectors_erased": float64(r.SectorsErased),
		"pages_written": float64(r.PagesWritten), "pages_skipped_blank": float64(r.PagesSkipped),
		"flash_protection_cleared": r.UnlockedFlash, "verified_bytes": float64(r.VerifiedBytes),
		"flash_jedec": fmt.Sprintf("0x%06x", r.JEDEC), "flash_kib": float64(r.FlashSizeBytes >> 10),
	}
}

func (d *device) refuse(prot target.Protection, addr uint32, n int, pol sws.Policy) error {
	if err := prot.Layout.CheckModify(addr, uint32(n), pol); err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return nil
}

func (d *device) flashErase(a map[string]any) (map[string]any, error) {
	addr, err := addr24(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	prot, err := d.modifiable("erase")
	if err != nil {
		return nil, err
	}
	n, err := lengthArg(a, int(prot.Layout.Size))
	if err != nil {
		return nil, argErr(err)
	}
	pol, err := d.policy(a)
	if err != nil {
		return nil, argErr(err)
	}
	if err := d.refuse(prot, addr, n, pol); err != nil {
		return nil, err
	}
	rep, err := d.prog.EraseRange(prot.Layout, addr, uint32(n), pol)
	if err != nil {
		return nil, fmt.Errorf("%w (done so far: %d sectors erased)", err, rep.SectorsErased)
	}
	return reportMap(rep), nil
}

func (d *device) flashProgram(a map[string]any) (map[string]any, error) {
	addr, err := addr24(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	img, err := d.image(a)
	if err != nil {
		return nil, argErr(err)
	}
	pol, err := d.policy(a)
	if err != nil {
		return nil, argErr(err)
	}
	if addr&(sws.FlashSector-1) != 0 {
		return nil, argErr(fmt.Errorf("address 0x%x is not 4 KiB aligned", addr))
	}
	prot, err := d.modifiable("program")
	if err != nil {
		return nil, err
	}
	if err := d.refuse(prot, addr, len(img), pol); err != nil {
		return nil, err
	}
	rep, err := d.prog.Program(prot.Layout, addr, img, pol)
	if err != nil {
		return nil, fmt.Errorf("%w (done so far: %d sectors erased, %d pages written; the flash may now hold a partial image)", err, rep.SectorsErased, rep.PagesWritten)
	}
	sum := sha256.Sum256(img)
	res := reportMap(rep)
	res["address"], res["length"], res["sha256"] = hex24(addr), float64(len(img)), hex.EncodeToString(sum[:])
	return res, nil
}
