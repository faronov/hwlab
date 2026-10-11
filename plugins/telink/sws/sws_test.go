package sws

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func newPair(t *testing.T) (*Programmer, *Sim) {
	t.Helper()
	s := NewSim()
	p := New(s)
	p.SetTimeout(40 * time.Millisecond)
	return p, s
}

// The reference tool's CRC table is the standard CRC-16/MODBUS one; this is
// the check value of that algorithm.
func TestCRC16Check(t *testing.T) {
	if got := CRC16([]byte("123456789")); got != 0x4b37 {
		t.Fatalf("CRC16 = 0x%04x, want 0x4b37", got)
	}
	f := withCRC([]byte{1, 2, 3})
	if !crcOK(f) {
		t.Fatal("withCRC frame fails crcOK")
	}
	f[1] ^= 1
	if crcOK(f) {
		t.Fatal("corrupted frame passes crcOK")
	}
}

func TestVersionAndConfigure(t *testing.T) {
	p, _ := newPair(t)
	v, err := p.Version()
	if err != nil {
		t.Fatal(err)
	}
	if v.Chip != "TLSR825x" || v.Firmware != 8 || v.SWDiv != 5 || v.AddrLen != 3 || v.SWBuf != DefaultSWBuf {
		t.Fatalf("version %+v", v)
	}
	if v.BitRateMb < 0.95 || v.BitRateMb > 0.97 {
		t.Fatalf("bit rate %v, want 24/5/5 = 0.96", v.BitRateMb)
	}
	if err := p.Configure(4, 3, DefaultSWBuf); err != nil {
		t.Fatal(err)
	}
	if c, _ := p.Cached(); c.SWDiv != 4 {
		t.Fatalf("cached swdiv %d", c.SWDiv)
	}
}

func TestActivateAndTargetID(t *testing.T) {
	p, s := newPair(t)
	if _, _, err := p.ReadChipID(); err == nil {
		t.Fatal("SWire read before activation must fail")
	} else {
		var pe *Error
		if !errors.As(err, &pe) || pe.Code != ErrRead {
			t.Fatalf("want programmer error 3, got %v", err)
		}
	}
	if err := p.SetReset(false); err != nil {
		t.Fatal(err)
	}
	if s.ResetLevel {
		t.Fatal("reset pin should be held low")
	}
	pc, err := p.Activate(200)
	if err != nil {
		t.Fatal(err)
	}
	if pc != 0x1000 {
		t.Fatalf("activation PC 0x%x", pc)
	}
	rev, id, err := p.ReadChipID()
	if err != nil || id != 0x5562 || rev != 3 {
		t.Fatalf("chip id rev=%d id=0x%x err=%v", rev, id, err)
	}
}

func TestNoTargetAndErrors(t *testing.T) {
	p, s := newPair(t)
	s.NoTarget = true
	_, err := p.Activate(100)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != ErrRead {
		t.Fatalf("want SWire timeout error, got %v", err)
	}
	if want := ErrorText(ErrRead); !bytes.Contains([]byte(err.Error()), []byte("SWM-SWS")) || want == "" {
		t.Fatalf("error should hint at wiring: %v", err)
	}
}

func TestTimeoutBadCRCAndDrop(t *testing.T) {
	p, s := newPair(t)
	s.DropNext = 1
	if _, err := p.Version(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	s.BadCRCNext = 1
	if _, err := p.Version(); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("want ErrBadResponse, got %v", err)
	}
	if _, err := p.Version(); err != nil {
		t.Fatalf("recovers after faults: %v", err)
	}
}

func activated(t *testing.T) (*Programmer, *Sim) {
	t.Helper()
	p, s := newPair(t)
	if _, err := p.Version(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Activate(100); err != nil {
		t.Fatal(err)
	}
	return p, s
}

func TestSWireBlocksAndWrite(t *testing.T) {
	p, _ := activated(t)
	data := bytes.Repeat([]byte{0xa5, 0x5a, 0x01}, 1000) // > MaxBlock: chunked
	if err := p.SWWrite(0x840000, data); err != nil {
		t.Fatal(err)
	}
	got, err := p.SWRead(0x840000, len(data))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("readback err=%v equal=%v", err, bytes.Equal(got, data))
	}
}

func TestDebugCycle(t *testing.T) {
	p, s := activated(t)
	if err := p.Stall(); err != nil {
		t.Fatal(err)
	}
	pc1, _ := p.PC()
	pc2, _ := p.PC()
	if pc1 != pc2 {
		t.Fatalf("stalled PC moves: 0x%x 0x%x", pc1, pc2)
	}
	for i := 0; i < 3; i++ {
		if err := p.StepOne(); err != nil {
			t.Fatal(err)
		}
	}
	pc3, _ := p.PC()
	if pc3 != pc1+6 {
		t.Fatalf("after 3 steps PC 0x%x, want 0x%x", pc3, pc1+6)
	}
	r, err := p.ReadRegisters()
	if err != nil || r.R[15] != pc3 || r.R[13] != 0x84fff0 {
		t.Fatalf("registers %+v err=%v", r, err)
	}
	if err := p.SetBreakpoint(0x2468); err != nil {
		t.Fatal(err)
	}
	pc, hit, err := p.WaitPC(0x2468, 200*time.Millisecond)
	if err != nil || !hit || pc != 0x2468 {
		t.Fatalf("breakpoint pc=0x%x hit=%v err=%v", pc, hit, err)
	}
	if err := p.Go(); err != nil {
		t.Fatal(err)
	}
	if ctl, _, steps, _ := s.Stats(); ctl != cpuGo || steps != 3 {
		t.Fatalf("ctl=0x%x steps=%d", ctl, steps)
	}
}

func TestStopVerifiesControlByte(t *testing.T) {
	p, s := activated(t)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	s.Asleep = true
	if _, err := p.PC(); err == nil {
		t.Fatal("a sleeping target must not answer")
	}
}

func TestSoftRebootNeedsActivate(t *testing.T) {
	p, s := activated(t)
	if err := p.SoftReboot(); err != nil {
		t.Fatal(err)
	}
	if _, _, steps, reboots := s.Stats(); reboots != 1 || steps != 0 {
		t.Fatal("no reboot recorded")
	}
	if _, err := p.PC(); err == nil {
		t.Fatal("reads must fail until the target is re-activated")
	}
	if _, err := p.Activate(100); err != nil {
		t.Fatal(err)
	}
}

func TestFlashReadJEDEC(t *testing.T) {
	p, s := newPair(t)
	id, err := p.JEDEC()
	if err != nil || id != 0xef4013 {
		t.Fatalf("jedec 0x%x err=%v", id, err)
	}
	got, err := p.FlashRead(0, 8)
	if err != nil || !bytes.Equal(got, s.Flash()[:8]) {
		t.Fatalf("flash read %x err=%v", got, err)
	}
	big, err := p.FlashRead(0x1000, 3000)
	if err != nil || len(big) != 3000 {
		t.Fatalf("multi-block read len=%d err=%v", len(big), err)
	}
}

func TestProgramWithUnlockAndVerify(t *testing.T) {
	p, s := newPair(t)
	if _, err := p.Version(); err != nil {
		t.Fatal(err)
	}
	s.ProtectFlash()
	s.BusyPolls = 2
	img := make([]byte, 0x1000+300)
	for i := range img {
		img[i] = byte(i*7 + 1)
	}
	for i := 512; i < 768; i++ { // a whole all-FF page is skipped
		img[i] = 0xff
	}
	rep, err := p.Program(tb04(), 0x10000, img, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.UnlockedFlash || rep.SectorsErased != 2 || rep.PagesSkipped != 1 || rep.VerifiedBytes != len(img) {
		t.Fatalf("report %+v", rep)
	}
	if !bytes.Equal(s.Flash()[0x10000:0x10000+len(img)], img) {
		t.Fatal("flash does not hold the image")
	}
	if rep.FlashSizeBytes != 0x80000 {
		t.Fatalf("flash size %d", rep.FlashSizeBytes)
	}
}

func TestProgramRefusesProtectedRegionsBeforeTouchingFlash(t *testing.T) {
	cases := []struct {
		name string
		addr uint32
		n    int
		pol  Policy
	}{
		{"factory eui", 0x76000, 16, Policy{Journals: true}},
		{"factory config", 0x77000, 16, Policy{Journals: true}},
		{"range reaching journals", 0x6f000, 0x2000, Policy{}},
		{"journal without opt in", 0x70000, 16, Policy{}},
		{"range reaching factory through journals", 0x74000, 0x4000, Policy{Journals: true}},
		{"beyond layout", 0x7f000, 16, Policy{Journals: true}},
		{"undescribed tail", 0x78000, 16, Policy{Journals: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, s := newPair(t)
			before := append([]byte(nil), s.Flash()...)
			if _, err := p.Program(tb04(), c.addr&^(FlashSector-1), make([]byte, c.n), c.pol); err == nil {
				t.Fatal("expected refusal")
			}
			if _, err := p.EraseRange(tb04(), c.addr, uint32(c.n), c.pol); err == nil {
				t.Fatal("expected erase refusal")
			}
			if len(s.FlashOps) != 0 {
				t.Fatalf("flash commands were sent despite the refusal: %+v", s.FlashOps)
			}
			if !bytes.Equal(before, s.Flash()) {
				t.Fatal("flash changed")
			}
		})
	}
}

func TestJournalsOnlyWithOptIn(t *testing.T) {
	p, s := newPair(t)
	if _, err := p.Version(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EraseRange(tb04(), 0x70000, 0x2000, Policy{Journals: true}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.Flash()[0x76000:0x76004], []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatal("factory EUI changed")
	}
}

func TestProgramNeedsSectorAlignment(t *testing.T) {
	p, _ := newPair(t)
	if _, err := p.Program(tb04(), 0x10100, []byte{1}, Policy{}); err == nil {
		t.Fatal("unaligned program address must be refused")
	}
}

func TestCapacityMismatchRefusesModify(t *testing.T) {
	p, s := newPair(t)
	l := tb04()
	l.Size = 0x100000
	l.Regions[0].End = 0x70000
	if _, err := p.Program(l, 0x1000, []byte{1}, Policy{}); err == nil {
		t.Fatal("a layout that does not match the JEDEC capacity must be refused")
	}
	if len(s.FlashOps) != 0 {
		t.Fatalf("flash touched: %+v", s.FlashOps)
	}
}

func TestProgramStopsWhenFlashCannotBeUnlocked(t *testing.T) {
	p, s := newPair(t)
	if _, err := p.Version(); err != nil {
		t.Fatal(err)
	}
	s.ProtectFlash()
	s.StuckProtect = true
	rep, err := p.Program(tb04(), 0x20000, []byte{1, 2, 3, 4}, Policy{})
	if err == nil {
		t.Fatal("programming must stop when the unlock does not read back as zero")
	}
	if rep.SectorsErased != 0 {
		t.Fatalf("no sector may be erased after a failed unlock: %+v", rep)
	}
	for _, op := range s.FlashOps {
		if op.Kind == "erase" || op.Kind == "write" {
			t.Fatalf("unexpected op %+v", op)
		}
	}
}

func TestLayoutHelpers(t *testing.T) {
	l := tb04()
	if err := l.CheckModify(0, 0x70000, Policy{}); err != nil {
		t.Fatalf("whole app region should be allowed: %v", err)
	}
	if err := l.CheckModify(0x6ffff, 2, Policy{}); err == nil {
		t.Fatal("one byte into the journal sector must be refused")
	}
	if err := l.CheckCapacity(0xef4013); err != nil {
		t.Fatal(err)
	}
	if err := l.CheckCapacity(0xef4014); err == nil {
		t.Fatal("1 MiB flash must not match the 512 KiB layout")
	}
	if err := l.CheckCapacity(0xef4000); err == nil {
		t.Fatal("capacity code 0 must be refused")
	}
}

// tb04 is a test layout; the real board layouts live in plugins/telink/target.
func tb04() Layout {
	return Layout{Name: "tb04-512k", Size: 0x80000, Regions: []Region{
		{"application", 0x00000, 0x70000, ClassApp},
		{"aps-journal", 0x70000, 0x72000, ClassJournal},
		{"child-journal", 0x72000, 0x74000, ClassJournal},
		{"security-journal", 0x74000, 0x76000, ClassJournal},
		{"factory-eui64", 0x76000, 0x77000, ClassFactory},
		{"factory-config-adc", 0x77000, 0x78000, ClassFactory},
	}}
}

func TestCancelStopsFlashReadAtBlockBoundary(t *testing.T) {
	p, s := activated(t)
	if err := p.Stall(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.SetContext(ctx)
	if _, err := p.FlashRead(0, 64); err != nil {
		t.Fatal(err)
	}
	cancel()
	n := len(s.FlashOps)
	_, err := p.FlashRead(0, 64)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if len(s.FlashOps) != n {
		t.Fatal("a cancelled call must not reach the programmer")
	}
	p.SetContext(nil)
	if _, err := p.FlashRead(0, 64); err != nil {
		t.Fatalf("works again once the context is cleared: %v", err)
	}
}

func TestMalformedAnswers(t *testing.T) {
	for name, fn := range map[string]func(*Sim){
		"bad crc":  func(s *Sim) { s.BadCRCNext = 1 },
		"no reply": func(s *Sim) { s.DropNext = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			p, s := activated(t)
			fn(s)
			if _, err := p.SWRead(0x800000, 8); err == nil {
				t.Fatal("a damaged answer must be an error, not data")
			}
			if _, err := p.SWRead(0x800000, 8); err != nil {
				t.Fatalf("recovers: %v", err)
			}
		})
	}
}

func TestFlashUID(t *testing.T) {
	p, _ := activated(t)
	uid, err := p.FlashUID()
	if err != nil || !bytes.Equal(uid, SimFlashUID[:]) {
		t.Fatalf("%x %v", uid, err)
	}
}
