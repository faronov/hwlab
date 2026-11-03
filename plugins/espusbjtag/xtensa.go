package espusbjtag

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/ownedtemp"
	"github.com/faronov/hwlab/internal/xtensa"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// ESP32-S3 (dual Xtensa LX7) path. The JTAG chain decides the architecture:
// one TAP per core, each with the ESP32-S3 IDCODE and a 5-bit IR, means the
// Xtensa On-Chip-Debug module (internal/xtensa) instead of a RISC-V DTM/DM.
// The `hart` argument then selects the core (chain position 0 is PRO_CPU,
// PRID 0xCDCD on the real board). Unlike the RISC-V system bus, memory is
// only reachable through a halted core, so reads and writes halt a running
// core for the access and resume it afterwards.

// s3DebugLevel is the ESP32-S3 core's debug interrupt level (XCHAL_DEBUGLEVEL;
// EPC6/EPS6 held the interrupted PC/PS on the real chip).
const s3DebugLevel = 6

// xtTarget is the Xtensa state of a session.
type xtTarget struct {
	cores    []*xtensa.Core
	examined []*xtensa.Info
	chip     *esp.Chip
	ev       esp.Evidence
}

// xtMethods are served by the Xtensa path on an ESP32-S3. Everything else
// that needs a debug module goes through ensureDM, which refuses explicitly.
var xtMethods = map[string]bool{
	"DebugProbe.chip_info": true, "DebugProbe.halt": true, "DebugProbe.resume": true, "DebugProbe.read_registers": true,
	"DebugProbe.step": true, "DebugProbe.harts": true, "DebugProbe.set_breakpoint": true, "DebugProbe.clear_breakpoint": true,
	"DebugProbe.list_breakpoints": true, "DebugProbe.wait_halt": true, "DebugProbe.reset": true,
	"MemoryAccess.read": true, "MemoryAccess.write": true,
}

// isXtensaChain reports an ESP32-S3 chain: every TAP has the S3 IDCODE and
// the IR lengths add up to 5 bits per TAP.
func isXtensaChain(c *jtag.Chain) bool {
	if c == nil || c.Status != jtag.StatusOK || len(c.Devices) == 0 || c.TotalIR != xtensa.IRLen*len(c.Devices) {
		return false
	}
	for _, d := range c.Devices {
		if d.Bypass || d.IDCode == nil || d.IDCode.Raw != esp.ChipS3.IDCode {
			return false
		}
	}
	return true
}

// errXtensaOnly is what a RISC-V-only method answers on an ESP32-S3.
var errXtensaOnly = status.Error(codes.FailedPrecondition,
	"ESP32-S3 (Xtensa) target: this method needs a RISC-V debug module (ESP32-C6/H2) and is not available here. "+
		"On the ESP32-S3 DebugProbe chip_info/halt/resume/read_registers/step/harts/set_breakpoint/clear_breakpoint/list_breakpoints/wait_halt/reset kind=hart, "+
		"MemoryAccess read/write, FlashProgramming (reset=none only) and reconnect work; reset kind=system/soc is not implemented for it")

// xtensaTarget returns the Xtensa state when the chain is an ESP32-S3 one,
// nil otherwise (also when the chain cannot be scanned: the RISC-V path then
// reports that error itself, exactly as before).
func (s *session) xtensaTarget(ctx context.Context) *xtTarget {
	if s.xt != nil {
		return s.xt
	}
	if s.dm != nil {
		return nil
	}
	c, err := s.ensureChain(ctx)
	if err != nil || !isXtensaChain(c) {
		return nil
	}
	irl := make([]int, len(c.Devices))
	for i := range irl {
		irl[i] = xtensa.IRLen
	}
	x := &xtTarget{examined: make([]*xtensa.Info, len(c.Devices))}
	for i := range c.Devices {
		dm, err := xtensa.NewDM(s.drv, irl, i)
		if err != nil {
			return nil
		}
		x.cores = append(x.cores, xtensa.NewCore(dm, s3DebugLevel))
	}
	s.xt = x
	return x
}

func (s *session) xtDispatch(ctx context.Context, x *xtTarget, method string, args map[string]any) (map[string]any, error) {
	switch method {
	case "DebugProbe.chip_info":
		return s.xtChipInfo(ctx, x, args)
	case "DebugProbe.halt":
		return s.xtHalt(ctx, x, args)
	case "DebugProbe.resume":
		return s.xtResume(ctx, x, args)
	case "DebugProbe.read_registers":
		return s.xtRegisters(ctx, x, args)
	case "DebugProbe.step":
		return s.xtStep(ctx, x, args)
	case "DebugProbe.harts":
		return s.xtHarts(ctx, x)
	case "DebugProbe.set_breakpoint":
		return s.xtSetBreakpoint(ctx, x, args)
	case "DebugProbe.clear_breakpoint":
		return s.xtClearBreakpoint(ctx, x, args)
	case "DebugProbe.list_breakpoints":
		return s.xtListBreakpoints(ctx, x, args)
	case "DebugProbe.wait_halt":
		return s.xtWaitHalt(ctx, x, args)
	case "DebugProbe.reset":
		return s.xtReset(ctx, x, args)
	case "MemoryAccess.read":
		return s.xtReadMem(ctx, x, args)
	case "MemoryAccess.write":
		return s.xtWriteMem(ctx, x, args)
	}
	return nil, status.Errorf(codes.Unimplemented, "method %s", method)
}

func (x *xtTarget) idcode(s *session) uint32 {
	if c := s.chain; c != nil && len(c.Devices) > 0 && c.Devices[0].IDCode != nil {
		return c.Devices[0].IDCode.Raw
	}
	return 0
}

// examine powers up the debug domain of core i once per session.
func (x *xtTarget) examine(ctx context.Context, i int) error {
	if x.examined[i] != nil {
		return nil
	}
	in, err := x.cores[i].Examine(ctx)
	if err != nil {
		if errors.Is(err, xtensa.ErrNoDebugModule) {
			return status.Errorf(codes.FailedPrecondition,
				"core %d has no active debug module (OCDID 0x%08x, PWRSTAT 0x%02x): it is held in reset or powered down (an ESP32-S3 in ROM download mode keeps core 1, the APP_CPU, in reset)",
				i, in.OCDID, in.PwrStat)
		}
		return err
	}
	x.examined[i] = &in
	return nil
}

// coreFor returns the core addressed by this call (`hart` argument or the
// session's configured hart), with its debug module powered up.
func (s *session) coreFor(ctx context.Context, x *xtTarget, args map[string]any) (*xtensa.Core, int, error) {
	h, ok, err := hartArg(args)
	if err != nil {
		return nil, 0, argErr(err)
	}
	if !ok {
		h = s.o.hart
	}
	if int(h) >= len(x.cores) {
		return nil, 0, argErr(fmt.Errorf("hart %d: the ESP32-S3 chain has %d Xtensa core(s), numbered from 0", h, len(x.cores)))
	}
	if err := x.examine(ctx, int(h)); err != nil {
		return nil, 0, err
	}
	return x.cores[h], int(h), nil
}

// xtAttach reports whether the core is halted; a core found halted (by an
// earlier session, a breakpoint or a step) gets its context saved, so its
// registers and memory can be used.
func xtAttach(ctx context.Context, c *xtensa.Core) (bool, error) {
	st, err := c.Status(ctx)
	if err != nil {
		return false, err
	}
	if st.Halted && !c.Halted() {
		if _, err := c.Halt(ctx); err != nil {
			return true, err
		}
	}
	return st.Halted, nil
}

// xtWithHalted runs fn with the core halted and resumes it afterwards only if
// this call halted it. halted says whether it was already halted.
func xtWithHalted(ctx context.Context, c *xtensa.Core, fn func() error) (wasHalted bool, err error) {
	if wasHalted, err = xtAttach(ctx, c); err != nil {
		return wasHalted, err
	}
	if wasHalted {
		return true, fn()
	}
	if _, err := c.Halt(ctx); err != nil {
		return false, err
	}
	defer func() {
		if rerr := c.Resume(context.WithoutCancel(ctx)); rerr != nil && err == nil {
			err = fmt.Errorf("operation done but the core did not resume: %w", rerr)
		}
	}()
	return false, fn()
}

func hex32(v uint32) string { return fmt.Sprintf("0x%08x", v) }

func causeName(h xtensa.Halt) string {
	if len(h.Causes) == 0 {
		return "none"
	}
	return strings.Join(h.Causes, "+")
}

func haltFields(res map[string]any, h xtensa.Halt) {
	res["pc"], res["ps"], res["dsr"] = hex32(h.PC), hex32(h.PS), hex32(h.DSR)
	res["debugcause"], res["cause_name"] = hex32(h.DebugCause), causeName(h)
}

// xtState is stateResult for a core.
func xtState(ctx context.Context, c *xtensa.Core, i int) map[string]any {
	res := map[string]any{"hart": float64(i), "core": float64(i)}
	st, err := c.Status(ctx)
	if err != nil {
		res["state"] = "unknown"
		res["error"] = err.Error()
		return res
	}
	res["dsr"] = hex32(st.DSR)
	res["halted"] = st.Halted
	res["state"] = map[bool]string{true: "halted", false: "running"}[st.Halted]
	if h, ok := c.LastHalt(); ok && st.Halted {
		haltFields(res, h)
	}
	return res
}

// xtChip selects the chip profile from the IDCODE and the ROM word read
// through the (halted, attached) core.
func (s *session) xtChip(ctx context.Context, x *xtTarget, c *xtensa.Core) (*esp.Chip, error) {
	if x.chip != nil {
		return x.chip, nil
	}
	ev, err := esp.ReadEvidence(ctx, esp.XtensaBus{C: c})
	if err != nil {
		return nil, fmt.Errorf("reading the ROM chip word: %w", err)
	}
	chip, err := esp.Detect(x.idcode(s), ev.ROMMagic)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "refusing: %v", err)
	}
	ev.Basis = fmt.Sprintf("ROM word at %#x is %#x (%s) and every TAP has IDCODE 0x%08x", esp.ChipMagicAddr, ev.ROMMagic, chip.ROMWordBasis, chip.IDCode)
	x.chip, x.ev = chip, ev
	return chip, nil
}

func (s *session) xtChipInfo(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	h, ok, err := hartArg(args)
	if err != nil {
		return nil, argErr(err)
	}
	if !ok {
		h = s.o.hart
	}
	res := map[string]any{
		"arch": "xtensa", "debug_level": float64(s3DebugLevel), "hart": float64(h),
		"basis": "JTAG IDCODE and the Xtensa OCD registers (OCDID, PWRSTAT, DSR, DCR) of every TAP; nothing was halted",
	}
	if c := s.chain; c != nil && len(c.Devices) > 0 && c.Devices[0].IDCode != nil {
		res["idcode"] = hex32(c.Devices[0].IDCode.Raw)
		res["ir_length"] = float64(c.TotalIR)
		res["chip_hint"] = chipHint(c.Devices[0].IDCode)
	}
	var cores []any
	avail := 0
	for i, c := range x.cores {
		m := map[string]any{"core": float64(i), "tap_index": float64(i)}
		in, err := c.Examine(ctx)
		m["ocdid"], m["pwrstat"] = hex32(in.OCDID), fmt.Sprintf("0x%02x", in.PwrStat)
		if err != nil {
			if !errors.Is(err, xtensa.ErrNoDebugModule) {
				return nil, err
			}
			m["available"] = false
			m["state"] = "unavailable"
			m["note"] = "no active debug module: the core is held in reset or powered down"
		} else {
			avail++
			x.examined[i] = &in
			m["available"] = true
			m["dcr"] = hex32(in.DCR)
			for k, v := range xtState(ctx, c, i) {
				m[k] = v
			}
		}
		if int(h) == i {
			res["hart_state"] = m["state"]
		}
		cores = append(cores, m)
	}
	res["cores"] = cores
	res["harts"] = float64(avail)
	res["cores_on_chain"] = float64(len(x.cores))
	if int(h) < len(x.cores) && x.examined[h] != nil {
		c := x.cores[h]
		if halted, err := xtAttach(ctx, c); err == nil && halted {
			if chip, err := s.xtChip(ctx, x, c); err == nil {
				res["chip"] = chip.Name
				res["rom_chip_word"] = hex32(x.ev.ROMMagic)
				res["chip_basis"] = x.ev.Basis
			} else {
				res["chip_profile_error"] = err.Error()
			}
		} else {
			res["chip_profile"] = "not checked: the ROM chip word can only be read through a halted core and chip_info never halts (DebugProbe.halt first, or use MemoryAccess.read)"
		}
	}
	return s.mark(res, true), nil
}

func (s *session) xtHalt(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	h, err := c.Halt(ctx)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"hart": float64(i), "core": float64(i), "state": "halted", "halted": true, "already_halted": h.Already}
	haltFields(res, h)
	res["note"] = "the core stays halted (Xtensa OCD debug mode, debug interrupt level 6) until DebugProbe.resume; pc/ps are the interrupted program's (EPC6/EPS6)"
	return s.mark(res, true), nil
}

func (s *session) xtResume(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	halted, err := xtAttach(ctx, c)
	if err != nil {
		return nil, err
	}
	res := map[string]any{}
	if halted {
		if err := c.Resume(ctx); err != nil {
			return nil, err
		}
		res["resumed"] = true
		if st, err := c.Status(ctx); err == nil && st.Halted {
			// An armed breakpoint or ICOUNT may stop the core again at once.
			res["halted_again"] = true
			if h, err := c.WaitHalt(ctx, 10*time.Millisecond); err == nil {
				res["halted_pc"], res["halted_cause_name"] = hex32(h.PC), causeName(h)
			}
		}
	} else {
		res["resumed"] = false
		res["note"] = "the core was not halted; nothing to resume"
	}
	for k, v := range xtState(ctx, c, i) {
		res[k] = v
	}
	return s.mark(res, true), nil
}

func (s *session) xtRegisters(ctx context.Context, x *xtTarget, args map[string]any) (res map[string]any, err error) {
	keep, err := boolArg(args, "keep_halted", false)
	if err != nil {
		return nil, err
	}
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	halted, err := xtAttach(ctx, c)
	if err != nil {
		return nil, err
	}
	wasRunning := !halted
	if wasRunning {
		if _, err := c.Halt(ctx); err != nil {
			return nil, err
		}
	}
	defer func() {
		if wasRunning && !keep {
			if rerr := c.Resume(context.WithoutCancel(ctx)); rerr != nil && err == nil {
				err = fmt.Errorf("registers were read but the core did not resume: %w", rerr)
			} else if rerr == nil && res != nil {
				res["resumed"] = true
			}
		}
	}()
	vals, err := c.ReadRegisters(ctx, xtensa.CoreRegisters)
	if err != nil {
		return nil, err
	}
	regs := map[string]any{}
	errs := map[string]any{}
	for _, v := range vals {
		if v.Error != "" {
			errs[v.Name] = v.Error
			continue
		}
		regs[v.Name] = hex32(v.Value)
	}
	res = map[string]any{
		"registers": regs, "hart": float64(i), "core": float64(i), "arch": "xtensa", "was_running": wasRunning, "halted": !wasRunning || keep,
		"basis": "RSR/WSR executed on the halted core through DIR0EXEC with values moved through DDR; a3 is the debugger's scratch register and is restored on resume. " +
			"a0-a15 are the current register window (windowbase/windowstart given), pc/ps are the interrupted program's (EPC6/EPS6); read-only",
	}
	if len(errs) > 0 {
		res["register_errors"] = errs
	}
	return s.mark(res, true), nil
}

func (s *session) xtReadMem(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	rq, err := parseRead(args)
	if err != nil {
		return nil, err
	}
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	var data []byte
	was, err := xtWithHalted(ctx, c, func() error {
		var e error
		data, e = c.ReadMem(ctx, rq.addr, rq.n)
		return e
	})
	if err != nil {
		return nil, xtMemErr(err, rq.addr, rq.n)
	}
	via := "Xtensa debug module: 32-bit loads (LDDR32.P) executed on core %d, which this call halted for the read and resumed"
	if was {
		via = "Xtensa debug module: 32-bit loads (LDDR32.P) executed on core %d, which was already halted and stays halted"
	}
	res := map[string]any{
		"address": hex32(rq.addr), "length": float64(rq.n), "hart": float64(i),
		"via": fmt.Sprintf(via, i), "elapsed_ms": float64(time.Since(start).Milliseconds()), "sha256": sum(data),
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

// xtMemErr explains a debug load/store that raised an exception.
func xtMemErr(err error, addr uint32, n int) error {
	var ee *xtensa.ExecError
	if errors.As(err, &ee) {
		return status.Errorf(codes.FailedPrecondition, "[0x%08x, +%d): the core raised an exception on the debug access (unmapped, protected or not data-bus addressable on the %s; %s is %s): %v",
			addr, n, esp.ChipS3.Name, hex32(addr), esp.ChipS3.Classify(addr), err)
	}
	return err
}

func (s *session) xtWriteMem(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	rq, err := parseWriteArgs(args)
	if err != nil {
		return nil, err
	}
	// The architecture already says ESP32-S3: its map is checked before the
	// device is touched; the ROM word confirms the profile below.
	if !esp.ChipS3.WritableSRAM(rq.addr, len(rq.data)) {
		return nil, argErr(fmt.Errorf("[0x%08x, +%d) is %s on the %s: generic memory writes are only allowed inside one SRAM block (ROM, the flash-mapped windows, peripherals and unmapped space are refused)",
			rq.addr, len(rq.data), esp.ChipS3.Classify(rq.addr), esp.ChipS3.Name))
	}
	if !s.o.perm.memWrite {
		return nil, denied("memory writes need allow_memory_write=true in the device configuration")
	}
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	var prev []byte
	var chip *esp.Chip
	was, err := xtWithHalted(ctx, c, func() error {
		var e error
		if chip, e = s.xtChip(ctx, x, c); e != nil {
			return e
		}
		if !chip.WritableSRAM(rq.addr, len(rq.data)) {
			return argErr(fmt.Errorf("[0x%08x, +%d) is %s on the %s: not writable SRAM", rq.addr, len(rq.data), chip.Classify(rq.addr), chip.Name))
		}
		if prev, e = c.ReadMem(ctx, rq.addr, len(rq.data)); e != nil {
			return fmt.Errorf("reading the previous content: %w", e)
		}
		if e = c.WriteMem(ctx, rq.addr, rq.data); e != nil {
			return e
		}
		got, e := c.ReadMem(ctx, rq.addr, len(rq.data))
		if e != nil {
			return fmt.Errorf("write done but the read-back failed: %w", e)
		}
		if hex.EncodeToString(got) != hex.EncodeToString(rq.data) {
			return fmt.Errorf("read-back mismatch at 0x%08x: wrote %x, read %x", rq.addr, rq.data, got)
		}
		return nil
	})
	if err != nil {
		return nil, xtMemErr(err, rq.addr, len(rq.data))
	}
	state := "running"
	if was {
		state = "halted"
	}
	return s.mark(map[string]any{
		"address": hex32(rq.addr), "length": float64(len(rq.data)), "verified": true, "chip": chip.Name,
		"previous_hex": hex.EncodeToString(prev), "class": string(chip.Classify(rq.addr)),
		"hart": float64(i), "hart_state": state, "elapsed_ms": float64(time.Since(start).Milliseconds()),
		"via": "Xtensa debug module on the halted core (32-bit SDDR32.P/LDDR32.P; 8/16-bit by read-modify-write of whole words), read back and compared; a core this call halted was resumed",
	}, true), nil
}

func (s *session) xtStep(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	n, err := intArg(args, "count", 1, 1, maxStepCount)
	if err != nil {
		return nil, err
	}
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	halted, err := xtAttach(ctx, c)
	if err != nil {
		return nil, err
	}
	if !halted {
		return nil, status.Errorf(codes.FailedPrecondition, "core %d is running: halt it first (DebugProbe.halt) - stepping never halts implicitly", i)
	}
	var steps []any
	var last xtensa.Halt
	done := 0
	for k := 0; k < n; k++ {
		before, _ := c.LastHalt()
		h, err := c.Step(ctx)
		if err != nil {
			return nil, fmt.Errorf("step %d of %d: %w", k+1, n, err)
		}
		last = h
		done++
		if len(steps) < 64 {
			steps = append(steps, map[string]any{
				"pc_before": hex32(before.PC), "pc_after": hex32(h.PC), "debugcause": hex32(h.DebugCause),
				"cause": float64(h.DebugCause), "cause_name": causeName(h), "pc_delta": float64(int64(int32(h.PC - before.PC))),
			})
		}
	}
	res := map[string]any{
		"hart": float64(i), "core": float64(i), "count": float64(done), "steps": steps, "pc": hex32(last.PC),
		"cause": float64(last.DebugCause), "cause_name": causeName(last), "halted": true,
		"basis": "ICOUNTLEVEL = debug level and ICOUNT = -2 armed for each step and restored afterwards (IBREAKENABLE too, when stepping off a breakpoint); each step was confirmed by DSR stopped and DEBUGCAUSE",
	}
	if done > len(steps) {
		res["steps_truncated"] = float64(done - len(steps))
	}
	return s.mark(res, true), nil
}

func (s *session) xtHarts(ctx context.Context, x *xtTarget) (map[string]any, error) {
	var out []any
	for i, c := range x.cores {
		m := map[string]any{"index": float64(i), "core": float64(i)}
		if d := s.chain; d != nil && i < len(d.Devices) && d.Devices[i].IDCode != nil {
			m["idcode"] = hex32(d.Devices[i].IDCode.Raw)
		}
		if err := x.examine(ctx, i); err != nil {
			if !errors.Is(err, xtensa.ErrNoDebugModule) && status.Code(err) != codes.FailedPrecondition {
				return nil, err
			}
			m["available"] = false
			m["state"] = "unavailable"
			m["note"] = status.Convert(err).Message()
		} else {
			m["available"] = true
			m["ocdid"] = hex32(x.examined[i].OCDID)
			for k, v := range xtState(ctx, c, i) {
				m[k] = v
			}
		}
		out = append(out, m)
	}
	return s.mark(map[string]any{
		"harts": out, "count": float64(len(out)), "selected": float64(s.o.hart), "arch": "xtensa",
		"basis": "one Xtensa TAP per core: OCDID/PWRSTAT/DSR of each; nothing was halted or resumed. A core is described only by what its debug module reports",
	}, true), nil
}

func (s *session) xtSetBreakpoint(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, argErr(err)
	}
	if !esp.ChipS3.Executable(addr) {
		return nil, argErr(fmt.Errorf("0x%08x is %s: the %s fetches instructions only through its instruction-bus windows (ROM 0x40000000.., SRAM 0x40370000.., flash 0x42000000.., RTC FAST)",
			addr, esp.ChipS3.Classify(addr), esp.ChipS3.Name))
	}
	ms, err := intArg(args, "run_wait_ms", 0, 0, 20000)
	if err != nil {
		return nil, err
	}
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	for _, b := range c.IBreaks() {
		if b.Addr == addr {
			return nil, status.Errorf(codes.AlreadyExists, "breakpoint at 0x%08x already set on core %d", addr, i)
		}
	}
	var bp xtensa.IBreak
	var slots int
	_, err = xtWithHalted(ctx, c, func() error {
		var e error
		if bp, e = c.SetIBreak(ctx, addr); e != nil {
			return e
		}
		slots, e = c.IBreakSlots(ctx)
		return e
	})
	if err != nil {
		if errors.Is(err, xtensa.ErrNoSlot) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, err
	}
	res := map[string]any{
		"hart": float64(i), "core": float64(i), "address": hex32(addr), "slot": float64(bp.Slot), "trigger": float64(bp.Slot),
		"slots": float64(slots), "installed": float64(len(c.IBreaks())),
		"note": "hardware instruction breakpoint (IBREAKA/IBREAKENABLE); the slot count was probed on the core. Removed by clear_breakpoint or when the session closes",
	}
	if ms > 0 {
		halted, err := xtAttach(ctx, c)
		if err != nil {
			return nil, err
		}
		if halted {
			if err := c.Resume(ctx); err != nil {
				return nil, err
			}
		}
		h, err := c.WaitHalt(ctx, time.Duration(ms)*time.Millisecond)
		res["run_wait_ms"] = float64(ms)
		if err != nil {
			res["hit"] = false
			res["note"] = fmt.Sprintf("the core did not halt within %d ms (breakpoint not reached): %v", ms, err)
		} else {
			res["hit"] = h.DebugCause&xtensa.CauseIBreak != 0 && h.PC == addr
			res["halted_pc"], res["cause"], res["cause_name"] = hex32(h.PC), float64(h.DebugCause), causeName(h)
		}
	}
	return s.mark(res, true), nil
}

func (s *session) xtClearBreakpoint(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	all, err := boolArg(args, "all", false)
	if err != nil {
		return nil, err
	}
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	var addrs []uint32
	if all {
		for _, b := range c.IBreaks() {
			addrs = append(addrs, b.Addr)
		}
	} else {
		addr, err := toAddr(args["address"])
		if err != nil {
			return nil, argErr(fmt.Errorf("%w (or all=true)", err))
		}
		found := false
		for _, b := range c.IBreaks() {
			found = found || b.Addr == addr
		}
		if !found {
			return nil, status.Errorf(codes.NotFound, "no breakpoint installed by this session at 0x%08x on core %d", addr, i)
		}
		addrs = []uint32{addr}
	}
	cleared := []any{}
	if len(addrs) > 0 {
		_, err = xtWithHalted(ctx, c, func() error {
			for _, a := range addrs {
				if err := c.ClearIBreak(ctx, a); err != nil {
					return err
				}
				cleared = append(cleared, hex32(a))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return s.mark(map[string]any{"hart": float64(i), "core": float64(i), "cleared": cleared, "installed": float64(len(c.IBreaks()))}, true), nil
}

func (s *session) xtListBreakpoints(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	var vals []xtensa.RegValue
	n := 0
	_, err = xtWithHalted(ctx, c, func() error {
		var e error
		if n, e = c.IBreakSlots(ctx); e != nil {
			return e
		}
		names := []string{"ibreakenable"}
		for k := 0; k < n; k++ {
			names = append(names, fmt.Sprintf("sr%d", xtensa.SRIBREAKA0+k))
		}
		vals, e = c.ReadRegisters(ctx, names)
		return e
	})
	if err != nil {
		return nil, err
	}
	for _, v := range vals {
		if v.Error != "" {
			return nil, fmt.Errorf("reading %s: %s", v.Name, v.Error)
		}
	}
	mine := map[int]bool{}
	installed := []any{}
	for _, b := range c.IBreaks() {
		mine[b.Slot] = true
		installed = append(installed, map[string]any{"address": hex32(b.Addr), "slot": float64(b.Slot)})
	}
	ibe := vals[0].Value
	slots := make([]any, n)
	free := 0
	for k := 0; k < n; k++ {
		en := ibe&(1<<k) != 0
		owner := "none"
		switch {
		case mine[k]:
			owner = "this session"
		case en:
			owner = "other (enabled by firmware or another debugger)"
		default:
			free++
		}
		slots[k] = map[string]any{"slot": float64(k), "ibreaka": hex32(vals[k+1].Value), "enabled": en, "owner": owner}
	}
	return s.mark(map[string]any{
		"hart": float64(i), "core": float64(i), "trigger_slots": float64(n), "free_slots": float64(free), "slots": slots,
		"ibreakenable": hex32(ibe), "installed_by_session": installed,
		"basis": "IBREAKA0.. probed until the core raises an exception (the slot count is discovered, not assumed) and IBREAKENABLE read on the halted core",
	}, true), nil
}

func (s *session) xtWaitHalt(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	ms, err := intArg(args, "wait_ms", 2000, 1, 20000)
	if err != nil {
		return nil, err
	}
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	h, err := c.WaitHalt(ctx, time.Duration(ms)*time.Millisecond)
	if err != nil {
		if errors.Is(err, xtensa.ErrTimeout) {
			return nil, status.Errorf(codes.DeadlineExceeded, "core %d did not halt within %d ms: %v", i, ms, err)
		}
		return nil, err
	}
	mine := false
	for _, b := range c.IBreaks() {
		mine = mine || b.Addr == h.PC
	}
	res := map[string]any{"hart": float64(i), "core": float64(i), "cause": float64(h.DebugCause), "halted": true,
		"breakpoint_hit": h.DebugCause&xtensa.CauseIBreak != 0 && mine}
	haltFields(res, h)
	return s.mark(res, true), nil
}

// xtReset resets one core through PWRCTL.CoreReset. Memory and peripherals
// keep their state; the core restarts at its reset vector (halted there with
// halt=true). kind=system has no Xtensa equivalent here, kind=soc goes
// through Plugin.soc and is refused by ensureDM.
func (s *session) xtReset(ctx context.Context, x *xtTarget, args map[string]any) (map[string]any, error) {
	kind, err := strArg(args, "kind")
	if err != nil {
		return nil, err
	}
	switch kind {
	case "hart":
	case "", "system":
		return nil, status.Error(codes.FailedPrecondition, "ESP32-S3 (Xtensa) target: kind=system (RISC-V ndmreset) does not exist here; kind=hart resets the selected core through the debug module's PWRCTL.CoreReset")
	default:
		return nil, argErr(fmt.Errorf("kind %q: want hart, system or soc", kind))
	}
	halt, err := boolArg(args, "halt", false)
	if err != nil {
		return nil, err
	}
	if !s.o.perm.reset {
		return nil, denied("reset needs allow_reset=true in the device configuration")
	}
	c, i, err := s.coreFor(ctx, x, args)
	if err != nil {
		return nil, err
	}
	forgot := len(c.IBreaks())
	start := time.Now()
	if err := c.Reset(ctx, halt); err != nil {
		return nil, err
	}
	x.examined[i] = nil
	if err := x.examine(ctx, i); err != nil {
		return nil, fmt.Errorf("core reset done but the debug module did not come back: %w", err)
	}
	res := map[string]any{
		"kind": "hart", "halt": halt, "usb_dropped": false, "breakpoints_forgotten": float64(forgot),
		"scope": "core reset (Xtensa PWRCTL.CoreReset): the selected core only; memory, peripherals and the other core keep their state",
		"note":  "not validated on real hardware yet: on a chip in ROM download mode a core reset without halt re-runs the ROM boot code",
	}
	if halt {
		h, err := c.WaitHalt(ctx, time.Second)
		if err != nil {
			return nil, status.Errorf(codes.DeadlineExceeded, "core reset requested but core %d did not halt at its reset vector: %v", i, err)
		}
		res["reset_observed"] = true
		haltFields(res, h)
	}
	res["elapsed_ms"] = float64(time.Since(start).Milliseconds())
	for k, v := range xtState(ctx, c, i) {
		res[k] = v
	}
	return s.mark(res, true), nil
}

// xtClose removes this session's breakpoints and lets go of halted cores
// with their scratch registers restored (they stay halted, as on RISC-V).
func (s *session) xtClose(ctx context.Context) {
	for i, c := range s.xt.cores {
		if s.xt.examined[i] == nil {
			continue
		}
		if bps := c.IBreaks(); len(bps) > 0 {
			_, _ = xtWithHalted(ctx, c, func() error {
				for _, b := range bps {
					_ = c.ClearIBreak(ctx, b.Addr)
				}
				return nil
			})
		}
		_ = c.Release(ctx)
	}
}

// xtIdentity is captureIdentity for an ESP32-S3: USB serial, IDCODE, the
// number of cores and core 0's OCDID (no halt; the ROM word needs one).
func (s *session) xtIdentity(ctx context.Context, x *xtTarget) (identity, error) {
	id := identity{USBSerial: s.entry.Dev.Serial, IDCode: x.idcode(s), Harts: len(x.cores)}
	if err := x.examine(ctx, 0); err != nil {
		return identity{}, err
	}
	id.OCDID = x.examined[0].OCDID
	return id, nil
}
