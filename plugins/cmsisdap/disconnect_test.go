package cmsisdapplugin

import "testing"

// disconnect releases the SWD attachment without closing the device, and the
// next connect attaches again.
func TestDisconnectReleasesAndReattaches(t *testing.T) {
	p, _ := simPlugin(t)
	openSim(t, p)
	if _, err := invoke(t, p, SimID, "DebugProbe", "disconnect", nil); err != nil {
		t.Fatalf("disconnect before any attach: %v", err)
	}
	c := mustInvoke(t, p, "DebugProbe", "connect", nil)
	if c["cpuid"] == nil {
		t.Fatalf("connect: %v", c)
	}
	r := mustInvoke(t, p, "DebugProbe", "disconnect", nil)
	if r["disconnected"] != true {
		t.Fatalf("disconnect: %v", r)
	}
	if c2 := mustInvoke(t, p, "DebugProbe", "connect", nil); c2["cpuid"] != c["cpuid"] {
		t.Fatalf("reconnect: %v vs %v", c2, c)
	}
}
