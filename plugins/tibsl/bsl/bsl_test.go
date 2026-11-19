package bsl

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"strings"
	"testing"
	"time"
)

func fastEntry(t *testing.T) {
	t.Helper()
	s, h := EntryStep, EntryHold
	EntryStep, EntryHold = time.Millisecond, time.Millisecond
	t.Cleanup(func() { EntryStep, EntryHold = s, h })
}

func connected(t *testing.T, s *Sim) *Client {
	t.Helper()
	c := New(s)
	c.Timeout = 200 * time.Millisecond
	if _, err := c.Connect(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPacketFraming(t *testing.T) {
	if got := hex.EncodeToString(Packet(CmdPing, nil)); got != "032020" {
		t.Fatalf("ping %s", got)
	}
	// MEMORY_READ 0x50001318, 32-bit, one word: checksum is the byte sum.
	if got := hex.EncodeToString(Packet(CmdMemoryRead, []byte{0x50, 0x00, 0x13, 0x18, 1, 1})); got != "09a72a500013180101" {
		t.Fatalf("read %s", got)
	}
}

func TestIdentifyCC2652P(t *testing.T) {
	s := NewSim()
	c := connected(t, s)
	ch, err := Identify(c)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Name != "CC2652P" || ch.WaferID != 0xBB41 || ch.PGRev != 3 || ch.FlashBytes != 352<<10 || ch.SectorBytes != 8192 || !ch.PA || ch.Package != "7x7 mm" || !ch.Writable {
		t.Fatalf("%+v", ch)
	}
	if ch.IEEEString() != "00:12:4B:00:20:CD:1D:7D" || ch.ConfigAddr != 0x57FA8 || ch.ConfigSector() != 0x56000 {
		t.Fatalf("ieee %s ccfg 0x%x sector 0x%x", ch.IEEEString(), ch.ConfigAddr, ch.ConfigSector())
	}
	// Unaligned head and tail use 8-bit reads.
	b, err := c.Read(0x101, 700, nil)
	if err != nil || !bytes.Equal(b, s.Flash[0x101:0x101+700]) {
		t.Fatalf("read %v", err)
	}
	crc, err := c.CRC32(0, len(s.Flash))
	if err != nil || crc != crc32.ChecksumIEEE(s.Flash) {
		t.Fatalf("crc 0x%08x %v", crc, err)
	}
	if _, err := c.Read(0x60000000, 4, nil); !errorsAsStatus(err, StatusInvalidAdr) {
		t.Fatalf("bad address: %v", err)
	}
}

func errorsAsStatus(err error, st byte) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == st
}

func TestConnectResyncsARunningBootloader(t *testing.T) {
	s := NewSim()
	connected(t, s)
	c := New(s)
	c.Timeout = 200 * time.Millisecond
	re, err := c.Connect(100 * time.Millisecond)
	if err != nil || !re {
		t.Fatalf("resynced=%v %v", re, err)
	}
	if _, err := c.ChipID(); err != nil {
		t.Fatal(err)
	}
}

func TestWiredEntryNeedsAtomicLines(t *testing.T) {
	fastEntry(t)
	for _, e := range []Entry{EntryDTRRTS, EntryRTSDTR} {
		s := NewSim()
		s.Wired, s.Inverted = true, e == EntryRTSDTR
		s.PowerOn()
		if s.InBootloader() {
			t.Fatal("a valid image with the pin released must boot the application")
		}
		// One line at a time passes through "reset released, pin high".
		seq := func(dtr, rts bool) (bool, error) {
			if s.Inverted {
				if err := s.SetLines(s.dtr, rts); err != nil {
					return false, err
				}
			} else if err := s.SetLines(dtr, s.rts); err != nil {
				return false, err
			}
			return false, s.SetLines(dtr, rts)
		}
		if _, err := Enter(context.Background(), seq, e); err != nil {
			t.Fatal(err)
		}
		if s.InBootloader() {
			t.Fatalf("%s: non-atomic changes must not enter the bootloader on this wiring", e)
		}
		atomic := func(dtr, rts bool) (bool, error) { return true, s.SetLines(dtr, rts) }
		if a, err := Enter(context.Background(), atomic, e); err != nil || !a {
			t.Fatal(a, err)
		}
		if err := Release(atomic, e); err != nil || !s.InBootloader() {
			t.Fatalf("%s: entry failed (%v): boots %v", e, err, s.Boots)
		}
		c := connected(t, s)
		if err := c.Reset(); err != nil || s.InBootloader() {
			t.Fatalf("reset with lines released must start the application: %v %v", err, s.Boots)
		}
	}
}

func TestCC2538Dialect(t *testing.T) {
	s := NewSim2538()
	c := connected(t, s)
	ch, err := Identify(c)
	if err != nil {
		t.Fatal(err)
	}
	if c.Family != FamilyCC2538 || ch.Name != "CC2538" || ch.FlashBytes != 512<<10 || ch.IEEEString() != "00:12:4B:00:01:02:03:04" || ch.ConfigAddr != 0x27FFD4 {
		t.Fatalf("%+v %s", ch, ch.IEEEString())
	}
	b, err := c.Read(CC2538FlashBase+2, 10, nil)
	if err != nil || !bytes.Equal(b, s.Flash[2:12]) {
		t.Fatalf("read %x %v", b, err)
	}
	if err := c.EraseRange(CC2538FlashBase+0x800, 0x800); err != nil || !Blank(s.Flash[0x800:0x1000]) {
		t.Fatal(err)
	}
	if err := c.Write(CC2538FlashBase+0x800, []byte{1, 2, 3, 4}, nil); err != nil || s.Flash[0x803] != 4 {
		t.Fatal(err)
	}
	cfg, _ := c.Read(ch.ConfigAddr, ch.ConfigLen, nil)
	if bd := backdoor(FamilyCC2538, cfg); !bd.BackdoorEnabled || bd.Pin != 3 || bd.ActiveHigh {
		t.Fatalf("%+v", bd)
	}
}

func realCCFG() []byte {
	b, _ := hex.DecodeString(SimCCFG)
	return b
}

func TestCheckConfig(t *testing.T) {
	cur := realCCFG()
	if v, _ := CheckConfig(FamilyCC26xx, cur, cur); len(v) != 0 {
		t.Fatalf("the shipped CCFG must pass: %v", v)
	}
	if v, n := CheckConfig(FamilyCC26xx, cur, bytes.Repeat([]byte{0xFF}, CCFGLen)); len(v) != 0 || len(n) != 1 {
		t.Fatalf("blank CCFG: %v %v", v, n)
	}
	mod := func(off int, v uint32) []byte {
		b := append([]byte(nil), cur...)
		binary.LittleEndian.PutUint32(b[off:], v)
		return b
	}
	for name, tc := range map[string]struct {
		next []byte
		want string
	}{
		"bootloader off":  {mod(CCFGBLConfig, 0x00FE0FC5), "bootloader would be disabled"},
		"backdoor off":    {mod(CCFGBLConfig, 0xC5FE0F00), "backdoor pin would be disabled"},
		"pin moved":       {mod(CCFGBLConfig, 0xC5FE0BC5), "would move"},
		"level flipped":   {mod(CCFGBLConfig, 0xC5FF0FC5), "would move"},
		"cpu dap locked":  {mod(CCFGTapDap0, 0xFF00C500), "CPU_DAP_ENABLE"},
		"pwrprof dropped": {mod(CCFGTapDap0, 0xFFC50000), "PWRPROF_TAP_ENABLE"},
		"chip erase off":  {mod(CCFGEraseConf, 0xFFFFFEFF), "CHIP_ERASE_DIS_N"},
		"sector protect":  {mod(CCFGProt, 0xFFFFFFFE), "write-protect"},
	} {
		v, _ := CheckConfig(FamilyCC26xx, cur, tc.next)
		if !strings.Contains(strings.Join(v, ";"), tc.want) {
			t.Errorf("%s: %v", name, v)
		}
	}
	// TI FA is already disabled on the shipped image: keeping it so is fine,
	// disabling it when enabled is refused.
	fa := mod(CCFGTIOptions, 0xFFFFFFC5)
	if v, _ := CheckConfig(FamilyCC26xx, fa, cur); !strings.Contains(strings.Join(v, ";"), "TI_FA_ENABLE") {
		t.Errorf("TI FA: %v", v)
	}
	if v, n := CheckConfig(FamilyCC26xx, cur, mod(CCFGImageValid, 0xFFFFFFFF)); len(v) != 0 || len(n) != 1 {
		t.Errorf("image invalid is a note, not a lockout: %v %v", v, n)
	}
	dec := DecodeConfig(FamilyCC26xx, cur)
	if bd := dec["bootloader"].(map[string]any); bd["backdoor_pin"] != float64(15) || bd["backdoor_active_high"] != false || dec["cpu_dap_enabled"] != true || dec["ti_fa_enabled"] != false {
		t.Fatalf("%v", dec)
	}
}

func TestCheckConfigCC2538(t *testing.T) {
	cur := NewSim2538().config()
	if v, _ := CheckConfig(FamilyCC2538, cur, cur); len(v) != 0 {
		t.Fatal(v)
	}
	next := append([]byte(nil), cur...)
	next[CC2538CCALen-1] &^= 0x80
	next[3] &^= 0x10
	v, _ := CheckConfig(FamilyCC2538, cur, next)
	if s := strings.Join(v, ";"); !strings.Contains(s, "debug lock") || !strings.Contains(s, "bootloader would be disabled") {
		t.Fatal(v)
	}
}
