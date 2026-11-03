//go:build hardware

package espusbjtag

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/xtensa"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// Opt-in for an ESP32-S3 in ROM download mode:
//
//	HWLAB_ESPJTAG_HW=1 HWLAB_ESPJTAG_CHIP=s3 go test -tags hardware ./plugins/espusbjtag/ -run S3 -v
//
// Nothing here writes flash or eFuses or resets the chip. The only memory
// write (TestHWS3SRAMWrite) goes to a DRAM word that is saved and restored.

func s3Rig(t *testing.T) (hw, *session) {
	t.Helper()
	if os.Getenv("HWLAB_ESPJTAG_CHIP") != "s3" {
		t.Skip("set HWLAB_ESPJTAG_CHIP=s3 to run the ESP32-S3 hardware tests")
	}
	h := hwRig(t)
	if err := h.OpenDevice(context.Background(), h.id, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.CloseDevice(context.Background(), h.id) })
	h.mu.Lock()
	s := h.devs[h.id]
	h.mu.Unlock()
	return h, s
}

func s3Cores(t *testing.T, s *session) []*xtensa.Core {
	t.Helper()
	ctx := context.Background()
	c, err := s.ensureChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("chain: status %s, %d TAP(s), total IR %d", c.Status, len(c.Devices), c.TotalIR)
	var cores []*xtensa.Core
	irl := make([]int, len(c.Devices))
	for i := range irl {
		irl[i] = xtensa.IRLen
	}
	for i := range c.Devices {
		dm, err := xtensa.NewDM(s.drv, irl, i)
		if err != nil {
			t.Fatal(err)
		}
		cores = append(cores, xtensa.NewCore(dm, 6))
	}
	return cores
}

// TestHWS3Probe powers up the debug domains of both TAPs and reads OCDID,
// PWRSTAT, DSR and DCR. It does not halt.
func TestHWS3Probe(t *testing.T) {
	_, s := s3Rig(t)
	ctx := context.Background()
	for i, c := range s3Cores(t, s) {
		t0 := time.Now()
		in, err := c.Examine(ctx)
		t.Logf("TAP %d: OCDID 0x%08x PWRCTL(prev) 0x%02x PWRSTAT 0x%02x -> 0x%02x DSR 0x%08x DCR 0x%08x (%v) err=%v",
			i, in.OCDID, in.PwrCtlPrev, in.PwrStat, in.PwrStatNow, in.DSR, in.DCR, time.Since(t0), err)
	}
}

// TestHWS3HaltResume halts core 0 for as short a time as possible, reads its
// identity registers, the watchdog configuration and the ROM chip word, and
// resumes.
func TestHWS3HaltResume(t *testing.T) {
	_, s := s3Rig(t)
	ctx := context.Background()
	c := s3Cores(t, s)[0]
	if _, err := c.Examine(ctx); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if err := c.DM.WriteReg(ctx, xtensa.RegDCRSET, xtensa.DCREnableOCD|xtensa.DCRDebugInterrupt); err != nil {
		t.Fatal(err)
	}
	var dsr uint32
	for i := 0; i < 100; i++ {
		v, err := c.DM.ReadReg(ctx, xtensa.RegDSR)
		if err != nil {
			t.Fatal(err)
		}
		if dsr = v; v&xtensa.DSRStopped != 0 {
			break
		}
	}
	dcr, _ := c.DM.ReadReg(ctx, xtensa.RegDCRSET)
	t.Logf("after debug interrupt: DSR 0x%08x DCR 0x%08x (%v)", dsr, dcr, time.Since(t0))
	h, err := c.Halt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("halt: %+v", h)
	regs, err := c.ReadRegisters(ctx, append(append([]string{}, xtensa.CoreRegisters...), "epc1", "depc", "ps_debug", "configid0", "configid1", "excsave1", "atomctl", "sr182", "sr198", "sr183", "sr199"))
	if err != nil {
		t.Error(err)
	}
	for _, r := range regs {
		t.Logf("  %-12s 0x%08x %s", r.Name, r.Value, r.Error)
	}
	for _, a := range []uint32{0x40001000, 0x60008098, 0x6000809C, 0x600080B0, 0x600080B4, 0x600080B8, 0x6001F048, 0x6001F064, 0x60020048, 0x60020064, 0x600C0000} {
		w, err := c.ReadWords(ctx, a, 1)
		if err != nil {
			t.Logf("  [%#x] error %v", a, err)
			continue
		}
		t.Logf("  [%#x] = 0x%08x", a, w[0])
	}
	dcr, _ = c.DM.ReadReg(ctx, xtensa.RegDCRSET)
	t.Logf("DCR before resume 0x%08x, halted for %v", dcr, time.Since(t0))
	if err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(ctx)
	t.Logf("after resume: %+v err=%v (total %v)", st, err, time.Since(t0))
	if st.Halted {
		t.Error("core still halted after resume")
	}
}

func logRegs(t *testing.T, c *xtensa.Core, names ...string) map[string]uint32 {
	t.Helper()
	regs, err := c.ReadRegisters(context.Background(), names)
	if err != nil {
		t.Error(err)
	}
	m := map[string]uint32{}
	for _, r := range regs {
		t.Logf("  %-12s 0x%08x %s", r.Name, r.Value, r.Error)
		m[r.Name] = r.Value
	}
	return m
}

// TestHWS3CoreOps exercises the generic Core directly: a faulting read and
// its side effects, an SRAM word write with read-back and restore, single
// steps and an instruction breakpoint on the download-mode ROM loop.
func TestHWS3CoreOps(t *testing.T) {
	_, s := s3Rig(t)
	ctx := context.Background()
	c := s3Cores(t, s)[0]
	if _, err := c.Examine(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := c.Halt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("halt: pc 0x%08x ps 0x%08x cause %v", h.PC, h.PS, h.Causes)
	defer func() {
		if c.Halted() {
			if err := c.Resume(ctx); err != nil {
				t.Error(err)
			}
		}
		st, err := c.Status(ctx)
		t.Logf("final status %+v err=%v", st, err)
	}()

	exc := []string{"exccause", "excvaddr", "epc1", "depc", "a3"}
	t.Log("exception registers before the faulting read:")
	before := logRegs(t, c, exc...)
	for _, a := range []uint32{0x00000000, 0x10000000, 0x3FC80000} {
		t0 := time.Now()
		_, err := c.ReadWords(ctx, a, 2)
		t.Logf("read 2 words at %#x: err=%v (%v)", a, err, time.Since(t0))
	}
	t.Log("exception registers after (live, before restore):")
	after := logRegs(t, c, exc...)
	for _, n := range exc {
		if before[n] != after[n] {
			t.Logf("  %s changed by the faulting read: 0x%08x -> 0x%08x", n, before[n], after[n])
		}
	}
	if w, err := c.ReadWords(ctx, 0x40000000, 4); err != nil {
		t.Errorf("read after fault: %v", err)
	} else {
		t.Logf("ROM 0x40000000: %08x", w)
	}

	// SRAM write: one DRAM word plus a byte-granular write, restored after.
	const addr = 0x3FC90000
	orig, err := c.ReadMem(ctx, addr, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("DRAM %#x original % x", addr, orig)
	pat := []uint32{0xA5A55A5A, 0x12345678}
	if err := c.WriteWords(ctx, addr, pat); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadWords(ctx, addr, 2)
	t.Logf("after word write %08x err=%v", got, err)
	if err != nil || got[0] != pat[0] || got[1] != pat[1] {
		t.Errorf("word read-back mismatch")
	}
	if err := c.WriteMem(ctx, addr+1, []byte{0xDE, 0xAD, 0xBE}); err != nil {
		t.Fatal(err)
	}
	b, err := c.ReadMem(ctx, addr, 8)
	t.Logf("after byte write at +1: % x err=%v", b, err)
	if want := []byte{0x5A, 0xDE, 0xAD, 0xBE, 0x78, 0x56, 0x34, 0x12}; string(b) != string(want) {
		t.Errorf("byte write read-back % x, want % x", b, want)
	}
	if err := c.WriteMem(ctx, addr, orig); err != nil {
		t.Fatal(err)
	}
	b, _ = c.ReadMem(ctx, addr, 8)
	if string(b) != string(orig) {
		t.Errorf("restore failed: % x", b)
	}

	// Single steps.
	for i := 0; i < 4; i++ {
		t0 := time.Now()
		st, err := c.Step(ctx)
		t.Logf("step %d: pc 0x%08x cause %v err=%v (%v)", i, st.PC, st.Causes, err, time.Since(t0))
		if err != nil {
			t.Fatal(err)
		}
	}
	logRegs(t, c, "icount", "icountlevel", "ibreakenable", "a3")

	// Breakpoint on the current PC (the ROM poll loop passes it again).
	n, err := c.IBreakSlots(ctx)
	t.Logf("IBREAK slots: %d err=%v", n, err)
	pc, _ := c.ReadRegister(ctx, "pc")
	bp, err := c.SetIBreak(ctx, pc)
	t.Logf("set breakpoint %+v err=%v", bp, err)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		if err := c.Resume(ctx); err != nil {
			t.Fatal(err)
		}
		hh, err := c.WaitHalt(ctx, 2*time.Second)
		t.Logf("breakpoint hit %d: pc 0x%08x cause %v err=%v (%v)", i, hh.PC, hh.Causes, err, time.Since(t0))
		if err != nil {
			t.Fatal(err)
		}
		if hh.PC != pc {
			t.Errorf("stopped at 0x%08x, want 0x%08x", hh.PC, pc)
		}
	}
	if err := c.ClearIBreak(ctx, pc); err != nil {
		t.Error(err)
	}
	logRegs(t, c, "ibreakenable", "ibreaka0", "ibreaka1", "a3")

	// Make the faulting read dirty EXCCAUSE etc. again, resume (which must
	// write them back), halt and compare.
	_, _ = c.ReadWords(ctx, 0x10000000, 1)
	if err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("exception registers after resume + halt (must equal the originals):")
	again := logRegs(t, c, exc...)
	for _, n := range exc {
		if n != "a3" && again[n] != before[n] {
			t.Errorf("%s not restored: 0x%08x, want 0x%08x", n, again[n], before[n])
		}
	}
}

// ---- plugin level (the methods an MCP client calls) ----

func s3Plugin(t *testing.T, cfg map[string]any) hw {
	t.Helper()
	if os.Getenv("HWLAB_ESPJTAG_CHIP") != "s3" {
		t.Skip("set HWLAB_ESPJTAG_CHIP=s3 to run the ESP32-S3 hardware tests")
	}
	h := hwRig(t)
	if err := h.OpenDevice(context.Background(), h.id, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.CloseDevice(context.Background(), h.id) })
	return h
}

func (h hw) callErr(capName, method string, args map[string]any) (map[string]any, error) {
	s, _ := structpb.NewStruct(args)
	res, err := h.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: h.id, Capability: capName, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return res.Result.AsMap(), nil
}

func s3Running(t *testing.T, h hw) {
	t.Helper()
	ci := h.call(t, "DebugProbe", "chip_info", nil)
	if ci["hart_state"] != "running" {
		t.Fatalf("core 0 is %v after the test", ci["hart_state"])
	}
}

// TestHWS3PluginReadOnly: scan, identify, chip_info and harts. Nothing halts.
func TestHWS3PluginReadOnly(t *testing.T) {
	h := s3Plugin(t, nil)
	sc := h.call(t, "JTAGScan", "scan", nil)
	id := h.call(t, "TargetIdentify", "identify", nil)
	ci := h.call(t, "DebugProbe", "chip_info", nil)
	hs := h.call(t, "DebugProbe", "harts", nil)
	t.Logf("scan %v\nidentify chip_hint %v\nchip_info %v\nharts %v", sc["devices"], id["chip_hint"], ci, hs["harts"])
	if ci["arch"] != "xtensa" || ci["idcode"] != "0x120034e5" || ci["hart_state"] != "running" {
		t.Fatalf("chip_info %v", ci)
	}
	if _, err := h.callErr("DebugProbe", "halt", map[string]any{"hart": 1.0}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("halt core 1: %v", err)
	} else {
		t.Logf("halt core 1 refused: %v", err)
	}
	s3Running(t, h)
}

// TestHWS3PluginDebug: halt, chip profile, registers, memory reads (ROM word
// and memory-map edges), step, a breakpoint in the download-mode ROM loop,
// resume.
func TestHWS3PluginDebug(t *testing.T) {
	h := s3Plugin(t, map[string]any{})
	hl := h.call(t, "DebugProbe", "halt", nil)
	t.Logf("halt %v", hl)
	defer func() { _, _ = h.callErr("DebugProbe", "resume", nil) }()
	ci := h.call(t, "DebugProbe", "chip_info", nil)
	if ci["chip"] != "ESP32-S3" || ci["rom_chip_word"] != "0x00000009" {
		t.Fatalf("chip profile %v", ci)
	}
	rr := h.call(t, "DebugProbe", "read_registers", nil)
	t.Logf("registers %v errors %v", rr["registers"], rr["register_errors"])
	if regs := rr["registers"].(map[string]any); regs["pc"] != hl["pc"] {
		t.Errorf("pc %v != halt pc %v", regs["pc"], hl["pc"])
	}
	rd := func(addr string, n float64) (string, error) {
		m, err := h.callErr("MemoryAccess", "read", map[string]any{"address": addr, "length": n, "allow_peripheral": strings.HasPrefix(addr, "0x6")})
		if err != nil {
			return "", err
		}
		return m["hex"].(string), nil
	}
	if v, err := rd("0x40001000", 4); err != nil || v != "09000000" {
		t.Fatalf("ROM word %s %v", v, err)
	}
	// Observed on the real chip: the holes after ROM1 and after SRAM2 read as
	// zero without a debug exception, so the profile's memory map (not a
	// fault) is what keeps writes out of them.
	for _, e := range []struct {
		addr, want string
	}{{"0x4005FFFC", ""}, {"0x40060000", "00000000"}, {"0x3FC88000", ""}, {"0x3FCFFFFC", ""}, {"0x3FD00000", "00000000"}, {"0x40370000", ""}, {"0x403DFFFC", ""}, {"0x600FE000", ""}, {"0x50000000", ""}} {
		v, err := rd(e.addr, 4)
		t.Logf("read %s: %s err=%v", e.addr, v, err)
		if err != nil || (e.want != "" && v != e.want) {
			t.Errorf("%s: %s %v", e.addr, v, err)
		}
	}
	for _, p := range [][2]string{{"0x3FF00000", "0x40040000"}, {"0x3FC88000", "0x40378000"}} {
		a, ea := rd(p[0], 32)
		b, eb := rd(p[1], 32)
		t.Logf("alias %s=%s (%v) %s=%s (%v) equal=%v", p[0], a, ea, p[1], b, eb, ea == nil && a == b)
		if ea != nil || eb != nil || a != b {
			t.Errorf("%s and %s are not aliases", p[0], p[1])
		}
	}
	st := h.call(t, "DebugProbe", "step", map[string]any{"count": 3.0})
	t.Logf("step %v", st["steps"])
	bp := h.call(t, "DebugProbe", "set_breakpoint", map[string]any{"address": "0x40041a7c", "run_wait_ms": 500.0})
	t.Logf("set_breakpoint %v", bp)
	if bp["hit"] != true {
		t.Errorf("breakpoint not hit: %v", bp)
	}
	t.Logf("list %v", h.call(t, "DebugProbe", "list_breakpoints", nil))
	h.call(t, "DebugProbe", "clear_breakpoint", map[string]any{"all": true})
	t0 := time.Now()
	res := h.call(t, "DebugProbe", "resume", nil)
	t.Logf("resume %v (%v)", res, time.Since(t0))
	if _, err := h.callErr("DebugProbe", "wait_halt", map[string]any{"wait_ms": 50.0}); status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("wait_halt after clearing: %v", err)
	}
	s3Running(t, h)
}

// TestHWS3PluginSRAMWrite writes 8 bytes of DRAM through MemoryAccess.write
// (allow_memory_write), checks them and writes the original bytes back.
func TestHWS3PluginSRAMWrite(t *testing.T) {
	h := s3Plugin(t, map[string]any{"allow_memory_write": true})
	const addr = "0x3FC90000"
	if _, err := h.callErr("MemoryAccess", "write", map[string]any{"address": "0x40001000", "data": "00"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ROM write: %v", err)
	}
	h.call(t, "DebugProbe", "halt", nil)
	defer func() { _, _ = h.callErr("DebugProbe", "resume", nil) }()
	w := h.call(t, "MemoryAccess", "write", map[string]any{"address": addr, "data": "a55a3cc3"})
	orig := w["previous_hex"].(string)
	t.Logf("write %v", w)
	h2 := h.call(t, "MemoryAccess", "write", map[string]any{"address": "0x3FC90001", "data": "11"})
	if h2["previous_hex"] != "5a" {
		t.Errorf("byte write previous %v", h2["previous_hex"])
	}
	back := h.call(t, "MemoryAccess", "write", map[string]any{"address": addr, "data": orig})
	if back["previous_hex"] != "a5113cc3" {
		t.Errorf("before restore %v", back["previous_hex"])
	}
	m := h.call(t, "MemoryAccess", "read", map[string]any{"address": addr, "length": 4.0})
	if m["hex"] != orig {
		t.Fatalf("restore: %v != %s", m["hex"], orig)
	}
	t.Logf("restored %s", orig)
	h.call(t, "DebugProbe", "resume", nil)
	s3Running(t, h)
}
