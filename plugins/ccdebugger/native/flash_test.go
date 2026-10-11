package native

import (
	"bytes"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/faronov/hwlab/plugins/ccdebugger/native/flashplan"
	"github.com/faronov/hwlab/plugins/ccdebugger/native/target"
)

func progPerms() Permissions {
	p := allPerms()
	p.FlashErase, p.FlashProgram, p.ChipErase, p.ConfigWrite = true, true, true, true
	return p
}

// progSession returns a reset-attached session with DMA enabled, ready to
// program, backed by the simulator.
func progSession(t *testing.T, p Permissions) (*sim, *Debugger) {
	t.Helper()
	s := newSim()
	c := cfg(ResetDebugSession, p)
	c.Sleep = func(time.Duration) {}
	d := mustNew(t, s, c)
	if _, err := d.AttachReset(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnableDMAAfterReset(); err != nil {
		t.Fatal(err)
	}
	return s, d
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) ^ seed ^ byte(i>>8)
		if b[i] == 0xFF {
			b[i] = 0xFE
		}
	}
	return b
}

func buildPlan(t *testing.T, d *Debugger, lock *target.Lock, addr int, data []byte, mode flashplan.Mode) flashplan.Plan {
	t.Helper()
	info, err := d.ReadChipInfo()
	if err != nil {
		t.Fatal(err)
	}
	if lock == nil {
		l, err := d.ReadLock()
		if err != nil {
			t.Fatal(err)
		}
		lock = &l
	}
	img, err := flashplan.FromBinary(data, addr)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := flashplan.Build(info.Geometry, lock, img, flashplan.Options{Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestChipIdentification(t *testing.T) {
	_, d := progSession(t, progPerms())
	info, err := d.ReadChipInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Chip != "CC2530" || info.FlashSize != 256<<10 || info.RAMSize != 8<<10 || info.PageSize != 2048 ||
		info.Pages != 128 || info.USB || !info.Programmable || info.AdapterTargetID != targetCC2530 {
		t.Fatalf("info = %+v", info)
	}
	if g, ok := d.Geometry(); !ok || g != info.Geometry {
		t.Fatal("geometry not cached")
	}
	id, rev, err := d.ReadChipIDCommand()
	if err != nil || id != 0xA5 || rev != 0x04 {
		t.Fatalf("GET_CHIP_ID = %02x %02x, %v", id, rev, err)
	}
}

func TestChipIdentificationRejectsUnknownAndMalformed(t *testing.T) {
	for name, set := range map[string]func(*sim){
		"unknown chip":  func(s *sim) { s.chipID = 0x00 },
		"zero flash":    func(s *sim) { s.info0 = 0x04 },
		"bad info bit2": func(s *sim) { s.info0 = 0x40 },
		"reserved bit7": func(s *sim) { s.info0 = 0xC4 },
	} {
		s, d := progSession(t, progPerms())
		set(s)
		if _, err := d.ReadChipInfo(); err == nil {
			t.Errorf("%s accepted", name)
		}
		if _, ok := d.Geometry(); ok {
			t.Errorf("%s: geometry cached from bad identity", name)
		}
	}
}

func TestChipIdentificationPreservesCPU(t *testing.T) {
	_, d := progSession(t, progPerms())
	before, err := d.ReadRegisters()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []func() error{
		func() error { _, e := d.ReadChipInfo(); return e },
		func() error { _, e := d.ReadLock(); return e },
		func() error { _, e := d.ReadInfoPage(); return e },
		func() error { _, e := d.ReadIEEE(); return e },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := d.ReadRegisters()
	if err != nil || after != before {
		t.Fatalf("registers changed: %+v -> %+v (%v)", before, after, err)
	}
}

func TestLockIEEEAndInfoPage(t *testing.T) {
	s, d := progSession(t, progPerms())
	lock, err := d.ReadLock()
	if err != nil || lock.DebugLocked || len(lock.LockedPages) != 0 {
		t.Fatalf("lock = %+v, %v", lock, err)
	}
	s.flash[0x3FFF0] = 0xFC // pages 0 and 1 locked
	s.flash[0x3FFFF] = 0x7F // debug lock bit clear
	lock, err = d.ReadLock()
	if err != nil || !lock.DebugLocked || len(lock.LockedPages) != 2 || !lock.PageLocked(1) || lock.PageLocked(2) {
		t.Fatalf("lock = %+v, %v", lock, err)
	}
	ieee, err := d.ReadIEEE()
	if err != nil || !ieee.PrimaryPresent || !ieee.SecondaryPresent {
		t.Fatalf("ieee = %+v, %v", ieee, err)
	}
	if got := target.IEEE(ieee.Secondary); got != "8877665544332211" {
		t.Fatalf("secondary = %s", got)
	}
	if got := target.IEEE(ieee.Primary); got != "0201FFEEDDCCBBAA" {
		t.Fatalf("primary = %s", got)
	}
	page, err := d.ReadInfoPage()
	if err != nil || len(page) != 2048 || !bytes.Equal(page[0x0C:0x14], ieee.Primary) {
		t.Fatalf("info page: len=%d err=%v", len(page), err)
	}
}

func TestReadFlashWholeChipAndBankCrossing(t *testing.T) {
	s, d := progSession(t, progPerms())
	var buf bytes.Buffer
	n, err := d.ReadFlash(0, 256<<10, &buf, nil)
	if err != nil || n != 256<<10 || !bytes.Equal(buf.Bytes(), s.flash) {
		t.Fatalf("full flash read: n=%d err=%v equal=%v", n, err, bytes.Equal(buf.Bytes(), s.flash))
	}
	b, err := d.ReadFlashBytes(0x7F00, 0x300)
	if err != nil || !bytes.Equal(b, s.flash[0x7F00:0x8200]) {
		t.Fatalf("bank crossing read wrong: %v", err)
	}
	for _, c := range [][2]int{{-1, 4}, {0, 0}, {256<<10 - 3, 4}, {256 << 10, 1}} {
		if _, err := d.ReadFlash(c[0], c[1], &bytes.Buffer{}, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("ReadFlash%v: %v", c, err)
		}
	}
	if st := d.State(); st != StateOpen {
		t.Fatalf("state %v", st)
	}
}

func TestProgramReadbackAndChunking(t *testing.T) {
	s, d := progSession(t, progPerms())
	orig := append([]byte(nil), s.flash...)
	img := pattern(5000, 3)
	// 0x1000 starts a page; 5000 bytes cover three pages, the last partly.
	plan := buildPlan(t, d, nil, 0x1000, img, flashplan.PreserveDeviceConfig)
	if len(plan.ErasePages) != 3 || plan.ErasePages[0] != 2 {
		t.Fatalf("erase pages = %v", plan.ErasePages)
	}
	res, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified || res.Verify != VerifyReadback || !res.ResetRequired || res.BytesProgrammed < 5000 {
		t.Fatalf("result = %+v", res)
	}
	if want := len(plan.Blocks); res.BlocksProgrammed != want || s.writeCount != want || s.eraseCount != 3 {
		t.Fatalf("blocks=%d writes=%d erases=%d want %d/%d/3", res.BlocksProgrammed, s.writeCount, s.eraseCount, want, want)
	}
	for _, b := range plan.Blocks {
		if len(b.Data) > flashplan.BlockSize || b.Addr%4 != 0 {
			t.Fatalf("bad block %#x+%d", b.Addr, len(b.Data))
		}
	}
	if !bytes.Equal(s.flash[0x1000:0x1000+5000], img) {
		t.Fatal("flash does not hold the image")
	}
	// The rest of the last erased page is blank; everything else is untouched.
	for i := range s.flash {
		in := i >= 0x1000 && i < 0x1000+3*2048
		switch {
		case in && i >= 0x1000+5000 && s.flash[i] != 0xFF:
			t.Fatalf("tail of erased page not blank at %#x", i)
		case !in && s.flash[i] != orig[i]:
			t.Fatalf("byte outside the plan changed at %#x", i)
		}
	}
}

func TestProgramNeverResumesAndNeedsReset(t *testing.T) {
	_, d := progSession(t, progPerms())
	plan := buildPlan(t, d, nil, 0x2000, pattern(64, 1), flashplan.PreserveDeviceConfig)
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram}); err != nil {
		t.Fatal(err)
	}
	st, err := d.ReadDebugStatus()
	if err != nil || st&CPUHalted == 0 {
		t.Fatalf("target not left halted: %v %v", st, err)
	}
	if _, err := d.Resume(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("resume after flash change: %v", err)
	}
	if _, err := d.Step(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("step after flash change: %v", err)
	}
	if d.State() != StateOpen {
		t.Fatalf("refusal faulted the session: %v", d.State())
	}
	if _, err := d.ResetHalt(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnableDMAAfterReset(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resume(); err != nil {
		t.Fatalf("resume after reset: %v", err)
	}
}

func TestProgramVerifyModes(t *testing.T) {
	for _, mode := range []VerifyMode{VerifyCRC, VerifyBoth} {
		_, d := progSession(t, progPerms())
		plan := buildPlan(t, d, nil, 0x4000, pattern(3000, 9), flashplan.PreserveDeviceConfig)
		res, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram, Verify: mode})
		if err != nil || !res.Verified || res.Verify != mode || res.CRCs != len(plan.Blocks) {
			t.Fatalf("%s: %+v, %v", mode, res, err)
		}
	}
	_, d := progSession(t, progPerms())
	plan := buildPlan(t, d, nil, 0x4000, pattern(100, 9), flashplan.PreserveDeviceConfig)
	res, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram, Verify: VerifyNone})
	if err != nil || res.Verified || res.Verify != VerifyNone {
		t.Fatalf("none: %+v, %v", res, err)
	}
	if _, err := ParseVerify("sometimes"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ParseVerify: %v", err)
	}
	if m, err := ParseVerify(""); err != nil || m != VerifyReadback {
		t.Fatalf("default verify = %q, %v", m, err)
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	s, d := progSession(t, progPerms())
	plan := buildPlan(t, d, nil, 0x4000, pattern(2000, 5), flashplan.PreserveDeviceConfig)
	s.corruptFlashWrite = true
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram}); !errors.Is(err, ErrVerify) {
		t.Fatalf("readback: %v", err)
	}
	if d.State() != StateFaulted {
		t.Fatalf("state %v", d.State())
	}

	s, d = progSession(t, progPerms())
	s.badCRC = true
	plan = buildPlan(t, d, nil, 0x4000, pattern(2000, 5), flashplan.PreserveDeviceConfig)
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram, Verify: VerifyCRC}); !errors.Is(err, ErrVerify) {
		t.Fatalf("crc: %v", err)
	}
}

func TestVerifyFlashStandalone(t *testing.T) {
	s, d := progSession(t, progPerms())
	blocks := []flashplan.Block{{Addr: 0x8000, Data: append([]byte(nil), s.flash[0x8000:0x8400]...)}}
	if n, err := d.VerifyFlash(blocks, VerifyBoth, nil); err != nil || n != 1 {
		t.Fatalf("verify: %d, %v", n, err)
	}
	blocks[0].Data[10] ^= 1
	if _, err := d.VerifyFlash(blocks, VerifyReadback, nil); !errors.Is(err, ErrVerify) {
		t.Fatalf("mismatch: %v", err)
	}
	if d.state != StateOpen {
		t.Fatalf("a read-only verify mismatch must not fault the session, state=%v", d.state)
	}
	if _, err := d.VerifyFlash(blocks, VerifyNone, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("none: %v", err)
	}
	big := []flashplan.Block{{Addr: 0x7C00, Data: make([]byte, 2048)}}
	if _, err := d.VerifyFlash(big, VerifyReadback, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized block: %v", err)
	}
}

func TestProgramDangerousOperationPolicy(t *testing.T) {
	s, d := progSession(t, progPerms())
	plan := buildPlan(t, d, nil, 0x2000, pattern(64, 1), flashplan.PreserveDeviceConfig)
	before := len(s.bulkWrites)
	if _, err := d.Program(plan, ProgramOptions{}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("no confirmation: %v", err)
	}
	if _, err := d.Program(plan, ProgramOptions{Confirm: "yes"}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("wrong confirmation: %v", err)
	}
	if len(s.bulkWrites) != before || s.eraseCount+s.writeCount != 0 {
		t.Fatal("refused program touched the adapter")
	}
	// Permission tiers are independent of confirmation.
	for name, mod := range map[string]func(*Permissions){
		"no flash program": func(p *Permissions) { p.FlashProgram, p.ConfigWrite = false, false },
		"no flash erase": func(p *Permissions) {
			p.FlashErase, p.FlashProgram, p.ChipErase, p.ConfigWrite = false, false, false, false
		},
	} {
		p := progPerms()
		mod(&p)
		if name == "no flash erase" {
			// An invalid combination never starts a session at all.
			p.FlashProgram = true
			if _, err := New(newSim(), cfg(ResetDebugSession, p)); !errors.Is(err, ErrPolicy) {
				t.Fatalf("%s: %v", name, err)
			}
			continue
		}
		s2, d2 := progSession(t, p)
		plan := buildPlan(t, d2, nil, 0x2000, pattern(64, 1), flashplan.PreserveDeviceConfig)
		if _, err := d2.Program(plan, ProgramOptions{Confirm: ConfirmProgram}); !errors.Is(err, ErrPolicy) {
			t.Fatalf("%s: %v", name, err)
		}
		if s2.eraseCount+s2.writeCount != 0 {
			t.Fatalf("%s wrote flash", name)
		}
	}
	if d.State() != StateOpen {
		t.Fatalf("state %v", d.State())
	}
}

func TestProgramRequiresDMAEnabledAndLeavesFlashAlone(t *testing.T) {
	s := newSim()
	c := cfg(ResetDebugSession, progPerms())
	d := mustNew(t, s, c)
	if _, err := d.AttachReset(); err != nil { // DMA still paused
		t.Fatal(err)
	}
	plan := buildPlan(t, d, nil, 0x2000, pattern(64, 1), flashplan.PreserveDeviceConfig)
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("paused DMA: %v", err)
	}
	if s.eraseCount != 0 || s.writeCount != 0 {
		t.Fatal("flash touched with DMA paused")
	}
	if d.State() != StateOpen {
		t.Fatalf("preflight refusal faulted the session: %v", d.State())
	}
}

func TestPlanCheckRejectsBadPlans(t *testing.T) {
	_, d := progSession(t, progPerms())
	good := buildPlan(t, d, nil, 0x2000, pattern(256, 1), flashplan.PreserveDeviceConfig)
	g := good.Geometry
	lock := mustLock(t, d)
	mut := func(f func(*flashplan.Plan)) flashplan.Plan {
		p := good
		p.Blocks = append([]flashplan.Block(nil), good.Blocks...)
		p.ErasePages = append([]int(nil), good.ErasePages...)
		f(&p)
		return p
	}
	cases := map[string]flashplan.Plan{
		"other chip":      mut(func(p *flashplan.Plan) { p.Geometry.ChipID = 0xB5 }),
		"outside flash":   mut(func(p *flashplan.Plan) { p.Blocks[0].Addr = g.FlashSize }),
		"unaligned":       mut(func(p *flashplan.Plan) { p.Blocks[0].Addr++ }),
		"odd length":      mut(func(p *flashplan.Plan) { p.Blocks[0].Data = p.Blocks[0].Data[:255] }),
		"too large":       mut(func(p *flashplan.Plan) { p.Blocks[0].Data = make([]byte, 2048) }),
		"unerased page":   mut(func(p *flashplan.Plan) { p.ErasePages = nil }),
		"empty":           mut(func(p *flashplan.Plan) { p.Blocks = nil }),
		"page oob":        mut(func(p *flashplan.Plan) { p.ErasePages = []int{g.Pages} }),
		"cross page":      mut(func(p *flashplan.Plan) { p.Blocks[0].Addr = 0x2000 - 512; p.Blocks[0].Data = make([]byte, 1024) }),
		"lock page":       mut(func(p *flashplan.Plan) { p.Blocks[0].Addr = g.LockPageAddr(); p.ErasePages = []int{g.LockPage()} }),
		"chip erase mode": mut(func(p *flashplan.Plan) { p.ChipErase = true }),
	}
	for name, p := range cases {
		if err := planCheck(p, g, lock); !errors.Is(err, ErrPolicy) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if err := planCheck(good, g, lock); err != nil {
		t.Fatal(err)
	}
}

func mustLock(t *testing.T, d *Debugger) target.Lock {
	t.Helper()
	l, err := d.ReadLock()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestPreserveDeviceConfigNeverTouchesLockPage(t *testing.T) {
	s, d := progSession(t, progPerms())
	info, _ := d.ReadChipInfo()
	lock := mustLock(t, d)
	img, _ := flashplan.FromBinary(pattern(32, 2), info.LockPageAddr())
	if _, err := flashplan.Build(info.Geometry, &lock, img, flashplan.Options{}); err == nil {
		t.Fatal("default mode accepted a lock-page write")
	}
	// A normal program next to the lock page leaves its 2 KB byte-identical.
	lockPage := append([]byte(nil), s.flash[info.LockPageAddr():]...)
	plan := buildPlan(t, d, nil, info.LockPageAddr()-2048, pattern(2048, 7), flashplan.PreserveDeviceConfig)
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lockPage, s.flash[info.LockPageAddr():]) {
		t.Fatal("lock page changed")
	}
}

func TestPreserveMACKeepsSecondaryIEEE(t *testing.T) {
	s, d := progSession(t, progPerms())
	info, _ := d.ReadChipInfo()
	ieee := append([]byte(nil), s.flash[info.SecondaryIEEEAddr():info.SecondaryIEEEAddr()+8]...)
	lock := mustLock(t, d)
	// An image that rewrites the lock page below the IEEE address only.
	data := bytes.Repeat([]byte{0x5A}, 64)
	plan := buildPlan(t, d, &lock, info.LockPageAddr(), data, flashplan.PreserveMAC)
	if !plan.TouchesConfig || len(plan.Preserve) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("config plan without config confirmation: %v", err)
	}
	if s.eraseCount != 0 {
		t.Fatal("refused config program erased flash")
	}
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram, ConfirmConfig: ConfirmConfig}); err != nil {
		t.Fatal(err)
	}
	if got := s.flash[info.SecondaryIEEEAddr() : info.SecondaryIEEEAddr()+8]; !bytes.Equal(got, ieee) {
		t.Fatalf("secondary IEEE changed: % x", got)
	}
	if !bytes.Equal(s.flash[info.LockPageAddr():info.LockPageAddr()+64], data) {
		t.Fatal("image not written")
	}
	// Overlapping the IEEE address is refused at plan time.
	img, _ := flashplan.FromBinary(make([]byte, 8), info.SecondaryIEEEAddr())
	if _, err := flashplan.Build(info.Geometry, &lock, img, flashplan.Options{Mode: flashplan.PreserveMAC}); err == nil {
		t.Fatal("preserve-mac accepted an IEEE overwrite")
	}
}

func TestConfigPermissionRequired(t *testing.T) {
	p := progPerms()
	p.ConfigWrite = false
	s, d := progSession(t, p)
	info, _ := d.ReadChipInfo()
	lock := mustLock(t, d)
	plan := buildPlan(t, d, &lock, info.LockPageAddr(), bytes.Repeat([]byte{1}, 64), flashplan.FullChipImage)
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram, ConfirmConfig: ConfirmConfig}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("no config permission: %v", err)
	}
	if s.eraseCount != 0 {
		t.Fatal("erased without config permission")
	}
}

func TestLockedPagesAreRefused(t *testing.T) {
	s, d := progSession(t, progPerms())
	s.flash[0x3FFF0] = 0xFB // page 2 locked
	lock := mustLock(t, d)
	info, _ := d.ReadChipInfo()
	img, _ := flashplan.FromBinary(pattern(64, 1), 0x1000)
	if _, err := flashplan.Build(info.Geometry, &lock, img, flashplan.Options{}); err == nil {
		t.Fatal("Build accepted a locked page")
	}
	if _, err := d.ErasePages([]int{2}, ErasePagesOptions{Confirm: ConfirmErasePages}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("ErasePages on a locked page: %v", err)
	}
	if s.eraseCount != 0 {
		t.Fatal("locked page erase was attempted")
	}
	// A stale plan is caught against the live lock bits.
	s.flash[0x3FFF0] = 0xFF
	plan := buildPlan(t, d, nil, 0x1000, pattern(64, 1), flashplan.PreserveDeviceConfig)
	s.flash[0x3FFF0] = 0xFB
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("stale plan: %v", err)
	}
	if s.eraseCount != 0 || s.writeCount != 0 {
		t.Fatal("stale plan wrote flash")
	}
}

func TestProgramRefusesLockedTarget(t *testing.T) {
	s, d := progSession(t, progPerms())
	plan := buildPlan(t, d, nil, 0x1000, pattern(64, 1), flashplan.PreserveDeviceConfig)
	s.locked = true
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram}); err == nil {
		t.Fatal("programmed a debug-locked target")
	}
	if s.eraseCount+s.writeCount != 0 {
		t.Fatal("flash touched")
	}
}

func TestErasePages(t *testing.T) {
	s, d := progSession(t, progPerms())
	if _, err := d.ErasePages([]int{3}, ErasePagesOptions{}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("no confirmation: %v", err)
	}
	res, err := d.ErasePages([]int{4, 3}, ErasePagesOptions{Confirm: ConfirmErasePages})
	if err != nil || !res.BlankCheck || len(res.Pages) != 2 || res.Pages[0] != 3 {
		t.Fatalf("erase: %+v, %v", res, err)
	}
	for i := 3 * 2048; i < 5*2048; i++ {
		if s.flash[i] != 0xFF {
			t.Fatalf("byte %#x not erased", i)
		}
	}
	if s.flash[3*2048-1] == 0xFF && s.flash[5*2048] == 0xFF {
		t.Fatal("neighbouring pages were erased")
	}
	for name, pages := range map[string][]int{
		"empty": nil, "negative": {-1}, "lock page": {127}, "past end": {128}, "duplicate": {3, 3},
	} {
		if _, err := d.ErasePages(pages, ErasePagesOptions{Confirm: ConfirmErasePages}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEraseFailureClasses(t *testing.T) {
	s, d := progSession(t, progPerms())
	s.flashAbort = true
	if _, err := d.ErasePages([]int{3}, ErasePagesOptions{Confirm: ConfirmErasePages}); !errors.Is(err, ErrEraseFailed) {
		t.Fatalf("abort: %v", err)
	}
	s, d = progSession(t, progPerms())
	s.flashBusyForever = true
	if _, err := d.ErasePages([]int{3}, ErasePagesOptions{Confirm: ConfirmErasePages}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("busy forever: %v", err)
	}
	if d.State() != StateFaulted {
		t.Fatalf("state %v", d.State())
	}
}

func TestWriteFailureClasses(t *testing.T) {
	s, d := progSession(t, progPerms())
	plan := buildPlan(t, d, nil, 0x3000, pattern(64, 1), flashplan.PreserveDeviceConfig)
	s.writeAbort = true
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram}); !errors.Is(err, ErrWriteFailed) {
		t.Fatalf("controller abort: %v", err)
	}
	if d.State() != StateFaulted {
		t.Fatalf("state %v", d.State())
	}
}

func TestChipEraseRequiresConfirmation(t *testing.T) {
	s, d := progSession(t, progPerms())
	before := len(s.bulkWrites)
	for _, tok := range []string{"", "yes", ConfirmProgram} {
		if _, err := d.ChipErase(ChipEraseOptions{Confirm: tok}); !errors.Is(err, ErrPolicy) {
			t.Fatalf("token %q: %v", tok, err)
		}
	}
	if len(s.bulkWrites) != before || s.chipEraseSent != 0 {
		t.Fatal("refused chip erase reached the adapter")
	}
	p := progPerms()
	p.ChipErase = false
	s2, d2 := progSession(t, p)
	if _, err := d2.ChipErase(ChipEraseOptions{Confirm: ConfirmChipErase}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("no chip-erase permission: %v", err)
	}
	if s2.chipEraseSent != 0 {
		t.Fatal("chip erase sent without permission")
	}
}

func TestChipEraseLeavesTargetHaltedAndDetached(t *testing.T) {
	s, d := progSession(t, progPerms())
	s.locked = true // an erase must be possible on a debug-locked target
	res, err := d.ChipErase(ChipEraseOptions{Confirm: ConfirmChipErase})
	if err != nil {
		t.Fatal(err)
	}
	if !res.LockedBefore || res.StatusAfter&(DebugLocked|ChipEraseBusy) != 0 || res.PollCount < 3 || !res.ReattachFirst {
		t.Fatalf("result = %+v", res)
	}
	for _, v := range s.flash {
		if v != 0xFF {
			t.Fatal("flash not erased")
		}
	}
	if !s.halted {
		t.Fatal("chip erase resumed the target")
	}
	for _, w := range s.bulkWrites {
		if len(w) == 2 && w[1] == cmdResume {
			t.Fatal("resume sent")
		}
	}
	if _, err := d.ReadDebugStatus(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("session usable without re-attach: %v", err)
	}
	if _, ok := d.Geometry(); ok {
		t.Fatal("geometry kept across chip erase")
	}
	if _, err := d.AttachReset(); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
}

func TestChipEraseFailureClasses(t *testing.T) {
	s, d := progSession(t, progPerms())
	s.eraseNever = true
	if _, err := d.ChipErase(ChipEraseOptions{Confirm: ConfirmChipErase}); !errors.Is(err, ErrTimeout) || errors.Is(err, ErrDisconnected) {
		t.Fatalf("timeout: %v", err)
	}
	if d.State() != StateFaulted {
		t.Fatalf("state %v", d.State())
	}

	s, d = progSession(t, progPerms())
	s.eraseDisconnect = true
	if _, err := d.ChipErase(ChipEraseOptions{Confirm: ConfirmChipErase}); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("disconnect: %v", err)
	}

	s, d = progSession(t, progPerms())
	s.locked, s.eraseKeepsLock = true, true
	if _, err := d.ChipErase(ChipEraseOptions{Confirm: ConfirmChipErase}); !errors.Is(err, ErrEraseFailed) {
		t.Fatalf("still locked: %v", err)
	}

	s, d = progSession(t, progPerms())
	s.erasePolls = 2 // an erase is already running
	before := s.chipEraseSent
	if _, err := d.ChipErase(ChipEraseOptions{Confirm: ConfirmChipErase}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("already busy: %v", err)
	}
	if s.chipEraseSent != before {
		t.Fatal("erase command sent while busy")
	}
	if d.State() != StateOpen {
		t.Fatalf("busy refusal faulted the session: %v", d.State())
	}
}

func TestDisconnectMidProgram(t *testing.T) {
	s, d := progSession(t, progPerms())
	plan := buildPlan(t, d, nil, 0x1000, pattern(3000, 4), flashplan.PreserveDeviceConfig)
	s.failBulkWriteAt = len(s.bulkWrites) + 60
	s.failErr = syscall.ENODEV
	_, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram})
	if !errors.Is(err, ErrDisconnected) {
		t.Fatalf("err = %v", err)
	}
	if d.State() != StateFaulted {
		t.Fatalf("state %v", d.State())
	}
	if _, err := d.ReadDebugStatus(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("session reusable after disconnect: %v", err)
	}
}

func TestMalformedReplyAbortsProgram(t *testing.T) {
	s, d := progSession(t, progPerms())
	plan := buildPlan(t, d, nil, 0x1000, pattern(300, 4), flashplan.PreserveDeviceConfig)
	s.shortReplyAt = s.bulkReads + 40
	if _, err := d.Program(plan, ProgramOptions{Confirm: ConfirmProgram}); err == nil {
		t.Fatal("short reply accepted")
	}
	if d.State() != StateFaulted {
		t.Fatalf("state %v", d.State())
	}
}

func TestWriteDeviceConfig(t *testing.T) {
	s, d := progSession(t, progPerms())
	info, _ := d.ReadChipInfo()
	newIEEE := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if _, err := d.WriteDeviceConfig(ConfigChange{SecondaryIEEE: newIEEE}, ConfigOptions{}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("no confirmation: %v", err)
	}
	if _, err := d.WriteDeviceConfig(ConfigChange{SecondaryIEEE: newIEEE}, ConfigOptions{Confirm: ConfirmConfig}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("no backup: %v", err)
	}
	failing := func([]byte) error { return errors.New("disk full") }
	if _, err := d.WriteDeviceConfig(ConfigChange{SecondaryIEEE: newIEEE}, ConfigOptions{Confirm: ConfirmConfig, BackupBefore: failing}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("failed backup: %v", err)
	}
	if s.eraseCount != 0 {
		t.Fatal("flash erased although the backup failed")
	}
	var saved []byte
	res, err := d.WriteDeviceConfig(ConfigChange{SecondaryIEEE: newIEEE}, ConfigOptions{
		Confirm: ConfirmConfig, BackupBefore: func(b []byte) error { saved = b; return nil },
	})
	if err != nil || !res.Verified {
		t.Fatalf("write: %+v, %v", res, err)
	}
	if len(saved) != 2048 || !bytes.Equal(saved[2048-24:2048-16], []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}) {
		t.Fatal("backup does not hold the previous page")
	}
	if got := s.flash[info.SecondaryIEEEAddr() : info.SecondaryIEEEAddr()+8]; !bytes.Equal(got, newIEEE) {
		t.Fatalf("IEEE = % x", got)
	}
	if got := s.flash[info.LockAddr():]; !bytes.Equal(got, bytes.Repeat([]byte{0xFF}, 16)) {
		t.Fatalf("lock bytes changed: % x", got)
	}
}

func TestDebugLockNeedsItsOwnConfirmation(t *testing.T) {
	s, d := progSession(t, progPerms())
	lockBytes := bytes.Repeat([]byte{0xFF}, 16)
	lockBytes[15] = 0x7F
	opt := ConfigOptions{Confirm: ConfirmConfig, BackupBefore: func([]byte) error { return nil }}
	if _, err := d.WriteDeviceConfig(ConfigChange{Lock: lockBytes}, opt); !errors.Is(err, ErrPolicy) {
		t.Fatalf("debug lock without confirmation: %v", err)
	}
	if s.eraseCount != 0 {
		t.Fatal("flash erased before the debug-lock confirmation")
	}
	opt.ConfirmLock = ConfirmDebugLock
	if _, err := d.WriteDeviceConfig(ConfigChange{Lock: lockBytes}, opt); err != nil {
		t.Fatal(err)
	}
	if s.flash[0x3FFFF] != 0x7F {
		t.Fatalf("lock byte = %#x", s.flash[0x3FFFF])
	}
	for name, ch := range map[string]ConfigChange{
		"empty": {}, "short IEEE": {SecondaryIEEE: []byte{1}}, "short lock": {Lock: []byte{1}},
	} {
		if _, err := d.WriteDeviceConfig(ch, opt); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestPipelinedReadMatchesSingleInstructionRead(t *testing.T) {
	s, d := progSession(t, progPerms())
	want, err := d.ReadFlashCode(0x8100, 200)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadFlashBytes(0x8100, 200)
	if err != nil || !bytes.Equal(got, want) || !bytes.Equal(got, s.flash[0x8100:0x8100+200]) {
		t.Fatalf("pipelined read differs: %v", err)
	}
}
