package jlink

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Two probes, like the real bench: a SAM-ICE with a nickname that leaks into
// the USB product string, and an nRF52840 DK on-board probe.
const emuList = `J-Link[0]: Connection: USB, Serial number: 900000002, ProductName: J-Link OB-SAM3U128-V2-NordicSem, Nickname: <not set>
J-Link[1]: Connection: USB, Serial number: 90000001, ProductName: SAM-ICE, Nickname: jlinkv9
`

// multiCmd answers like the commander does for the probe selected with -USB.
type multiCmd struct {
	mu      sync.Mutex
	runs    []run
	started []string
	// answerAs makes every probe answer with this serial (a mix-up).
	answerAs string
	// busy/active measure concurrency per serial.
	active map[string]int
	maxAct map[string]int
	hold   time.Duration
	// slept is closed-over so tests can observe overlap across probes.
	overlap bool
	cur     int
}

func (m *multiCmd) Available() (string, error) { return "/fake/JLinkExe", nil }

func (m *multiCmd) Run(_ context.Context, serial string, t Target, script []string) (string, error) {
	m.mu.Lock()
	m.runs = append(m.runs, run{serial, t, script})
	if m.active == nil {
		m.active, m.maxAct = map[string]int{}, map[string]int{}
	}
	m.active[serial]++
	if m.active[serial] > m.maxAct[serial] {
		m.maxAct[serial] = m.active[serial]
	}
	m.cur++
	if m.cur > 1 {
		m.overlap = true
	}
	m.mu.Unlock()
	time.Sleep(m.hold)
	m.mu.Lock()
	m.active[serial]--
	m.cur--
	m.mu.Unlock()

	if serial == "" {
		return emuList, nil
	}
	sn := serial
	if m.answerAs != "" {
		sn = m.answerAs
	}
	fw := "J-Link V9 compiled May  7 2021 16:26:12"
	if serial == "900000002" {
		fw = "J-Link OB-SAM3U128-V2-NordicSemi compiled Jul  8 2025 10:14:41"
	}
	out := "Connecting to J-Link via USB...O.K.\nFirmware: " + fw + "\nHardware version: V9.70\nS/N: " + sn + "\nVTref=3.319V\n"
	if t.Device != "" {
		out += "Found SW-DP with ID 0x2BA01477\nFound Cortex-M4 r0p1, Little endian.\nCPUID register: 0x410FC241. Implementer code: 0x41 (ARM)\nCortex-M4 identified.\n"
	}
	for _, c := range script {
		if strings.HasPrefix(c, "ShowEmuList") {
			out += emuList
		}
	}
	return out, nil
}

func (m *multiCmd) Start(_ context.Context, serial string, _ Target, port int, _ uint64) (func(), error) {
	m.mu.Lock()
	m.started = append(m.started, fmt.Sprintf("%s:%d", serial, port))
	m.mu.Unlock()
	return func() {}, nil
}

func twoProbeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(name, pid, serial, product string) {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range map[string]string{"idVendor": "1366", "idProduct": pid, "serial": serial, "product": product} {
			if err := os.WriteFile(filepath.Join(d, k), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("2-2", "0105", "000090000001", "J-Link_jlinkv9")
	mk("2-4", "1025", "000900000002", "J-Link")
	return root
}

func invokeOn(p *Plugin, id, capName, method string, args map[string]any) (map[string]any, error) {
	st, err := structpb.NewStruct(args)
	if err != nil {
		return nil, err
	}
	resp, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: capName, Method: method, Args: st})
	if err != nil {
		return nil, err
	}
	return resp.GetResult().AsMap(), nil
}

func TestEveryProbeIsListedWithModelFromTheCommander(t *testing.T) {
	p := NewWith(twoProbeTree(t), &multiCmd{})
	devs, err := p.ListDevices(context.Background())
	if err != nil || len(devs) != 2 {
		t.Fatalf("devs=%v err=%v", devs, err)
	}
	by := map[string]*pluginv1.Device{}
	for _, d := range devs {
		by[d.Id] = d
	}
	ice, ok := by["90000001"]
	if !ok || ice.Name != "SAM-ICE" || ice.Properties["model"] != "SAM-ICE" || ice.Properties["nickname"] != "jlinkv9" || ice.Properties["product"] != "J-Link_jlinkv9" {
		t.Errorf("SAM-ICE: %+v", ice)
	}
	ob := by["900000002"]
	if ob == nil || !strings.HasPrefix(ob.Name, "J-Link OB-SAM3U128") || ob.Properties["nickname"] != "" {
		t.Errorf("OB: %+v", ob)
	}
}

func TestInfoNamesTheProbeBySerial(t *testing.T) {
	m := &multiCmd{}
	p := NewWith(twoProbeTree(t), m)
	for _, c := range [][2]string{{"90000001", "SAM-ICE"}, {"900000002", "J-Link OB-SAM3U128-V2-NordicSem"}} {
		serial, want := c[0], c[1]
		res, err := invokeOn(p, serial, "DebugProbe", "info", nil)
		if err != nil {
			t.Fatal(err)
		}
		if res["serial"] != serial || res["model"] != want {
			t.Errorf("%s: %+v", serial, res)
		}
	}
	if r := m.runs[len(m.runs)-1]; r.serial != "900000002" {
		t.Errorf("the last call must target the last requested probe: %+v", r)
	}
}

func TestSwitchingProbesNeverMixesSessions(t *testing.T) {
	m := &multiCmd{}
	p := NewWith(twoProbeTree(t), m)
	a, b := "90000001", "900000002"
	if err := p.OpenDevice(context.Background(), a, map[string]any{"target": "EFR32MG1PxxxF256"}); err != nil {
		t.Fatal(err)
	}
	if err := p.OpenDevice(context.Background(), b, map[string]any{"target": "NRF52840_XXAA"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		id, want := a, "EFR32MG1PxxxF256"
		if i%2 == 1 {
			id, want = b, "NRF52840_XXAA"
		}
		res, err := invokeOn(p, id, "DebugProbe", "connect", nil)
		if err != nil || res["probe_serial"] != id || res["target"] != want || res["core_revision"] != "r0p1" || res["cpuid"] != "0x410FC241" {
			t.Fatalf("%s: %+v err=%v", id, res, err)
		}
		if r := m.runs[len(m.runs)-1]; r.serial != id || r.t.Device != want {
			t.Fatalf("call %d ran on %+v, want probe %s target %s", i, r, id, want)
		}
	}
	if _, err := invokeOn(p, a, "DebugProbe", "reset", map[string]any{"target": "EFR32MG1PxxxF256", "run": true}); err != nil {
		t.Fatal(err)
	}
	// Closing one probe's session leaves the other's configuration alone.
	if err := p.CloseDevice(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := invokeOn(p, b, "DebugProbe", "connect", nil); err != nil {
		t.Fatalf("probe %s lost its config when %s closed: %v", b, a, err)
	}
	if _, err := invokeOn(p, a, "DebugProbe", "connect", nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("closed probe must not inherit the other probe's target: %v", err)
	}
}

func TestAnswerFromAnotherProbeIsRefused(t *testing.T) {
	p := NewWith(twoProbeTree(t), &multiCmd{answerAs: "900000002"})
	_, err := invokeOn(p, "90000001", "DebugProbe", "connect", map[string]any{"target": "EFR32MG1PxxxF256"})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "answered from probe 900000002") {
		t.Fatalf("a mix-up must be an error, got %v", err)
	}
}

func TestDeviceIdMustBeASerial(t *testing.T) {
	m := &multiCmd{}
	p := NewWith(twoProbeTree(t), m)
	for _, id := range []string{"", "2-2", "90000001; rm", "-USB"} {
		if _, err := invokeOn(p, id, "DebugProbe", "info", nil); status.Code(err) != codes.InvalidArgument {
			t.Errorf("id %q: %v", id, err)
		}
	}
	if len(m.runs) != 0 {
		t.Errorf("no commander call may run without a serial: %+v", m.runs)
	}
}

func TestProbeConnectFailureIsAnError(t *testing.T) {
	f := &fakeCmd{out: "J-Link>h\nConnecting to J-Link ...FAILED: Cannot connect to the probe/programmer.\n"}
	_, err := invoke(t, NewWith(t.TempDir(), f), "DebugProbe", "halt", map[string]any{"target": "X"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("halt on an unreachable probe must fail, got %v", err)
	}
	f.out = "Connecting to J-Link ...FAILED\n"
	if _, err := invoke(t, NewWith(t.TempDir(), f), "DebugProbe", "resume", map[string]any{"target": "X"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("unknown serial must fail, got %v", err)
	}
}

func TestDifferentProbesRunConcurrentlySameProbeSerialises(t *testing.T) {
	m := &multiCmd{hold: 60 * time.Millisecond}
	p := NewWith(twoProbeTree(t), m)
	var wg sync.WaitGroup
	for _, id := range []string{"90000001", "90000001", "900000002", "900000002"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := invokeOn(p, id, "DebugProbe", "info", nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for serial, n := range m.maxAct {
		if serial != "" && n != 1 {
			t.Errorf("probe %s ran %d commanders at once", serial, n)
		}
	}
	if !m.overlap {
		t.Error("two different probes should not wait for each other")
	}
}

func TestTwoRTTCapturesUseDifferentTelnetPorts(t *testing.T) {
	m := &multiCmd{}
	p := NewWith(twoProbeTree(t), m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, id := range []string{"90000001", "900000002"} {
		// The telnet server never answers, so begin fails after the timeout; the
		// ports handed out are what matters.
		_, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id,
			Config: map[string]any{"target": "X", "rtt_connect_timeout_ms": float64(100)}, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4))
		if err == nil {
			t.Fatal("expected the unreachable telnet server to fail")
		}
	}
	if len(m.started) != 2 {
		t.Fatalf("started %v", m.started)
	}
	pa, pb := strings.Split(m.started[0], ":")[1], strings.Split(m.started[1], ":")[1]
	if pa != pb {
		t.Logf("ports %s and %s", pa, pb) // sequential captures may reuse a released port
	}
	p.mu.Lock()
	left := len(p.rttPorts)
	p.mu.Unlock()
	if left != 0 {
		t.Errorf("%d telnet ports still reserved after the captures ended", left)
	}
}

func TestConcurrentRTTReservationsAreDistinct(t *testing.T) {
	p := NewWith(t.TempDir(), &multiCmd{})
	a := p.reserveRTTPort("127.0.0.1")
	b := p.reserveRTTPort("127.0.0.1")
	if a == b || a < DefaultRTTPort || b < DefaultRTTPort {
		t.Fatalf("ports %d %d", a, b)
	}
}

func TestParseRegsIgnoresVTrefAndHaltStateIsRead(t *testing.T) {
	out := "VTref=3.319V\nPC = 0001634A, CycleCnt = 00000000\nR0 = 00000001, R1 = 00000002\nCPU is halted (PC = 0x0001634A)\nCPU is not halted\n"
	regs := parseRegs(out)
	if _, bad := regs["VTREF"]; bad {
		t.Errorf("VTref is not a register: %v", regs)
	}
	if regs["R1"] != "00000002" {
		t.Errorf("regs %v", regs)
	}
	if h, _, ok := haltState(out); !ok || h {
		t.Errorf("last state must win: halted=%v ok=%v", h, ok)
	}
	if h, pc, ok := haltState("CPU is halted (PC = 0x0001634A)"); !ok || !h || pc != "0x0001634A" {
		t.Errorf("halted=%v pc=%s", h, pc)
	}
	if _, _, ok := haltState("nothing"); ok {
		t.Error("no state expected")
	}
}

func TestHaltFailsWhenTargetStillRunning(t *testing.T) {
	p := NewWith(t.TempDir(), &fakeCmd{out: "S/N: 683123456\nCPU is not halted\n"})
	if _, err := invoke(t, p, "DebugProbe", "halt", map[string]any{"target": "X"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("halt that left the CPU running must fail: %v", err)
	}
	p = NewWith(t.TempDir(), &fakeCmd{out: "S/N: 683123456\nCPU is halted (PC = 0x00000100)\n"})
	res, err := invoke(t, p, "DebugProbe", "halt", map[string]any{"target": "X"})
	if err != nil || res["halted"] != true || res["pc"] != "0x00000100" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestSharedProbeLimitIsExplainedWhenAnotherProbeHoldsRTT(t *testing.T) {
	p := NewWith(t.TempDir(), &fakeCmd{out: "Connecting to J-Link ...FAILED: Cannot connect to the probe/programmer.\n"})
	_, err := invoke(t, p, "DebugProbe", "connect", map[string]any{"target": "X"})
	if err == nil || strings.Contains(err.Error(), "RTT Commander") {
		t.Fatalf("no RTT session open, no hint expected: %v", err)
	}
	p.rttHeld["90000001"] = true
	_, err = invoke(t, p, "DebugProbe", "connect", map[string]any{"target": "X"})
	if err == nil || !strings.Contains(err.Error(), "probe 90000001") {
		t.Fatalf("expected the RTT hint naming the other probe: %v", err)
	}
}

func TestRTTFalseHoldsNoCommanderSession(t *testing.T) {
	m := &multiCmd{}
	p := NewWith(twoProbeTree(t), m)
	c, start, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "900000002",
		Config: map[string]any{"target": "NRF52840_XXAA", "rtt": false}, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4))
	if err != nil || c == nil || start.Streams[0].Attributes["rtt"] != "disabled" {
		t.Fatalf("%v %+v", err, start)
	}
	if len(m.started) != 0 || len(p.otherRTTHolders("")) != 0 {
		t.Errorf("rtt:false must not start a session: %v", m.started)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
}
