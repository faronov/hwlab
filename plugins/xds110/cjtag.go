package xds110plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/adiv5/jtagdp"
	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx/service"
	"github.com/faronov/hwlab/internal/ti/icepick"
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

// DebugLocked marks a CC13xx/CC26xx whose ICEPick answers but whose CPU DAP
// does not join the scan chain: the CCFG keeps the debug port closed.
const DebugLocked = "DEBUG_LOCKED"

// dapIdleClocks are the Run-Test/Idle clocks after each AP access over cJTAG,
// so the AHB access completes before the next scan samples its acknowledge.
const dapIdleClocks = 4

// cjtagLink is the attachment through the ICEPick (interface=cjtag).
type cjtagLink struct {
	eng       *xds.ScanEngine
	router    *icepick.Router
	dap       *jtagdp.Link
	icepickID uint32
	dapID     uint32
	format    uint32
}

// lockedError is the attach failure of a chip whose DAP stays off the chain.
type lockedError struct {
	icepickID uint32
	irCapture uint32
	dapID     uint32
}

func (e *lockedError) Error() string {
	return fmt.Sprintf("%s: the ICEPick answers (IDCODE 0x%08X) but the CPU DAP did not join the scan chain after routing (chain IR capture 0x%03X, DAP IDCODE read 0x%08X): the CCFG keeps the CPU DAP closed (CCFG_TAP_DAP_0.CPU_DAP_ENABLE is not 0xC5). Recovery erases the whole chip: DebugProbe.cc26xx_chip_erase",
		DebugLocked, e.icepickID, e.irCapture, e.dapID)
}

func formatName(f uint32) string {
	switch f {
	case xds.FormatOScan1:
		return "OScan1"
	case xds.FormatOScan2:
		return "OScan2"
	case xds.FormatJTAG:
		return "JTAG (4-pin)"
	}
	return fmt.Sprintf("format %d", f)
}

// activateCJTAG puts the target into the two-pin scan format. The probe
// pulses nTRST and clocks TCK for about 50 ms on either side first, which
// returns a target still in a two-pin format to its power-up state (the
// activation sequence expects that state). The cJTAG property is enabled
// when the firmware knows it; firmware 3.0.0.32 answers it with an error
// and activates the two-pin formats anyway.
func (d *device) activateCJTAG(o options) (*xds.ScanEngine, error) {
	_ = d.probe.CJTAGDisconnect()
	var xe *xds.Error
	if err := d.probe.SetProperty(xds.PropertyCJTAG, 1); err != nil && !errors.As(err, &xe) {
		return nil, err
	}
	settle := o.clockHz / 1000 * 50
	for _, step := range []func() error{
		func() error { return d.probe.SetTRST(false) },
		func() error { return d.probe.CycleTCK(settle) },
		func() error { return d.probe.SetTRST(true) },
		func() error { return d.probe.CycleTCK(settle) },
		func() error { return d.probe.CJTAGConnectFormat(o.cjtagFormat) },
	} {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return &xds.ScanEngine{P: d.probe}, nil
}

// noICEPickHint explains an ICEPick that does not answer.
func noICEPickHint(err error) error {
	return status.Errorf(codes.FailedPrecondition, "no TI ICEPick answered over two-pin cJTAG: %v (check TMS->TMSC, TCK->TCKC, GND and VTref wiring and target power; a part that is not a CC13xx/CC26xx needs interface=swd)", err)
}

// attachCJTAG activates the two-pin format, identifies the ICEPick, routes
// the CPU DAP into the chain and brings the JTAG-DP and MEM-AP up. It never
// halts, resets or writes the target.
func (d *device) attachCJTAG(o options) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eng, err := d.activateCJTAG(o)
	if err != nil {
		return d.attachErr(err)
	}
	fail := func(err error) error {
		_ = d.probe.CJTAGDisconnect()
		return err
	}
	r := &icepick.Router{S: eng}
	id, _, err := r.Identify(ctx)
	if err != nil {
		if errors.Is(err, icepick.ErrNoICEPick) {
			return fail(noICEPickHint(err))
		}
		return fail(d.attachErr(err))
	}
	if err := r.LinkTAP(ctx, icepick.PortCPUDAP); err != nil {
		return fail(d.attachErr(err))
	}
	ln, ok, err := r.ProbeLinked(ctx, jtagdp.IRLen, jtagdp.IRIDCode)
	if err != nil {
		return fail(d.attachErr(err))
	}
	if !ok {
		return fail(status.Error(codes.FailedPrecondition, (&lockedError{icepickID: id, irCapture: ln.IRCapture, dapID: ln.IDCode}).Error()))
	}
	link := &jtagdp.Link{S: eng, IRPost: icepick.IRLen, DRPost: 1, IDCode: ln.IDCode, IdleAfterAP: dapIdleClocks}
	t, err := adiv5.Connect(link, o.ap)
	if err != nil {
		return fail(status.Errorf(codes.FailedPrecondition, "the CPU DAP (IDCODE 0x%08X) is on the chain behind the ICEPick, but the JTAG-DP/MEM-AP did not come up: %v", ln.IDCode, err))
	}
	d.tgt, d.core, d.cc, d.attached = t, cortexm.New(adiv5.Bus{T: t}), nil, o
	d.cj = &cjtagLink{eng: eng, router: r, dap: link, icepickID: id, dapID: ln.IDCode, format: o.cjtagFormat}
	d.trig = triggers{}
	return nil
}

// icepickInfo reports the ICEPick and the routing of the CPU DAP without
// touching the core: it works on a chip whose CCFG keeps the DAP closed.
func (d *device) icepickInfo(o options) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.detach(); err != nil {
		return nil, err
	}
	if _, err := d.probe.SetClock(o.clockHz / 1000); err != nil {
		return nil, err
	}
	eng, err := d.activateCJTAG(o)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.probe.CJTAGDisconnect() }()
	r := &icepick.Router{S: eng}
	res := map[string]any{"interface": "cJTAG " + formatName(o.cjtagFormat), "clock_hz": float64(o.clockHz)}
	id, irc, err := r.Identify(ctx)
	res["icepick_ir_capture"] = fmt.Sprintf("0x%02X", irc)
	if err != nil {
		if errors.Is(err, icepick.ErrNoICEPick) {
			return nil, noICEPickHint(err)
		}
		return nil, err
	}
	res["icepick_idcode"] = hex32(id)
	res["icepick_part"] = fmt.Sprintf("0x%04X", id>>12&0xFFFF)
	res["icepick_version"] = float64(id >> 28)
	res["icepick_manufacturer"] = fmt.Sprintf("0x%03X", id>>1&0x7FF)
	code, err := r.Code(ctx)
	if err != nil {
		return nil, err
	}
	res["icepick_code"] = hex32(code)
	if err := r.Connect(ctx); err != nil {
		return nil, err
	}
	if v, err := r.Read(ctx, icepick.BlockControl, icepick.RegControl); err == nil {
		res["control_register_raw"] = hex32(v)
	}
	if v, err := r.Read(ctx, icepick.BlockDebug, icepick.PortCPUDAP); err == nil {
		res["cpu_dap_router_register_before_raw"] = hex32(v)
	}
	if err := r.LinkTAP(ctx, icepick.PortCPUDAP); err != nil {
		return nil, err
	}
	ln, ok, err := r.ProbeLinked(ctx, jtagdp.IRLen, jtagdp.IRIDCode)
	if err != nil {
		return nil, err
	}
	res["chain_ir_capture"] = fmt.Sprintf("0x%03X", ln.IRCapture)
	res["cpu_dap_linked"] = ok
	res["debug_locked"] = !ok
	if ok {
		res["cpu_dap_idcode"] = hex32(ln.IDCode)
		res["note"] = "the CPU DAP joined the chain behind the ICEPick; the core was not touched. interface=cjtag gives full debug and memory access"
	} else {
		res["note"] = (&lockedError{icepickID: id, irCapture: ln.IRCapture, dapID: ln.IDCode}).Error()
	}
	return res, nil
}

// dropLink forgets a cJTAG attachment whose link went down (a reset drops
// the two-pin format); nothing is sent to the target.
func (d *device) dropLink() {
	if d.cj != nil {
		_ = d.probe.CJTAGDisconnect()
	}
	d.tgt, d.core, d.cc, d.cj = nil, nil, nil, nil
	d.trig = triggers{}
}

// resetCJTAG resets a CC13xx/CC26xx reached over cJTAG. A system or pin
// reset can take the two-pin scan format and the ICEPick routing down with
// it, so the attachment is always rebuilt afterwards; the reset is only
// reported when the core shows it (S_RESET_ST, or C_DEBUGEN cleared by the
// reset of the debug logic).
func (d *device) resetCJTAG(ctx context.Context, kind string, halt bool) (map[string]any, error) {
	o := d.attached
	res := map[string]any{"kind": kind, "halt_requested": halt, "interface": "cjtag"}
	if err := d.trig.restoreAll(ctx, d.core); err != nil {
		return nil, fmt.Errorf("debug trigger cleanup before reset: %w", err)
	}
	switch kind {
	case "core":
		rr, err := d.core.Reset(ctx, cortexm.ResetCore, halt)
		if err != nil {
			return nil, err
		}
		res["halted"], res["reset_seen"], res["debug_kept"] = rr.Halted, rr.ResetSeen, rr.DebugKept
		res["reset_vector"], res["at_reset_vector"] = hex32(rr.ResetVector), rr.AtVector
		if rr.Halted {
			res["pc"] = hex32(rr.PC)
		}
		res["reattached"] = false
		return res, nil
	case "sysreset":
		bus := d.core.Bus
		if err := bus.Write32(ctx, cortexm.AddrDHCSR, 0xA05F0000|cortexm.DHCSRDebugEn); err != nil {
			return nil, err
		}
		_ = d.core.ClearCause(ctx)
		if _, err := d.core.Status(ctx); err != nil { // clears a stale S_RESET_ST
			return nil, err
		}
		// The write can be the last scan the two-pin link carries.
		_ = bus.Write32(ctx, cortexm.AddrAIRCR, 0x05FA0000|1<<2)
	case "hardware":
		if err := d.pulseSRST(ctx, 20*time.Millisecond); err != nil {
			return nil, err
		}
	default:
		return nil, argErr(fmt.Errorf("kind %q: want sysreset, core or hardware", kind))
	}
	// A debug port that survives the reset shows S_RESET_ST here (the read
	// clears it); one that went down with it does not answer.
	time.Sleep(20 * time.Millisecond)
	pre, perr := d.core.Status(ctx)
	if perr != nil {
		res["debug_lost"] = true
	}
	d.dropLink()
	time.Sleep(100 * time.Millisecond)
	if err := d.attach(o); err != nil {
		return nil, fmt.Errorf("reset requested; the debug port did not come back over cJTAG: %w", err)
	}
	d.cj.dap.SetContext(ctx)
	st, err := d.core.Status(ctx)
	if err != nil {
		return nil, err
	}
	seen := (perr == nil && pre.ResetSeen) || st.ResetSeen || !st.DebugEnabled
	res["reattached"], res["reset_seen"] = true, seen
	if !seen {
		return nil, fmt.Errorf("re-attached after the %s request, but the core shows no reset (DHCSR 0x%08x): a reset is not proven", kind, st.DHCSR)
	}
	rel, err := d.releaseBoot(ctx, halt)
	if err != nil {
		return nil, err
	}
	for k, v := range rel {
		res[k] = v
	}
	return res, nil
}

// romBase/romEnd bound the CC13xx/CC26xx boot ROM.
const romBase, romEnd = 0x10000000, 0x10040000

// releaseBoot finishes a reset over cJTAG. The TCK activity of the
// re-activation right after the reset makes the CC13xx/CC26xx boot ROM wait
// for the debugger ("halt in boot"): the core spins in ROM with its stack in
// GPRAM until a debugger has halted and released it (observed on a
// CC2652P). The core is halted there and either released into the
// application (halt false, then sampled once to show it left the ROM) or
// run to a breakpoint on the application's reset vector (halt true).
func (d *device) releaseBoot(ctx context.Context, halt bool) (map[string]any, error) {
	res := map[string]any{}
	hs, err := d.core.Halt(ctx)
	if err != nil {
		return nil, err
	}
	pc, err := d.core.ReadRegister(ctx, cortexm.RegPC)
	if err != nil {
		return nil, err
	}
	inROM := pc >= romBase && pc < romEnd
	res["boot_rom_wait"] = inROM
	res["pc_after_reset"] = hex32(pc)
	// In the boot ROM VTOR points at the ROM's own table; the application
	// the ROM starts has its vector table at flash address 0.
	vec, err := d.core.Bus.Read32(ctx, 4)
	if err != nil {
		return nil, err
	}
	vec &^= 1
	res["reset_vector"] = hex32(vec)
	if halt && inROM && vec != 0 && vec < romBase {
		fpb, err := d.core.DiscoverFPB(ctx)
		if err != nil {
			return nil, err
		}
		saved, err := fpb.Save(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := fpb.Set(ctx, vec); err != nil {
			return nil, err
		}
		_ = d.core.ClearCause(ctx)
		_, rerr := d.core.Resume(ctx)
		st, werr := d.core.WaitHalted(ctx, 2*time.Second)
		if err := fpb.Restore(context.WithoutCancel(ctx), saved); err != nil {
			return nil, fmt.Errorf("restore the FPB after the reset vector stop: %w", err)
		}
		if err := errors.Join(rerr, werr); err != nil {
			return nil, fmt.Errorf("run from the boot ROM to the reset vector 0x%08x: %w", vec, err)
		}
		pc, err = d.core.ReadRegister(ctx, cortexm.RegPC)
		if err != nil {
			return nil, err
		}
		res["halted"], res["pc"] = st.Halted, hex32(pc)
		res["at_reset_vector"] = pc&^1 == vec
		if pc&^1 != vec {
			return nil, fmt.Errorf("the core stopped at 0x%08x, not at the reset vector 0x%08x", pc, vec)
		}
		return res, nil
	}
	if halt {
		res["halted"], res["pc"], res["at_reset_vector"] = hs.Halted, hex32(pc), pc&^1 == vec
		if !inROM {
			res["note"] = "the core had already left the boot ROM when it was halted after re-attaching: not stopped at its reset vector"
		}
		return res, nil
	}
	if _, err := d.core.Resume(ctx); err != nil {
		return nil, err
	}
	if !inROM {
		res["halted"] = false
		return res, nil
	}
	// Show that the boot continued: the core must be out of the ROM shortly.
	time.Sleep(200 * time.Millisecond)
	if _, err := d.core.Halt(ctx); err != nil {
		return nil, err
	}
	pc2, err := d.core.ReadRegister(ctx, cortexm.RegPC)
	if err != nil {
		return nil, err
	}
	if _, err := d.core.Resume(ctx); err != nil {
		return nil, err
	}
	res["halted"], res["pc_sample"] = false, hex32(pc2)
	res["left_boot_rom"] = pc2 < romBase || pc2 >= romEnd
	return res, nil
}

// runCJTAG routes a method on a target attached over cJTAG.
func (d *device) runCJTAG(ctx context.Context, method string, args map[string]any) (map[string]any, error) {
	d.cj.dap.SetContext(ctx)
	if method == "DebugProbe.reset" {
		kind, _ := args["kind"].(string)
		if kind == "" {
			kind = "sysreset"
		}
		halt, _, err := optBool(args, "halt")
		if err != nil {
			return nil, argErr(err)
		}
		return d.resetCJTAG(ctx, kind, halt)
	}
	if cc26Handles[method] {
		c, err := d.cc26(ctx)
		if err != nil {
			return nil, err
		}
		switch method {
		case "DebugProbe.chip_info":
			return c.identify()
		case "FlashProgramming.info":
			return c.flashInfo()
		case "FlashProgramming.ccfg_info":
			return c.ccfgInfo()
		case "FlashProgramming.read":
			return c.flashRead(args)
		case "FlashProgramming.verify":
			return c.flashVerify(args)
		case "FlashProgramming.backup":
			return c.flashBackup(args)
		case "FlashProgramming.erase":
			return c.flashErase(args)
		case "FlashProgramming.program":
			return c.flashProgram(args)
		case "FlashProgramming.restore":
			return c.flashRestore(args)
		}
	}
	if capab, name, _ := strings.Cut(method, "."); service.Handles(capab, name) {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is a CC23xx (SWD) method; the target attached over cJTAG is a CC13xx/CC26xx", method)
	}
	return d.coreMethod(ctx, method, args)
}

// lockedChipInfo is chip_info for a chip whose DAP stays closed: what the
// ICEPick tells, and the way out.
func (d *device) lockedChipInfo(o options) (map[string]any, error) {
	res, err := d.icepickInfo(o)
	if err != nil {
		return nil, err
	}
	res["family"] = "CC26xx"
	res["recovery"] = "DebugProbe.cc26xx_chip_erase (device opened with allow_chip_erase=true and interface=cjtag, confirm=\"" + tokICEPickErase + "\") erases flash, NV and CCFG and reopens the debug port; the firmware and its NV data are lost"
	return res, nil
}
