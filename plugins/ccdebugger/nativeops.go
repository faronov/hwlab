package ccdebugger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/ccdebugger/native"
)

// Backend names for the `backend` device config key.
const (
	backendAuto   = "auto"
	backendNative = "native"
	backendTool   = "cc-tool"
)

// Opener opens the native USB transport of one debugger.
type Opener func(native.Address) (native.Transport, error)

// devConfig is the parsed per-device configuration.
type devConfig struct {
	backend string
	target  string
	native  native.Config
}

// dangerousKeys are config keys that widen what a session may do to the
// target; the core asks for confirmation when a run sets them.
var dangerousKeys = []string{"access", "allow_cpu_control", "allow_target_reset", "allow_memory_access",
	"allow_memory_write", "allow_breakpoints", "allow_dma_enable",
	"allow_flash_erase", "allow_flash_program", "allow_chip_erase", "allow_config_write"}

func boolKey(cfg map[string]any, k string) (bool, error) {
	v, ok := cfg[k]
	if !ok {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, status.Errorf(codes.InvalidArgument, "%s must be true or false", k)
	}
	return b, nil
}

func parseConfig(cfg map[string]any) (devConfig, error) {
	dc := devConfig{backend: backendAuto}
	if v, ok := cfg["backend"]; ok {
		s, _ := v.(string)
		switch s {
		case backendAuto, backendNative, backendTool:
			dc.backend = s
		default:
			return dc, status.Errorf(codes.InvalidArgument, "backend %v: want auto, native or cc-tool", v)
		}
	}
	if v, ok := cfg["target"]; ok {
		s, ok := v.(string)
		if !ok {
			return dc, status.Error(codes.InvalidArgument, "target must be a string (e.g. CC2530)")
		}
		dc.target = s
	}
	access := ""
	if v, ok := cfg["access"]; ok {
		if access, ok = v.(string); !ok {
			return dc, status.Error(codes.InvalidArgument, "access must be a string")
		}
	}
	a, err := native.ParseAccess(access)
	if err != nil {
		return dc, status.Error(codes.InvalidArgument, err.Error())
	}
	dc.native = native.Config{Access: a, Timeout: 10 * time.Second}
	if v, ok := cfg["timeout_ms"]; ok {
		ms, ok := v.(float64)
		if !ok || ms < 1 || ms > 60000 || ms != math.Trunc(ms) {
			return dc, status.Error(codes.InvalidArgument, "timeout_ms must be an integer between 1 and 60000")
		}
		dc.native.Timeout = time.Duration(ms) * time.Millisecond
	}
	for k, dst := range map[string]*bool{
		"allow_cpu_control": &dc.native.Permissions.CPUControl, "allow_target_reset": &dc.native.Permissions.TargetReset,
		"allow_memory_access": &dc.native.Permissions.MemoryAccess, "allow_memory_write": &dc.native.Permissions.MemoryWrite,
		"allow_breakpoints": &dc.native.Permissions.Breakpoints, "allow_dma_enable": &dc.native.Permissions.DMAEnable,
		"allow_flash_erase": &dc.native.Permissions.FlashErase, "allow_flash_program": &dc.native.Permissions.FlashProgram,
		"allow_chip_erase": &dc.native.Permissions.ChipErase, "allow_config_write": &dc.native.Permissions.ConfigWrite,
	} {
		if *dst, err = boolKey(cfg, k); err != nil {
			return dc, err
		}
	}
	if err := dc.native.Validate(); err != nil {
		return dc, status.Error(codes.InvalidArgument, err.Error())
	}
	return dc, nil
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, native.ErrPolicy):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, native.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, native.ErrVerify):
		return status.Error(codes.DataLoss, err.Error())
	case errors.Is(err, native.ErrTimeout):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, native.ErrTargetLocked):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Unavailable, err.Error())
	}
}

func hexU(v uint64) string { return fmt.Sprintf("0x%X", v) }

func intArg(args map[string]any, key string, required bool) (int, error) {
	v, ok := args[key]
	if !ok {
		if required {
			return 0, status.Errorf(codes.InvalidArgument, "%s is required", key)
		}
		return 0, nil
	}
	switch x := v.(type) {
	case float64:
		if x != math.Trunc(x) || x < 0 || x > 1<<30 {
			break
		}
		return int(x), nil
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 0, 32); err == nil && n >= 0 {
			return int(n), nil
		}
	}
	return 0, status.Errorf(codes.InvalidArgument, "%s must be a non-negative integer (decimal or 0x hex)", key)
}

// nativeSession returns the open native debugger of a device, opening it
// lazily. It must be called with the device lock held.
func (p *Plugin) nativeSession(id string) (*native.Debugger, error) {
	if p.open == nil {
		return nil, status.Error(codes.FailedPrecondition, "the native CC-Debugger backend is not available in this build or on this OS; use the cc-tool backend")
	}
	p.mu.Lock()
	dc, ok := p.conf[id]
	sess := p.sess[id]
	p.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "device %q is not open", id)
	}
	if dc.backend == backendTool {
		return nil, status.Error(codes.FailedPrecondition, "this device is configured with backend=cc-tool, which has no native debug operations")
	}
	if sess != nil {
		return sess, nil
	}
	all := scanSysfs(p.sysfs)
	var found *Debugger
	for i, d := range all {
		if deviceID(d, all) == id {
			found = &all[i]
		}
	}
	if found == nil {
		return nil, status.Errorf(codes.NotFound, "CC-Debugger %q is not connected", id)
	}
	if found.PID != "16a2" {
		return nil, status.Errorf(codes.FailedPrecondition, "%s (%s:%s) is not supported by the native backend, which implements the CC-Debugger 0451:16a2 only; use backend=cc-tool", found.Product, TIVID, found.PID)
	}
	b, n, _ := strings.Cut(found.Address, ":")
	bus, _ := strconv.Atoi(b)
	dev, _ := strconv.Atoi(n)
	tr, err := p.open(native.Address{Bus: bus, Address: dev})
	if err != nil {
		return nil, mapErr(err)
	}
	d, err := native.New(tr, dc.native)
	if err != nil {
		return nil, errors.Join(mapErr(err), tr.Close())
	}
	p.mu.Lock()
	p.sess[id] = d
	p.mu.Unlock()
	return d, nil
}

// dropSession closes the native session so cc-tool can claim the interface.
func (p *Plugin) dropSession(id string) {
	p.mu.Lock()
	d := p.sess[id]
	delete(p.sess, id)
	p.mu.Unlock()
	if d != nil {
		if err := d.Close(); err != nil {
			slog.Warn("close native CC-Debugger session", "err", err)
		}
	}
}

func statusMap(s native.Status) map[string]any { return s.Decode() }

func controlResult(r native.CPUControlResult) map[string]any {
	return map[string]any{"status_before": statusMap(r.StatusBefore), "status_after": statusMap(r.StatusAfter), "command_sent": r.CommandSent}
}

func (p *Plugin) nativeInfo(id string) (map[string]any, error) {
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	st, err := d.ReadAdapterState()
	if err != nil {
		return nil, mapErr(err)
	}
	target := ""
	if st.TargetID == 0x2530 {
		target = "CC2530"
	}
	cfg := d.Config()
	perms := map[string]any{
		"cpu_control": cfg.Permissions.CPUControl, "target_reset": cfg.Permissions.TargetReset,
		"memory_access": cfg.Permissions.MemoryAccess, "memory_write": cfg.Permissions.MemoryWrite,
		"breakpoints": cfg.Permissions.Breakpoints, "dma_enable": cfg.Permissions.DMAEnable,
		"flash_erase": cfg.Permissions.FlashErase, "flash_program": cfg.Permissions.FlashProgram,
		"chip_erase": cfg.Permissions.ChipErase, "config_write": cfg.Permissions.ConfigWrite,
	}
	_, tool := p.tooling()
	res := map[string]any{
		"backend": backendNative, "programmer": "CC Debugger", "target": target,
		"adapter": map[string]any{"target_id": hexU(uint64(st.TargetID)), "firmware_version": hexU(uint64(st.FirmwareVersion)), "firmware_revision": hexU(uint64(st.FirmwareRev))},
		"session": map[string]any{"access": string(cfg.Access), "permissions": perms, "attached": d.Attached(), "state": d.State().String()},
		"capabilities": []any{"info", "status", "attach", "reset_halt", "halt", "resume", "step", "read_registers", "read_sfr",
			"set_breakpoint", "wait_halted", "enable_dma", "memory_read", "memory_write(xdata)",
			"chip_info", "read_lock", "read_ieee", "read_mac", "read_info_page", "flash_read", "verify"},
		// Implemented, simulator-tested, and deliberately NOT yet run on real hardware.
		"implemented_hardware_unverified": []any{"chip_erase", "erase_pages", "program", "write_config", "crc_verify"},
		"unsupported":                     []any{"rtt"},
		"cc_tool":                         tool,
	}
	if g, ok := d.Geometry(); ok {
		res["chip"] = geometryMap(g)
	} else {
		res["chip_note"] = "run chip_info on an attached, halted target to identify it"
	}
	return res, nil
}

func (p *Plugin) nativeStatus(id string) (map[string]any, error) {
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	st, err := d.ReadDebugStatus()
	if err != nil {
		return nil, mapErr(err)
	}
	cfg, err := d.ReadDebugConfig()
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"status": statusMap(st), "debug_config": native.DecodeConfig(cfg)}, nil
}

func (p *Plugin) nativeAttach(id string) (map[string]any, error) {
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	r, err := d.AttachReset()
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"status_after": statusMap(r.StatusAfter), "reset_sent": r.ResetSent, "halted": true}, nil
}

func (p *Plugin) nativeControl(id, op string) (map[string]any, error) {
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	switch op {
	case "halt":
		r, err := d.Halt()
		return controlResult(r), mapErr(err)
	case "resume":
		r, err := d.Resume()
		return controlResult(r), mapErr(err)
	case "reset_halt":
		r, err := d.ResetHalt()
		return controlResult(r), mapErr(err)
	case "step":
		r, err := d.Step()
		return map[string]any{"status_before": statusMap(r.StatusBefore), "status_after": statusMap(r.StatusAfter), "accumulator": float64(r.Accumulator)}, mapErr(err)
	case "enable_dma":
		cfg, err := d.EnableDMAAfterReset()
		return map[string]any{"debug_config": native.DecodeConfig(cfg)}, mapErr(err)
	}
	return nil, status.Errorf(codes.Unimplemented, "unsupported operation %s", op)
}

func (p *Plugin) nativeRegisters(id string) (map[string]any, error) {
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	r, err := d.ReadRegisters()
	if err != nil {
		return nil, mapErr(err)
	}
	rs := make([]any, len(r.R))
	for i, v := range r.R {
		rs[i] = float64(v)
	}
	return map[string]any{
		"pc": hexU(uint64(r.PC)), "bank": float64(r.Bank), "a": float64(r.A), "b": float64(r.B), "sp": float64(r.SP),
		"psw": float64(r.PSW), "dptr0": hexU(uint64(r.DPTR0)), "dptr1": hexU(uint64(r.DPTR1)),
		"dps": float64(r.DPS), "mpage": float64(r.MPAGE), "r": rs,
	}, nil
}

func (p *Plugin) nativeSFR(id string, args map[string]any) (map[string]any, error) {
	addr, err := intArg(args, "address", true)
	if err != nil {
		return nil, err
	}
	if addr > 0xFF {
		return nil, status.Error(codes.InvalidArgument, "address must be a direct SFR address 0x80..0xFF")
	}
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	v, err := d.ReadSFR(uint8(addr))
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"address": hexU(uint64(addr)), "value": float64(v)}, nil
}

func (p *Plugin) nativeBreakpoint(id string, args map[string]any) (map[string]any, error) {
	slot, err := intArg(args, "slot", true)
	if err != nil {
		return nil, err
	}
	addr, err := intArg(args, "address", true)
	if err != nil {
		return nil, err
	}
	enabled := true
	if v, ok := args["enabled"].(bool); ok {
		enabled = v
	}
	if addr > 0xFFFF {
		return nil, status.Error(codes.InvalidArgument, "address must be below 0x8000")
	}
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	r, err := d.SetBreakpoint(slot, uint16(addr), enabled)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"slot": float64(r.Slot), "address": hexU(uint64(r.Address)), "enabled": r.Enabled, "status_after": statusMap(r.StatusAfter)}, nil
}

func (p *Plugin) nativeWait(id string, args map[string]any) (map[string]any, error) {
	ms, err := intArg(args, "timeout_ms", false)
	if err != nil {
		return nil, err
	}
	if ms == 0 {
		ms = 5000
	}
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	st, pc, err := d.WaitHalted(time.Duration(ms) * time.Millisecond)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"status": statusMap(st), "pc": hexU(uint64(pc))}, nil
}

// Memory spaces of MemoryAccess.
const (
	maxInlineBytes = 1024
	chunk          = 256
)

func spaceLimit(space string) (int, bool) {
	switch space {
	case "xdata":
		return 0x2000, true
	case "code":
		return 0x8000, true
	case "flash":
		return native.FlashSize, true
	}
	return 0, false
}

func (p *Plugin) nativeMemRead(id string, args map[string]any) (map[string]any, error) {
	space, _ := args["space"].(string)
	if space == "" {
		space = "code"
	}
	limit, ok := spaceLimit(space)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "space %q: want xdata, code or flash", space)
	}
	addr, err := intArg(args, "address", true)
	if err != nil {
		return nil, err
	}
	length, err := intArg(args, "length", true)
	if err != nil {
		return nil, err
	}
	path := ""
	maxLen := maxInlineBytes
	if _, ok := args["path"]; ok {
		if path, err = pathArg(args); err != nil {
			return nil, err
		}
		maxLen = limit
		if ow, _ := args["overwrite"].(bool); !ow {
			if _, err := os.Stat(path); err == nil {
				return nil, status.Errorf(codes.AlreadyExists, "%s exists; pass overwrite=true to replace it", path)
			}
		}
	}
	if length < 1 || length > maxLen || addr+length > limit {
		return nil, status.Errorf(codes.InvalidArgument, "length must be 1..%d and stay inside the %s space (0..0x%X)", maxLen, space, limit-1)
	}
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, length)
	for off := 0; off < length; {
		n := min(chunk, length-off)
		a := addr + off
		if space == "flash" { // a physical read must not cross a 32 KiB bank
			n = min(n, 0x8000-a%0x8000)
		}
		var b []byte
		switch space {
		case "xdata":
			b, err = d.ReadXDATA(a, n)
		case "code":
			b, err = d.ReadCode(a, n)
		default:
			b, err = d.ReadFlashCode(a, n)
		}
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, b...)
		off += n
	}
	sum := sha256.Sum256(out)
	res := map[string]any{"space": space, "address": hexU(uint64(addr)), "length": float64(length), "sha256": hex.EncodeToString(sum[:])}
	if path == "" {
		res["data"] = hex.EncodeToString(out)
		return res, nil
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return nil, status.Errorf(codes.Internal, "write %s: %v", path, err)
	}
	res["path"] = path
	return res, nil
}

func (p *Plugin) nativeMemWrite(id string, args map[string]any) (map[string]any, error) {
	if s, _ := args["space"].(string); s != "" && s != "xdata" {
		return nil, status.Error(codes.InvalidArgument, "only space=xdata (RAM below 0x1E00) can be written; flash is never written natively")
	}
	addr, err := intArg(args, "address", true)
	if err != nil {
		return nil, err
	}
	hexData, _ := args["data"].(string)
	data, err := hex.DecodeString(strings.ReplaceAll(strings.TrimPrefix(hexData, "0x"), " ", ""))
	if err != nil || len(data) == 0 || len(data) > maxInlineBytes {
		return nil, status.Errorf(codes.InvalidArgument, "data must be a hex string of 1..%d bytes", maxInlineBytes)
	}
	if addr+len(data) > native.WriteLimitXDATA {
		return nil, status.Errorf(codes.InvalidArgument, "the writable XDATA range is 0..0x%X", native.WriteLimitXDATA-1)
	}
	d, err := p.nativeSession(id)
	if err != nil {
		return nil, err
	}
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		if err := d.WriteXDATA(addr+off, data[off:end]); err != nil {
			return nil, mapErr(err)
		}
	}
	return map[string]any{"space": "xdata", "address": hexU(uint64(addr)), "length": float64(len(data)), "verified": true}, nil
}

// useNative reports whether an operation that both backends could serve goes
// to the native one.
func (p *Plugin) useNative(id string) (bool, error) {
	p.mu.Lock()
	dc, ok := p.conf[id]
	p.mu.Unlock()
	backend := backendAuto
	if ok {
		backend = dc.backend
	}
	switch backend {
	case backendTool:
		return false, nil
	case backendNative:
		if p.open == nil {
			return false, status.Error(codes.FailedPrecondition, "the native backend is not available on this OS")
		}
		return true, nil
	}
	return p.open != nil, nil
}
