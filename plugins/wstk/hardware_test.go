//go:build hardware && (linux || darwin)

package wstk

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Real-kit tests, opt-in with HWLAB_WSTK_SERIAL (the J-Link serial of the
// kit). They only read: kit info, board list and a short AEM capture. The
// target is never reset, halted or programmed, but the J-Link is held for
// the length of the capture, so no other tool can use the probe meanwhile.

func hwKit(t *testing.T) (*Plugin, string) {
	t.Helper()
	s := os.Getenv("HWLAB_WSTK_SERIAL")
	if s == "" {
		t.Skip("set HWLAB_WSTK_SERIAL to the J-Link serial of a Wireless Starter Kit")
	}
	return hwPlugin(), usbPrefix + s
}

func TestHWInfo(t *testing.T) {
	p, id := hwKit(t)
	res := invoke(t, p, id, pluginv1.CapPowerMeasurement, "info", nil)
	t.Logf("info: %v", res)
	if res["firmware"] == "" || res["boards"] == "" {
		t.Fatalf("no kit info: %v", res)
	}
	res = invoke(t, p, id, pluginv1.CapPowerMeasurement, "read_current", nil)
	t.Logf("read_current: %v", res)
}

func TestHWAEMCapture(t *testing.T) {
	p, id := hwKit(t)
	d := 5 * time.Second
	if v := os.Getenv("HWLAB_WSTK_SECONDS"); v != "" {
		if s, err := time.ParseDuration(v + "s"); err == nil {
			d = s
		}
	}
	sink := pluginsdk.NewTestSink(4096)
	c, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: id, Duration: d, Clock: pluginsdk.NewClock(time.Now().UnixNano())}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	var n int
	var sum, sumV float64
	lo, hi := math.Inf(1), math.Inf(-1)
	var first, last, maxJump int64
	jumps := 0
	for _, ev := range sink.Drain() {
		if l := ev.GetLog(); l != nil {
			t.Logf("log %s: %s", l.GetLevel(), l.GetMessage())
		}
		pb := ev.GetPower()
		if pb == nil {
			continue
		}
		if first == 0 {
			first = ev.GetTimestampNs()
		} else if j := ev.GetTimestampNs() - last; j != 0 {
			jumps++
			maxJump = max(maxJump, j)
		}
		last = ev.GetTimestampNs() + int64(len(pb.GetCurrentNa()))*pb.GetSamplePeriodNs()
		for i, na := range pb.GetCurrentNa() {
			ma := float64(na) / 1e6
			sum += ma
			sumV += float64(pb.GetVoltageUv()[i]) / 1e6
			lo, hi = math.Min(lo, ma), math.Max(hi, ma)
			n++
		}
	}
	res := c.Result()
	t.Logf("%d batches started after the previous one ended (largest step %.3f ms)", jumps, float64(maxJump)/1e6)
	t.Logf("%d samples over %.3f s (stats %v, clock %v): current mean %.4f mA min %.4f max %.4f, voltage mean %.3f V",
		n, float64(last-first)/1e9, res.GetStats(), res.GetClock(), sum/float64(n), lo, hi, sumV/float64(n))
	if n < int(d.Seconds()*10000*0.9) || sum/float64(n) <= 0 || sumV/float64(n) < 1 {
		t.Fatalf("implausible capture: %d samples", n)
	}
}
