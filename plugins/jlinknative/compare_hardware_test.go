//go:build hardware && linux

package jlinknativeplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/jlink"
)

// TestHWCompareWithSegger runs the same operations through jlink-native and
// through jlink-segger (JLinkExe) on the same probe and target, one after the
// other (the probe cannot be shared), and compares stable values and timings.
// Opt-in: HWLAB_JLINKNATIVE_HW=1 and HWLAB_JLINKNATIVE_SEGGER_TARGET=<device>.
func TestHWCompareWithSegger(t *testing.T) {
	np := New()
	id := hwID(t, np)
	target := os.Getenv("HWLAB_JLINKNATIVE_SEGGER_TARGET")
	if target == "" {
		t.Skip("set HWLAB_JLINKNATIVE_SEGGER_TARGET (e.g. EFR32MG1PxxxF256) to compare with JLinkExe")
	}
	serial := strings.TrimPrefix(id, "jlink-native:")
	sp := jlink.New()

	type op struct {
		name   string
		native func() (map[string]any, error)
		segger func() (map[string]any, error)
	}
	rd := func(addr, n int) op {
		args := func(extra map[string]any) map[string]any {
			a := map[string]any{"address": float64(addr), "length": float64(n)}
			for k, v := range extra {
				a[k] = v
			}
			return a
		}
		return op{
			name:   "read " + itoa(n) + " B @" + itoa(addr),
			native: func() (map[string]any, error) { return hwInvoke(np, id, "MemoryAccess", "read", args(nil)) },
			segger: func() (map[string]any, error) {
				return hwInvokeSegger(sp, serial, "MemoryAccess", "read", args(map[string]any{"target": target}))
			},
		}
	}
	ops := []op{
		{"connect",
			func() (map[string]any, error) { return hwInvoke(np, id, "DebugProbe", "connect", nil) },
			func() (map[string]any, error) {
				return hwInvokeSegger(sp, serial, "DebugProbe", "connect", map[string]any{"target": target})
			}},
		rd(0, 16), rd(0, 2048), rd(0, 65536), rd(0x20000000, 2048),
		{"halt + registers",
			func() (map[string]any, error) {
				return hwInvoke(np, id, "DebugProbe", "read_registers", map[string]any{"halt": true})
			},
			func() (map[string]any, error) {
				return hwInvokeSegger(sp, serial, "DebugProbe", "read_registers", map[string]any{"target": target})
			}},
	}
	for _, o := range ops {
		var tn, ts time.Duration
		var rn, rs map[string]any
		for i := 0; i < 3; i++ {
			t0 := time.Now()
			r, err := o.native()
			d := time.Since(t0)
			if err != nil {
				t.Fatalf("native %s: %v", o.name, err)
			}
			if i == 0 || d < tn {
				tn = d
			}
			rn = r
		}
		for i := 0; i < 3; i++ {
			t0 := time.Now()
			r, err := o.segger()
			d := time.Since(t0)
			if err != nil {
				t.Fatalf("segger %s: %v", o.name, err)
			}
			if i == 0 || d < ts {
				ts = d
			}
			rs = r
		}
		t.Logf("PERF %-24s native %9s   segger %9s", o.name, tn.Round(10*time.Microsecond), ts.Round(10*time.Microsecond))
		if h1, ok := rn["hex"].(string); ok {
			h2, _ := rs["hex"].(string)
			if !strings.EqualFold(h1, h2) {
				t.Errorf("%s: bytes differ between backends", o.name)
			}
		}
		if r1, ok := rn["registers"].(map[string]any); ok {
			r2, _ := rs["registers"].(map[string]any)
			var a, b []string
			for k := range r1 {
				a = append(a, k)
			}
			for k := range r2 {
				b = append(b, strings.ToLower(k))
			}
			sort.Strings(a)
			sort.Strings(b)
			t.Logf("register names native=%v segger=%v", a, b)
		}
	}
	// the native debug state must be left running for the next test
	_, _ = hwInvoke(np, id, "DebugProbe", "resume", nil)
}

func itoa(n int) string { return fmt.Sprint(n) }

func hwInvokeSegger(p *jlink.Plugin, id, capName, method string, args map[string]any) (map[string]any, error) {
	s, _ := structpb.NewStruct(args)
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: capName, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return r.Result.AsMap(), nil
}

func digestLines(evs []*pluginv1.Event, source string) (int, string) {
	h := sha256.New()
	n := 0
	for _, e := range evs {
		if e.GetLog().GetSource() != source {
			continue
		}
		n++
		h.Write([]byte(e.GetLog().GetMessage() + "\n"))
	}
	return n, hex.EncodeToString(h.Sum(nil))[:16]
}

// TestHWRTTCompareWithSegger resets the target, captures its boot RTT output
// natively, resets again and captures it through jlink-segger (JLinkExe), then
// compares line counts and a digest. The payload itself is never printed.
// The IEEE address in the boot lines is the same across resets, so the digests
// must be equal.
func TestHWRTTCompareWithSegger(t *testing.T) {
	np := New()
	id := hwID(t, np)
	target := os.Getenv("HWLAB_JLINKNATIVE_SEGGER_TARGET")
	if target == "" {
		t.Skip("set HWLAB_JLINKNATIVE_SEGGER_TARGET")
	}
	serial := strings.TrimPrefix(id, "jlink-native:")
	ctx := context.Background()
	if err := np.OpenDevice(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	addr := os.Getenv("HWLAB_JLINKNATIVE_RTT_ADDR")
	if _, err := hwInvoke(np, id, "DebugProbe", "reset", map[string]any{"kind": "sysreset"}); err != nil {
		t.Fatal(err)
	}
	sink := pluginsdk.NewTestSink(1024)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	c, _, err := np.StartCapture(cctx, pluginsdk.CaptureRequest{DeviceID: id, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	<-cctx.Done()
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	nn, nd := digestLines(sink.Drain(), RTTLogSource)
	if _, err := hwInvoke(np, id, "DebugProbe", "reset", map[string]any{"kind": "sysreset"}); err != nil {
		t.Fatal(err)
	}
	_ = np.CloseDevice(ctx, id)

	sp := jlink.New()
	ssink := pluginsdk.NewTestSink(1024)
	sctx, scancel := context.WithTimeout(ctx, 12*time.Second)
	defer scancel()
	cfg := map[string]any{"target": target}
	if addr != "" {
		cfg["rtt_address"] = addr
	}
	sc, _, err := sp.StartCapture(sctx, pluginsdk.CaptureRequest{DeviceID: serial, Config: cfg, Clock: pluginsdk.NewClock(0)}, ssink)
	if err != nil {
		t.Fatal(err)
	}
	<-sctx.Done()
	_ = sc.Wait()
	sn, sd := digestLines(ssink.Drain(), jlink.LogSource)
	t.Logf("native: %d lines digest %s   segger: %d lines digest %s", nn, nd, sn, sd)
	if nn == 0 || sn == 0 {
		t.Fatalf("one side produced no RTT lines (native %d, segger %d)", nn, sn)
	}
	if nn != sn || nd != sd {
		t.Errorf("RTT output differs between backends")
	}
}
