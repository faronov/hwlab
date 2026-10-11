//go:build hardware && linux

package targetaccept_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/targetaccept"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	cmsisdapplugin "github.com/faronov/hwlab/plugins/cmsisdap"
	xds110plugin "github.com/faronov/hwlab/plugins/xds110"
)

// Real-hardware acceptance of ONE Cortex-M target behind an LP-XDS110ET, driven
// through both backends of the same probe: the native XDS110 plugin and the
// CMSIS-DAP HID plugin. Opt-in with HWLAB_CC2340=1.
//
//	HWLAB_CC2340=1                  run
//	HWLAB_CC2340_SERIAL=<serial>    pick the probe when several are attached
//	HWLAB_CC2340_REPORT_DIR=<dir>   write sanitized per-backend JSON reports
//	HWLAB_CC2340_RESET=0            skip the reset phase
//
// The target is powered by the LP-XDS110ET 3V3 pin (jumper P9 on XDS); hwlab does not
// switch it (docs/notes/phase27-lp-xds110et-target-power.md).
// SRAM and registers are saved and restored; flash is never written. The
// destructive flash suite lives in flash_hardware_test.go.

type pluginLike interface {
	ListDevices(context.Context) ([]*pluginv1.Device, error)
	OpenDevice(context.Context, string, map[string]any) error
	CloseDevice(context.Context, string) error
	Invoke(context.Context, *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error)
}

func probeID(t *testing.T, p pluginLike) string {
	t.Helper()
	want := os.Getenv("HWLAB_CC2340_SERIAL")
	ds, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range ds {
		if strings.HasPrefix(d.Id, "sim") {
			continue
		}
		if want == "" || strings.HasSuffix(d.Id, ":"+want) {
			ids = append(ids, d.Id)
		}
	}
	if len(ids) != 1 {
		t.Skipf("need exactly one probe (found %v); select with HWLAB_CC2340_SERIAL", ids)
	}
	return ids[0]
}

func invoker(t *testing.T, p pluginLike, id string) targetaccept.Invoker {
	return func(c, m string, a map[string]any) (map[string]any, error) {
		s, err := structpb.NewStruct(a)
		if err != nil {
			t.Fatal(err)
		}
		r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: c, Method: m, Args: s})
		if err != nil {
			return nil, err
		}
		return r.Result.AsMap(), nil
	}
}

func save(t *testing.T, rep *targetaccept.Report) {
	dir := os.Getenv("HWLAB_CC2340_REPORT_DIR")
	if dir == "" {
		return
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rep.Backend+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCC2340BothBackends(t *testing.T) {
	if os.Getenv("HWLAB_CC2340") != "1" {
		t.Skip("set HWLAB_CC2340=1 to run the acceptance against the attached target")
	}
	ctx := context.Background()
	x, c := xds110plugin.New(), cmsisdapplugin.New()
	id := probeID(t, x)
	if cid := probeID(t, c); cid != id {
		t.Fatalf("the two plugins see different probes: %s vs %s", id, cid)
	}
	if err := x.OpenDevice(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	defer x.CloseDevice(ctx, id)
	xinv := invoker(t, x, id)
	// Every CC23xx reset ends in SACI; the plugins leave it again on the next
	// core access, so no power cycle is needed. recoverPower stays as a last
	// resort for a target that is really wedged.
	recoverPower := func() error {
		return errors.New("no power cycle available: the LP-XDS110ET cannot switch its 3V3 pin (needs an external PowerSwitch)")
	}
	var phases []string
	if v := os.Getenv("HWLAB_CC2340_PHASES"); v != "" {
		phases = strings.Split(v, ",")
	}
	opts := func(backend string) targetaccept.Options {
		return targetaccept.Options{
			Recover: func() error { return recoverPower() },
			Backend: backend, Log: t.Logf, ClockHz: 400000, Steps: 100,
			Reset: os.Getenv("HWLAB_CC2340_RESET") != "0", ResetVectorBreakpoint: true,
			Phases: phases, SkipROMTable: os.Getenv("HWLAB_SKIPROM") == "1",
		}
	}
	xr := targetaccept.Run(xinv, opts("xds110"))
	save(t, xr)
	// Release the XDS110's own SWD attachment (power stays on) so the HID
	// backend gets the debug port.
	if _, err := xinv("DebugProbe", "disconnect", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.OpenDevice(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	defer c.CloseDevice(ctx, id)
	cr := targetaccept.Run(invoker(t, c, id), opts("cmsisdap"))
	save(t, cr)
	for _, r := range []*targetaccept.Report{xr, cr} {
		if err := r.Err(); err != nil {
			t.Error(err)
		}
		t.Logf("%s: %d pass, %d fail, %d unsupported by the attached hardware, %d info", r.Backend, r.Count(targetaccept.Pass), r.Count(targetaccept.Fail), r.Count(targetaccept.UnsupportedHW), r.Count(targetaccept.Info))
		t.Logf("%s perf (ms): %v", r.Backend, r.PerfMS)
	}
	for _, d := range targetaccept.Compare(xr, cr) {
		t.Errorf("backends differ: %s", d)
	}
}
