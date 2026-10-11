package jlinknativeplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/cortexm"
)

func stateMap(st cortexm.State) map[string]any {
	return map[string]any{"halted": st.Halted, "sleeping": st.Sleeping, "lockup": st.Lockup, "debug_enabled": st.DebugEnabled}
}

func (d *device) connectInfo(ctx context.Context) (map[string]any, error) {
	ci, err := d.core.ReadIdentity(ctx)
	if err != nil {
		return nil, err
	}
	st, err := d.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	res := stateMap(st)
	res["dpidr"], res["ap_idr"] = hex32(d.tgt.DPIDR), hex32(d.tgt.APIDR)
	res["core"], res["cpuid"] = ci.Name, hex32(ci.CPUID)
	res["part"] = fmt.Sprintf("0x%03x", ci.PartNo)
	res["revision"] = fmt.Sprintf("r%dp%d", ci.Variant, ci.Revision)
	res["implementer"] = fmt.Sprintf("0x%02x", ci.Implementer)
	res["architecture"] = ci.ArchName
	res["clock_hz"], res["ap"] = float64(d.attached.clockHz), float64(d.attached.ap)
	return res, nil
}

func (d *device) apScan() (map[string]any, error) {
	aps, err := d.tgt.ScanAPs(8)
	if err != nil {
		return nil, err
	}
	list := []any{}
	var rom []any
	for _, ap := range aps {
		list = append(list, map[string]any{
			"index": float64(ap.Index), "idr": hex32(ap.IDR), "base": hex32(ap.Base), "cfg": hex32(ap.CFG), "class": ap.Class, "type": ap.Type,
		})
		if ap.Index == d.attached.ap && ap.Base&1 != 0 && ap.Base != 0xFFFFFFFF {
			comps, werr := d.tgt.WalkROM(ap.Base)
			for _, c := range comps {
				rom = append(rom, map[string]any{"address": hex32(c.Address), "class": float64(c.Class), "part": fmt.Sprintf("0x%03x", c.Part), "name": c.Name, "pid": fmt.Sprintf("0x%x", c.PID)})
			}
			if werr != nil {
				rom = append(rom, map[string]any{"error": werr.Error()})
			}
		}
	}
	return map[string]any{"dpidr": hex32(d.tgt.DPIDR), "aps": list, "rom_table": rom, "selected_ap": float64(d.attached.ap)}, nil
}

func (d *device) withPC(ctx context.Context, res map[string]any, st cortexm.State) (map[string]any, error) {
	if st.Halted {
		pc, err := d.core.ReadRegister(ctx, cortexm.RegPC)
		if err != nil {
			return nil, err
		}
		res["pc"] = hex32(pc)
	}
	return res, nil
}

func (d *device) status(ctx context.Context) (map[string]any, error) {
	st, err := d.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	return d.withPC(ctx, stateMap(st), st)
}

func (d *device) halt(ctx context.Context) (map[string]any, error) {
	st, err := d.core.Halt(ctx)
	if err != nil {
		return nil, err
	}
	return d.withPC(ctx, stateMap(st), st)
}

func (d *device) resume(ctx context.Context) (map[string]any, error) {
	st, err := d.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	stepped := false
	if st.Halted {
		// Resuming onto a breakpoint would halt again at once; step over it.
		pc, err := d.core.ReadRegister(ctx, cortexm.RegPC)
		if err != nil {
			return nil, err
		}
		if _, mine := d.trig.bps[pc&^1]; mine {
			if _, err := d.core.Step(ctx, false); err != nil {
				return nil, err
			}
			stepped = true
		}
	}
	st, err = d.core.Resume(ctx)
	if err != nil {
		return nil, err
	}
	res, err := d.withPC(ctx, stateMap(st), st)
	if err != nil {
		return nil, err
	}
	res["stepped_over_breakpoint"] = stepped
	return res, nil
}

func (d *device) step(ctx context.Context, a map[string]any) (map[string]any, error) {
	n, err := optInt(a, "count", 1, 1, maxStepCount)
	if err != nil {
		return nil, err
	}
	mask, _, err := optBool(a, "mask_interrupts")
	if err != nil {
		return nil, argErr(err)
	}
	st, err := d.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	if !st.Halted {
		return nil, status.Error(codes.FailedPrecondition, "the core is running: halt it first; stepping never halts implicitly")
	}
	var first, last cortexm.StepResult
	changed, wide, steps := 0, 0, []any{}
	for i := 0; i < n; i++ {
		r, err := d.core.Step(ctx, mask)
		if err != nil {
			return nil, fmt.Errorf("step %d of %d: %w", i+1, n, err)
		}
		if i == 0 {
			first = r
		}
		last = r
		if r.PCAfter != r.PCBefore {
			changed++
		}
		if int64(r.PCAfter)-int64(r.PCBefore) == 4 {
			wide++
		}
		if n <= 10 {
			steps = append(steps, map[string]any{"pc_before": hex32(r.PCBefore), "pc_after": hex32(r.PCAfter), "cause": r.Cause.String()})
		}
	}
	res := map[string]any{
		"steps": float64(n), "all_halted": true, "pc_changed": float64(changed), "advanced_by_4": float64(wide),
		"pc_first": hex32(first.PCBefore), "pc": hex32(last.PCAfter), "halted": true,
	}
	if n <= 10 {
		res["trace"] = steps
	}
	return res, nil
}

func (d *device) causeMap(res map[string]any, c cortexm.Cause) {
	res["cause"], res["dfsr"] = c.String(), hex32(c.DFSR)
}

func (d *device) waitHalt(ctx context.Context, a map[string]any) (map[string]any, error) {
	ms, err := optInt(a, "wait_ms", 2000, 1, 30000)
	if err != nil {
		return nil, err
	}
	st, err := d.core.WaitHalted(ctx, time.Duration(ms)*time.Millisecond)
	if err != nil {
		return nil, err
	}
	res, err := d.withPC(ctx, stateMap(st), st)
	if err != nil {
		return nil, err
	}
	c, err := d.core.HaltCause(ctx)
	if err != nil {
		return nil, err
	}
	d.causeMap(res, c)
	hit := false
	if s, ok := res["pc"].(string); ok {
		var pc uint32
		_, _ = fmt.Sscanf(s, "0x%x", &pc)
		_, mine := d.trig.bps[pc&^1]
		hit = c.Has("breakpoint") && mine
	}
	res["breakpoint_hit"], res["watchpoint_hit"] = hit, c.Has("watchpoint")
	return res, nil
}

func (d *device) readRegisters(ctx context.Context, a map[string]any) (map[string]any, error) {
	halt, _, err := optBool(a, "halt")
	if err != nil {
		return nil, argErr(err)
	}
	if halt {
		if _, err := d.core.Halt(ctx); err != nil {
			return nil, err
		}
	}
	r, err := d.core.ReadAll(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for i, v := range r.R {
		out[fmt.Sprintf("r%d", i)] = hex32(v)
	}
	out["sp"], out["lr"], out["pc"], out["xpsr"] = hex32(r.SP), hex32(r.LR), hex32(r.PC), hex32(r.XPSR)
	out["msp"], out["psp"], out["special"] = hex32(r.MSP), hex32(r.PSP), hex32(r.Special)
	return map[string]any{
		"registers": out, "halted": true, "pc": hex32(r.PC),
		"control": hex32(uint32(r.Control())), "primask": hex32(uint32(r.Primask())),
		"basepri": hex32(uint32(r.Basepri())), "faultmask": hex32(uint32(r.Faultmask())),
	}, nil
}

func (d *device) writeRegister(ctx context.Context, a map[string]any) (map[string]any, error) {
	name, _ := a["name"].(string)
	id, ok := cortexm.ParseRegister(name)
	if !ok {
		return nil, argErr(fmt.Errorf("register %q: want one of %s", name, strings.Join(registerNames(), ", ")))
	}
	v, err := u32(a, "value")
	if err != nil {
		return nil, argErr(err)
	}
	st, err := d.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	if !st.Halted {
		return nil, cortexm.ErrNotHalted
	}
	if err := d.core.WriteRegister(ctx, id, v); err != nil {
		return nil, err
	}
	back, err := d.core.ReadRegister(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"register": strings.ToLower(name), "value": hex32(back), "written": hex32(v)}, nil
}

func registerNames() []string {
	var out []string
	for id := cortexm.RegisterID(0); id <= 20; id++ {
		if n := cortexm.RegisterName(id); !strings.HasPrefix(n, "reg") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (d *device) readMemory(a map[string]any) (map[string]any, error) {
	addr, err := u32(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	n, ok, err := optNumber(a, "length")
	if err != nil {
		return nil, argErr(err)
	}
	if !ok || n < 1 || n != math.Trunc(n) || n > maxReadBytes {
		return nil, argErr(fmt.Errorf("length %v: want an integer 1..%d", n, maxReadBytes))
	}
	data, err := d.tgt.ReadMem(addr, int(n))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	res := map[string]any{"address": hex32(addr), "length": float64(len(data)), "sha256": hex.EncodeToString(sum[:])}
	if len(data) <= inlineBytes {
		res["hex"] = hex.EncodeToString(data)
	} else {
		res["hex_preview"] = hex.EncodeToString(data[:previewBytes])
		res["truncated"] = true
		res["note"] = fmt.Sprintf("only the first %d bytes are shown; reads over %d bytes return a sha256 (read in smaller windows for the data)", previewBytes, inlineBytes)
	}
	return res, nil
}

func (d *device) writeMemory(a map[string]any) (map[string]any, error) {
	addr, err := u32(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	s, _ := a["data"].(string)
	s = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(s), " ", ""), "0x")
	data, err := hex.DecodeString(s)
	if err != nil || len(data) == 0 {
		return nil, argErr(fmt.Errorf("data: want a non-empty hex string"))
	}
	if len(data) > maxWriteBytes {
		return nil, argErr(fmt.Errorf("data is %d bytes; at most %d per write", len(data), maxWriteBytes))
	}
	verify, set, err := optBool(a, "verify")
	if err != nil {
		return nil, argErr(err)
	}
	if !set {
		verify = true
	}
	if err := d.tgt.WriteMem(addr, data); err != nil {
		return nil, err
	}
	res := map[string]any{"address": hex32(addr), "length": float64(len(data)), "verified": false}
	if verify {
		back, err := d.tgt.ReadMem(addr, len(data))
		if err != nil {
			return nil, err
		}
		if string(back) != string(data) {
			return nil, status.Errorf(codes.DataLoss, "write verify failed at 0x%08x: read back %s", addr, hex.EncodeToString(back))
		}
		res["verified"] = true
	}
	return res, nil
}
