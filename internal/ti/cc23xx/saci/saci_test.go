package saci

import (
	"context"
	"errors"
	"hash/crc32"
	"testing"
)

func newClient(s *Sim) *Client {
	c := New(s)
	c.Timeout = 50e6
	return c
}

func TestBasicCommands(t *testing.T) {
	ctx := context.Background()
	s := NewSim(8 * SectorSize)
	c := newClient(s)
	if err := c.Nop(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := c.DieID(ctx)
	if err != nil || len(id) != 4 {
		t.Fatalf("die id: %v %v", id, err)
	}
	req, err := c.RequestPasswordID(ctx)
	if err != nil || req.Needed {
		t.Fatalf("pwd: %+v %v", req, err)
	}
	s.DebugAuth = 0xA5
	if req, _ = c.RequestPasswordID(ctx); !req.Needed || len(req.PwdID) != 2 {
		t.Fatalf("pwd needed: %+v", req)
	}
	if err := c.SubmitAuth(ctx, []uint32{1, 2}); err == nil {
		t.Fatal("short password must be rejected before sending")
	}
	var re *ResultError
	if err := c.SubmitAuth(ctx, []uint32{1, 2, 3}); !errors.As(err, &re) || re.Result != ResInvalidPwd {
		t.Fatalf("want INVALID_PWD_PARAM, got %v", err)
	}
	if err := c.ExitHalt(ctx); err != nil || s.Exited != "DEBUG_EXIT_SACI_HALT" {
		t.Fatalf("exit: %v %q", err, s.Exited)
	}
}

func TestSequenceTagsAdvance(t *testing.T) {
	ctx := context.Background()
	c := newClient(NewSim(SectorSize))
	seen := map[uint8]bool{}
	for i := 0; i < 300; i++ {
		seq, err := c.Send(ctx, CmdNop, 0)
		if err != nil {
			t.Fatal(err)
		}
		if seq == 0 {
			t.Fatal("sequence tag 0 is skipped")
		}
		seen[seq] = true
		if _, err := c.finish(ctx, c.Timeout, CmdNop, seq); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 255 {
		t.Fatalf("expected 255 distinct tags, got %d", len(seen))
	}
}

func TestEraseProgramVerify(t *testing.T) {
	ctx := context.Background()
	s := NewSim(8 * SectorSize)
	c := newClient(s)
	img := make([]byte, 3000)
	for i := range img {
		img[i] = byte(i*7 + 1)
	}
	// Programming over erased flash, split at the sector boundary.
	if err := c.ProgramMain(ctx, 0, img[:SectorSize]); err != nil {
		t.Fatal(err)
	}
	if err := c.ProgramMain(ctx, SectorSize, img[SectorSize:]); err != nil {
		t.Fatal(err)
	}
	if err := c.ProgramMain(ctx, 1000, img[:SectorSize]); err == nil {
		t.Fatal("range crossing a sector must be rejected")
	}
	full := append(append([]byte{}, img...), make([]byte, 0)...)
	region := s.Flash[:2*SectorSize]
	if string(region[:3000]) != string(full) {
		t.Fatal("flash content differs from image")
	}
	if err := c.VerifyMain(ctx, 0, 2*SectorSize, crc32.ChecksumIEEE(region), false); err != nil {
		t.Fatal(err)
	}
	var re *ResultError
	if err := c.VerifyMain(ctx, 0, 2*SectorSize, 0xDEADBEEF, false); !errors.As(err, &re) || re.Result != ResCRC32Mismatch {
		t.Fatalf("want CRC32_MISMATCH, got %v", err)
	}
	if err := c.VerifyMain(ctx, 0, SectorSize, 0, true); !errors.As(err, &re) || re.Result != ResBlankCheckFailed {
		t.Fatalf("want BLANK_CHECK_FAILED, got %v", err)
	}
	if err := c.EraseChip(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.VerifyMain(ctx, 0, uint32(len(s.Flash)), 0, true); err != nil {
		t.Fatalf("blank after erase: %v", err)
	}
	// After a chip erase the CCFG is invalid and exits are refused.
	if err := c.ExitHalt(ctx); !errors.As(err, &re) || re.Result != ResNotAllowed {
		t.Fatalf("want NOT_ALLOWED, got %v", err)
	}
}

func TestEraseRetainAndPermissions(t *testing.T) {
	ctx := context.Background()
	s := NewSim(4 * SectorSize)
	c := newClient(s)
	if err := c.ProgramMain(ctx, 0, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if err := c.EraseChip(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if s.Flash[0] != 1 {
		t.Fatal("retained sector 0 was erased")
	}
	s.AllowErase = false
	var re *ResultError
	if err := c.EraseChip(ctx, 0); !errors.As(err, &re) || re.Result != ResNotAllowed {
		t.Fatalf("want NOT_ALLOWED, got %v", err)
	}
}

func TestCCFGProgram(t *testing.T) {
	ctx := context.Background()
	s := NewSim(2 * SectorSize)
	c := newClient(s)
	if err := c.EraseChip(ctx, 0); err != nil {
		t.Fatal(err)
	}
	sec := make([]byte, SectorSize)
	for i := range sec {
		sec[i] = 0xFF
	}
	sec[0x18] = 0xAA
	if err := c.ProgramCCFG(ctx, sec[:100], false); err == nil {
		t.Fatal("short CCFG sector must be rejected")
	}
	if err := c.ProgramCCFG(ctx, sec, false); err != nil {
		t.Fatal(err)
	}
	if s.CCFG[0x18] != 0xAA || !s.CCFGValid {
		t.Fatal("CCFG not stored")
	}
	var re *ResultError
	if err := c.ProgramCCFG(ctx, sec, false); !errors.As(err, &re) || re.Result != ResFlashFSMError {
		t.Fatalf("programming a non-erased CCFG must fail: %v", err)
	}
}

func TestResetDevice(t *testing.T) {
	ctx := context.Background()
	s := NewSim(SectorSize)
	c := newClient(s)
	if err := c.ResetDevice(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := c.ResetDevice(ctx, false); err != nil {
		t.Fatal(err)
	}
	if s.Resets != 2 {
		t.Fatalf("resets: %d", s.Resets)
	}
	// Without waitForSwdDisconnect no response is generated.
	if _, err := c.Recv(ctx, 10e6); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
}

func TestPack(t *testing.T) {
	w := Pack([]byte{1, 2, 3, 4, 5}, 0xFF)
	if len(w) != 2 || w[0] != 0x04030201 || w[1] != 0xFFFFFF05 {
		t.Fatalf("%#x", w)
	}
}
