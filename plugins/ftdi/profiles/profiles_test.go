package profiles

import "testing"

func TestBuiltinLayoutsValidate(t *testing.T) {
	for _, p := range All() {
		if err := p.Layout.Validate(); err != nil {
			t.Errorf("%s: %v", p.Name, err)
		}
	}
}

func TestSipeedProfile(t *testing.T) {
	p := SipeedBL702
	if p.Implementation != CompatibleEmulator {
		t.Error("the Sipeed adapter is an emulator")
	}
	if p.JTAGInterface != 0 || p.UARTInterface != 1 {
		t.Errorf("channel mapping: jtag=%d uart=%d", p.JTAGInterface, p.UARTInterface)
	}
	l := p.Layout
	// OpenOCD: layout_init 0x0008 0x001b
	if l.InitValue != 0x08 || l.InitDir != 0x1B {
		t.Errorf("init value/dir %#x/%#x", l.InitValue, l.InitDir)
	}
	if l.Mask(l.TCK) != 0x01 || l.Mask(l.TDI) != 0x02 || l.Mask(l.TDO) != 0x04 || l.Mask(l.TMS) != 0x08 {
		t.Error("TCK/TDI/TDO/TMS masks")
	}
	if l.InitDir&l.Mask(l.TDO) != 0 {
		t.Error("TDO must be an input")
	}
	if l.NSRST != NoPin || l.NTRST != NoPin || l.Mask(l.NSRST) != 0 {
		t.Error("reset lines are unverified and must not be exposed")
	}
	if !p.Quirks.LoopbackIgnored || !p.Quirks.FixedClock || !p.Quirks.FakeGPIO || !p.Quirks.OnlyReadWriteShifts {
		t.Error("quirks")
	}
	if p.NominalClockHz != 6_000_000 {
		t.Error("nominal clock")
	}
	if len(p.Sources) < 2 {
		t.Error("provenance missing")
	}
}

func TestMatch(t *testing.T) {
	if p := Match(0x0403, 0x6010, "SIPEED", "JTAG Debugger"); p.Name != "sipeed-bl702" {
		t.Errorf("got %s", p.Name)
	}
	if p := Match(0x0403, 0x6010, "sipeed", "jtag debugger"); p.Name != "sipeed-bl702" {
		t.Errorf("case-insensitive: %s", p.Name)
	}
	// strings are never required for generic FT2232 support
	if p := Match(0x0403, 0x6010, "FTDI", "Dual RS232"); p.Name != "generic-ft2232" {
		t.Errorf("got %s", p.Name)
	}
	if p := Match(0x0403, 0x6010, "", ""); p.Name != "generic-ft2232" {
		t.Errorf("got %s", p.Name)
	}
	if p := Match(0x1234, 0x5678, "SIPEED", "JTAG Debugger"); p.Name != "generic-ft2232" {
		t.Errorf("a different VID/PID must not select the Sipeed profile: %s", p.Name)
	}
}

func TestByNameAndNames(t *testing.T) {
	if _, ok := ByName("sipeed-bl702"); !ok {
		t.Fatal()
	}
	if _, ok := ByName("nope"); ok {
		t.Fatal()
	}
	if n := Names(); len(n) != 2 || n[0] != "generic-ft2232" {
		t.Fatalf("%v", n)
	}
}

func TestLayoutValidateRejectsBadLayouts(t *testing.T) {
	bad := StandardLayout
	bad.TDI = 0
	if bad.Validate() == nil {
		t.Error("duplicate pin")
	}
	bad = StandardLayout
	bad.InitDir = 0x0F // TDO as output
	if bad.Validate() == nil {
		t.Error("TDO output")
	}
	bad = StandardLayout
	bad.InitDir = 0x01
	if bad.Validate() == nil {
		t.Error("missing outputs")
	}
}
