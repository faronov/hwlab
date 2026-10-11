package stm32_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

func setupF1(t *testing.T, kb int) (*stm32.SimF1, *cortexm.Core, *stm32.Profile, *stm32.Flash) {
	t.Helper()
	ctx := context.Background()
	s := stm32.NewSimF1(kb)
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

func image(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13 + 5)
	}
	return b
}

func TestF1Detect(t *testing.T) {
	_, _, p, _ := setupF1(t, 128)
	if p.Family != stm32.F1 || p.DevID != 0x410 || p.RevID != 0x2003 || p.FlashKB != 128 || p.FlashSizeSource != "register" ||
		!p.Programmable || len(p.Sectors) != 128 || p.PageSize != 1024 || p.SRAMKB != 20 {
		t.Fatalf("%+v", p)
	}
}

func TestF1DetectBlankSizeRegisterIsProbed(t *testing.T) {
	for _, kb := range []int{64, 128} {
		s := stm32.NewSimF1(kb)
		s.SizeReg = 0xFFFFFFFF
		p, err := stm32.Detect(context.Background(), cortexm.New(s.Sim))
		if err != nil {
			t.Fatal(err)
		}
		if p.FlashKB != kb || p.FlashSizeSource != "probed" || !bytes.Contains([]byte(p.Note), []byte("upper bound")) {
			t.Errorf("%d KB: %+v", kb, p)
		}
	}
	// A register value beyond the family maximum is still refused.
	s := stm32.NewSimF1(64)
	s.SizeReg = 4000
	if _, err := stm32.Detect(context.Background(), cortexm.New(s.Sim)); !errors.Is(err, stm32.ErrNotSTM32) {
		t.Fatalf("implausible size accepted: %v", err)
	}
}

func TestF1EraseProgramVerifyRunsTheLoader(t *testing.T) {
	ctx := context.Background()
	s, c, p, f := setupF1(t, 64)
	before, _ := c.ReadAll(ctx)
	sramBefore, _ := c.Read(ctx, 0x20000000, 4096)
	img := image(3000)
	for _, sec := range p.Covering(stm32.FlashBase, len(img)) {
		if err := f.EraseSector(ctx, sec); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Program(ctx, stm32.FlashBase, img); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.Flash[:3000], img) || s.Programs == 0 || s.Erases != 3 || s.WordWriteFaults != 0 {
		t.Fatalf("flash does not hold the image: programs %d erases %d", s.Programs, s.Erases)
	}
	after, _ := c.ReadAll(ctx)
	if before != after {
		t.Errorf("core state not restored:\n%+v\n%+v", before, after)
	}
	if sramAfter, _ := c.Read(ctx, 0x20000000, 4096); !bytes.Equal(sramBefore, sramAfter) {
		t.Error("SRAM not restored")
	}
	if st, _ := c.Status(ctx); !st.Halted {
		t.Error("core must stay halted")
	}
	if cause, _ := c.HaltCause(ctx); cause.Has("breakpoint") {
		t.Errorf("the loader's BKPT leaked into DFSR: %s", cause)
	}
	if cr, _ := c.Bus.Read32(ctx, 0x40022010); cr&0x80 == 0 {
		t.Error("controller must be re-locked")
	}
}

func TestF1ProgramSkipsBlankAndEqual(t *testing.T) {
	ctx := context.Background()
	s, _, p, f := setupF1(t, 64)
	img := bytes.Repeat([]byte{0xFF}, 2048)
	copy(img[100:], []byte{1, 2, 3, 4})
	if err := f.EraseSector(ctx, p.Sectors[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.Program(ctx, stm32.FlashBase, img[:1024]); err != nil {
		t.Fatal(err)
	}
	if s.Programs != 2 {
		t.Fatalf("only the two non-blank halfwords should be written, got %d", s.Programs)
	}
	s.Programs = 0
	if err := f.Program(ctx, stm32.FlashBase, img[:1024]); err != nil || s.Programs != 0 {
		t.Fatalf("an identical image must be a no-op: %v, %d", err, s.Programs)
	}
}

func TestF1ProgramNeedsErase(t *testing.T) {
	ctx := context.Background()
	_, _, p, f := setupF1(t, 64)
	_ = p
	if err := f.Program(ctx, stm32.FlashBase, []byte{0x12, 0x34}); err != nil {
		t.Fatal(err)
	}
	if err := f.Program(ctx, stm32.FlashBase, []byte{0x56, 0x78}); !errors.Is(err, stm32.ErrNotErased) {
		t.Fatalf("got %v", err)
	}
}

func TestF1CorruptProgramIsDetected(t *testing.T) {
	ctx := context.Background()
	s, _, _, f := setupF1(t, 64)
	s.CorruptProgram = true
	if err := f.Program(ctx, stm32.FlashBase, image(64)); !errors.Is(err, stm32.ErrVerify) {
		t.Fatalf("got %v", err)
	}
}

func TestF1BusyIsWaitedFor(t *testing.T) {
	ctx := context.Background()
	s, _, p, f := setupF1(t, 64)
	s.BusyReads = 3
	if err := f.EraseSector(ctx, p.Sectors[1]); err != nil {
		t.Fatal(err)
	}
	if err := f.Program(ctx, p.Sectors[1].Addr, image(256)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.Flash[1024:1024+256], image(256)) {
		t.Fatal("not programmed")
	}
}

func TestF1LoaderReportsControllerError(t *testing.T) {
	ctx := context.Background()
	s, c, _, f := setupF1(t, 64)
	s.HiddenWRPR = 0xFFFFFFFE // protects the first 4 KiB without WRPR saying so
	err := f.Program(ctx, stm32.FlashBase+0x20, image(32))
	if !errors.Is(err, stm32.ErrController) {
		t.Fatalf("got %v", err)
	}
	if st, _ := c.Status(ctx); !st.Halted {
		t.Error("core must be left halted")
	}
}

func TestF1Protection(t *testing.T) {
	ctx := context.Background()
	s, _, _, f := setupF1(t, 64)
	pr, err := f.ReadProtection(ctx)
	if err != nil || pr.RDP != 0 || len(pr.WriteProtected) != 0 || pr.OptionBytes[0] != 0xA5 {
		t.Fatalf("%+v %v", pr, err)
	}
	s.WRPR = 0xFFFFFFFE
	pr, _ = f.ReadProtection(ctx)
	if len(pr.WriteProtected) != 4 || pr.WriteProtected[0] != 0 {
		t.Fatalf("%+v", pr)
	}
	_, _, p, _ := setupF1(t, 64)
	if err := f.EraseSector(ctx, p.Sectors[0]); !errors.Is(err, stm32.ErrProtected) {
		t.Fatalf("erase of a write-protected page: %v", err)
	}
	if err := f.Program(ctx, stm32.FlashBase, []byte{1, 2}); !errors.Is(err, stm32.ErrProtected) {
		t.Fatalf("program of a write-protected page: %v", err)
	}
	if s.Erases != 0 || s.Programs != 0 {
		t.Fatal("nothing may be written to a protected page")
	}
	s.WRPR, s.RDP = 0xFFFFFFFF, 0x00
	if err := f.EraseSector(ctx, p.Sectors[0]); !errors.Is(err, stm32.ErrProtected) {
		t.Fatalf("RDP active: %v", err)
	}
}

func TestF1RangeAlignAndHalt(t *testing.T) {
	ctx := context.Background()
	_, c, _, f := setupF1(t, 64)
	if err := f.Program(ctx, stm32.FlashBase+1, []byte{1, 2}); !errors.Is(err, stm32.ErrAlign) {
		t.Fatalf("got %v", err)
	}
	if err := f.Program(ctx, stm32.FlashBase, []byte{1, 2, 3}); !errors.Is(err, stm32.ErrAlign) {
		t.Fatalf("got %v", err)
	}
	if err := f.Program(ctx, stm32.FlashBase+64<<10, []byte{1, 2}); !errors.Is(err, stm32.ErrRange) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.Program(ctx, stm32.FlashBase, []byte{1, 2}); err == nil {
		t.Fatal("a running core must be refused")
	}
}
