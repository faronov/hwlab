package espusbjtag

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// bpKey identifies a breakpoint: triggers belong to one hart.
type bpKey struct {
	hart uint32
	addr uint32
}

const maxStepCount = 1000

func denied(format string, a ...any) error {
	return status.Errorf(codes.PermissionDenied, format, a...)
}

func hartArg(args map[string]any) (uint32, bool, error) {
	v, ok, err := optNumber(args, "hart")
	if err != nil || !ok {
		return 0, false, err
	}
	if v < 0 || v > 7 || v != math.Trunc(v) {
		return 0, false, fmt.Errorf("hart %v: want an integer 0..7", v)
	}
	return uint32(v), true, nil
}

// dmFor returns the debug module with the hart for this call selected: the
// `hart` argument, otherwise the session's configured hart. A hart chosen for
// one call never leaks into the next.
func (s *session) dmFor(ctx context.Context, args map[string]any) (*riscv.DM, error) {
	dm, err := s.ensureDM(ctx)
	if err != nil {
		return nil, err
	}
	h, ok, err := hartArg(args)
	if err != nil {
		return nil, argErr(err)
	}
	if !ok {
		h = s.o.hart
	}
	if dm.Hart() != h {
		if err := dm.Select(ctx, h); err != nil {
			return nil, err
		}
	}
	return dm, nil
}

// curHart is the hart the last dmFor selected.
func (s *session) curHart() float64 {
	if s.dm != nil {
		return float64(s.dm.Hart())
	}
	return float64(s.o.hart)
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

// ---- memory write ----

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
	if !esp.WritableSRAM(addr, len(data)) {
		return writeReq{}, argErr(fmt.Errorf("[0x%08x, +%d) is %s: generic memory writes are only allowed inside one SRAM block (ROM, the flash-mapped window, peripherals and unmapped space are refused; use the flash methods for flash)",
			addr, len(data), esp.Classify(addr)))
	}
	return writeReq{addr, data}, nil
}

const maxWriteBytes = 4096

func (s *session) writeMem(ctx context.Context, args map[string]any) (map[string]any, error) {
	rq, err := parseWrite(args)
	if err != nil {
		return nil, err
	}
	if !s.o.perm.memWrite {
		return nil, denied("memory writes need allow_memory_write=true in the device configuration")
	}
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	prev, err := dm.ReadMem(ctx, rq.addr, len(rq.data))
	if err != nil {
		return nil, fmt.Errorf("reading the previous content: %w", err)
	}
	start := time.Now()
	if err := dm.WriteMem(ctx, rq.addr, rq.data); err != nil {
		return nil, err
	}
	got, err := dm.ReadMem(ctx, rq.addr, len(rq.data))
	if err != nil {
		return nil, fmt.Errorf("write done but the read-back failed: %w", err)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(rq.data) {
		return nil, fmt.Errorf("read-back mismatch at 0x%08x: wrote %x, read %x", rq.addr, rq.data, got)
	}
	st, _ := dm.Status(ctx)
	res := map[string]any{
		"address": fmt.Sprintf("0x%08x", rq.addr), "length": float64(len(rq.data)), "verified": true,
		"previous_hex": hex.EncodeToString(prev), "class": string(esp.Classify(rq.addr)),
		"hart": s.curHart(), "hart_state": string(st.State), "elapsed_ms": float64(time.Since(start).Milliseconds()),
		"via": "system bus access (32-bit; 8/16-bit by read-modify-write), read back and compared",
	}
	if st.State == riscv.HartRunning {
		res["note"] = "the hart was running: the firmware may have changed this memory concurrently; the read-back matched at that instant"
	}
	return s.mark(res, true), nil
}

// ---- step ----

func (s *session) step(ctx context.Context, args map[string]any) (map[string]any, error) {
	n, err := intArg(args, "count", 1, 1, maxStepCount)
	if err != nil {
		return nil, err
	}
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	st, err := dm.Status(ctx)
	if err != nil {
		return nil, err
	}
	if st.State != riscv.HartHalted {
		return nil, status.Errorf(codes.FailedPrecondition, "hart %d is %s: halt it first (DebugProbe.halt) - stepping never halts implicitly", dm.Hart(), st.State)
	}
	rs, err := dm.StepN(ctx, n)
	if err != nil {
		return nil, err
	}
	steps := make([]any, 0, min(len(rs), 64))
	for i, r := range rs {
		if i >= 64 {
			break
		}
		steps = append(steps, map[string]any{
			"pc_before": fmt.Sprintf("0x%08x", r.PCBefore), "pc_after": fmt.Sprintf("0x%08x", r.PCAfter),
			"cause": float64(r.Cause), "cause_name": r.CauseStr, "pc_delta": float64(int64(int32(r.PCAfter - r.PCBefore))),
		})
	}
	last := rs[len(rs)-1]
	res := map[string]any{
		"hart": s.curHart(), "count": float64(len(rs)), "steps": steps, "pc": fmt.Sprintf("0x%08x", last.PCAfter),
		"cause": float64(last.Cause), "cause_name": last.CauseStr, "halted": true,
		"basis": "dcsr.step set for the run and restored afterwards; each step was confirmed by dmstatus resume-ack, halt and dcsr.cause=step",
	}
	if len(rs) > len(steps) {
		res["steps_truncated"] = float64(len(rs) - len(steps))
	}
	return s.mark(res, true), nil
}

// ---- harts ----

func (s *session) harts(ctx context.Context, args map[string]any) (map[string]any, error) {
	dm, err := s.ensureDM(ctx)
	if err != nil {
		return nil, err
	}
	hs, err := dm.Harts(ctx, 8)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(hs))
	for i, h := range hs {
		m, err := toMap(h)
		if err != nil {
			return nil, err
		}
		out[i] = m
	}
	return s.mark(map[string]any{
		"harts": out, "count": float64(len(hs)), "selected": s.curHart(),
		"basis": "dmstatus/hartinfo for every hart; registers only for harts that were already halted. Nothing was halted or resumed by this call. A hart is described only by what the debug module reports, never by an assumed role",
	}, true), nil
}

// ---- breakpoints ----

func execOK(addr uint32) bool {
	switch esp.Classify(addr) {
	case esp.ClassROM, esp.ClassFlashMap:
		return true
	case esp.ClassSRAM:
		return true
	}
	return false
}

// withHaltedLocked is withHalted for callers that already hold s.mu.
func (s *session) withHaltedLocked(ctx context.Context, dm *riscv.DM, fn func() error) error {
	return s.withHalted(ctx, dm, fn)
}

// withHalted runs fn with the hart halted and resumes it afterwards only if
// this call halted it.
func (s *session) withHalted(ctx context.Context, dm *riscv.DM, fn func() error) (err error) {
	st, err := dm.Status(ctx)
	if err != nil {
		return err
	}
	switch st.State {
	case riscv.HartHalted:
		return fn()
	case riscv.HartRunning:
		if _, err := dm.Halt(ctx); err != nil {
			return err
		}
		defer func() {
			if _, rerr := dm.Resume(context.WithoutCancel(ctx)); rerr != nil && err == nil {
				err = fmt.Errorf("operation done but the hart did not resume: %w", rerr)
			}
		}()
		return fn()
	}
	return fmt.Errorf("hart %d is %s", dm.Hart(), st.State)
}

func (s *session) setBreakpoint(ctx context.Context, args map[string]any) (map[string]any, error) {
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, argErr(err)
	}
	if addr%2 != 0 {
		return nil, argErr(fmt.Errorf("address 0x%x is not 2-byte aligned (RISC-V instructions are)", addr))
	}
	if !execOK(addr) {
		return nil, argErr(fmt.Errorf("0x%08x is %s: not executable memory", addr, esp.Classify(addr)))
	}
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	key := bpKey{dm.Hart(), addr}
	if _, dup := s.bps[key]; dup {
		return nil, status.Errorf(codes.AlreadyExists, "breakpoint at 0x%08x already set on hart %d", addr, dm.Hart())
	}
	var bp riscv.Breakpoint
	err = s.withHalted(ctx, dm, func() error {
		var e error
		bp, e = dm.SetBreakpoint(ctx, addr)
		return e
	})
	if err != nil {
		if errors.Is(err, riscv.ErrNoTrigger) || errors.Is(err, riscv.ErrTrigger) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, err
	}
	if s.bps == nil {
		s.bps = map[bpKey]riscv.Breakpoint{}
	}
	s.bps[key] = bp
	res := map[string]any{
		"hart": float64(dm.Hart()), "address": fmt.Sprintf("0x%08x", addr), "trigger": float64(bp.Trigger), "trigger_type": float64(bp.Type),
		"previous_tdata1": fmt.Sprintf("0x%08x", bp.Saved.TData1), "previous_tdata2": fmt.Sprintf("0x%08x", bp.Saved.TData2),
		"installed": float64(len(s.bps)),
		"note":      "hardware execution trigger; the slot's previous content is restored by clear_breakpoint or when the session closes",
	}
	ms, err := intArg(args, "run_wait_ms", 0, 0, 20000)
	if err != nil {
		return nil, err
	}
	if ms > 0 {
		// One-shot flow for a call that owns the whole session: resume, wait
		// for the trigger to halt the hart, leave it halted.
		st, err := dm.Status(ctx)
		if err != nil {
			return nil, err
		}
		if st.State == riscv.HartHalted {
			// Only the acknowledge is awaited: the trigger may halt the hart again at once.
			if err := dm.ResumeNoWait(ctx); err != nil {
				return nil, err
			}
		}
		hi, err := dm.WaitHalt(ctx, time.Duration(ms)*time.Millisecond)
		res["run_wait_ms"] = float64(ms)
		if err != nil {
			res["hit"] = false
			res["note"] = fmt.Sprintf("the hart did not halt within %d ms (breakpoint not reached): %v", ms, err)
		} else {
			res["hit"] = hi.Cause == 2 && hi.PC == addr
			res["halted_pc"], res["cause"], res["cause_name"] = fmt.Sprintf("0x%08x", hi.PC), float64(hi.Cause), hi.CauseStr
		}
	}
	return s.mark(res, true), nil
}

func (s *session) clearBreakpoint(ctx context.Context, args map[string]any) (map[string]any, error) {
	all, err := boolArg(args, "all", false)
	if err != nil {
		return nil, err
	}
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	var keys []bpKey
	if all {
		for k := range s.bps {
			if k.hart == dm.Hart() {
				keys = append(keys, k)
			}
		}
	} else {
		addr, err := toAddr(args["address"])
		if err != nil {
			return nil, argErr(fmt.Errorf("%w (or all=true)", err))
		}
		k := bpKey{dm.Hart(), addr}
		if _, ok := s.bps[k]; !ok {
			return nil, status.Errorf(codes.NotFound, "no breakpoint installed by this session at 0x%08x on hart %d", addr, dm.Hart())
		}
		keys = []bpKey{k}
	}
	var cleared []any
	err = s.withHalted(ctx, dm, func() error {
		for _, k := range keys {
			if err := dm.ClearBreakpoint(ctx, s.bps[k]); err != nil {
				return fmt.Errorf("restoring trigger %d: %w", s.bps[k].Trigger, err)
			}
			cleared = append(cleared, fmt.Sprintf("0x%08x", k.addr))
			delete(s.bps, k)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cleared == nil {
		cleared = []any{}
	}
	return s.mark(map[string]any{"hart": float64(dm.Hart()), "cleared": cleared, "installed": float64(len(s.bps))}, true), nil
}

// dropBreakpoints restores the triggers this session changed (session close).
func (s *session) dropBreakpoints(ctx context.Context) {
	if len(s.bps) == 0 || s.dm == nil {
		return
	}
	prev := s.dm.Hart()
	for k, bp := range s.bps {
		if s.dm.Hart() != k.hart && s.dm.Select(ctx, k.hart) != nil {
			continue
		}
		if st, err := s.dm.Status(ctx); err == nil && st.State == riscv.HartRunning {
			if _, err := s.dm.Halt(ctx); err != nil {
				continue
			}
			_ = s.dm.ClearBreakpoint(ctx, bp)
			_, _ = s.dm.Resume(ctx)
		} else if err == nil {
			_ = s.dm.ClearBreakpoint(ctx, bp)
		}
	}
	_ = s.dm.Select(ctx, prev)
	s.bps = nil
}

func (s *session) listBreakpoints(ctx context.Context, args map[string]any) (map[string]any, error) {
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	var ts []riscv.Trigger
	err = s.withHalted(ctx, dm, func() error {
		var e error
		ts, e = dm.Triggers(ctx)
		return e
	})
	if err != nil {
		return nil, err
	}
	slots := make([]any, len(ts))
	free := 0
	for i, t := range ts {
		m, err := toMap(t)
		if err != nil {
			return nil, err
		}
		m["tdata1"], m["tdata2"] = fmt.Sprintf("0x%08x", t.TData1), fmt.Sprintf("0x%08x", t.TData2)
		slots[i] = m
		if !t.InUse && !t.DMode {
			free++
		}
	}
	var mine []any
	for k, bp := range s.bps {
		if k.hart == dm.Hart() {
			mine = append(mine, map[string]any{"address": fmt.Sprintf("0x%08x", k.addr), "trigger": float64(bp.Trigger)})
		}
	}
	if mine == nil {
		mine = []any{}
	}
	return s.mark(map[string]any{
		"hart": float64(dm.Hart()), "trigger_slots": float64(len(ts)), "free_slots": float64(free), "slots": slots, "installed_by_session": mine,
		"basis": "tselect/tdata1/tdata2 enumeration on the hart; the slot count is whatever the hart answers, nothing is assumed",
	}, true), nil
}

// waitHalt waits for the selected hart to halt and says why.
func (s *session) waitHalt(ctx context.Context, args map[string]any) (map[string]any, error) {
	ms, err := intArg(args, "wait_ms", 2000, 1, 20000)
	if err != nil {
		return nil, err
	}
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	hi, err := dm.WaitHalt(ctx, time.Duration(ms)*time.Millisecond)
	if err != nil {
		if errors.Is(err, riscv.ErrHaltTimeout) {
			return nil, status.Errorf(codes.DeadlineExceeded, "hart %d did not halt within %d ms: %v", dm.Hart(), ms, err)
		}
		return nil, err
	}
	_, mine := s.bps[bpKey{dm.Hart(), hi.PC}]
	return s.mark(map[string]any{
		"hart": float64(dm.Hart()), "pc": fmt.Sprintf("0x%08x", hi.PC), "cause": float64(hi.Cause), "cause_name": hi.CauseStr,
		"dcsr": fmt.Sprintf("0x%08x", hi.DCSR), "breakpoint_hit": hi.Cause == 2 && mine, "halted": true,
	}, true), nil
}

// ---- reset (hart / system) ----

// resetKinds that keep the USB device alive. A SoC reset is handled by
// Plugin.soc because it replaces the whole session.
func (s *session) reset(ctx context.Context, args map[string]any) (map[string]any, error) {
	kind, err := strArg(args, "kind")
	if err != nil {
		return nil, err
	}
	if kind == "" {
		kind = "system"
	}
	rk := riscv.ResetKind(kind)
	if rk != riscv.ResetHart && rk != riscv.ResetSystem {
		return nil, argErr(fmt.Errorf("kind %q: want hart, system or soc", kind))
	}
	halt, err := boolArg(args, "halt", false)
	if err != nil {
		return nil, err
	}
	if !s.o.perm.reset {
		return nil, denied("reset needs allow_reset=true in the device configuration")
	}
	dm, err := s.dmFor(ctx, args)
	if err != nil {
		return nil, err
	}
	// Trigger and breakpoint state does not survive a reset reliably: forget
	// what this session installed on the hart instead of "restoring" it.
	forgot := 0
	for k := range s.bps {
		if k.hart == dm.Hart() || rk == riscv.ResetSystem {
			delete(s.bps, k)
			forgot++
		}
	}
	start := time.Now()
	// A hart that is already in debug mode is not reliably released by a reset
	// without halt on this debug module, so the reset always halts at the reset
	// vector and the "run" variant resumes from there.
	if err := dm.Reset(ctx, rk, true); err != nil {
		return nil, err
	}
	dm.InvalidateCache()
	var st riscv.Status
	deadline := time.Now().Add(3 * time.Second)
	seen := false
	for {
		if st, err = dm.Status(ctx); err != nil {
			return nil, err
		}
		if st.HaveReset {
			seen = true
		}
		if seen && st.State == riscv.HartHalted {
			break
		}
		if time.Now().After(deadline) {
			if !seen {
				return nil, status.Errorf(codes.FailedPrecondition, "reset requested but dmstatus.havereset was never set: reset NOT observed (hart %d is %s)", dm.Hart(), st.State)
			}
			return nil, status.Errorf(codes.DeadlineExceeded, "reset observed but hart %d did not halt at the reset vector within 3 s (state %s)", dm.Hart(), st.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := dm.FinishReset(ctx); err != nil {
		return nil, err
	}
	if !halt {
		if _, err := dm.Resume(ctx); err != nil {
			return nil, fmt.Errorf("reset done but the hart did not resume: %w", err)
		}
	}
	res := map[string]any{
		"kind": kind, "halt": halt, "reset_observed": true, "usb_dropped": false, "elapsed_ms": float64(time.Since(start).Milliseconds()),
		"breakpoints_forgotten": float64(forgot),
	}
	if rk == riscv.ResetHart {
		res["scope"] = "hart reset (dmcontrol.hartreset): the selected hart only"
	} else {
		res["scope"] = "non-debug-module reset (dmcontrol.ndmreset): every hart and peripheral the debug module can reset; the USB Serial/JTAG unit is expected to stay up. Use kind=soc for a full chip reset with USB re-enumeration"
	}
	st, _ = dm.Status(ctx)
	for k, v := range s.stateResult(ctx, dm, st) {
		res[k] = v
	}
	if st.State == riscv.HartHalted {
		if hi, err := dm.HaltedInfo(ctx); err == nil {
			res["cause"], res["cause_name"] = float64(hi.Cause), hi.CauseStr
		}
	}
	return s.mark(res, true), nil
}
