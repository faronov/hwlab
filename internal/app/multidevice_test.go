package app_test

import (
	"context"
	"testing"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/timeline"
)

// SYNTHETIC test (fake plugin, simulated devices) - not a hardware result. The
// real two-device calibration is TestRealPPK2SaleaeSessionReplaysOffline.
//
// Two independent capture devices that see the SAME physical edge (the shape of
// a PPK2 digital input and a Saleae channel on one wire) have unrelated clocks.
// Here both are simulated; one is given a known clock_offset and calibrate must
// recover it from the shared edges. This proves the plumbing only. It says
// nothing about the real timing accuracy of a PPK2 or a Saleae, which has not
// been measured. The recovered delta is the injected offset plus the (simulated)
// difference between the two recorded capture request anchors.
func TestCalibrateRecoversKnownOffsetBetweenTwoCaptureDevices(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	e, err := experiment.Parse([]byte(`
name: two-logic
duration: 1s
devices:
  ref:
    plugin: fake
    device: logic0
    realtime: false
  other:
    plugin: fake
    device: logic0
    realtime: false
    clock_offset: 2500us
`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.RunExperiment(ctx, e, true)
	if err != nil || res.Status != "complete" {
		t.Fatalf("run: %+v err=%v", res, err)
	}
	edge := func(src string) timeline.Filter {
		return timeline.Filter{Sources: []string{src}, Types: []string{timeline.TypeDigitalEdge}, Contains: "IRQ rising"}
	}
	s, err := a.OpenSession(res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	anchors := map[string]int64{}
	for _, c := range s.Meta().Captures {
		anchors[c.Alias] = c.RequestNs
	}
	ref, haveRef := anchors["ref"]
	other, haveOther := anchors["other"]
	if !haveRef || !haveOther {
		t.Fatalf("missing capture anchors: %v", anchors)
	}
	want := other - ref + 2_500_000
	cal, err := a.Calibrate(app.CalibrateQuery{Session: res.SessionID, A: edge("ref"), B: edge("other"), WindowNs: 20_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if cal.Pairs != 10 || cal.SuggestedOffsetNs != -want || cal.MADNs != 0 || cal.MedianNs != want {
		t.Errorf("calibration %+v, want delta %d ns from recorded anchors %v", cal, want, anchors)
	}
	if len(cal.Warnings) != 0 {
		t.Errorf("warnings %v", cal.Warnings)
	}
}
