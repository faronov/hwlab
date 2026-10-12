package ftdiplugin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/testutil"
	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/ftdi/sim"
)

type rig struct {
	*Plugin
	mu      sync.Mutex
	devs    []*sim.Device
	opens   []int
	detach  []string
	openErr error
}

func newRig(t *testing.T, cfg sim.Config) *rig {
	t.Helper()
	r := &rig{}
	list := func() []Entry {
		d := sim.USBDevice(0)
		es := []Entry{{Dev: d, Sim: &cfg}}
		return nameEntries(es)
	}
	open := func(e Entry, iface int, opt usb.OpenOptions) (usb.Port, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.openErr != nil {
			return nil, r.openErr
		}
		c := *e.Sim
		c.Channel = iface
		c.DetachedDriver = opt.DetachDriver
		d := sim.New(c)
		r.devs = append(r.devs, d)
		r.opens = append(r.opens, iface)
		r.detach = append(r.detach, opt.DetachDriver)
		return d, nil
	}
	r.Plugin = NewWith(list, open)
	return r
}

func (r *rig) id(t *testing.T) string {
	t.Helper()
	ds, err := r.ListDevices(context.Background())
	if err != nil || len(ds) != 1 {
		t.Fatalf("%v %v", ds, err)
	}
	return ds[0].Id
}

func (r *rig) call(t *testing.T, capName, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	s, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: r.id(t), Capability: capName, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return res.Result.AsMap(), nil
}

func (r *rig) must(t *testing.T, capName, method string, args map[string]any) map[string]any {
	t.Helper()
	m, err := r.call(t, capName, method, args)
	if err != nil {
		t.Fatalf("%s.%s: %v", capName, method, err)
	}
	return m
}

var oneTAP = sim.Config{Sipeed: true, FloatHigh: true, Chain: []sim.TAP{{IDCode: 0x1BA01477, IRLen: 4}}}

func TestListDevicesSipeedProfile(t *testing.T) {
	r := newRig(t, oneTAP)
	ds, _ := r.ListDevices(context.Background())
	d := ds[0]
	if d.Properties["profile"] != "sipeed-bl702" || d.Properties["implementation"] != "compatible-emulator" {
		t.Errorf("profile props: %v", d.Properties)
	}
	if d.Properties["jtag_channel"] != "A" || d.Properties["companion_uart_channel"] != "B" || d.Properties["synthetic"] != "true" {
		t.Errorf("mapping: %v", d.Properties)
	}
	got := map[string]bool{}
	for _, c := range d.Capabilities {
		got[c.Id] = true
	}
	want := map[string]bool{"JTAGTransport": true, "JTAGScan": true, "TargetIdentify": true}
	if len(got) != 3 {
		t.Fatalf("capabilities %v", got)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing %s", k)
		}
	}
	for _, bad := range []string{"DebugProbe", "MemoryAccess", "FlashProgramming"} {
		if got[bad] {
			t.Errorf("must not claim %s", bad)
		}
	}
	if !strings.HasPrefix(d.Id, "0403:6010:") {
		t.Errorf("id %q", d.Id)
	}
}

func TestInfoWithoutOpenClaimsNothing(t *testing.T) {
	r := newRig(t, oneTAP)
	m := r.must(t, "JTAGTransport", "info", nil)
	if m["open"] != false || len(r.devs) != 0 {
		t.Fatalf("info claimed the device: %v opens=%d", m["open"], len(r.devs))
	}
	if m["cpu_debug"] != false {
		t.Errorf("cpu_debug %v", m["cpu_debug"])
	}
	u := m["companion_uart"].(map[string]any)
	if u["channel"] != "B" {
		t.Errorf("uart %v", u)
	}
}

func TestScanSingleTAPClaimsOnlyInterface0(t *testing.T) {
	r := newRig(t, oneTAP)
	m := r.must(t, "JTAGScan", "scan", map[string]any{"measure_ir": true})
	if m["status"] != "ok" || m["count"] != float64(1) || m["synthetic"] != true || m["target_verified"] != false {
		t.Fatalf("scan: %v", m)
	}
	dev := m["devices"].([]any)[0].(map[string]any)
	if dev["idcode"].(map[string]any)["raw"] != float64(0x1BA01477) {
		t.Errorf("device %v", dev)
	}
	if len(r.opens) != 1 || r.opens[0] != 0 {
		t.Errorf("claimed interfaces %v, want only 0", r.opens)
	}
	if r.detach[0] != "ftdi_sio" {
		t.Errorf("detach %q", r.detach[0])
	}
	if !r.devs[0].Closed() {
		t.Error("per-call lease did not release the interface")
	}
}

func TestScanStableAcrossRepeats(t *testing.T) {
	r := newRig(t, oneTAP)
	ctx := context.Background()
	if err := r.OpenDevice(ctx, r.id(t), nil); err != nil {
		t.Fatal(err)
	}
	defer func() { testutil.CheckError(t, r.CloseDevice(ctx, r.id(t))) }()
	first := r.must(t, "JTAGScan", "scan", nil)
	for i := 0; i < 4; i++ {
		again := r.must(t, "JTAGScan", "scan", nil)
		a, b := first["devices"].([]any)[0].(map[string]any), again["devices"].([]any)[0].(map[string]any)
		if a["idcode"].(map[string]any)["raw"] != b["idcode"].(map[string]any)["raw"] {
			t.Fatalf("scan %d unstable", i)
		}
	}
	if len(r.opens) != 1 {
		t.Errorf("open device reopened: %v", r.opens)
	}
}

func TestNoTargetReportedHonestly(t *testing.T) {
	r := newRig(t, sim.Config{Sipeed: true, FloatHigh: true})
	m := r.must(t, "JTAGScan", "scan", nil)
	if m["status"] != "no-device" || m["count"] != float64(0) {
		t.Fatalf("%v", m)
	}
	id := r.must(t, "TargetIdentify", "identify", nil)
	if id["count"] != float64(0) || id["target_verified"] != false {
		t.Fatalf("%v", id)
	}
}

func TestIdentifyDecodesIDCode(t *testing.T) {
	r := newRig(t, oneTAP)
	m := r.must(t, "TargetIdentify", "identify", nil)
	ts := m["targets"].([]any)
	if len(ts) != 1 {
		t.Fatalf("%v", m)
	}
	t0 := ts[0].(map[string]any)
	if t0["idcode"] != "0x1ba01477" || t0["part"] != "0xba01" || t0["version"] != float64(1) {
		t.Errorf("%v", t0)
	}
	if !strings.Contains(m["basis"].(string), "no CPU") {
		t.Errorf("basis %v", m["basis"])
	}
}

func TestSelftestSipeedLoopbackIgnored(t *testing.T) {
	r := newRig(t, oneTAP)
	m := r.must(t, "JTAGTransport", "selftest", nil)
	lb := m["loopback"].(map[string]any)
	if m["ok"] != true || lb["supported"] != false || lb["note"] == "" {
		t.Fatalf("%v", m)
	}
	if m["clock_fixed_by_firmware"] != true {
		t.Errorf("fixed clock not reported: %v", m)
	}
}

func TestSelftestLoopbackCapableAdapter(t *testing.T) {
	r := newRig(t, sim.Config{FloatHigh: true})
	m := r.must(t, "JTAGTransport", "selftest", nil)
	if m["loopback"].(map[string]any)["supported"] != true {
		t.Fatalf("%v", m)
	}
}

func TestShiftNeedsBitsAndValidatesData(t *testing.T) {
	r := newRig(t, oneTAP)
	for name, args := range map[string]map[string]any{
		"no bits":   {},
		"zero":      {"bits": 0},
		"too big":   {"bits": 100000},
		"fraction":  {"bits": 2.5},
		"bad hex":   {"bits": 8, "data": "zz"},
		"wrong len": {"bits": 16, "data": "ff"},
		"bad capt":  {"bits": 8, "capture": "yes"},
	} {
		_, err := r.call(t, "JTAGTransport", "shift_dr", args)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	m := r.must(t, "JTAGTransport", "shift_dr", map[string]any{"bits": 32})
	if m["out"] == "" || m["register"] != "DR" {
		t.Errorf("%v", m)
	}
}

func TestShiftsAreConfirmDanger(t *testing.T) {
	r := newRig(t, oneTAP)
	for _, ms := range r.Info().Methods {
		want := pluginv1.Danger_DANGER_SAFE
		if ms.Name == "shift_ir" || ms.Name == "shift_dr" {
			want = pluginv1.Danger_DANGER_CONFIRM
		}
		if ms.Danger != want {
			t.Errorf("%s danger %v want %v", ms.Name, ms.Danger, want)
		}
	}
}

func TestTapReset(t *testing.T) {
	r := newRig(t, oneTAP)
	m := r.must(t, "JTAGTransport", "tap_reset", nil)
	if m["ok"] != true {
		t.Fatalf("%v", m)
	}
}

func TestUnknownMethodAndSettings(t *testing.T) {
	r := newRig(t, oneTAP)
	if _, err := r.call(t, "DebugProbe", "halt", nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("DebugProbe.halt: %v", err)
	}
	if _, err := r.call(t, "MemoryAccess", "read", nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("MemoryAccess.read: %v", err)
	}
	for name, args := range map[string]map[string]any{
		"bad clock":    {"clock_hz": 1},
		"bad iface":    {"interface": 9},
		"bad profile":  {"profile": "nope"},
		"bad detach":   {"detach_kernel_driver": "yes"},
		"bad latency":  {"latency_ms": 0},
		"bad timeout":  {"timeout_ms": 1},
		"clock string": {"clock_hz": "fast"},
	} {
		_, err := r.call(t, "JTAGScan", "scan", args)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := r.OpenDevice(context.Background(), r.id(t), map[string]any{"speed": 1}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown setting accepted: %v", err)
	}
	if len(r.devs) != 0 {
		t.Errorf("a rejected request still claimed the device")
	}
}

func TestSettingsCannotChangeWhileOpen(t *testing.T) {
	r := newRig(t, oneTAP)
	ctx := context.Background()
	if err := r.OpenDevice(ctx, r.id(t), nil); err != nil {
		t.Fatal(err)
	}
	defer func() { testutil.CheckError(t, r.CloseDevice(ctx, r.id(t))) }()
	if _, err := r.call(t, "JTAGScan", "scan", map[string]any{"clock_hz": 1000000}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("%v", err)
	}
}

func TestOpenCloseReopenReleases(t *testing.T) {
	r := newRig(t, oneTAP)
	ctx := context.Background()
	id := r.id(t)
	for i := 0; i < 3; i++ {
		if err := r.OpenDevice(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
		if err := r.CloseDevice(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	for i, d := range r.devs {
		if !d.Closed() {
			t.Errorf("open %d left the interface claimed", i)
		}
	}
	if err := r.CloseDevice(ctx, id); err != nil {
		t.Errorf("closing a closed device: %v", err)
	}
}

func TestNoDetachWhenDisabled(t *testing.T) {
	r := newRig(t, oneTAP)
	r.must(t, "JTAGScan", "scan", map[string]any{"detach_kernel_driver": false})
	if r.detach[0] != "" {
		t.Errorf("detached %q although disabled", r.detach[0])
	}
}

func TestOpenErrorsMapToStatuses(t *testing.T) {
	for _, c := range []struct {
		err  error
		want codes.Code
	}{
		{usb.ErrPermission, codes.PermissionDenied},
		{usb.ErrBusy, codes.FailedPrecondition},
		{errors.New("boom"), codes.Unavailable},
	} {
		r := newRig(t, oneTAP)
		r.openErr = c.err
		err := r.OpenDevice(context.Background(), r.id(t), nil)
		if status.Code(err) != c.want {
			t.Errorf("%v -> %v, want %v", c.err, status.Code(err), c.want)
		}
	}
}

func TestDisconnectDuringScan(t *testing.T) {
	r := newRig(t, oneTAP)
	ctx := context.Background()
	id := r.id(t)
	if err := r.OpenDevice(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	r.devs[0].Disconnect()
	_, err := r.call(t, "JTAGScan", "scan", nil)
	if err == nil || !strings.Contains(err.Error(), "unplugged") {
		t.Fatalf("%v", err)
	}
	if err := r.CloseDevice(ctx, id); err != nil && !errors.Is(err, usb.ErrDisconnected) {
		t.Logf("close after unplug: %v", err)
	}
}

func TestUnknownDevice(t *testing.T) {
	r := newRig(t, oneTAP)
	_, err := r.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "nope", Capability: "JTAGScan", Method: "scan"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("%v", err)
	}
}

func TestNameEntries(t *testing.T) {
	d := sim.USBDevice(0)
	d.Serial = "Factory AIOT/Pro"
	es := nameEntries([]Entry{{Dev: d}})
	if es[0].ID != "0403:6010:Factory_AIOT_Pro" {
		t.Errorf("%q", es[0].ID)
	}
	d2 := d
	d2.SysName = "2-3"
	es = nameEntries([]Entry{{Dev: d}, {Dev: d2}})
	if !strings.Contains(es[0].ID, "@") || es[0].ID == es[1].ID {
		t.Errorf("duplicates not disambiguated: %v %v", es[0].ID, es[1].ID)
	}
	d.Serial = ""
	if es = nameEntries([]Entry{{Dev: d}}); !strings.Contains(es[0].ID, "@") {
		t.Errorf("%q", es[0].ID)
	}
}

func TestStartCaptureUnimplemented(t *testing.T) {
	r := newRig(t, oneTAP)
	if _, _, err := r.StartCapture(context.Background(), pluginsdk.CaptureRequest{}, nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("%v", err)
	}
}

// An interface already bound to anything but ftdi_sio (for example usbfs,
// held by another session) must never be taken over.
func TestOnlyFtdiSioIsEverDetached(t *testing.T) {
	for _, drv := range []string{"usbfs", "other_serial", ""} {
		r := newRig(t, oneTAP)
		list := r.list
		r.list = func() []Entry {
			es := list()
			ifs := append([]usb.Interface(nil), es[0].Dev.Interfaces...)
			ifs[0].Driver = drv
			es[0].Dev.Interfaces = ifs
			return es
		}
		r.must(t, "JTAGScan", "scan", nil)
		if r.detach[0] != "" {
			t.Errorf("driver %q: detach %q requested", drv, r.detach[0])
		}
	}
}
