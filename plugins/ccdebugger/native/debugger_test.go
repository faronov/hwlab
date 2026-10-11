package native

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func cfg(a Access, p Permissions) Config {
	return Config{Access: a, Permissions: p, Timeout: 5 * time.Second}
}

func allPerms() Permissions {
	return Permissions{CPUControl: true, TargetReset: true, MemoryAccess: true, MemoryWrite: true, Breakpoints: true, DMAEnable: true}
}

func mustNew(t *testing.T, s *sim, c Config) *Debugger {
	t.Helper()
	d, err := New(s, c)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestConfigPolicy(t *testing.T) {
	for name, c := range map[string]Config{
		"adapter-only with grant":  cfg(AdapterOnly, Permissions{MemoryAccess: true}),
		"reset without reset perm": cfg(ResetDebugSession, Permissions{MemoryAccess: true}),
		"write without access":     cfg(ExistingDebugSession, Permissions{MemoryWrite: true}),
		"dma without reset":        cfg(ExistingDebugSession, Permissions{DMAEnable: true}),
		"bad access":               cfg("whatever", Permissions{}),
		"zero timeout":             {Access: AdapterOnly},
		"timeout too long":         {Access: AdapterOnly, Timeout: time.Minute + time.Second},
	} {
		if _, err := New(newSim(), c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(newSim(), cfg(AdapterOnly, Permissions{})); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAccess("nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ParseAccess: %v", err)
	}
	if a, _ := ParseAccess(""); a != AdapterOnly {
		t.Fatalf("empty access = %q, want adapter-only", a)
	}
}

func TestAdapterOnlyNeverTouchesTarget(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(AdapterOnly, Permissions{}))
	st, err := d.ReadAdapterState()
	if err != nil || st.TargetID != targetCC2530 || st.FirmwareVersion != 5 || st.FirmwareRev != 9 {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if _, err := d.ReadDebugStatus(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("status: %v", err)
	}
	if _, err := d.ReadPC(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("pc: %v", err)
	}
	if _, err := d.AttachReset(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("attach: %v", err)
	}
	if len(s.bulkWrites) != 0 || len(s.ctrlWrites) != 0 {
		t.Fatalf("adapter-only sent traffic: bulk=%d ctrl=%d", len(s.bulkWrites), len(s.ctrlWrites))
	}
	if d.State() != StateOpen {
		t.Fatalf("policy refusals must not fault the session, state=%v", d.State())
	}
}

func TestExistingSessionPermissionsAreIndependent(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{MemoryAccess: true}))
	if _, err := d.Halt(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("halt without cpu control: %v", err)
	}
	if _, err := d.ResetHalt(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("reset without reset permission: %v", err)
	}
	if err := d.WriteXDATA(0, []byte{1}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("write without write permission: %v", err)
	}
	if _, err := d.SetBreakpoint(0, 0x100, true); !errors.Is(err, ErrPolicy) {
		t.Fatalf("breakpoint without permission: %v", err)
	}
	if _, err := d.EnableDMAAfterReset(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("dma without permission: %v", err)
	}
	if len(s.bulkWrites) != 0 {
		t.Fatal("refused operations sent target commands")
	}
	if _, err := d.ReadXDATA(0x100, 4); err != nil {
		t.Fatalf("permitted read: %v", err)
	}
}

func TestAttachResetSequence(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(ResetDebugSession, allPerms()))
	if _, err := d.ReadPC(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("target op before attach: %v", err)
	}
	res, err := d.AttachReset()
	if err != nil || res.StatusAfter != resetStatus || !res.ResetSent {
		t.Fatalf("attach = %+v, %v", res, err)
	}
	if len(s.ctrlWrites) != 3 {
		t.Fatalf("control writes = %+v", s.ctrlWrites)
	}
	c5, c8, c9 := s.ctrlWrites[0], s.ctrlWrites[1], s.ctrlWrites[2]
	if c5.req != 0xC5 || c8.req != 0xC8 || c8.value != 1 || c9.req != 0xC9 || c9.value != 0 || c9.index != 1 || len(c9.data) != 0 {
		t.Fatalf("control writes = %+v", s.ctrlWrites)
	}
	want := bytes.Repeat([]byte{' '}, 48)
	copy(want, "CC2530")
	copy(want[16:], "DID:")
	copy(want[21:], "0102")
	if !bytes.Equal(c8.data, want) {
		t.Fatalf("chip info = %q", c8.data)
	}
	if _, err := d.AttachReset(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("second attach: %v", err)
	}
	pc, err := d.ReadPC()
	if err != nil || pc != 0 {
		t.Fatalf("pc = %d, %v", pc, err)
	}
}

func TestAttachNeedsResetPolicy(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(ExistingDebugSession, allPerms()))
	if _, err := d.AttachReset(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("attach on existing session: %v", err)
	}
}

func TestOnlyReviewedResetFormIsEverSent(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(ResetDebugSession, allPerms()))
	d.AttachReset()
	d.ResetHalt()
	d.EnableDMAAfterReset()
	d.Resume()
	for _, c := range s.ctrlWrites {
		if c.req == 0xC9 && !(c.reqType == 0x40 && c.value == 0 && c.index == 1 && len(c.data) == 0) {
			t.Fatalf("unreviewed reset request sent: %+v", c)
		}
	}
}

func TestWrongTargetSendsNothing(t *testing.T) {
	s := newSim()
	s.targetID = 0x2541
	d := mustNew(t, s, cfg(ExistingDebugSession, allPerms()))
	if _, err := d.ReadPC(); !errors.Is(err, ErrTransport) {
		t.Fatalf("pc: %v", err)
	}
	if len(s.bulkWrites) != 0 {
		t.Fatal("target commands sent to a non-CC2530")
	}
	if d.State() != StateFaulted {
		t.Fatalf("state = %v", d.State())
	}
	d2 := mustNew(t, s, cfg(ResetDebugSession, allPerms()))
	if _, err := d2.AttachReset(); !errors.Is(err, ErrTransport) {
		t.Fatalf("attach: %v", err)
	}
	if len(s.ctrlWrites) != 0 {
		t.Fatalf("attach sent control writes to a non-CC2530: %+v", s.ctrlWrites)
	}
}

func TestEnableDMAThenResume(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(ResetDebugSession, allPerms()))
	if _, err := d.AttachReset(); err != nil {
		t.Fatal(err)
	}
	cfgv, err := d.ReadDebugConfig()
	if err != nil || cfgv != resetConfig {
		t.Fatalf("config = %#x, %v", cfgv, err)
	}
	cfgv, err = d.EnableDMAAfterReset()
	if err != nil || cfgv != dmaEnabledConfig || s.config != dmaEnabledConfig {
		t.Fatalf("dma = %#x, %v (sim %#x)", cfgv, err, s.config)
	}
	if _, err := d.EnableDMAAfterReset(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("second DMA enable must be refused: %v", err)
	}
	res, err := d.Resume()
	if err != nil || res.StatusAfter&CPUHalted != 0 || s.halted {
		t.Fatalf("resume = %+v, %v", res, err)
	}
}

func TestDMAEligibilityLostByExecutionOrReads(t *testing.T) {
	for name, op := range map[string]func(*Debugger){
		"step":   func(d *Debugger) { d.Step() },
		"resume": func(d *Debugger) { d.Resume() },
	} {
		s := newSim()
		d := mustNew(t, s, cfg(ResetDebugSession, allPerms()))
		d.AttachReset()
		op(d)
		if _, err := d.EnableDMAAfterReset(); !errors.Is(err, ErrPolicy) {
			t.Errorf("%s: DMA enable after execution: %v", name, err)
		}
	}
	s := newSim()
	d := mustNew(t, s, cfg(ResetDebugSession, allPerms()))
	d.AttachReset()
	s.config = 0x02 // someone changed the debug configuration
	if _, err := d.ReadDebugConfig(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnableDMAAfterReset(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("DMA enable after config drift: %v", err)
	}
}

func TestDMAEnableOnlyForNormalResetState(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(ResetDebugSession, allPerms()))
	d.AttachReset()
	s.config = 0x06
	if _, err := d.EnableDMAAfterReset(); !errors.Is(err, ErrTransport) {
		t.Fatalf("unexpected config accepted: %v", err)
	}
}

func TestReadRegistersPreservesAndMatches(t *testing.T) {
	s := newSim()
	s.sfr[0xF0-0x80] = 0x11
	s.sfr[0x92-0x80] = 0x00
	d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{MemoryAccess: true}))
	r, err := d.ReadRegisters()
	if err != nil {
		t.Fatal(err)
	}
	if r.A != 0x5A || r.B != 0x11 || r.SP != 7 || r.DPTR0 != 0x1234 || r.R != [8]uint8{1, 2, 3, 4, 5, 6, 7, 8} || r.PC != 0 {
		t.Fatalf("registers = %+v", r)
	}
	if s.a != 0x5A || s.dptr0 != 0x1234 {
		t.Fatalf("CPU state disturbed: a=%#x dptr=%#x", s.a, s.dptr0)
	}
}

func TestReadSFRAllowList(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{MemoryAccess: true}))
	if v, err := d.ReadSFR(0x81); err != nil || v != 7 {
		t.Fatalf("SP = %d, %v", v, err)
	}
	if _, err := d.ReadSFR(0x88); !errors.Is(err, ErrInvalid) {
		t.Fatalf("peripheral SFR: %v", err)
	}
	if s.a != 0x5A {
		t.Fatal("accumulator not restored")
	}
}

func TestReadXDATAAndCode(t *testing.T) {
	s := newSim()
	copy(s.xdata[0x100:], []byte{1, 2, 3, 4, 5})
	d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{MemoryAccess: true}))
	got, err := d.ReadXDATA(0x100, 5)
	if err != nil || !bytes.Equal(got, []byte{1, 2, 3, 4, 5}) {
		t.Fatalf("xdata = %v, %v", got, err)
	}
	code, err := d.ReadCode(0x10, 8)
	if err != nil || !bytes.Equal(code, s.flash[0x10:0x18]) {
		t.Fatalf("code = %x, %v", code, err)
	}
	if s.dptr0 != 0x1234 || s.a != 0x5A {
		t.Fatalf("state not restored: dptr=%#x a=%#x", s.dptr0, s.a)
	}
	for _, c := range []struct{ addr, n int }{{-1, 1}, {0x2000, 1}, {0x1FFF, 2}, {0, 0}, {0, 257}} {
		if _, err := d.ReadXDATA(c.addr, c.n); !errors.Is(err, ErrInvalid) {
			t.Errorf("ReadXDATA(%#x,%d): %v", c.addr, c.n, err)
		}
	}
	if _, err := d.ReadCode(0x7FFF, 2); !errors.Is(err, ErrInvalid) {
		t.Errorf("code across 0x8000: %v", err)
	}
}

func TestReadFlashCodeBanked(t *testing.T) {
	s := newSim()
	s.sfr[0x9F-0x80] = 0x01
	d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{MemoryAccess: true}))
	for _, addr := range []int{0x8000, 0x18010, 0x3E7F0} {
		got, err := d.ReadFlashCode(addr, 16)
		if err != nil {
			t.Fatalf("%#x: %v", addr, err)
		}
		if !bytes.Equal(got, s.flash[addr:addr+16]) {
			t.Errorf("%#x: got %x want %x", addr, got, s.flash[addr:addr+16])
		}
		if s.sfr[0x9F-0x80] != 0x01 {
			t.Fatalf("FMAP not restored after %#x: %#x", addr, s.sfr[0x9F-0x80])
		}
	}
	if got, err := d.ReadFlashCode(0x10, 4); err != nil || !bytes.Equal(got, s.flash[0x10:0x14]) {
		t.Fatalf("bank0 = %x, %v", got, err)
	}
	if _, err := d.ReadFlashCode(0x7FFF, 2); !errors.Is(err, ErrInvalid) {
		t.Errorf("bank crossing: %v", err)
	}
	if _, err := d.ReadFlashCode(FlashSize-1, 2); !errors.Is(err, ErrInvalid) {
		t.Errorf("past flash end: %v", err)
	}
	s.sfr[0xC7-0x80] = 0x09 // XMAP set
	if _, err := d.ReadFlashCode(0x8000, 4); !errors.Is(err, ErrTransport) {
		t.Errorf("XMAP overlay accepted: %v", err)
	}
}

func TestWriteXDATA(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{MemoryAccess: true, MemoryWrite: true}))
	if err := d.WriteXDATA(0x200, []byte{9, 8, 7}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.xdata[0x200:0x203], []byte{9, 8, 7}) {
		t.Fatalf("xdata = %v", s.xdata[0x200:0x203])
	}
	if s.a != 0x5A || s.dptr0 != 0x1234 {
		t.Fatal("CPU state not restored after write")
	}
	if err := d.WriteXDATA(WriteLimitXDATA-1, []byte{1, 2}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("write past limit: %v", err)
	}
	if err := d.WriteXDATA(0, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty write: %v", err)
	}
	s.corruptSFR = true
	if err := d.WriteXDATA(0x300, []byte{1}); !errors.Is(err, ErrTransport) {
		t.Fatalf("readback mismatch: %v", err)
	}
	if d.State() != StateFaulted {
		t.Fatal("a readback mismatch must fault the session")
	}
}

func TestBreakpoint(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{Breakpoints: true}))
	res, err := d.SetBreakpoint(2, 0x0123, true)
	if err != nil || res.Slot != 2 || res.Address != 0x123 || !res.Enabled {
		t.Fatalf("bp = %+v, %v", res, err)
	}
	found := false
	for _, w := range s.bulkWrites {
		found = found || bytes.Equal(w, []byte{0xAF, cmdSetHWBreak, 0x28, 0x01, 0x23})
	}
	if !found {
		t.Fatalf("breakpoint packet not sent: %x", s.bulkWrites)
	}
	if _, err := d.SetBreakpoint(4, 0, true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("slot 4: %v", err)
	}
	if _, err := d.SetBreakpoint(0, 0x8000, true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("banked address: %v", err)
	}
}

func TestHaltResumeStep(t *testing.T) {
	s := newSim()
	s.halted = false
	d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{CPUControl: true}))
	r, err := d.Halt()
	if err != nil || !r.CommandSent || r.StatusAfter&CPUHalted == 0 {
		t.Fatalf("halt = %+v, %v", r, err)
	}
	if r, err = d.Halt(); err != nil || r.CommandSent {
		t.Fatalf("halting a halted CPU must send nothing: %+v, %v", r, err)
	}
	st, err := d.Step()
	if err != nil || st.Accumulator != 0x5A {
		t.Fatalf("step = %+v, %v", st, err)
	}
	if r, err = d.Resume(); err != nil || s.halted {
		t.Fatalf("resume = %+v, %v", r, err)
	}
	if _, err := d.Step(); !errors.Is(err, ErrTransport) {
		t.Fatalf("step while running: %v", err)
	}
}

func TestLockedTargetRefused(t *testing.T) {
	s := newSim()
	s.locked = true
	d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{MemoryAccess: true}))
	if _, err := d.ReadXDATA(0, 1); !errors.Is(err, ErrTransport) {
		t.Fatalf("locked target: %v", err)
	}
}

func TestFailuresFaultTheSession(t *testing.T) {
	cases := map[string]func(*sim){
		"write failure": func(s *sim) { s.failBulkWriteAt = 2 },
		"short reply":   func(s *sim) { s.shortReplyAt = 2 },
	}
	for name, setup := range cases {
		s := newSim()
		setup(s)
		d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{MemoryAccess: true}))
		if _, err := d.ReadRegisters(); !errors.Is(err, ErrTransport) {
			t.Fatalf("%s: %v", name, err)
		}
		if d.State() != StateFaulted {
			t.Fatalf("%s: state %v", name, d.State())
		}
		writes := len(s.bulkWrites)
		if _, err := d.ReadPC(); !errors.Is(err, ErrPolicy) {
			t.Fatalf("%s: faulted session accepted a request: %v", name, err)
		}
		if len(s.bulkWrites) != writes {
			t.Fatalf("%s: faulted session sent traffic", name)
		}
	}
}

func TestDeadline(t *testing.T) {
	s := newSim()
	s.slowWrite = 15 * time.Millisecond
	c := cfg(ExistingDebugSession, Permissions{MemoryAccess: true})
	c.Timeout = 20 * time.Millisecond
	d := mustNew(t, s, c)
	if _, err := d.ReadXDATA(0, 16); !errors.Is(err, ErrTransport) {
		t.Fatalf("deadline: %v", err)
	}
}

func TestReentrantCallRefused(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(ExistingDebugSession, Permissions{MemoryAccess: true}))
	var inner error
	err := d.do(nil, func(*deadline) error {
		_, inner = d.ReadAdapterState()
		return nil
	})
	if err != nil || !errors.Is(inner, ErrPolicy) {
		t.Fatalf("outer %v, inner %v", err, inner)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s := newSim()
	d := mustNew(t, s, cfg(AdapterOnly, Permissions{}))
	if err := d.Close(); err != nil || !s.closed {
		t.Fatalf("close: %v closed=%v", err, s.closed)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadAdapterState(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("closed session: %v", err)
	}
}

func TestStatusDecode(t *testing.T) {
	m := Status(0x22).Decode()
	if m["cpu_halted"] != true || m["oscillator_stable"] != true || m["debug_locked"] != false {
		t.Fatalf("decode = %v", m)
	}
	if DecodeConfig(0x26)["dma_pause"] != true || DecodeConfig(0x22)["dma_pause"] != false {
		t.Fatal("config decode")
	}
}
