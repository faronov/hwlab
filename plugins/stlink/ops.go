package stlinkplugin

import (
	"context"

	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/faronov/hwlab/internal/ownedtemp"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

const (
	maxStepCount  = 1000
	maxReadBytes  = 4096
	maxReadFile   = 16 << 20
	maxWriteBytes = 4096

	// sramLo..sramHi is the Arm SRAM region (without the bit-band alias).
	sramLo, sramHi = 0x20000000, 0x21FFFFFF
	// periph regions where a read can have side effects: peripherals, and the
	// private peripheral bus (reading DHCSR clears S_RESET_ST).
	periph1Lo, periph1Hi = 0x40000000, 0x5FFFFFFF
	periph2Lo, periph2Hi = 0xE0000000, 0xFFFFFFFF
)

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func denied(format string, a ...any) error {
	return status.Errorf(codes.PermissionDenied, format, a...)
}

func boolArg(args map[string]any, key string, def bool) (bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return def, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, argErr(fmt.Errorf("%s: want true or false", key))
	}
	return b, nil
}

func strArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", nil
	}
	str, isStr := v.(string)
	if !isStr {
		return "", argErr(fmt.Errorf("%s: want a string", key))
	}
	return str, nil
}

func intArg(args map[string]any, key string, def, lo, hi int) (int, error) {
	v, ok, err := optNumber(args, key)
	if err != nil {
		return 0, argErr(err)
	}
	if !ok {
		return def, nil
	}
	if v < float64(lo) || v > float64(hi) || v != math.Trunc(v) {
		return 0, argErr(fmt.Errorf("%s %v: want an integer %d..%d", key, v, lo, hi))
	}
	return int(v), nil
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
		return 0, errors.New(`address must be a number or a string like "0x20000000"`)
	}
	if u > math.MaxUint32 {
		return 0, fmt.Errorf("address 0x%x does not fit the 32-bit address space", u)
	}
	return uint32(u), nil
}

func hexw(v uint32) string { return fmt.Sprintf("0x%08x", v) }

// region names an Arm memory region for messages.
func region(addr uint32) string {
	switch {
	case addr < 0x20000000:
		return "code region (flash/ROM)"
	case addr <= sramHi:
		return "SRAM"
	case addr < 0x40000000:
		return "SRAM bit-band alias"
	case addr <= periph1Hi:
		return "peripheral region"
	case addr < 0xA0000000:
		return "external RAM/device region"
	case addr < 0xE0000000:
		return "external device region"
	}
	return "private peripheral bus / system region"
}

// vendorHint reads vendor identification registers. The result is board or
// vendor knowledge obtained from the target, never derived from the Arm core.
func (s *session) vendorHint(ctx context.Context) (string, uint32, *stm32.Profile) {
	if p, err := stm32.Detect(ctx, s.core); err == nil {
		return fmt.Sprintf("%s (DBGMCU_IDCODE dev 0x%03x, flash-size register %d KB)", p.Name, p.DevID, p.FlashKB), uint32(p.RevID)<<16 | uint32(p.DevID), p
	}
	part, err := s.core.Bus.Read32(ctx, 0x10000100)
	if err != nil {
		return "", 0, nil
	}
	names := map[uint32]string{0x52840: "nRF52840", 0x52833: "nRF52833", 0x52832: "nRF52832", 0x52820: "nRF52820",
		0x52811: "nRF52811", 0x52810: "nRF52810", 0x52805: "nRF52805"}
	if n, ok := names[part]; ok {
		return fmt.Sprintf("%s (FICR.INFO.PART 0x%08x)", n, part), part, nil
	}
	return "", 0, nil
}

// mark adds the evidence labels every target answer carries.
func (s *session) mark(res map[string]any) map[string]any {
	res["target_verified"] = s.entry.Sim == nil
	if s.entry.Sim != nil {
		res["synthetic"] = true
	}
	res["probe_serial"] = s.entry.Serial
	res["device"] = s.entry.ID
	return res
}

func stateName(st cortexm.State) string {
	switch {
	case st.Halted:
		return "halted"
	case st.Lockup:
		return "lockup"
	case st.Sleeping:
		return "sleeping"
	}
	return "running"
}

func (s *session) stateResult(ctx context.Context, st cortexm.State) map[string]any {
	res := map[string]any{"state": stateName(st), "halted": st.Halted, "dhcsr": hexw(st.DHCSR), "debug_enabled": st.DebugEnabled}
	if st.Halted {
		if pc, err := s.core.ReadRegister(ctx, cortexm.RegPC); err == nil {
			res["pc"] = hexw(pc)
		}
	}
	return res
}

func causeMap(res map[string]any, c cortexm.Cause) {
	res["dfsr"] = fmt.Sprintf("0x%02x", c.DFSR)
	names := make([]any, len(c.Names))
	for i, n := range c.Names {
		names[i] = n
	}
	res["cause"] = names
}

// ---- identity ----

func (s *session) chipInfo(ctx context.Context) (map[string]any, error) {
	id, err := s.core.ReadIdentity(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	hint, word, prof := s.vendorHint(ctx)
	s.chip = prof
	mv := 0
	if s.info.VoltageOK {
		mv = s.info.VoltageMV
	}
	core := map[string]any{
		"cpuid": hexw(id.CPUID), "implementer": fmt.Sprintf("0x%02x", id.Implementer), "part_number": fmt.Sprintf("0x%03x", id.PartNo),
		"variant": float64(id.Variant), "revision": float64(id.Revision), "architecture_field": fmt.Sprintf("0x%x", id.Architecture),
	}
	if id.Name != "" {
		core["name"] = id.Name
		core["revision_name"] = fmt.Sprintf("r%dp%d", id.Variant, id.Revision)
	}
	if id.ArchName != "" {
		core["architecture"] = id.ArchName
	}
	res := map[string]any{
		"basis": "probe identity, ADIv5 debug port and Cortex-M CPUID; the vendor line is read from vendor registers and is a hint, not derived from the core",
		"probe": s.probeFacts(),
		"debug_port": map[string]any{
			"transport": s.info.Transport, "dp_idcode": hexw(s.info.DPIDCode), "clock_khz": float64(s.actualKHz),
			"target_voltage_mv": float64(mv), "target_voltage_measured": s.info.VoltageOK,
		},
		"core":   core,
		"vendor": map[string]any{"hint": hint, "register_word": hexw(word), "recognised": hint != ""},
		"state":  s.stateResult(ctx, st),
	}
	if hint == "" {
		res["vendor"].(map[string]any)["note"] = "no recognised vendor identification register answered; the target is identified only as an Arm Cortex-M behind this debug port"
	}
	if prof != nil {
		res["stm32"] = map[string]any{"name": prof.Name, "family": string(prof.Family), "flash_kb": float64(prof.FlashKB), "flash_size_source": prof.FlashSizeSource, "dev_id": fmt.Sprintf("0x%03x", prof.DevID), "rev_id": fmt.Sprintf("0x%04x", prof.RevID), "programmable": prof.Programmable}
	}
	if f, err := s.core.DiscoverFPB(ctx); err == nil {
		res["breakpoint_unit"] = map[string]any{"kind": "FPB", "revision": float64(f.Info.Rev), "code_comparators": float64(f.Info.NumCode), "literal_comparators": float64(f.Info.NumLit)}
	} else if errors.Is(err, cortexm.ErrAbsent) {
		res["breakpoint_unit"] = map[string]any{"present": false, "reason": err.Error()}
	}
	demcr, derr := s.core.Bus.Read32(ctx, cortexm.AddrDEMCR)
	if d, err := s.core.DiscoverDWT(ctx); err == nil {
		res["watchpoint_unit"] = map[string]any{"kind": "DWT", "comparators": float64(d.NumComp)}
		if derr == nil && s.dwt == nil {
			_ = s.core.Bus.Write32(ctx, cortexm.AddrDEMCR, demcr)
		}
	} else if errors.Is(err, cortexm.ErrAbsent) {
		res["watchpoint_unit"] = map[string]any{"present": false, "reason": err.Error()}
	}
	return s.mark(res), nil
}

func (s *session) probeFacts() map[string]any {
	v := s.conn.Ver
	return map[string]any{
		"serial": s.entry.Serial, "vid": fmt.Sprintf("%04x", s.entry.Dev.VID), "pid": fmt.Sprintf("%04x", s.entry.Dev.PID),
		"firmware": v.Name, "generation": v.Generation, "stlink_v": float64(v.STLinkV), "jtag_v": float64(v.JTAGV), "swim_v": float64(v.SWIMV),
		"serial_descriptor_confirmed": s.serialDesc != "" && s.serialDesc == s.entry.Serial,
	}
}

// ---- state ----

func (s *session) halt(ctx context.Context) (map[string]any, error) {
	st, err := s.core.Halt(ctx)
	if err != nil {
		return nil, err
	}
	res := s.stateResult(ctx, st)
	if c, err := s.core.HaltCause(ctx); err == nil {
		causeMap(res, c)
	}
	res["note"] = "the core stays halted until DebugProbe.resume"
	return s.mark(res), nil
}

// stepOverBreakpoint steps the instruction at the current PC with its
// breakpoint temporarily removed, so a resume does not halt on the very
// instruction it starts at.
func (s *session) stepOverBreakpoint(ctx context.Context, pc uint32) (bool, error) {
	bp, has := s.bps[pc&^1]
	if !has || s.fpb == nil {
		return false, nil
	}
	if err := s.fpb.ClearSlot(ctx, bp.Slot); err != nil {
		return false, err
	}
	_, serr := s.core.Step(ctx, false)
	nb, rerr := s.fpb.Set(ctx, bp.Addr)
	if rerr == nil {
		s.bps[bp.Addr] = nb
	}
	if serr != nil {
		return true, serr
	}
	return true, rerr
}

func (s *session) resume(ctx context.Context) (map[string]any, error) {
	st, err := s.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	res := map[string]any{}
	if !st.Halted {
		res["resumed"] = false
		res["note"] = "the core was not halted; nothing to resume"
		for k, v := range s.stateResult(ctx, st) {
			res[k] = v
		}
		return s.mark(res), nil
	}
	pc, err := s.core.ReadRegister(ctx, cortexm.RegPC)
	if err != nil {
		return nil, err
	}
	if stepped, err := s.stepOverBreakpoint(ctx, pc); err != nil {
		return nil, err
	} else if stepped {
		res["stepped_over_breakpoint"] = hexw(pc)
	}
	_ = s.core.ClearCause(ctx)
	st, err = s.core.Resume(ctx)
	if err != nil {
		return nil, err
	}
	res["resumed"] = true
	if st.Halted {
		res["halted_again"] = true
		if c, err := s.core.HaltCause(ctx); err == nil {
			causeMap(res, c)
		}
	}
	for k, v := range s.stateResult(ctx, st) {
		res[k] = v
	}
	return s.mark(res), nil
}

func (s *session) waitHalt(ctx context.Context, args map[string]any) (map[string]any, error) {
	ms, err := intArg(args, "wait_ms", 2000, 1, 30000)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	for {
		st, err := s.core.Status(ctx)
		if err != nil {
			return nil, err
		}
		if st.Halted {
			res := s.stateResult(ctx, st)
			c, _ := s.core.HaltCause(ctx)
			causeMap(res, c)
			if pc, ok := res["pc"].(string); ok {
				a, _ := toAddr(pc)
				_, mine := s.bps[a&^1]
				res["breakpoint_hit"] = c.Has("breakpoint") && mine
			}
			res["watchpoint_hit"] = c.Has("watchpoint")
			return s.mark(res), nil
		}
		if time.Now().After(deadline) {
			return nil, status.Errorf(codes.DeadlineExceeded, "the core did not halt within %d ms (it is %s)", ms, stateName(st))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (s *session) step(ctx context.Context, args map[string]any) (map[string]any, error) {
	n, err := intArg(args, "count", 1, 1, maxStepCount)
	if err != nil {
		return nil, err
	}
	mask, err := boolArg(args, "mask_interrupts", false)
	if err != nil {
		return nil, err
	}
	st, err := s.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	if !st.Halted {
		return nil, status.Errorf(codes.FailedPrecondition, "the core is %s: halt it first (DebugProbe.halt); stepping never halts implicitly", stateName(st))
	}
	var steps []any
	var last cortexm.StepResult
	moved := 0
	for i := 0; i < n; i++ {
		pc, err := s.core.ReadRegister(ctx, cortexm.RegPC)
		if err != nil {
			return nil, err
		}
		var r cortexm.StepResult
		if bp, has := s.bps[pc&^1]; has && s.fpb != nil {
			if err = s.fpb.ClearSlot(ctx, bp.Slot); err != nil {
				return nil, err
			}
			r, err = s.core.Step(ctx, mask)
			if nb, rerr := s.fpb.Set(ctx, bp.Addr); rerr == nil {
				s.bps[bp.Addr] = nb
			} else if err == nil {
				err = rerr
			}
		} else {
			r, err = s.core.Step(ctx, mask)
		}
		if err != nil {
			return nil, fmt.Errorf("step %d of %d: %w", i+1, n, err)
		}
		last = r
		if r.PCAfter != r.PCBefore {
			moved++
		}
		if len(steps) < 64 {
			steps = append(steps, map[string]any{"pc_before": hexw(r.PCBefore), "pc_after": hexw(r.PCAfter), "pc_delta": float64(int64(int32(r.PCAfter - r.PCBefore)))})
		}
	}
	after, err := s.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	if after.Stepping || after.MaskInts {
		return nil, fmt.Errorf("DHCSR.C_STEP/C_MASKINTS is still set after stepping (DHCSR 0x%08x)", after.DHCSR)
	}
	res := map[string]any{
		"count": float64(n), "moved": float64(moved), "steps": steps, "pc": hexw(last.PCAfter), "halted": after.Halted,
		"step_bit_restored": !after.Stepping, "mask_interrupts": mask,
		"basis": "DHCSR.C_STEP for each step, confirmed by S_HALT and DFSR.HALTED; C_STEP and C_MASKINTS were cleared again afterwards",
	}
	causeMap(res, last.Cause)
	if n > len(steps) {
		res["steps_truncated"] = float64(n - len(steps))
	}
	return s.mark(res), nil
}

var regLayout = []string{"r0", "r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8", "r9", "r10", "r11", "r12"}

func (s *session) registers(ctx context.Context, args map[string]any) (res map[string]any, err error) {
	keep, err := boolArg(args, "keep_halted", false)
	if err != nil {
		return nil, err
	}
	cross, err := boolArg(args, "crosscheck", false)
	if err != nil {
		return nil, err
	}
	st, err := s.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	wasRunning := !st.Halted
	if wasRunning {
		if _, err := s.core.Halt(ctx); err != nil {
			return nil, err
		}
	}
	defer func() {
		if wasRunning && !keep {
			if _, rerr := s.core.Resume(context.WithoutCancel(ctx)); rerr != nil && err == nil {
				err = fmt.Errorf("registers were read but the core did not resume: %w", rerr)
			} else if rerr == nil && res != nil {
				res["resumed"] = true
			}
		}
	}()
	r, err := s.core.ReadAll(ctx)
	if err != nil {
		return nil, err
	}
	regs := map[string]any{}
	for i, n := range regLayout {
		regs[n] = hexw(r.R[i])
	}
	regs["sp"], regs["lr"], regs["pc"], regs["xpsr"] = hexw(r.SP), hexw(r.LR), hexw(r.PC), hexw(r.XPSR)
	regs["msp"], regs["psp"] = hexw(r.MSP), hexw(r.PSP)
	regs["control"] = fmt.Sprintf("0x%02x", r.Control())
	regs["faultmask"], regs["basepri"], regs["primask"] = fmt.Sprintf("0x%02x", r.Faultmask()), fmt.Sprintf("0x%02x", r.Basepri()), fmt.Sprintf("0x%02x", r.Primask())
	res = map[string]any{"registers": regs, "was_running": wasRunning, "halted": !wasRunning || keep,
		"basis": "native READALLREGS plus DCRSR for the special registers; read-only"}
	if cross {
		pc, err := s.core.ReadRegisterDCRSR(ctx, cortexm.RegPC)
		if err != nil {
			return nil, err
		}
		res["pc_via_dcrsr"] = hexw(pc)
		res["native_matches_dcrsr"] = pc == r.PC
	}
	return s.mark(res), nil
}

func (s *session) writeRegister(ctx context.Context, args map[string]any) (map[string]any, error) {
	if !s.o.perm.memWrite {
		return nil, denied("writing core registers needs allow_memory_write=true in the device configuration")
	}
	name, err := strArg(args, "name")
	if err != nil {
		return nil, err
	}
	id, ok := cortexm.ParseRegister(name)
	if !ok {
		return nil, argErr(fmt.Errorf("register %q: want r0..r12, sp, lr, pc, xpsr, msp, psp or special", name))
	}
	v, err := toAddr(args["value"])
	if err != nil {
		return nil, argErr(fmt.Errorf("value: %w", err))
	}
	st, err := s.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	if !st.Halted {
		return nil, status.Errorf(codes.FailedPrecondition, "the core is %s: registers can only be written while halted", stateName(st))
	}
	prev, err := s.core.ReadRegister(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.core.WriteRegister(ctx, id, v); err != nil {
		return nil, err
	}
	return s.mark(map[string]any{"register": cortexm.RegisterName(id), "previous": hexw(prev), "value": hexw(v), "verified": true}), nil
}

func (s *session) harts(ctx context.Context) (map[string]any, error) {
	st, err := s.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	id, err := s.core.ReadIdentity(ctx)
	if err != nil {
		return nil, err
	}
	h := map[string]any{"hart": float64(0), "state": stateName(st), "xlen": float64(32), "cpuid": hexw(id.CPUID), "core": id.Name}
	return s.mark(map[string]any{
		"count": float64(1), "harts": []any{h},
		"note": "a Cortex-M debug port exposes exactly one core; multi-core parts show one AP per core, which this backend does not enumerate",
	}), nil
}

// ---- reset ----

func (s *session) reset(ctx context.Context, args map[string]any) (map[string]any, error) {
	kind, err := strArg(args, "kind")
	if err != nil {
		return nil, err
	}
	if kind == "" {
		kind = "system"
	}
	halt, err := boolArg(args, "halt", false)
	if err != nil {
		return nil, err
	}
	if kind != "system" && kind != "core" && kind != "pin" {
		return nil, argErr(fmt.Errorf("kind %q: want system, core or pin", kind))
	}
	if !s.o.perm.reset {
		return nil, denied("reset needs allow_reset=true in the device configuration")
	}
	if kind == "pin" {
		if tok, _ := strArg(args, "confirm"); tok != "reset-pin" {
			return nil, denied(`driving the NRST pin resets the whole target: pass confirm="reset-pin"`)
		}
	}
	if err := s.dropTriggers(ctx); err != nil {
		return nil, fmt.Errorf("debug trigger cleanup before reset: %w", err)
	}
	start := time.Now()
	res := map[string]any{"kind": kind, "halt": halt}
	var obs map[string]any
	if kind == "pin" {
		obs, err = s.pinReset(ctx, halt)
	} else {
		var rr cortexm.ResetResult
		rr, err = s.core.Reset(ctx, cortexm.ResetKind(kind), halt)
		obs = map[string]any{"reset_seen": rr.ResetSeen, "debug_kept": rr.DebugKept, "halted": rr.Halted, "pre_reset_vector": hexw(rr.PreResetVector), "at_reset_vector": rr.AtVector}
		if rr.Halted {
			// The vector is only read after the reset when the core was halted at it.
			obs["pc"], obs["reset_vector"] = hexw(rr.PC), hexw(rr.ResetVector)
		}
	}
	for k, v := range obs {
		res[k] = v
	}
	res["elapsed_ms"] = float64(time.Since(start).Milliseconds())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "reset %s: %v", kind, err)
	}
	res["reset_observed"] = true
	// Breakpoint comparators may or may not survive: report what is there.
	forgot := 0
	if s.fpb != nil {
		live, lerr := s.fpb.List(ctx)
		if lerr == nil {
			have := map[uint32]bool{}
			for _, b := range live {
				have[b.Addr] = true
			}
			for a := range s.bps {
				if !have[a] {
					delete(s.bps, a)
					forgot++
				}
			}
			res["breakpoints_active_after_reset"] = float64(len(live))
		}
	}
	res["breakpoints_forgotten"] = float64(forgot)
	return s.mark(res), nil
}

// pinReset drives NRST and requires evidence that a reset happened. The probe
// cannot know whether NRST is wired, so an unobserved reset is an error.
func (s *session) pinReset(ctx context.Context, halt bool) (map[string]any, error) {
	before, err := s.core.Status(ctx) // also clears the sticky S_RESET_ST
	if err != nil {
		return nil, err
	}
	demcr, err := s.core.Bus.Read32(ctx, cortexm.AddrDEMCR)
	if err != nil {
		return nil, err
	}
	if halt {
		if err := s.core.Bus.Write32(ctx, cortexm.AddrDEMCR, demcr|cortexm.DEMCRVCCoreReset); err != nil {
			return nil, err
		}
	}
	restore := func() { _ = s.core.Bus.Write32(context.WithoutCancel(ctx), cortexm.AddrDEMCR, demcr) }
	if err := s.conn.PinReset(ctx, 100*time.Millisecond); err != nil {
		restore()
		return nil, err
	}
	obs := map[string]any{"nrst_driven": true}
	var after cortexm.State
	seen, lost := false, false
	deadline := time.Now().Add(2 * time.Second)
	for {
		after, err = s.core.Status(ctx)
		if err == nil {
			seen = seen || after.ResetSeen
			lost = lost || (before.DebugEnabled && !after.DebugEnabled)
			if seen || lost || (halt && after.Halted) {
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	obs["reset_seen"], obs["debug_enable_lost"] = seen, lost
	if !seen && !lost && (!halt || !after.Halted) {
		restore()
		return obs, errors.New("NRST was driven low and released but no reset was observed (S_RESET_ST stayed clear and debug stayed enabled): NRST is probably not connected to the target; the pin reset is NOT claimed")
	}
	if halt {
		if _, err := s.core.Halt(ctx); err != nil {
			restore()
			return obs, err
		}
		pc, err := s.core.ReadRegister(ctx, cortexm.RegPC)
		if err != nil {
			restore()
			return obs, err
		}
		vec := s.core.ResetVectorNow(ctx)
		obs["pc"], obs["reset_vector"] = hexw(pc&^1), hexw(vec)
		obs["at_reset_vector"] = vec != 0 && pc&^1 == vec
		restore()
		if vec == 0 || pc&^1 != vec {
			return obs, fmt.Errorf("halted at 0x%08x after the pin reset, not at the reset vector 0x%08x", pc&^1, vec)
		}
		return obs, nil
	}
	restore()
	obs["halted"] = after.Halted
	return obs, nil
}

// ---- triggers ----

func (s *session) ensureFPB(ctx context.Context) (*cortexm.FPB, error) {
	if s.fpb != nil {
		return s.fpb, nil
	}
	f, err := s.core.DiscoverFPB(ctx)
	if err != nil {
		return nil, err
	}
	s.fpb = f
	return f, nil
}

func (s *session) ensureDWT(ctx context.Context) (*cortexm.DWT, error) {
	d, err := s.core.DiscoverDWT(ctx)
	if err == nil {
		s.dwt = d
	}
	return d, err
}

// restoreDEMCRLocked puts DEMCR back if only discovery had turned TRCENA on.
func (s *session) restoreDEMCRLocked(ctx context.Context) error {
	if len(s.dwt.Owned()) == 0 {
		if err := s.core.CleanupWatchpoints(ctx); err != nil {
			return err
		}
		s.dwt = nil
	}
	return nil
}

// dropTriggers restores the comparators this session changed.
func (s *session) dropTriggers(ctx context.Context) error {
	if s.core == nil {
		return nil
	}
	if err := s.core.CleanupDebug(ctx); err != nil {
		return err
	}
	s.bps, s.fpbSaved, s.fpb, s.dwt = map[uint32]cortexm.Breakpoint{}, nil, nil, nil
	return nil
}

func (s *session) setBreakpoint(ctx context.Context, args map[string]any) (map[string]any, error) {
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, argErr(err)
	}
	thumb := addr&1 != 0
	addr &^= 1
	f, err := s.ensureFPB(ctx)
	if err != nil {
		return nil, err
	}
	if _, dup := s.bps[addr]; dup {
		return nil, status.Errorf(codes.AlreadyExists, "breakpoint at 0x%08x already set", addr)
	}
	if s.fpbSaved == nil {
		saved, err := f.Save(ctx)
		if err != nil {
			return nil, err
		}
		s.fpbSaved = &saved
	}
	bp, err := f.Set(ctx, addr)
	if err != nil {
		if len(s.bps) == 0 {
			s.fpbSaved = nil
		}
		if strings.Contains(err.Error(), "in use") {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, err
	}
	s.bps[addr] = bp
	res := map[string]any{
		"address": hexw(addr), "slot": float64(bp.Slot), "comparator": hexw(bp.Comp), "installed": float64(len(s.bps)),
		"slots_total": float64(f.Info.NumCode), "thumb_bit_stripped": thumb,
		"note": "FPB hardware breakpoint; the original comparator state is restored when the last one is cleared or the session closes",
	}
	ms, err := intArg(args, "run_wait_ms", 0, 0, 20000)
	if err != nil {
		return nil, err
	}
	if ms > 0 {
		rr, err := s.resume(ctx)
		if err != nil {
			return nil, err
		}
		res["resumed"] = rr["resumed"]
		wctx, cancel := context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond+time.Second)
		w, werr := s.waitHalt(wctx, map[string]any{"wait_ms": float64(ms)})
		cancel()
		res["run_wait_ms"] = float64(ms)
		if werr != nil {
			res["hit"] = false
			res["note"] = fmt.Sprintf("the core did not halt within %d ms (breakpoint not reached)", ms)
		} else {
			pcs, _ := w["pc"].(string)
			pc, _ := toAddr(pcs)
			res["hit"] = w["breakpoint_hit"] == true && pc&^1 == addr
			res["halted_pc"], res["cause"] = w["pc"], w["cause"]
		}
	}
	return s.mark(res), nil
}

func (s *session) clearBreakpoint(ctx context.Context, args map[string]any) (map[string]any, error) {
	all, err := boolArg(args, "all", false)
	if err != nil {
		return nil, err
	}
	f, err := s.ensureFPB(ctx)
	if err != nil {
		return nil, err
	}
	var addrs []uint32
	if all {
		for a := range s.bps {
			addrs = append(addrs, a)
		}
		sort.Slice(addrs, func(i, j int) bool { return addrs[i] < addrs[j] })
	} else {
		a, err := toAddr(args["address"])
		if err != nil {
			return nil, argErr(fmt.Errorf("%w (or all=true)", err))
		}
		a &^= 1
		if _, ok := s.bps[a]; !ok {
			return nil, status.Errorf(codes.NotFound, "no breakpoint installed by this session at 0x%08x", a)
		}
		addrs = []uint32{a}
	}
	cleared := []any{}
	for _, a := range addrs {
		if err := f.ClearSlot(ctx, s.bps[a].Slot); err != nil {
			return nil, fmt.Errorf("clearing the breakpoint at 0x%08x: %w", a, err)
		}
		delete(s.bps, a)
		cleared = append(cleared, hexw(a))
	}
	restored := false
	if len(s.bps) == 0 && s.fpbSaved != nil {
		if err := f.Restore(ctx, *s.fpbSaved); err != nil {
			return nil, fmt.Errorf("breakpoints cleared but the original FPB state could not be restored: %w", err)
		}
		s.fpbSaved, restored = nil, true
	}
	return s.mark(map[string]any{"cleared": cleared, "installed": float64(len(s.bps)), "original_fpb_state_restored": restored}), nil
}

func (s *session) listBreakpoints(ctx context.Context) (map[string]any, error) {
	f, err := s.ensureFPB(ctx)
	if err != nil {
		return nil, err
	}
	live, err := f.List(ctx)
	if err != nil {
		return nil, err
	}
	slots := []any{}
	for _, b := range live {
		_, mine := s.bps[b.Addr]
		slots = append(slots, map[string]any{"slot": float64(b.Slot), "address": hexw(b.Addr), "comparator": hexw(b.Comp), "installed_by_session": mine})
	}
	return s.mark(map[string]any{
		"slots_total": float64(f.Info.NumCode), "slots_in_use": float64(len(live)), "free_slots": float64(f.Info.NumCode - len(live)),
		"fpb_revision": float64(f.Info.Rev), "enabled": f.Info.Enabled, "breakpoints": slots,
		"basis": "FP_CTRL.NUM_CODE and the FP_COMPn registers; the count is what the core reports",
	}), nil
}

func (s *session) setWatchpoint(ctx context.Context, args map[string]any) (map[string]any, error) {
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, argErr(err)
	}
	kind, err := strArg(args, "kind")
	if err != nil {
		return nil, err
	}
	if kind == "" {
		kind = "write"
	}
	bits, err := intArg(args, "mask_bits", 2, 0, 15)
	if err != nil {
		return nil, err
	}
	d, err := s.ensureDWT(ctx)
	if err != nil {
		return nil, err
	}
	wp, err := d.Set(ctx, addr, bits, cortexm.WatchKind(kind))
	if err != nil {
		if errors.Is(err, cortexm.ErrAlreadySet) {
			return nil, status.Error(codes.AlreadyExists, err.Error())
		}
		if strings.Contains(err.Error(), "no free DWT comparator") {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, err
	}
	return s.mark(map[string]any{
		"address": hexw(wp.Addr), "slot": float64(wp.Slot), "kind": string(wp.Kind), "size_bytes": float64(int(1) << uint(wp.Mask)),
		"comparators_total": float64(d.NumComp), "installed": float64(len(s.dwt.Owned())),
		"note": "DWT data-address watchpoint; DEMCR.TRCENA and the comparator are restored when the last one is cleared or the session closes",
	}), nil
}

func (s *session) clearAllWatch(ctx context.Context) error {
	if err := s.core.CleanupWatchpoints(ctx); err != nil {
		return err
	}
	s.dwt = nil
	return nil
}

func (s *session) clearWatchpoint(ctx context.Context, args map[string]any) (map[string]any, error) {
	all, err := boolArg(args, "all", false)
	if err != nil {
		return nil, err
	}
	if s.dwt == nil && len(s.dwt.Owned()) == 0 {
		return nil, status.Error(codes.NotFound, "no watchpoint installed by this session")
	}
	cleared := []any{}
	if all {
		for a := range s.dwt.Owned() {
			cleared = append(cleared, hexw(a))
		}
		if err := s.clearAllWatch(ctx); err != nil {
			return nil, err
		}
	} else {
		a, err := toAddr(args["address"])
		if err != nil {
			return nil, argErr(fmt.Errorf("%w (or all=true)", err))
		}
		_, ok := s.dwt.Owned()[a]
		if !ok {
			return nil, status.Errorf(codes.NotFound, "no watchpoint installed by this session at 0x%08x", a)
		}
		if err := s.dwt.Clear(ctx, a); err != nil {
			return nil, err
		}
		cleared = append(cleared, hexw(a))
		if len(s.dwt.Owned()) == 0 {
			if err := s.clearAllWatch(ctx); err != nil {
				return nil, err
			}
		}
	}
	if err := s.restoreDEMCRLocked(ctx); err != nil {
		return nil, err
	}
	return s.mark(map[string]any{"cleared": cleared, "installed": float64(len(s.dwt.Owned()))}), nil
}

func (s *session) listWatchpoints(ctx context.Context) (map[string]any, error) {
	d, err := s.ensureDWT(ctx)
	if err != nil {
		return nil, err
	}
	live, err := d.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, w := range live {
		_, mine := s.dwt.Owned()[w.Addr]
		out = append(out, map[string]any{"slot": float64(w.Slot), "address": hexw(w.Addr), "kind": string(w.Kind), "size_bytes": float64(int(1) << uint(w.Mask)), "installed_by_session": mine})
	}
	res := map[string]any{"comparators_total": float64(d.NumComp), "in_use": float64(len(live)), "watchpoints": out,
		"basis": "DWT_CTRL.NUMCOMP and the DWT_FUNCTIONn registers; the count is what the core reports"}
	if err := s.restoreDEMCRLocked(ctx); err != nil {
		return nil, err
	}
	return s.mark(res), nil
}

// ---- memory ----

type readReq struct {
	addr uint32
	n    int
	path string
}

// parseRead validates a read before any probe is opened.
func parseRead(args map[string]any) (readReq, error) {
	addr, err := toAddr(args["address"])
	if err != nil {
		return readReq{}, argErr(err)
	}
	nf, ok, err := optNumber(args, "length")
	if err != nil {
		return readReq{}, argErr(err)
	}
	path, err := strArg(args, "path")
	if err != nil {
		return readReq{}, err
	}
	limit := maxReadBytes
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
	allow, err := boolArg(args, "allow_peripheral", false)
	if err != nil {
		return readReq{}, err
	}
	end := uint64(addr) + uint64(n) - 1
	if !allow {
		a := uint64(addr)
		switch {
		case a <= periph1Hi && end >= periph1Lo:
			return readReq{}, argErr(fmt.Errorf("0x%08x..0x%08x touches the peripheral region, where a read can have side effects (FIFO pops, cleared flags): pass allow_peripheral=true to read it anyway", addr, end))
		case end >= periph2Lo:
			return readReq{}, argErr(fmt.Errorf("0x%08x..0x%08x touches the private peripheral bus/system region, where a read can have side effects (DHCSR.S_RESET_ST is cleared by a read): pass allow_peripheral=true to read it anyway", addr, end))
		}
	}
	return readReq{addr, n, path}, nil
}

func sumHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *session) readMem(ctx context.Context, args map[string]any) (map[string]any, error) {
	rq, err := parseRead(args)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	data, err := s.core.Read(ctx, rq.addr, rq.n)
	if err != nil {
		return nil, err
	}
	el := time.Since(start)
	res := map[string]any{
		"address": hexw(rq.addr), "length": float64(rq.n), "region": region(rq.addr),
		"via": "MEM-AP block read (the core is not halted by this call)", "elapsed_ms": float64(el.Milliseconds()), "sha256": sumHex(data),
	}
	if el > 0 && rq.n >= 1024 {
		res["throughput_kib_s"] = float64(rq.n) / 1024 / el.Seconds()
	}
	if rq.path == "" {
		res["hex"] = hex.EncodeToString(data)
		return s.mark(res), nil
	}
	overwrite, err := boolArg(args, "overwrite", false)
	if err != nil {
		return nil, err
	}
	if err := ownedtemp.WriteFile(rq.path, data, overwrite); err != nil {
		return nil, argErr(err)
	}
	res["path"] = rq.path
	return s.mark(res), nil
}

type writeReq struct {
	addr uint32
	data []byte
}

func parseWrite(args map[string]any) (writeReq, error) {
	addr, err := toAddr(args["address"])
	if err != nil {
		return writeReq{}, argErr(err)
	}
	hs, err := strArg(args, "data")
	if err != nil {
		return writeReq{}, err
	}
	data, err := hex.DecodeString(hs)
	if err != nil || len(data) == 0 {
		return writeReq{}, argErr(fmt.Errorf("data: want 1..%d bytes of hex", maxWriteBytes))
	}
	if len(data) > maxWriteBytes {
		return writeReq{}, argErr(fmt.Errorf("data: %d bytes exceeds the %d-byte limit", len(data), maxWriteBytes))
	}
	if uint64(addr) < sramLo || uint64(addr)+uint64(len(data))-1 > sramHi {
		return writeReq{}, argErr(fmt.Errorf("[0x%08x, +%d) is %s: generic memory writes are only allowed inside SRAM 0x%08x-0x%08x (flash, peripherals, the private peripheral bus and the bit-band alias are refused; use the flash methods for flash)",
			addr, len(data), region(addr), uint32(sramLo), uint32(sramHi)))
	}
	return writeReq{addr, data}, nil
}

func (s *session) writeMem(ctx context.Context, args map[string]any) (map[string]any, error) {
	rq, err := parseWrite(args)
	if err != nil {
		return nil, err
	}
	if !s.o.perm.memWrite {
		return nil, denied("memory writes need allow_memory_write=true in the device configuration")
	}
	prev, err := s.core.Read(ctx, rq.addr, len(rq.data))
	if err != nil {
		return nil, fmt.Errorf("reading the previous content: %w", err)
	}
	start := time.Now()
	werr := s.core.Write(ctx, rq.addr, rq.data)
	got, rerr := s.core.Read(ctx, rq.addr, len(rq.data))
	switch {
	case werr != nil:
		return nil, werr
	case rerr != nil:
		return nil, fmt.Errorf("write done but the read-back failed: %w", rerr)
	case hex.EncodeToString(got) != hex.EncodeToString(rq.data):
		// Put the old bytes back so a failed write leaves nothing behind.
		rbErr := s.core.Write(ctx, rq.addr, prev)
		msg := fmt.Sprintf("read-back mismatch at 0x%08x: wrote %x, read %x (the address may not be backed by RAM)", rq.addr, rq.data, got)
		if rbErr != nil {
			msg += fmt.Sprintf("; restoring the previous bytes also failed: %v", rbErr)
		} else {
			msg += "; the previous bytes were restored"
		}
		return nil, status.Error(codes.DataLoss, msg)
	}
	st, _ := s.core.Status(ctx)
	res := map[string]any{
		"address": hexw(rq.addr), "length": float64(len(rq.data)), "verified": true, "previous_hex": hex.EncodeToString(prev),
		"region": region(rq.addr), "core_state": stateName(st), "elapsed_ms": float64(time.Since(start).Milliseconds()),
		"via": "MEM-AP (32-bit words; 8-bit writes for unaligned edges), read back and compared",
	}
	if !st.Halted {
		res["note"] = "the core was running: the firmware may change this memory concurrently; the read-back matched at that instant"
	}
	return s.mark(res), nil
}

// ---- clock ----

func (s *session) setSpeed(ctx context.Context, args map[string]any) (map[string]any, error) {
	khz, err := intArg(args, "khz", 0, 1, 4000)
	if err != nil {
		return nil, err
	}
	if khz == 0 {
		return nil, argErr(errors.New("khz: required"))
	}
	if s.info.Transport != "swd" {
		return nil, status.Error(codes.FailedPrecondition, "the clock can only be set for SWD")
	}
	prev := s.actualKHz
	got, err := s.conn.SetSWDClock(ctx, khz)
	if err != nil {
		return nil, err
	}
	// The new clock is proven by a read; if it fails the old clock is restored.
	if _, rerr := s.core.ReadIdentity(ctx); rerr != nil {
		if _, e := s.conn.SetSWDClock(ctx, prev); e != nil {
			return nil, cortexm.WithCleanupError(fmt.Errorf("SWD at %d kHz does not work: %w", got, rerr),
				fmt.Errorf("restore previous %d kHz: %w", prev, e))
		}
		return nil, fmt.Errorf("SWD at %d kHz does not work (%w); the previous %d kHz was restored", got, rerr, prev)
	}
	s.actualKHz = got
	return s.mark(map[string]any{"requested_khz": float64(khz), "actual_khz": float64(got), "previous_khz": float64(prev),
		"supported_khz": khzList(s.conn.SupportedSWDKHz())}), nil
}

// ---- info ----

func (s *session) facts() map[string]any {
	res := usbFacts(s.entry)
	res["open"] = true
	res["probe"] = s.probeFacts()
	mv := 0
	if s.info.VoltageOK {
		mv = s.info.VoltageMV
	}
	res["transport"] = map[string]any{
		"mode": s.info.Transport, "clock_khz": float64(s.actualKHz), "requested_khz": float64(s.o.clock),
		"supported_khz": khzList(s.conn.SupportedSWDKHz()), "dp_idcode": hexw(s.info.DPIDCode), "target_voltage_mv": float64(mv),
	}
	if s.serialChecked != "" {
		res["serial_note"] = s.serialChecked
	}
	return res
}

func khzList(v []int) []any {
	out := make([]any, len(v))
	for i, k := range v {
		out[i] = float64(k)
	}
	return out
}
