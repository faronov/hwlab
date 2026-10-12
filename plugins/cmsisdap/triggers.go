package cmsisdapplugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/plugins/cmsisdap/dap"
)

// triggers is the FPB/DWT state installed through one open device, so that it
// can be listed, cleared and restored to what the target had before.
type triggers struct {
	fpb      *cortexm.FPB
	fpbSaved *cortexm.FPBState
	bps      map[uint32]cortexm.Breakpoint

	dwt *cortexm.DWT
}

// restoreAll removes every trigger installed by this device and puts the
// comparators and DEMCR back as they were. Best effort: it runs on close.
func (t *triggers) restoreAll(ctx context.Context, c *cortexm.Core) error {
	if c == nil {
		return nil
	}
	if err := c.CleanupDebug(ctx); err != nil {
		return err
	}
	t.bps, t.fpbSaved, t.fpb, t.dwt = nil, nil, nil, nil
	return nil
}

func (t *triggers) clearWatch(ctx context.Context, c *cortexm.Core) error {
	if err := c.CleanupWatchpoints(ctx); err != nil {
		return err
	}
	t.dwt = nil
	return nil
}

func (d *device) ensureFPB(ctx context.Context) (*cortexm.FPB, error) {
	if d.trig.fpb != nil {
		return d.trig.fpb, nil
	}
	f, err := d.core.DiscoverFPB(ctx)
	if err != nil {
		return nil, err
	}
	d.trig.fpb = f
	return f, nil
}

func (d *device) setBreakpoint(ctx context.Context, a map[string]any) (map[string]any, error) {
	addr, err := u32(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	thumb := addr&1 != 0
	addr &^= 1
	f, err := d.ensureFPB(ctx)
	if err != nil {
		return nil, err
	}
	if d.trig.bps == nil {
		d.trig.bps = map[uint32]cortexm.Breakpoint{}
	}
	if _, dup := d.trig.bps[addr]; dup {
		return nil, status.Errorf(codes.AlreadyExists, "breakpoint at 0x%08x already set", addr)
	}
	if d.trig.fpbSaved == nil {
		saved, err := f.Save(ctx)
		if err != nil {
			return nil, err
		}
		d.trig.fpbSaved = &saved
	}
	bp, err := f.Set(ctx, addr)
	if err != nil {
		if len(d.trig.bps) == 0 {
			d.trig.fpbSaved = nil
		}
		if strings.Contains(err.Error(), "in use") {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, err
	}
	d.trig.bps[addr] = bp
	res := map[string]any{
		"address": hex32(addr), "slot": float64(bp.Slot), "comparator": hex32(bp.Comp), "installed": float64(len(d.trig.bps)),
		"slots_total": float64(f.Info.NumCode), "thumb_bit_stripped": thumb,
		"note": "FPB hardware breakpoint; the original comparator state is restored when the last one is cleared or the device closes",
	}
	ms, err := optInt(a, "run_wait_ms", 0, 0, 20000)
	if err != nil {
		return nil, err
	}
	if ms > 0 {
		rr, err := d.resume(ctx)
		if err != nil {
			return nil, err
		}
		res["resumed"] = rr["halted"] == false
		w, werr := d.waitHalt(ctx, map[string]any{"wait_ms": float64(ms)})
		res["run_wait_ms"] = float64(ms)
		switch {
		case werr != nil && status.Code(classify(werr)) == codes.DeadlineExceeded:
			res["hit"] = false
			res["note"] = fmt.Sprintf("the core did not halt within %d ms (breakpoint not reached)", ms)
		case werr != nil:
			return nil, werr
		default:
			res["hit"] = w["breakpoint_hit"] == true && w["pc"] == hex32(addr)
			res["halted_pc"], res["cause"] = w["pc"], w["cause"]
		}
	}
	return res, nil
}

func (d *device) clearBreakpoint(ctx context.Context, a map[string]any) (map[string]any, error) {
	all, _, err := optBool(a, "all")
	if err != nil {
		return nil, argErr(err)
	}
	f, err := d.ensureFPB(ctx)
	if err != nil {
		return nil, err
	}
	var addrs []uint32
	if all {
		for k := range d.trig.bps {
			addrs = append(addrs, k)
		}
		sort.Slice(addrs, func(i, j int) bool { return addrs[i] < addrs[j] })
	} else {
		v, err := u32(a, "address")
		if err != nil {
			return nil, argErr(fmt.Errorf("%w (or all=true)", err))
		}
		v &^= 1
		if _, ok := d.trig.bps[v]; !ok {
			return nil, status.Errorf(codes.NotFound, "no breakpoint installed by this device at 0x%08x", v)
		}
		addrs = []uint32{v}
	}
	cleared := []any{}
	for _, v := range addrs {
		if err := f.ClearSlot(ctx, d.trig.bps[v].Slot); err != nil {
			return nil, fmt.Errorf("clearing the breakpoint at 0x%08x: %w", v, err)
		}
		delete(d.trig.bps, v)
		cleared = append(cleared, hex32(v))
	}
	restored := false
	if len(d.trig.bps) == 0 && d.trig.fpbSaved != nil {
		if err := f.Restore(ctx, *d.trig.fpbSaved); err != nil {
			return nil, fmt.Errorf("breakpoints cleared but the original FPB state could not be restored: %w", err)
		}
		d.trig.fpbSaved, restored = nil, true
	}
	return map[string]any{"cleared": cleared, "installed": float64(len(d.trig.bps)), "original_fpb_state_restored": restored}, nil
}

func (d *device) listBreakpoints(ctx context.Context) (map[string]any, error) {
	f, err := d.ensureFPB(ctx)
	if errors.Is(err, cortexm.ErrAbsent) {
		return map[string]any{"present": false, "slots_total": float64(0), "status": UnsupportedByHW, "reason": err.Error()}, nil
	}
	if err != nil {
		return nil, err
	}
	live, err := f.List(ctx)
	if err != nil {
		return nil, err
	}
	slots := []any{}
	for _, b := range live {
		_, mine := d.trig.bps[b.Addr]
		slots = append(slots, map[string]any{"slot": float64(b.Slot), "address": hex32(b.Addr), "comparator": hex32(b.Comp), "installed_by_device": mine})
	}
	return map[string]any{
		"present": true, "slots_total": float64(f.Info.NumCode), "slots_in_use": float64(len(live)), "free_slots": float64(f.Info.NumCode - len(live)),
		"fpb_revision": float64(f.Info.Rev), "enabled": f.Info.Enabled, "breakpoints": slots,
		"basis": "FP_CTRL.NUM_CODE and the FP_COMPn registers; the count is what the core reports",
	}, nil
}

func (d *device) ensureDWT(ctx context.Context) (*cortexm.DWT, error) {
	w, err := d.core.DiscoverDWT(ctx)
	if err == nil {
		d.trig.dwt = w
	}
	return w, err
}

func (d *device) setWatchpoint(ctx context.Context, a map[string]any) (map[string]any, error) {
	addr, err := u32(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	kind, _ := a["kind"].(string)
	if kind == "" {
		kind = "write"
	}
	bits, err := optInt(a, "mask_bits", 2, 0, 15)
	if err != nil {
		return nil, err
	}
	w, err := d.ensureDWT(ctx)
	if err != nil {
		return nil, err
	}
	wp, err := w.Set(ctx, addr, bits, cortexm.WatchKind(kind))
	if err != nil {
		if errors.Is(err, cortexm.ErrAlreadySet) {
			return nil, status.Error(codes.AlreadyExists, err.Error())
		}
		if strings.Contains(err.Error(), "no free DWT comparator") {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		if strings.Contains(err.Error(), "unknown watchpoint kind") || strings.Contains(err.Error(), "aligned") || strings.Contains(err.Error(), "out of range") {
			return nil, argErr(err)
		}
		return nil, err
	}
	return map[string]any{
		"address": hex32(wp.Addr), "slot": float64(wp.Slot), "kind": string(wp.Kind), "size_bytes": float64(int(1) << uint(wp.Mask)),
		"comparators_total": float64(w.NumComp), "installed": float64(len(d.trig.dwt.Owned())),
		"note": "DWT data-address watchpoint; DEMCR.TRCENA and the comparator are restored when the last one is cleared or the device closes",
	}, nil
}

func (d *device) clearWatchpoint(ctx context.Context, a map[string]any) (map[string]any, error) {
	all, _, err := optBool(a, "all")
	if err != nil {
		return nil, argErr(err)
	}
	if d.trig.dwt == nil && len(d.trig.dwt.Owned()) == 0 {
		return nil, status.Error(codes.NotFound, "no watchpoint installed by this device")
	}
	cleared := []any{}
	if all {
		for k := range d.trig.dwt.Owned() {
			cleared = append(cleared, hex32(k))
		}
		if err := d.trig.clearWatch(ctx, d.core); err != nil {
			return nil, err
		}
	} else {
		addr, err := u32(a, "address")
		if err != nil {
			return nil, argErr(fmt.Errorf("%w (or all=true)", err))
		}
		_, ok := d.trig.dwt.Owned()[addr]
		if !ok {
			return nil, status.Errorf(codes.NotFound, "no watchpoint installed by this device at 0x%08x", addr)
		}
		if err := d.trig.dwt.Clear(ctx, addr); err != nil {
			return nil, err
		}
		cleared = append(cleared, hex32(addr))
		if len(d.trig.dwt.Owned()) == 0 {
			if err := d.trig.clearWatch(ctx, d.core); err != nil {
				return nil, err
			}
		}
	}
	return map[string]any{"cleared": cleared, "installed": float64(len(d.trig.dwt.Owned()))}, nil
}

func (d *device) listWatchpoints(ctx context.Context) (map[string]any, error) {
	w, err := d.ensureDWT(ctx)
	if errors.Is(err, cortexm.ErrAbsent) {
		return map[string]any{"present": false, "comparators_total": float64(0), "status": UnsupportedByHW, "reason": err.Error()}, nil
	}
	if err != nil {
		return nil, err
	}
	live, err := w.List(ctx)
	if err != nil {
		return nil, err
	}
	slots := []any{}
	for _, x := range live {
		_, mine := d.trig.dwt.Owned()[x.Addr]
		slots = append(slots, map[string]any{"slot": float64(x.Slot), "address": hex32(x.Addr), "kind": string(x.Kind), "size_bytes": float64(int(1) << uint(x.Mask)), "installed_by_device": mine})
	}
	return map[string]any{
		"present": true, "comparators_total": float64(w.NumComp), "in_use": float64(len(live)), "watchpoints": slots,
		"basis": "DWT_CTRL.NUMCOMP and the DWT_COMPn/FUNCTIONn registers; the count is what the core reports",
	}, nil
}

// reset implements the four reset kinds. They are different mechanisms and
// each is only reported as performed when it was observed.
func (d *device) reset(ctx context.Context, a map[string]any) (map[string]any, error) {
	kind := "sysreset"
	if v, ok := a["kind"]; ok && v != nil {
		s, isStr := v.(string)
		if !isStr || (s != "sysreset" && s != "core" && s != "dap_reset" && s != "hardware") {
			return nil, argErr(fmt.Errorf("kind %v: want sysreset, core, dap_reset or hardware", v))
		}
		kind = s
	}
	halt, _, err := optBool(a, "halt")
	if err != nil {
		return nil, argErr(err)
	}
	if err := d.trig.restoreAll(ctx, d.core); err != nil {
		return nil, fmt.Errorf("debug trigger cleanup before reset: %w", err)
	}
	var res map[string]any
	switch kind {
	case "sysreset", "core":
		ck := cortexm.ResetSystem
		if kind == "core" {
			ck = cortexm.ResetCore
		}
		rr, err := d.core.Reset(ctx, ck, halt)
		if errors.Is(err, cortexm.ErrDebugLost) {
			// The link dropped at the reset request. Report what was seen and
			// leave the next call to re-attach; the reset is not claimed as
			// confirmed by the core.
			if err := d.detach(); err != nil {
				return nil, err
			}
			return map[string]any{
				"kind": string(ck), "halt_requested": halt, "halted": false, "reset_seen": false,
				"debug_lost": true,
				"note":       "the SWD link went down at the reset request and the core could not be read afterwards; the reset itself is not confirmed by S_RESET_ST. The next call re-attaches and reports whether the debug port came back",
			}, nil
		}
		if err != nil {
			return nil, err
		}
		res = map[string]any{
			"halted": rr.Halted, "reset_seen": rr.ResetSeen, "debug_kept": rr.DebugKept,
			"reset_vector": hex32(rr.ResetVector), "at_reset_vector": rr.AtVector,
		}
		if rr.Halted {
			res["pc"] = hex32(rr.PC)
		}
	case "dap_reset":
		executed := false
		res, err = d.observedReset(ctx, halt, func() error {
			var e error
			executed, e = d.probe.ResetTarget()
			if e == nil && !executed {
				e = errors.New("the probe has no device-specific reset sequence (DAP_ResetTarget reports not implemented); nothing was reset")
			}
			return e
		})
		if res != nil {
			res["dap_reset_executed"] = executed
		}
		if err != nil {
			return nil, err
		}
	case "hardware":
		res, err = d.observedReset(ctx, halt, d.pulseNRESET)
		if err != nil {
			return nil, err
		}
	}
	res["kind"], res["halt_requested"] = kind, halt
	return res, nil
}

// pulseNRESET drives the nRESET output low for 100 ms and releases it.
// pulseNRESETLow drives nRESET low for low and releases it.
func (d *device) pulseNRESETLow(ctx context.Context, low time.Duration) error {
	if _, err := d.probe.Pins(0, dap.PinnRESET, 0); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-time.After(low):
	}
	_, err := d.probe.Pins(dap.PinnRESET, dap.PinnRESET, 0)
	return err
}

func (d *device) pulseNRESET() error {
	if _, err := d.probe.Pins(0, dap.PinnRESET, 0); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	_, err := d.probe.Pins(dap.PinnRESET, dap.PinnRESET, 0)
	return err
}

// recoverByNRESET handles a hardware reset requested while the debug port is
// unreachable (some cores reset the DAP with a system reset and leave it down).
// The reset line is pulsed and the attachment retried; the reset is claimed
// only because the port that did not answer before answers afterwards.
func (d *device) recoverByNRESET(o options, attachErr error) (map[string]any, error) {
	if err := d.detach(); err != nil {
		return nil, err
	}
	if err := d.pulseNRESET(); err != nil {
		return nil, err
	}
	var last error
	for i := 0; i < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		if last = d.attach(o); last == nil {
			return map[string]any{
				"kind": "hardware", "driven": true, "halted": false, "recovered_debug_port": true,
				"previous_error": attachErr.Error(),
				"note":           "the debug port did not answer before the nRESET pulse and answers after it",
			}, nil
		}
	}
	return nil, status.Errorf(codes.FailedPrecondition, "nRESET was pulsed but the debug port still does not answer: %v (was: %v)", last, attachErr)
}

// observedReset runs a reset mechanism that the probe cannot confirm and
// requires target-side evidence: S_RESET_ST seen, or C_DEBUGEN lost, or (with
// halt) the core halted at its reset vector. Without evidence the reset is NOT
// claimed: the pin or sequence is probably not connected.
func (d *device) observedReset(ctx context.Context, halt bool, drive func() error) (map[string]any, error) {
	before, err := d.core.Status(ctx) // also clears the sticky S_RESET_ST
	if err != nil {
		return nil, err
	}
	cpuidBefore, _ := d.core.Bus.Read32(ctx, cortexm.AddrCPUID)
	demcr, err := d.core.Bus.Read32(ctx, cortexm.AddrDEMCR)
	if err != nil {
		return nil, err
	}
	if halt {
		if err := d.core.Bus.Write32(ctx, cortexm.AddrDEMCR, demcr|cortexm.DEMCRVCCoreReset); err != nil {
			return nil, err
		}
	}
	restore := func() { _ = d.core.Bus.Write32(context.WithoutCancel(ctx), cortexm.AddrDEMCR, demcr) }
	if err := drive(); err != nil {
		restore()
		return nil, err
	}
	obs := map[string]any{"driven": true}
	// A reset can take the debug port down; bring it back before reading.
	_ = d.tgt.Reinit()
	var after cortexm.State
	seen, lost, gone := false, false, false
	deadline := time.Now().Add(2 * time.Second)
	for {
		var serr error
		after, serr = d.core.Status(ctx)
		if serr == nil {
			seen = seen || after.ResetSeen
			lost = lost || (before.DebugEnabled && !after.DebugEnabled)
			// A core that lost its debug state (or whose debug registers read
			// as another value) was reset even if S_RESET_ST is not kept.
			if cp, cerr := d.core.Bus.Read32(ctx, cortexm.AddrCPUID); cpuidBefore != 0 && (cerr != nil || cp != cpuidBefore) {
				gone = true
			}
			if seen || lost || gone || (halt && after.Halted) {
				break
			}
		} else {
			gone = true
			_ = d.tgt.Reinit()
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	obs["reset_seen"], obs["debug_enable_lost"], obs["core_registers_changed"] = seen, lost, gone
	if !seen && !lost && !gone && (!halt || !after.Halted) {
		restore()
		return obs, errors.New("the reset was driven but no reset was observed (S_RESET_ST stayed clear and debug stayed enabled): the nRESET line is probably not connected to the target; this reset is NOT claimed")
	}
	if halt {
		if _, err := d.core.Halt(ctx); err != nil {
			// A reset was observed but the part gates its debug access
			// afterwards, so the halt request cannot take effect.
			if err := d.detach(); err != nil {
				return nil, err
			}
			obs["halted"], obs["debug_lost"] = false, true
			obs["note"] = "the reset was observed (core registers changed) but the debug access did not answer afterwards, so the core could not be halted at the reset vector: " + err.Error()
			return obs, nil
		}
		pc, err := d.core.ReadRegister(ctx, cortexm.RegPC)
		if err != nil {
			restore()
			return obs, err
		}
		vec := d.core.ResetVectorNow(ctx)
		obs["pc"], obs["reset_vector"] = hex32(pc&^1), hex32(vec)
		obs["at_reset_vector"] = vec != 0 && pc&^1 == vec
		restore()
		obs["halted"] = true
		return obs, nil
	}
	restore()
	obs["halted"] = after.Halted
	return obs, nil
}
