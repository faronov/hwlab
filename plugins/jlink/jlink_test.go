package jlink

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/testutil"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// fakeCmd records scripts and replies with canned output.
type fakeCmd struct {
	mu       sync.Mutex
	missing  bool
	out      string
	runs     []run
	started  []Target
	rttAddrs []uint64
	stopped  int
	reply    func(script []string) string // overrides out when set
}

type run struct {
	serial string
	t      Target
	script []string
}

func (f *fakeCmd) Available() (string, error) {
	if f.missing {
		return "", fmt.Errorf("JLinkExe not found")
	}
	return "/fake/JLinkExe", nil
}

func (f *fakeCmd) Run(_ context.Context, serial string, t Target, script []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, run{serial, t, script})
	if f.reply != nil {
		return f.reply(script), nil
	}
	return f.out, nil
}

func (f *fakeCmd) Start(_ context.Context, _ string, t Target, _ int, addr uint64) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, t)
	f.rttAddrs = append(f.rttAddrs, addr)
	return func() { f.mu.Lock(); f.stopped++; f.mu.Unlock() }, nil
}

func (f *fakeCmd) last() run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[len(f.runs)-1]
}

const connectOut = `SEGGER J-Link Commander V7.94e (Compiled Jun 1 2023)
DLL version V7.94e, compiled Jun 1 2023 11:11:21

Connecting to J-Link via USB...O.K.
Firmware: J-Link OB-nRF5340-NordicSemi compiled Jan  3 2023 17:15:45
Hardware version: V1.00
S/N: 683123456
VTref=3.300V
Connecting to target via SWD
Found SW-DP with ID 0x2BA01477
Cortex-M4 identified.
`

func sysfsTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(name, vid, pid, serial, product string) {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range map[string]string{"idVendor": vid, "idProduct": pid, "serial": serial, "product": product} {
			if err := os.WriteFile(filepath.Join(d, k), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("1-2", "1366", "1051", "000683123456", "J-Link OB")
	mk("1-3", "1915", "c00a", "PPK2FIXTURE1", "PPK2")                       // not SEGGER
	if err := os.MkdirAll(filepath.Join(root, "usb1"), 0o755); err != nil { // root hub without ids
		t.Fatal(err)
	}
	return root
}

func TestDiscoveryNeedsNoSeggerSoftware(t *testing.T) {
	p := NewWith(sysfsTree(t), &fakeCmd{missing: true})
	devs, err := p.ListDevices(context.Background())
	if err != nil || len(devs) != 1 {
		t.Fatalf("devs=%v err=%v", devs, err)
	}
	d := devs[0]
	if d.Id != "683123456" || d.Serial != "683123456" || d.Name != "J-Link OB" {
		t.Errorf("device: %+v", d)
	}
	if len(d.Capabilities) != 1 || d.Capabilities[0].Id != pluginv1.CapDebugProbe || d.Capabilities[0].Attributes["operations"] != "discovery-only" {
		t.Errorf("without the commander only discovery must be advertised: %+v", d.Capabilities)
	}
	info := p.Info()
	if len(info.RuntimeRequirements) != 1 || info.RuntimeRequirements[0].Satisfied {
		t.Errorf("runtime requirement must be reported unsatisfied: %+v", info.RuntimeRequirements)
	}
}

func TestCapabilitiesWithCommander(t *testing.T) {
	p := NewWith(sysfsTree(t), &fakeCmd{})
	devs, _ := p.ListDevices(context.Background())
	got := map[string]bool{}
	for _, c := range devs[0].Capabilities {
		got[c.Id] = true
	}
	for _, want := range []string{pluginv1.CapDebugProbe, pluginv1.CapMemoryAccess, pluginv1.CapFlashProgramming, pluginv1.CapRTT, pluginv1.CapLiveRawStream} {
		if !got[want] {
			t.Errorf("missing capability %s", want)
		}
	}
	if !p.Info().RuntimeRequirements[0].Satisfied {
		t.Error("requirement should be satisfied")
	}
}

func TestMethodDangerClassification(t *testing.T) {
	danger := map[string]pluginv1.Danger{}
	for _, m := range New().Info().Methods {
		danger[m.Capability+"."+m.Name] = m.Danger
	}
	for _, m := range []string{"DebugProbe.halt", "DebugProbe.resume", "DebugProbe.reset", "DebugProbe.read_registers", "MemoryAccess.write", "FlashProgramming.program"} {
		if danger[m] != pluginv1.Danger_DANGER_CONFIRM {
			t.Errorf("%s must require confirmation", m)
		}
	}
	for _, m := range []string{"DebugProbe.info", "DebugProbe.connect", "MemoryAccess.read"} {
		if danger[m] != pluginv1.Danger_DANGER_SAFE {
			t.Errorf("%s should be safe", m)
		}
	}
}

func invoke(t *testing.T, p *Plugin, capName, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	st, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "683123456", Capability: capName, Method: method, Args: st})
	if err != nil {
		return nil, err
	}
	return resp.GetResult().AsMap(), nil
}

func TestInfoReportsProbeAndVTref(t *testing.T) {
	f := &fakeCmd{out: connectOut}
	res, err := invoke(t, NewWith(t.TempDir(), f), "DebugProbe", "info", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res["vtref_v"] != 3.3 || res["serial"] != "683123456" || !strings.Contains(res["firmware"].(string), "J-Link OB") {
		t.Errorf("info: %+v", res)
	}
	if r := f.last(); r.t.Device != "" || strings.Join(r.script, ",") != "f,ShowEmuList USB" || r.serial != "683123456" {
		t.Errorf("info must not need a target: %+v", r)
	}
}

func TestConnectAndHarmlessOps(t *testing.T) {
	f := &fakeCmd{out: connectOut}
	p := NewWith(t.TempDir(), f)
	if _, err := invoke(t, p, "DebugProbe", "connect", nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no target must be InvalidArgument, got %v", err)
	}
	res, err := invoke(t, p, "DebugProbe", "connect", map[string]any{"target": "NRF52840_XXAA", "speed_khz": float64(1000)})
	if err != nil || res["core"] != "Cortex-M4" || res["ok"] != true || res["dp_id"] != "0x2BA01477" {
		t.Fatalf("connect: %+v err=%v", res, err)
	}
	if r := f.last(); r.t.Device != "NRF52840_XXAA" || r.t.SpeedKHz != 1000 {
		t.Errorf("target not passed: %+v", r.t)
	}
	if _, err := invoke(t, p, "DebugProbe", "reset", map[string]any{"target": "NRF52840_XXAA"}); err != nil {
		t.Fatal(err)
	}
	if got := f.last().script; strings.Join(got, ",") != "r,g,IsHalted" {
		t.Errorf("reset script %v", got)
	}
	if _, err := invoke(t, p, "DebugProbe", "reset", map[string]any{"target": "X", "run": false}); err != nil {
		t.Fatal(err)
	}
	if got := f.last().script; strings.Join(got, ",") != "r,IsHalted" {
		t.Errorf("reset run=false script %v", got)
	}
}

func TestOpenDeviceConfigIsUsedByInvoke(t *testing.T) {
	f := &fakeCmd{out: connectOut}
	p := NewWith(t.TempDir(), f)
	if err := p.OpenDevice(context.Background(), "683123456", map[string]any{"target": "NRF5340_XXAA_APP", "interface": "jtag"}); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, p, "DebugProbe", "halt", nil); err != nil {
		t.Fatal(err)
	}
	if r := f.last(); r.t.Device != "NRF5340_XXAA_APP" || r.t.Interface != "JTAG" {
		t.Errorf("%+v", r.t)
	}
	if err := p.OpenDevice(context.Background(), "x", map[string]any{"interface": "uart"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad interface: %v", err)
	}
}

func TestConnectFailureIsReported(t *testing.T) {
	f := &fakeCmd{out: "Connecting to J-Link via USB...O.K.\nCannot connect to target.\n"}
	_, err := invoke(t, NewWith(t.TempDir(), f), "DebugProbe", "connect", map[string]any{"target": "NRF52840_XXAA"})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "Cannot connect to target") {
		t.Fatalf("err=%v", err)
	}
}

func TestMissingCommanderGivesInstallHint(t *testing.T) {
	_, err := invoke(t, NewWith(t.TempDir(), &fakeCmd{missing: true}), "DebugProbe", "connect", map[string]any{"target": "X"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "JLinkExe") {
		t.Fatalf("err=%v", err)
	}
}

func TestReadMemory(t *testing.T) {
	f := &fakeCmd{out: "Connecting...\n20000000 = DE AD BE EF 00 11 22 33\n20000008 = 44 55\n"}
	res, err := invoke(t, NewWith(t.TempDir(), f), "MemoryAccess", "read",
		map[string]any{"target": "X", "address": "0x20000000", "length": float64(10)})
	if err != nil || res["hex"] != "deadbeef001122334455" {
		t.Fatalf("read: %+v err=%v", res, err)
	}
	if got := f.last().script[0]; got != "mem8 0x20000000 0xA" {
		t.Errorf("script %q", got)
	}
	if _, err := invoke(t, NewWith(t.TempDir(), f), "MemoryAccess", "read", map[string]any{"target": "X", "address": "0x0", "length": float64(1 << 21)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("oversized read: %v", err)
	}
	f.out = "nothing useful\n"
	if _, err := invoke(t, NewWith(t.TempDir(), f), "MemoryAccess", "read", map[string]any{"target": "X", "address": "0x0", "length": float64(4)}); err == nil {
		t.Error("a short read must be an error, not silent zeros")
	}
}

func TestParseMemWords(t *testing.T) {
	b, err := parseMem("20000000 = 04030201 08070605\n", 4, 8)
	if err != nil || fmt.Sprintf("%x", b) != "0102030405060708" {
		t.Fatalf("%x %v", b, err)
	}
}

func TestWriteMemoryScript(t *testing.T) {
	got := writeScript(0x20000002, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	// 0x20000004 is word-aligned: bytes 3..6 form one word, the unaligned head and tail go byte by byte.
	want := []string{"w1 0x20000002 0x01", "w1 0x20000003 0x02", "w4 0x20000004 0x06050403", "w1 0x20000008 0x07", "w1 0x20000009 0x08"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	f := &fakeCmd{out: connectOut}
	res, err := invoke(t, NewWith(t.TempDir(), f), "MemoryAccess", "write", map[string]any{"target": "X", "address": "0x20000000", "data": "00112233"})
	if err != nil || res["written"] != 4.0 {
		t.Fatalf("%v %v", res, err)
	}
	if _, err := invoke(t, NewWith(t.TempDir(), f), "MemoryAccess", "write", map[string]any{"target": "X", "address": "0x0", "data": "zz"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad hex: %v", err)
	}
}

func TestRegisters(t *testing.T) {
	f := &fakeCmd{out: "PC = 000010D4, CycleCnt = 00000000\nR0 = 00000001, R1 = 20000400, R13(SP) = 20001000\nXPSR = 61000000\n"}
	res, err := invoke(t, NewWith(t.TempDir(), f), "DebugProbe", "read_registers", map[string]any{"target": "X"})
	if err != nil {
		t.Fatal(err)
	}
	regs := res["registers"].(map[string]any)
	if regs["PC"] != "000010D4" || regs["R13(SP)"] != "20001000" || regs["XPSR"] != "61000000" {
		t.Errorf("regs: %v", regs)
	}
}

func TestProgram(t *testing.T) {
	dir := t.TempDir()
	hexF := filepath.Join(dir, "fw.hex")
	binF := filepath.Join(dir, "fw.bin")
	for _, p := range []string{hexF, binF} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeCmd{out: connectOut}
	p := NewWith(t.TempDir(), f)
	if _, err := invoke(t, p, "FlashProgramming", "program", map[string]any{"target": "X", "path": hexF}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.last().script, "|"); got != fmt.Sprintf("h|loadfile %q|r|g", hexF) {
		t.Errorf("script %s", got)
	}
	if _, err := invoke(t, p, "FlashProgramming", "program", map[string]any{"target": "X", "path": binF}); status.Code(err) != codes.InvalidArgument {
		t.Errorf(".bin without address: %v", err)
	}
	if _, err := invoke(t, p, "FlashProgramming", "program", map[string]any{"target": "X", "path": binF, "address": "0x1000", "run": false}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.last().script, "|"); got != fmt.Sprintf("h|loadfile %q 0x1000|r", binF) {
		t.Errorf("script %s", got)
	}
	if _, err := invoke(t, p, "FlashProgramming", "program", map[string]any{"target": "X", "path": filepath.Join(dir, "nope.hex")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing file: %v", err)
	}
}

func TestParseEmuList(t *testing.T) {
	ps := parseEmuList("J-Link[0]: Connection: USB, Serial number: 000683123456, ProductName: J-Link OB-nRF5340-NordicSemi\nJ-Link[1]: Connection: USB, Serial number: 260100000, ProductName: J-Link PRO\n")
	if len(ps) != 2 || ps[0].Serial != "683123456" || ps[1].Product != "J-Link PRO" {
		t.Errorf("%+v", ps)
	}
}

func TestEmuListUsedWhenNoSysfs(t *testing.T) {
	f := &fakeCmd{out: "J-Link[0]: Connection: USB, Serial number: 683123456, ProductName: J-Link OB\n"}
	devs, _ := NewWith(filepath.Join(t.TempDir(), "absent"), f).ListDevices(context.Background())
	if len(devs) != 1 || devs[0].Id != "683123456" {
		t.Fatalf("%+v", devs)
	}
}

func TestExecCommanderArgsAndScript(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "JLinkExe")
	// A stand-in for JLinkExe: records its arguments and the command file.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + dir + "/args\nwhile [ $# -gt 0 ]; do [ \"$1\" = -CommandFile ] && cp \"$2\" " + dir + "/cmds; shift; done\necho 'Cortex-M4 identified.'\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &ExecCommander{Exe: exe}
	out, err := c.Run(context.Background(), "683123456", Target{Device: "NRF52840_XXAA", SpeedKHz: 2000}, []string{"h", "mem8 0x0 4"})
	if err != nil || !strings.Contains(out, "Cortex-M4 identified") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	args, _ := os.ReadFile(dir + "/args")
	for _, want := range []string{"-USB\n683123456", "-Device\nNRF52840_XXAA", "-If\nSWD", "-Speed\n2000", "-AutoConnect\n1", "-ExitOnError\n1"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	cmds, _ := os.ReadFile(dir + "/cmds")
	if string(cmds) != "h\nmem8 0x0 4\nexit\n" {
		t.Errorf("command file %q", cmds)
	}
	if _, err := (&ExecCommander{Exe: filepath.Join(dir, "missing")}).find(); err != nil {
		// Falls back to PATH/well-known locations; only an error when none exist.
		if !strings.Contains(err.Error(), ExeEnv) {
			t.Errorf("hint should mention %s: %v", ExeEnv, err)
		}
	}
}

func TestExecCommanderTimeout(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "JLinkExe")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := (&ExecCommander{Exe: exe, Timeout: 300 * time.Millisecond}).Run(context.Background(), "1", Target{}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v", err)
	}
}

// rttServer plays the J-Link RTT telnet port.
func rttServer(t *testing.T, chunks []string, gap time.Duration) (port int, closeConn func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testutil.CheckError(t, l.Close()) })
	done := make(chan struct{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { testutil.CheckError(t, c.Close()) }()
		for _, ch := range chunks {
			time.Sleep(gap)
			_, _ = c.Write([]byte(ch))
		}
		<-done
	}()
	return l.Addr().(*net.TCPAddr).Port, func() { close(done) }
}

func runRTT(t *testing.T, f *fakeCmd, cfg map[string]any, dur time.Duration) ([]*pluginv1.Event, *pluginv1.StopCaptureResponse) {
	t.Helper()
	p := NewWith(t.TempDir(), f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := pluginsdk.NewTestSink(64)
	cap, start, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: "683123456", Config: cfg, Clock: pluginsdk.NewClock(1_000)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(start.Streams) != 1 || start.Streams[0].Kind != "log" {
		t.Errorf("streams: %+v", start.Streams)
	}
	time.Sleep(dur)
	cancel()
	if err := cap.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	return sink.Drain(), cap.Result()
}

func TestRTTLinesBecomeLogEvents(t *testing.T) {
	port, done := rttServer(t, []string{
		"SEGGER J-Link V7.94e - Real time terminal output\r\nSEGGER J-Link (unknown) V1.0, SN=900000002\r\nProcess: JLinkExe\r\n",
		"radio_tx_",
		"start\r\n\x1b[1;31m<err> app: tx failed\x1b[0m\r\n<wrn> low battery\r\n",
	}, 80*time.Millisecond)
	defer done()
	f := &fakeCmd{}
	evs, res := runRTT(t, f, map[string]any{"rtt_port": float64(port), "target": "NRF52840_XXAA"}, 700*time.Millisecond)

	var app []*pluginv1.Log
	var times []int64
	for _, e := range evs {
		l := e.GetLog()
		if l == nil {
			t.Fatalf("unexpected payload %T", e.GetPayload())
		}
		if l.Source == LogSource {
			app = append(app, l)
			times = append(times, e.TimestampNs)
		}
	}
	if len(app) != 3 {
		t.Fatalf("want 3 target lines (banner excluded), got %d of %d events: %+v", len(app), len(evs), evs)
	}
	if app[0].Attributes["channel"] != "0" {
		t.Errorf("RTT lines must carry their channel: %+v", app[0].Attributes)
	}
	if app[0].Message != "radio_tx_start" || app[0].Level != "info" {
		t.Errorf("a line split across reads must be reassembled: %+v", app[0])
	}
	if app[1].Level != "error" || app[1].Message != "<err> app: tx failed" || app[2].Level != "warn" {
		t.Errorf("levels/ANSI: %+v %+v", app[1], app[2])
	}
	for i := 1; i < len(times); i++ {
		if times[i] < times[i-1] {
			t.Errorf("timestamps not monotonic: %v", times)
		}
	}
	if times[0] < 1_000 {
		t.Errorf("timestamps must be on the session timeline (>= anchor): %v", times)
	}
	if res.Clock == nil || !res.Clock.ErrorMinBounded || res.Clock.ErrorMaxBounded || res.Clock.ErrorMinNs != 0 {
		t.Errorf("RTT clock must be [0, +inf): %+v", res.Clock)
	}
	if res.Stats.Events != uint64(len(evs)) {
		t.Errorf("stats %d vs events %d", res.Stats.Events, len(evs))
	}
	if len(f.started) != 1 || f.started[0].Device != "NRF52840_XXAA" || f.stopped != 1 {
		t.Errorf("commander must be launched once and stopped: started=%v stopped=%d", f.started, f.stopped)
	}
}

func TestRTTControlBlockAddressIsPassedToCommander(t *testing.T) {
	port, done := rttServer(t, []string{"x\n"}, 20*time.Millisecond)
	defer done()
	f := &fakeCmd{}
	runRTT(t, f, map[string]any{"rtt_port": float64(port), "target": "NRF52840_XXAA", "rtt_address": "0x20038000"}, 300*time.Millisecond)
	if len(f.rttAddrs) != 1 || f.rttAddrs[0] != 0x20038000 {
		t.Fatalf("rtt_address not passed on: %v", f.rttAddrs)
	}
	f2 := &fakeCmd{}
	runRTT(t, f2, map[string]any{"rtt_port": float64(port), "target": "NRF52840_XXAA"}, 200*time.Millisecond)
	if len(f2.rttAddrs) != 1 || f2.rttAddrs[0] != 0 {
		t.Fatalf("without rtt_address the J-Link software searches itself: %v", f2.rttAddrs)
	}
}

func TestRTTAttachWithoutLaunch(t *testing.T) {
	port, done := rttServer(t, []string{"hello\n"}, 20*time.Millisecond)
	defer done()
	f := &fakeCmd{missing: true} // no commander needed when another J-Link program serves RTT
	evs, _ := runRTT(t, f, map[string]any{"rtt_port": float64(port), "rtt_launch": false}, 300*time.Millisecond)
	if len(evs) != 1 || evs[0].GetLog().Message != "hello" {
		t.Fatalf("%+v", evs)
	}
	if len(f.started) != 0 {
		t.Error("commander must not be launched")
	}
}

func TestRTTNotReachable(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	testutil.CheckError(t, l.Close())
	f := &fakeCmd{}
	p := NewWith(t.TempDir(), f)
	_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "1", Clock: pluginsdk.NewClock(0),
		Config: map[string]any{"rtt_port": float64(port), "target": "X", "rtt_connect_timeout_ms": float64(300)}}, pluginsdk.NewTestSink(4))
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("err=%v", err)
	}
	if f.stopped != 1 {
		t.Error("a launched commander must be stopped when RTT never comes up")
	}
}

func TestRTTConfigValidation(t *testing.T) {
	p := NewWith(t.TempDir(), &fakeCmd{})
	for name, cfg := range map[string]map[string]any{
		"launch without target": {},
		"bad port":              {"target": "X", "rtt_port": float64(70000)},
	} {
		if _, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "1", Config: cfg, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4)); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := NewWith(t.TempDir(), &fakeCmd{missing: true}).StartCapture(context.Background(),
		pluginsdk.CaptureRequest{DeviceID: "1", Config: map[string]any{"target": "X"}, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("missing commander: %v", err)
	}
}

func TestLaunchArgsEnableRTT(t *testing.T) {
	a := strings.Join(append(Target{Device: "X", Interface: "JTAG"}.args("5"), "-RTTTelnetPort", "19021"), " ")
	for _, want := range []string{"-USB 5", "-If JTAG", "-Device X", "-RTTTelnetPort 19021"} {
		if !strings.Contains(a, want) {
			t.Errorf("%q lacks %q", a, want)
		}
	}
}
