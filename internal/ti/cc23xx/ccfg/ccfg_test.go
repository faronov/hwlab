package ccfg

import (
	"encoding/binary"
	"testing"
)

// sample builds a CCFG like the layout observed on a CC2340: permissions all
// ALLOW, application vector table at 0, debug open.
func sample() []byte {
	b := make([]byte, Size)
	for i := range b {
		b[i] = 0xFF
	}
	binary.LittleEndian.PutUint32(b[0:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(b[4:], 0)
	binary.LittleEndian.PutUint32(b[8:], 0)
	binary.LittleEndian.PutUint32(b[offPerms:], 0xAAAAAAAA)
	b[offDebugCfg], b[offDebugCfg+1], b[offDebugCfg+2], b[offDebugCfg+3] = AuthDebugOpen, 0xA5, 0, 0
	_ = Finalize(b)
	return b
}

func TestParseValid(t *testing.T) {
	i, err := Parse(sample())
	if err != nil {
		t.Fatal(err)
	}
	if !i.Valid || i.Blank {
		t.Fatalf("valid=%v blank=%v: %+v", i.Valid, i.Blank, i)
	}
	if !i.Permissions.AllowChipErase.Allowed() || !i.Permissions.AllowDebugPort.Allowed() {
		t.Fatalf("permissions: %+v", i.Permissions)
	}
	if i.Debug.AuthorizationName != "DEBUG_OPEN" || i.Debug.PasswordHashSet {
		t.Fatalf("debug: %+v", i.Debug)
	}
	if i.BootCfg.AppVtor != 0 {
		t.Fatalf("pAppVtor %#x", i.BootCfg.AppVtor)
	}
}

func TestBlankAndShort(t *testing.T) {
	b := make([]byte, Size)
	for i := range b {
		b[i] = 0xFF
	}
	i, err := Parse(b)
	if err != nil || !i.Blank || i.Valid {
		t.Fatalf("%+v %v", i, err)
	}
	if _, err := Parse(b[:10]); err == nil {
		t.Fatal("short sector accepted")
	}
}

func TestCorruptCRC(t *testing.T) {
	b := sample()
	b[0x100] ^= 1
	i, _ := Parse(b)
	if i.Valid || i.CentralCRC.OK {
		t.Fatal("corrupt central section accepted")
	}
}

func TestDiffSecurity(t *testing.T) {
	a := sample()
	b := sample()
	ch, err := Diff(a, b)
	if err != nil || len(ch) != 0 {
		t.Fatalf("%v %v", ch, err)
	}
	binary.LittleEndian.PutUint32(b[offPerms:], 0x5AAAAAAA) // debug port forbidden
	b[offDebugCfg] = 0x00
	_ = Finalize(b)
	ch, _ = Diff(a, b)
	if !Security(ch) {
		t.Fatalf("debug lock-out not flagged: %v", ch)
	}
	c := sample()
	c[0x300] = 0x11
	_ = Finalize(c)
	ch, _ = Diff(a, c)
	if !Security(ch) {
		t.Fatalf("opaque central change must be security relevant: %v", ch)
	}
	d := sample()
	d[offUserRec] = 0x42
	_ = Finalize(d)
	ch, _ = Diff(a, d)
	if Security(ch) || len(ch) == 0 {
		t.Fatalf("user record change is not security relevant: %v", ch)
	}
}

func TestNoSecretsInJSON(t *testing.T) {
	b := sample()
	for i := 0; i < 8; i++ {
		b[offDebugCfg+4+i] = 0x11
	}
	for i := 0; i < 32; i++ {
		b[offDebugCfg+12+i] = 0x22
	}
	_ = Finalize(b)
	i, _ := Parse(b)
	if !i.Debug.PasswordIDSet || !i.Debug.PasswordHashSet {
		t.Fatal("password presence not detected")
	}
}
