package device

import "testing"

func TestSelectNameMustBeUniqueButSerialAlwaysWins(t *testing.T) {
	devs := []Device{
		{ID: "jlink/111", PluginID: "jlink", LocalID: "111", Serial: "111", Name: "J-Link"},
		{ID: "jlink/222", PluginID: "jlink", LocalID: "222", Serial: "222", Name: "J-Link"},
		{ID: "uart/x", PluginID: "uart", LocalID: "x", Name: "J-Link"},
	}
	if _, err := Select(devs, "jlink", "J-Link"); err == nil {
		t.Fatal("a name shared by two probes must be ambiguous")
	}
	if _, err := Select(devs, "jlink", ""); err == nil {
		t.Fatal("empty selector with two probes must fail")
	}
	for _, sel := range []string{"222", "jlink/222"} {
		d, err := Select(devs, "jlink", sel)
		if err != nil || d.LocalID != "222" {
			t.Fatalf("%s: %+v %v", sel, d, err)
		}
	}
	if d, err := Select(devs, "uart", "J-Link"); err != nil || d.LocalID != "x" {
		t.Fatalf("unique name in its own plugin: %+v %v", d, err)
	}
	if _, err := Select(devs, "jlink", "333"); err == nil {
		t.Fatal("unknown serial must fail")
	}
}
