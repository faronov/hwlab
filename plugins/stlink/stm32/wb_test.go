package stm32_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

func setupWB(t *testing.T) (*stm32.SimWB, *cortexm.Core, *stm32.Profile, *stm32.Flash) {
	t.Helper()
	ctx := context.Background()
	s := stm32.NewSimWB()
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

func TestWBDetect(t *testing.T) {
	_, _, p, _ := setupWB(t)
	if p.Family != stm32.WB || p.DevID != 0x495 || p.FlashKB != 1024 || p.PageSize != 4096 || !p.Programmable ||
		p.SecureBase != 0x080CB000 || p.UserSize() != 0xCB000 || len(p.Sectors) != 0xCB || p.SFR != 0x10CB {
		t.Fatalf("%+v", p)
	}
	if last := p.Sectors[len(p.Sectors)-1]; last.Addr+last.Size != p.SecureBase {
		t.Fatalf("last page %+v does not end at the secure start", last)
	}
	if _, ok := p.SectorAt(0x080CB000); ok {
		t.Fatal("a secure page is listed as a user page")
	}
}

func TestWBEraseProgramVerify(t *testing.T) {
	s, c, _, fl := setupWB(t)
	ctx := context.Background()
	sec, _ := fl.P.SectorAt(0x08001000)
	if err := fl.EraseSector(ctx, sec); err != nil {
		t.Fatal(err)
	}
	img := image(4096)
	if err := fl.Program(ctx, 0x08001000, img); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(ctx, 0x08001000, len(img))
	if err != nil || !bytes.Equal(got, img) {
		t.Fatalf("read-back: %v", err)
	}
	if s.Programs != 512 || s.Erases != 1 {
		t.Fatalf("programs %d erases %d", s.Programs, s.Erases)
	}
	// Re-programming identical data skips every double-word.
	if err := fl.Program(ctx, 0x08001000, img); err != nil || s.Programs != 512 {
		t.Fatalf("identical program: %v (%d)", err, s.Programs)
	}
	// A changed, non-erased double-word needs an erase first.
	img[9] ^= 0xFF
	if err := fl.Program(ctx, 0x08001000, img); !errors.Is(err, stm32.ErrNotErased) {
		t.Fatalf("non-erased double-word: %v", err)
	}
	if !fl.NeedsErase(got[:16], img[:16]) || fl.NeedsErase(got[:16], got[:16]) {
		t.Fatal("NeedsErase")
	}
	// The controller is locked again afterwards.
	if cr, _ := c.Bus.Read32(ctx, 0x58004014); cr>>31 != 1 {
		t.Fatalf("FLASH_CR 0x%08x: not re-locked", cr)
	}
}

func TestWBRefusals(t *testing.T) {
	ctx := context.Background()
	user := stm32.Sector{Addr: 0x08002000, Size: 4096}

	t.Run("alignment", func(t *testing.T) {
		_, _, _, fl := setupWB(t)
		if err := fl.Program(ctx, 0x08002004, image(8)); !errors.Is(err, stm32.ErrAlign) {
			t.Fatal(err)
		}
		if err := fl.Program(ctx, 0x08002000, image(12)); !errors.Is(err, stm32.ErrAlign) {
			t.Fatal(err)
		}
	})
	t.Run("secure area", func(t *testing.T) {
		s, _, _, fl := setupWB(t)
		if err := fl.Program(ctx, 0x080CAFF8, image(16)); !errors.Is(err, stm32.ErrRange) {
			t.Fatal(err)
		}
		if err := fl.EraseSector(ctx, stm32.Sector{Addr: 0x080CB000, Size: 4096}); !errors.Is(err, stm32.ErrRange) {
			t.Fatal(err)
		}
		if s.Erases != 0 || s.Programs != 0 {
			t.Fatal("the flash was touched")
		}
	})
	t.Run("cpu2 booted", func(t *testing.T) {
		s, c, _, fl := setupWB(t)
		if err := c.Bus.Write32(ctx, 0x5800040C, 1<<15); err != nil {
			t.Fatal(err)
		}
		if err := fl.EraseSector(ctx, user); !errors.Is(err, stm32.ErrCPU2Running) {
			t.Fatal(err)
		}
		if s.Erases != 0 || s.FlashWhileC2 != 0 {
			t.Fatal("erased while CPU2 was booted")
		}
	})
	t.Run("cpu1 running", func(t *testing.T) {
		s, c, _, fl := setupWB(t)
		if _, err := c.Resume(ctx); err != nil {
			t.Fatal(err)
		}
		if err := fl.EraseSector(ctx, user); err == nil || s.Erases != 0 {
			t.Fatalf("erased with CPU1 running: %v", err)
		}
	})
	t.Run("rdp", func(t *testing.T) {
		s, _, _, fl := setupWB(t)
		s.OPTR = s.OPTR&^0xFF | 0xBB
		if err := fl.EraseSector(ctx, user); !errors.Is(err, stm32.ErrProtected) {
			t.Fatal(err)
		}
	})
	t.Run("pcrop", func(t *testing.T) {
		s, _, _, fl := setupWB(t)
		s.PCROPA = [2]uint32{0x10, 0x20}
		if err := fl.EraseSector(ctx, user); !errors.Is(err, stm32.ErrProtected) {
			t.Fatal(err)
		}
	})
	t.Run("wrp", func(t *testing.T) {
		s, _, _, fl := setupWB(t)
		s.WRP1AR = 0x00030002 // pages 2..3
		if err := fl.EraseSector(ctx, user); !errors.Is(err, stm32.ErrProtected) {
			t.Fatal(err)
		}
		if s.Erases != 0 {
			t.Fatal("erased a write-protected page")
		}
	})
	t.Run("sfsa moved", func(t *testing.T) {
		s, _, _, fl := setupWB(t)
		s.SFSA = 0xF4
		if err := fl.EraseSector(ctx, user); !errors.Is(err, stm32.ErrProtected) {
			t.Fatal(err)
		}
	})
}
