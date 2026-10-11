package targetaccept

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

// RunFirmware checks the acceptance firmware (AcceptanceFirmware) after it was
// programmed: it must boot, run, be debuggable, hit breakpoints and survive
// resets. The generic debug phases are run separately through Run.
func RunFirmware(inv Invoker, o Options, fw Firmware) *Report {
	if o.BreakpointRounds <= 0 {
		o.BreakpointRounds = 5
	}
	r := &runner{inv: inv, o: o, rep: &Report{Backend: o.Backend, Facts: map[string]string{}, PerfMS: map[string]float64{}, Raw: map[string]any{}}}
	defer func() {
		_, _ = r.dp("clear_breakpoint", map[string]any{"all": true})
		_, _ = r.dp("resume", nil)
	}()
	const ph = "firmware"
	if _, err := r.dp("connect", r.connectArgs()); !r.ok(ph, "connect", err == nil, "%v", err) {
		return r.rep
	}
	counter := func() (uint32, error) {
		b, err := r.read(fw.CounterAddr, 4)
		if err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint32(b), nil
	}
	advances := func(label string) bool {
		c1, e1 := counter()
		time.Sleep(60 * time.Millisecond)
		c2, e2 := counter()
		return r.ok(ph, label, e1 == nil && e2 == nil && c2 > c1, "counter %d -> %d (%v %v)", c1, c2, e1, e2)
	}
	advances("application runs: the SRAM counter advances")
	flash, err := r.read(0, len(fw.Image))
	r.ok(ph, "flash content equals the acceptance image", err == nil && bytes.Equal(flash, fw.Image), "%d bytes, err %v", len(flash), err)
	r.ok(ph, "firmware marker is in flash", err == nil && len(flash) >= fw.MarkerOffset+len(fw.Marker) && string(flash[fw.MarkerOffset:fw.MarkerOffset+len(fw.Marker)]) == fw.Marker, "marker %q", fw.Marker)

	if _, err := r.dp("halt", nil); !r.ok(ph, "halt", err == nil, "%v", err) {
		return r.rep
	}
	regs, err := r.regs()
	pc := regs["pc"]
	r.ok(ph, "registers readable and PC is inside the image", err == nil && pc >= fw.ResetHandler && pc < 0x80, "pc %08x err %v", pc, err)
	c1, e1 := counter()
	c2, e2 := counter()
	r.ok(ph, "counter is stable while halted", e1 == nil && e2 == nil && c1 == c2, "%d %d", c1, c2)

	var last uint32
	hits := 0
	for round := 1; round <= o.BreakpointRounds; round++ {
		bp, err := r.dp("set_breakpoint", map[string]any{"address": hexw(fw.BreakpointFunc), "run_wait_ms": 1500.0})
		if err != nil {
			r.add(ph, fmt.Sprintf("breakpoint round %d", round), Fail, "%v", err)
			break
		}
		hpc, _ := parseU32(bp["halted_pc"])
		c, ce := counter()
		ok := bp["hit"] == true && hpc == fw.BreakpointFunc && ce == nil && c > last
		if ok {
			hits++
		}
		r.ok(ph, fmt.Sprintf("breakpoint round %d: bp_func hit, counter advanced", round), ok, "hit=%v pc=%08x counter %d (previous %d)", bp["hit"], hpc, c, last)
		last = c
		if _, err := r.dp("clear_breakpoint", map[string]any{"address": hexw(fw.BreakpointFunc)}); err != nil {
			r.add(ph, fmt.Sprintf("breakpoint round %d clear", round), Fail, "%v", err)
		}
		if _, err := r.dp("resume", nil); err != nil {
			r.add(ph, fmt.Sprintf("breakpoint round %d resume", round), Fail, "%v", err)
		}
		time.Sleep(20 * time.Millisecond)
		if _, err := r.dp("halt", nil); err != nil {
			r.add(ph, fmt.Sprintf("breakpoint round %d halt", round), Fail, "%v", err)
			break
		}
	}
	r.ok(ph, "every breakpoint round hit", hits == o.BreakpointRounds, "%d of %d", hits, o.BreakpointRounds)
	r.fact("firmware.bp_hits", fmt.Sprintf("%d/%d", hits, o.BreakpointRounds))

	for i := 1; i <= 3; i++ {
		_, err := r.dp("reset", map[string]any{"kind": "sysreset", "halt": true})
		regs, rerr := r.regs()
		r.ok(ph, fmt.Sprintf("reset-and-halt %d stops at the application reset handler", i), err == nil && rerr == nil && regs["pc"] == fw.ResetHandler, "pc %08x want %08x (reset %v, regs %v)", regs["pc"], fw.ResetHandler, err, rerr)
		if _, err := r.dp("resume", nil); err != nil {
			r.add(ph, fmt.Sprintf("reset-and-halt %d resume", i), Fail, "%v", err)
		}
		advances(fmt.Sprintf("reset-and-halt %d: application runs after resume", i))
	}
	_, err = r.dp("reset", map[string]any{"kind": "hardware"})
	r.ok(ph, "hardware nRESET", err == nil, "%v", err)
	advances("application runs after nRESET")
	_, err = r.dp("reset", map[string]any{"kind": "sysreset"})
	r.ok(ph, "system reset while running", err == nil, "%v", err)
	advances("application runs after SYSRESETREQ")
	return r.rep
}
