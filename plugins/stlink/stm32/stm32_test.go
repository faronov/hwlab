package stm32_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

func setup(t *testing.T, kb int) (*stm32.SimF4, *cortexm.Core, *stm32.Profile, *stm32.Flash) {
	t.Helper()
	ctx := context.Background()
	s := stm32.NewSimF4(kb)
	c := cortexm.New(s.Sim)
	p, err := stm32.Detect(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	return s, c, p, stm32.NewFlash(c, p, 3300)
}

func TestDetectLayouts(t *testing.T) {
	for _, tc := range []struct{ kb, sectors int }{{256, 6}, {512, 8}, {1024, 12}} {
		_, _, p, _ := setup(t, tc.kb)
		if len(p.Sectors) != tc.sectors {
			t.Errorf("%d KB: %d sectors, want %d", tc.kb, len(p.Sectors), tc.sectors)
		}
		var total uint32
		for i, s := range p.Sectors {
			if i == 0 && s.Addr != stm32.FlashBase {
				t.Errorf("first sector at 0x%x", s.Addr)
			}
			total += s.Size
		}
		if total != uint32(tc.kb)<<10 || !p.Programmable || p.Family != stm32.F4 {
			t.Errorf("%d KB: total 0x%x programmable %v family %s", tc.kb, total, p.Programmable, p.Family)
		}
	}
}

func TestNotSTM32(t *testing.T) {
	// The attached nRF52840 reads 0 at DBGMCU_IDCODE.
	c := cortexm.New(cortexm.NewSim())
	_, err := stm32.Detect(context.Background(), c)
	if !errors.Is(err, stm32.ErrNotSTM32) {
		t.Fatalf("got %v", err)
	}
}

func TestUnsupportedVariants(t *testing.T) {
	s := stm32.NewSimF4(1024)
	s.DevID = 0x20006419
	_, err := stm32.Detect(context.Background(), cortexm.New(s.Sim))
	if !errors.Is(err, stm32.ErrUnsupported) {
		t.Fatalf("dual-bank F42x must be refused: %v", err)
	}
	s.DevID = 0x20006413
	s.FlashKB = 4096 // implausible for this id
	_, err = stm32.Detect(context.Background(), cortexm.New(s.Sim))
	if !errors.Is(err, stm32.ErrNotSTM32) {
		t.Fatalf("implausible flash size must not be accepted: %v", err)
	}
}

func TestEraseProgramVerify(t *testing.T) {
	ctx := context.Background()
	s, _, p, f := setup(t, 512)
	img := make([]byte, 3000)
	for i := range img {
		img[i] = byte(i*7 + 1)
	}
	sec := p.Sectors[0]
	if err := f.EraseSector(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if err := f.Program(ctx, sec.Addr, img[:len(img)&^3]); err != nil {
		t.Fatal(err)
	}
	got, err := f.Read(ctx, sec.Addr, len(img)&^3)
	if err != nil || !bytes.Equal(got, img[:len(img)&^3]) {
		t.Fatalf("read back differs: %v", err)
	}
	if s.Erases != 1 || s.Programs == 0 {
		t.Fatalf("erases %d programs %d", s.Erases, s.Programs)
	}
	// The controller is locked again.
	cr, _ := s.Read32(ctx, 0x40023C10)
	if cr&(1<<31) == 0 {
		t.Fatal("controller left unlocked")
	}
}

func TestProgramNeedsErase(t *testing.T) {
	ctx := context.Background()
	_, _, p, f := setup(t, 256)
	a := p.Sectors[1].Addr
	if err := f.Program(ctx, a, []byte{0x00, 0x00, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	// Setting bits again without an erase cannot work; it must be reported.
	err := f.Program(ctx, a, []byte{0xFF, 0x01, 0x00, 0x00})
	if !errors.Is(err, stm32.ErrVerify) {
		t.Fatalf("want ErrVerify, got %v", err)
	}
}

func TestCorruptedProgramIsDetected(t *testing.T) {
	ctx := context.Background()
	s, _, p, f := setup(t, 256)
	s.CorruptProgram = true
	err := f.Program(ctx, p.Sectors[0].Addr, bytes.Repeat([]byte{0x5A}, 64))
	if !errors.Is(err, stm32.ErrVerify) {
		t.Fatalf("corruption must fail verification: %v", err)
	}
}

func TestBusyIsWaitedFor(t *testing.T) {
	ctx := context.Background()
	s, _, p, f := setup(t, 256)
	s.BusyReads = 5
	if err := f.EraseSector(ctx, p.Sectors[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.Program(ctx, p.Sectors[0].Addr, bytes.Repeat([]byte{0x11}, 16)); err != nil {
		t.Fatal(err)
	}
}

func TestProtectionRefused(t *testing.T) {
	ctx := context.Background()
	s, _, p, f := setup(t, 256)
	s.NWRP = 0xFFF &^ 1 // sector 0 write-protected
	if err := f.EraseSector(ctx, p.Sectors[0]); !errors.Is(err, stm32.ErrProtected) {
		t.Fatalf("erase of a write-protected sector: %v", err)
	}
	if err := f.Program(ctx, p.Sectors[0].Addr, make([]byte, 4)); !errors.Is(err, stm32.ErrProtected) {
		t.Fatalf("program into a write-protected sector: %v", err)
	}
	s.NWRP = 0xFFF
	s.RDP = 0x55 // level 1
	if err := f.EraseSector(ctx, p.Sectors[1]); !errors.Is(err, stm32.ErrProtected) {
		t.Fatalf("readout protection must stop every modification: %v", err)
	}
	if s.Erases != 0 {
		t.Fatalf("a refused call erased %d sectors", s.Erases)
	}
}

func TestRangeAndAlignment(t *testing.T) {
	ctx := context.Background()
	_, _, p, f := setup(t, 256)
	end := stm32.FlashBase + p.FlashSize()
	if _, err := f.Read(ctx, end-4, 8); !errors.Is(err, stm32.ErrRange) {
		t.Fatalf("read past the end: %v", err)
	}
	if err := f.Program(ctx, end, make([]byte, 4)); !errors.Is(err, stm32.ErrRange) {
		t.Fatalf("program past the end: %v", err)
	}
	if err := f.Program(ctx, stm32.FlashBase+2, make([]byte, 4)); !errors.Is(err, stm32.ErrAlign) {
		t.Fatalf("unaligned program: %v", err)
	}
}

func TestNeedsHaltedCore(t *testing.T) {
	ctx := context.Background()
	_, c, p, f := setup(t, 256)
	if _, err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.EraseSector(ctx, p.Sectors[0]); err == nil {
		t.Fatal("erase while the core runs must be refused")
	}
}

func TestLowVoltageRefused(t *testing.T) {
	ctx := context.Background()
	_, _, p, f := setup(t, 256)
	f.VoltageMV = 2300
	if err := f.EraseSector(ctx, p.Sectors[0]); err == nil {
		t.Fatal("x32 operations below 2.7 V must be refused")
	}
}
