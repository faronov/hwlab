package espusbjtag

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/faronov/hwlab/internal/xtensa"
	"github.com/faronov/hwlab/internal/xtensa/xtsim"
)

// s3Loop is the download-mode ROM poll loop observed on a real ESP32-S3.
var s3Loop = []uint32{0x40041a76, 0x40041a79, 0x40041a7c}

type s3SimRig struct {
	*opsRig
	core0, core1 *xtsim.Target
}

// newS3 simulates an ESP32-S3 in ROM download mode: two TAPs (core 0 at chain
// position 0 running the ROM loop, core 1 held in reset), the ROM chip word 9
// at 0x40001000 and 64 KiB of DRAM at 0x3FC88000.
func newS3(t *testing.T, cfg map[string]any) *s3SimRig {
	t.Helper()
	rom := make([]byte, 0x2000)
	binary.LittleEndian.PutUint32(rom[0x1000:], 9)
	mk := func(prid uint32, held bool, regions []xtsim.Region) *xtsim.Target {
		return xtsim.New(xtsim.Config{IDCode: 0x120034e5, OCDID: 0x0b339fd2, PRID: prid, Held: held, Loop: s3Loop, Regions: regions})
	}
	c0 := mk(0xCDCD, false, []xtsim.Region{
		{Base: 0x40000000, Data: rom, ReadOnly: true},
		{Base: 0x3FC88000, Data: make([]byte, 0x10000)},
	})
	c1 := mk(0xABAB, true, nil)
	sc := SimConfig()
	sc.Xtensa = &xtsim.Chain{TAPs: []*xtsim.Target{c0, c1}}
	r := newRigCfg(t, sc)
	if cfg != nil {
		if err := r.OpenDevice(context.Background(), wantID, cfg); err != nil {
			t.Fatal(err)
		}
	}
	return &s3SimRig{opsRig: &opsRig{rig: r}, core0: c0, core1: c1}
}

func TestS3ScanAndIdentify(t *testing.T) {
	r := newS3(t, nil)
	sc := r.must(t, "JTAGScan", "scan", nil)
	if devs, _ := sc["devices"].([]any); len(devs) != 2 || sc["total_ir_length"] != 10.0 {
		t.Fatalf("scan %v", sc)
	}
	id := r.must(t, "TargetIdentify", "identify", nil)
	h, _ := id["chip_hint"].(map[string]any)
	if !strings.Contains(h["consistent_with"].(string), "ESP32-S3") {
		t.Fatalf("identify %v", id)
	}
}

func TestS3ChipInfo(t *testing.T) {
	r := newS3(t, nil)
	ci := r.must(t, "DebugProbe", "chip_info", nil)
	if ci["arch"] != "xtensa" || ci["harts"] != 1.0 || ci["cores_on_chain"] != 2.0 || ci["hart_state"] != "running" || ci["idcode"] != "0x120034e5" {
		t.Fatalf("chip_info %v", ci)
	}
	cores := ci["cores"].([]any)
	c0, c1 := cores[0].(map[string]any), cores[1].(map[string]any)
	if c0["ocdid"] != "0x0b339fd2" || c0["available"] != true || c1["available"] != false || c1["state"] != "unavailable" {
		t.Fatalf("cores %v", cores)
	}
	if ci["chip"] != nil || !strings.Contains(ci["chip_profile"].(string), "never halts") || r.core0.Stopped() {
		t.Fatalf("chip_info must not halt: %v", ci)
	}
	r.must(t, "DebugProbe", "halt", nil)
	ci = r.must(t, "DebugProbe", "chip_info", nil)
	if ci["chip"] != "ESP32-S3" || ci["rom_chip_word"] != "0x00000009" || ci["hart_state"] != "halted" {
		t.Fatalf("chip_info halted %v", ci)
	}
	r.must(t, "DebugProbe", "resume", nil)
}

func TestS3HaltRegistersResume(t *testing.T) {
	r := newS3(t, nil)
	r.core0.SetAR(3, 0x1234abcd)
	h := r.must(t, "DebugProbe", "halt", nil)
	if h["halted"] != true || h["already_halted"] != false || !strings.Contains(h["cause_name"].(string), "debug-interrupt") {
		t.Fatalf("halt %v", h)
	}
	pc := h["pc"].(string)
	if pc != "0x40041a76" && pc != "0x40041a79" && pc != "0x40041a7c" {
		t.Fatalf("pc %s", pc)
	}
	if h2 := r.must(t, "DebugProbe", "halt", nil); h2["already_halted"] != true || h2["pc"] != pc {
		t.Fatalf("second halt %v", h2)
	}
	rr := r.must(t, "DebugProbe", "read_registers", nil)
	regs := rr["registers"].(map[string]any)
	if regs["a3"] != "0x1234abcd" || regs["pc"] != pc || rr["was_running"] != false || rr["halted"] != true {
		t.Fatalf("registers %v", rr)
	}
	res := r.must(t, "DebugProbe", "resume", nil)
	if res["resumed"] != true || res["state"] != "running" || r.core0.Stopped() || r.core0.AR(3) != 0x1234abcd {
		t.Fatalf("resume %v (a3 %#x)", res, r.core0.AR(3))
	}
	if res := r.must(t, "DebugProbe", "resume", nil); res["resumed"] != false {
		t.Fatalf("resume of a running core %v", res)
	}
	// A running core is halted for the dump and resumed again.
	rr = r.must(t, "DebugProbe", "read_registers", nil)
	if rr["was_running"] != true || rr["resumed"] != true || r.core0.Stopped() {
		t.Fatalf("registers while running %v", rr)
	}
	rr = r.must(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})
	if rr["halted"] != true || !r.core0.Stopped() {
		t.Fatalf("keep_halted %v", rr)
	}
	r.must(t, "DebugProbe", "resume", nil)
}

func TestS3MemoryRead(t *testing.T) {
	r := newS3(t, nil)
	m := r.must(t, "MemoryAccess", "read", map[string]any{"address": "0x40001000", "length": 4.0})
	if m["hex"] != "09000000" || r.core0.Stopped() || !strings.Contains(m["via"].(string), "resumed") {
		t.Fatalf("read %v", m)
	}
	r.core0.SetMem(0x3FC90001, []byte{0xaa, 0xbb, 0xcc})
	if m := r.must(t, "MemoryAccess", "read", map[string]any{"address": "0x3FC90001", "length": 3.0}); m["hex"] != "aabbcc" {
		t.Fatalf("unaligned read %v", m)
	}
	r.fail(t, codes.FailedPrecondition, "exception", "MemoryAccess", "read", map[string]any{"address": "0x3FF00000", "length": 4.0})
	if r.core0.Stopped() {
		t.Fatal("core left halted after a faulting read")
	}
	if v, _ := r.core0.SR(xtensa.SREXCCAUSE); v != 0 {
		t.Fatalf("EXCCAUSE not restored: %#x", v)
	}
}

func TestS3MemoryWrite(t *testing.T) {
	w := map[string]any{"address": "0x3FC90002", "data": "11223344"}
	newS3(t, nil).fail(t, codes.PermissionDenied, "allow_memory_write", "MemoryAccess", "write", w)
	r := newS3(t, allow)
	for _, a := range []string{"0x40001000", "0x42000000", "0x60008098", "0x3FC00000", "0x3FCFFFFE"} {
		r.fail(t, codes.InvalidArgument, "SRAM", "MemoryAccess", "write", map[string]any{"address": a, "data": "11223344"})
	}
	r.core0.SetMem(0x3FC90000, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	m := r.must(t, "MemoryAccess", "write", w)
	if m["verified"] != true || m["previous_hex"] != "03040506" || m["chip"] != "ESP32-S3" || m["hart_state"] != "running" {
		t.Fatalf("write %v", m)
	}
	if got := r.core0.Mem(0x3FC90000, 8); string(got) != "\x01\x02\x11\x22\x33\x44\x07\x08" || r.core0.Stopped() {
		t.Fatalf("memory % x", got)
	}
	if r.core1.Stopped() {
		t.Fatal("core 1 touched")
	}
}

func TestS3WriteRefusesWrongROMWord(t *testing.T) {
	r := newS3(t, allow)
	r.must(t, "DebugProbe", "halt", nil)
	// Not an ESP32-S3 ROM after all: no profile, no write.
	r.core0.SetMem(0x40001000, []byte{0x55, 0, 0, 0})
	r.fail(t, codes.FailedPrecondition, "refusing", "MemoryAccess", "write", map[string]any{"address": "0x3FC90000", "data": "00"})
	if r.core0.Mem(0x3FC90000, 1)[0] != 0 || !r.core0.Stopped() {
		t.Fatal("write went through or the halted core was resumed")
	}
}

func TestS3Step(t *testing.T) {
	r := newS3(t, nil)
	r.fail(t, codes.FailedPrecondition, "halt it first", "DebugProbe", "step", nil)
	r.must(t, "DebugProbe", "halt", nil)
	st := r.must(t, "DebugProbe", "step", map[string]any{"count": 2.0})
	steps := st["steps"].([]any)
	if st["count"] != 2.0 || len(steps) != 2 || st["halted"] != true || !r.core0.Stopped() {
		t.Fatalf("step %v", st)
	}
	s0 := steps[0].(map[string]any)
	if s0["pc_before"] == s0["pc_after"] || !strings.Contains(s0["cause_name"].(string), "icount") {
		t.Fatalf("step 0 %v", s0)
	}
	r.must(t, "DebugProbe", "resume", nil)
}

func TestS3Breakpoints(t *testing.T) {
	r := newS3(t, map[string]any{})
	r.fail(t, codes.InvalidArgument, "instruction-bus", "DebugProbe", "set_breakpoint", map[string]any{"address": "0x3FC90000"})
	bp := r.must(t, "DebugProbe", "set_breakpoint", map[string]any{"address": "0x40041a7c", "run_wait_ms": 500.0})
	if bp["hit"] != true || bp["halted_pc"] != "0x40041a7c" || bp["slots"] != 2.0 || bp["slot"] != 0.0 {
		t.Fatalf("set_breakpoint %v", bp)
	}
	r.fail(t, codes.AlreadyExists, "already set", "DebugProbe", "set_breakpoint", map[string]any{"address": "0x40041a7c"})
	ls := r.must(t, "DebugProbe", "list_breakpoints", nil)
	slots := ls["slots"].([]any)
	if ls["trigger_slots"] != 2.0 || ls["free_slots"] != 1.0 || slots[0].(map[string]any)["owner"] != "this session" || slots[0].(map[string]any)["ibreaka"] != "0x40041a7c" {
		t.Fatalf("list %v", ls)
	}
	r.must(t, "DebugProbe", "resume", nil)
	wh := r.must(t, "DebugProbe", "wait_halt", map[string]any{"wait_ms": 500.0})
	if wh["breakpoint_hit"] != true || wh["pc"] != "0x40041a7c" {
		t.Fatalf("wait_halt %v", wh)
	}
	r.fail(t, codes.NotFound, "no breakpoint", "DebugProbe", "clear_breakpoint", map[string]any{"address": "0x40041a76"})
	cl := r.must(t, "DebugProbe", "clear_breakpoint", map[string]any{"all": true})
	if len(cl["cleared"].([]any)) != 1 || cl["installed"] != 0.0 {
		t.Fatalf("clear %v", cl)
	}
	if v, _ := r.core0.SR(xtensa.SRIBREAKENABLE); v != 0 {
		t.Fatalf("IBREAKENABLE %#x", v)
	}
	r.must(t, "DebugProbe", "resume", nil)
	r.fail(t, codes.DeadlineExceeded, "did not halt", "DebugProbe", "wait_halt", map[string]any{"wait_ms": 20.0})
}

func TestS3Harts(t *testing.T) {
	r := newS3(t, nil)
	h := r.must(t, "DebugProbe", "harts", nil)
	hs := h["harts"].([]any)
	if h["count"] != 2.0 || hs[0].(map[string]any)["state"] != "running" || hs[1].(map[string]any)["available"] != false {
		t.Fatalf("harts %v", h)
	}
	r.fail(t, codes.FailedPrecondition, "held in reset", "DebugProbe", "halt", map[string]any{"hart": 1.0})
	r.fail(t, codes.InvalidArgument, "2 Xtensa core", "DebugProbe", "halt", map[string]any{"hart": 2.0})
	if r.core0.Stopped() || r.core1.Stopped() {
		t.Fatal("harts halted something")
	}
}

func TestS3RISCVOnlyMethodsRefused(t *testing.T) {
	r := newS3(t, allow)
	// FlashProgramming works on the S3 (s3flash_test.go); on this rig without
	// the watchdog peripherals it fails cleanly on the first access that
	// faults and resumes the core it halted.
	for _, m := range []string{"info", "read"} {
		r.fail(t, codes.FailedPrecondition, "exception", "FlashProgramming", m, map[string]any{"address": "0x0", "length": 16.0})
	}
	r.fail(t, codes.FailedPrecondition, "kind=hart", "DebugProbe", "reset", nil)
	r.fail(t, codes.FailedPrecondition, "kind=hart", "DebugProbe", "reset", map[string]any{"kind": "system"})
	r.fail(t, codes.FailedPrecondition, "Xtensa", "DebugProbe", "reset", map[string]any{"kind": "soc", "confirm": "reset-soc"})
	if r.core0.Stopped() || r.core0.InReset() {
		t.Fatal("a refused method touched the core")
	}
	if f := r.must(t, "DebugProbe", "info", nil); !strings.Contains(f["target_protocol"].(string), "Xtensa") {
		t.Fatalf("info after the Xtensa path was used: %v", f["target_protocol"])
	}
}

func TestS3ResetHart(t *testing.T) {
	newS3(t, nil).fail(t, codes.PermissionDenied, "allow_reset", "DebugProbe", "reset", map[string]any{"kind": "hart"})
	r := newS3(t, allow)
	res := r.must(t, "DebugProbe", "reset", map[string]any{"kind": "hart", "halt": true})
	if res["reset_observed"] != true || res["pc"] != "0x40041a76" || !r.core0.Stopped() {
		t.Fatalf("reset %v", res)
	}
	r.must(t, "DebugProbe", "resume", nil)
}

func TestS3CloseRestoresAndReconnect(t *testing.T) {
	r := newS3(t, map[string]any{})
	r.core0.SetAR(3, 0xfeedf00d)
	r.must(t, "DebugProbe", "halt", nil)
	r.must(t, "MemoryAccess", "read", map[string]any{"address": "0x3FC88000", "length": 8.0})
	r.must(t, "DebugProbe", "set_breakpoint", map[string]any{"address": "0x40041a79"})
	rc := r.must(t, "DebugProbe", "reconnect", nil)
	if rc["identity_verified"] != true || rc["ocdid"] != "0x0b339fd2" || rc["idcode"] != "0x120034e5" || rc["harts"] != 2.0 {
		t.Fatalf("reconnect %v", rc)
	}
	// The old session was closed: breakpoint removed, a3 restored, core
	// still halted (closing never resumes).
	if v, _ := r.core0.SR(xtensa.SRIBREAKENABLE); v != 0 || r.core0.AR(3) != 0xfeedf00d || !r.core0.Stopped() {
		t.Fatalf("after close: IBREAKENABLE %#x a3 %#x stopped %v", v, r.core0.AR(3), r.core0.Stopped())
	}
	r.must(t, "DebugProbe", "resume", nil)
	if err := r.CloseDevice(context.Background(), wantID); err != nil {
		t.Fatal(err)
	}
	if r.core0.Stopped() || r.core0.AR(3) != 0xfeedf00d {
		t.Fatal("state after close")
	}
}
