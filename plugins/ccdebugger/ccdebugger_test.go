package ccdebugger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Real output of `cc-tool -d 2:4 -t` against a CC-Debugger with a CC2530.
const realInfo = `Programmer: CC Debugger
  Target: CC2530
  Device info: 
   Name: CC Debugger
   Debugger ID: 0701
   Version: 0x05CC
   Revision: 0x0044

  Target info: 
   Name: CC2530
   Revision: 0x24
   Internal ID: 0xA5
   ID: 0x2530
   Flash size: 256 KB
   Flash page size: 2
   RAM size: 8 KB
   Lock data size: 16 B
`

const realMAC = `Programmer: CC Debugger
  Target: CC2530
  MAC addresses, primary: 00:12:4B:00:19:92:B8:DA, secondary: FF:FF:FF:FF:FF:FF:FF:FF
`

type runRec struct{ args []string }

type fakeTool struct {
	err   error
	out   string
	runs  []runRec
	write func(args []string)
}

func (f *fakeTool) Available() (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "/fake/cc-tool", nil
}

func (f *fakeTool) Run(_ context.Context, args []string) (string, error) {
	f.runs = append(f.runs, runRec{args})
	if f.write != nil {
		f.write(args)
	}
	if strings.Contains(f.out, "not found") {
		return f.out, errors.New("exit status 1")
	}
	return f.out, nil
}

func sysfsWith(t *testing.T, entries ...map[string]string) string {
	root := t.TempDir()
	for i, e := range entries {
		dir := filepath.Join(root, "2-"+string(rune('1'+i)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range e {
			if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

var ccDebugger = map[string]string{"idVendor": "0451", "idProduct": "16a2", "product": "CC Debugger", "serial": "90000003", "busnum": "2", "devnum": "4"}

func invoke(t *testing.T, p *Plugin, capName, method, id string, args map[string]any) (map[string]any, error) {
	t.Helper()
	st, _ := structpb.NewStruct(args)
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: capName, Method: method, Args: st})
	if err != nil {
		return nil, err
	}
	return r.GetResult().AsMap(), nil
}

func TestParseInfoRealOutput(t *testing.T) {
	in := parseInfo(realInfo)
	if in.Programmer != "CC Debugger" || in.Target != "CC2530" {
		t.Fatalf("%+v", in)
	}
	if in.Device["debugger id"] != "0701" || in.Device["version"] != "0x05CC" {
		t.Fatalf("device %+v", in.Device)
	}
	if in.TargetInfo["id"] != "0x2530" || in.TargetInfo["flash size"] != "256 KB" || in.TargetInfo["ram size"] != "8 KB" {
		t.Fatalf("target %+v", in.TargetInfo)
	}
}

func TestParseMAC(t *testing.T) {
	p, s, ok := parseMAC(realMAC)
	if !ok || p != "00:12:4B:00:19:92:B8:DA" || s != "" {
		t.Fatalf("%q %q %v", p, s, ok)
	}
	if _, _, ok := parseMAC("nothing"); ok {
		t.Fatal("parsed garbage")
	}
}

func TestDiscoveryAndCapabilityGating(t *testing.T) {
	root := sysfsWith(t, ccDebugger, map[string]string{"idVendor": "1915", "idProduct": "c00a"})
	p := NewWith(root, &fakeTool{})
	devs, _ := p.ListDevices(context.Background())
	if len(devs) != 1 || devs[0].Id != "90000003" {
		t.Fatalf("%v", devs)
	}
	if !pluginv1.HasCapability(devs[0].Capabilities, pluginv1.CapFlashProgramming) || pluginv1.HasCapability(devs[0].Capabilities, pluginv1.CapRTT) || pluginv1.HasCapability(devs[0].Capabilities, pluginv1.CapMemoryAccess) {
		t.Fatalf("caps %v", devs[0].Capabilities)
	}

	p = NewWith(root, &fakeTool{err: errors.New("cc-tool not found: install it")})
	devs, _ = p.ListDevices(context.Background())
	if len(devs) != 1 || pluginv1.HasCapability(devs[0].Capabilities, pluginv1.CapFlashProgramming) || devs[0].Capabilities[0].Attributes["missing"] != "cc-tool" {
		t.Fatalf("without cc-tool: %v", devs)
	}
	info := p.Info()
	if info.RuntimeRequirements[0].Satisfied {
		t.Fatal("requirement should be unsatisfied")
	}
}

func TestDuplicateSerialsGetAddress(t *testing.T) {
	second := map[string]string{}
	for k, v := range ccDebugger {
		second[k] = v
	}
	second["devnum"] = "7"
	p := NewWith(sysfsWith(t, ccDebugger, second), &fakeTool{})
	devs, _ := p.ListDevices(context.Background())
	if len(devs) != 2 || devs[0].Id != "90000003@2:4" || devs[1].Id != "90000003@2:7" {
		t.Fatalf("%v %v", devs[0].Id, devs[1].Id)
	}
}

func TestDangerClassification(t *testing.T) {
	want := map[string]pluginv1.Danger{
		"info": pluginv1.Danger_DANGER_SAFE, "read_mac": pluginv1.Danger_DANGER_SAFE,
		"read": pluginv1.Danger_DANGER_SAFE, "read_info_page": pluginv1.Danger_DANGER_SAFE,
		"reset": pluginv1.Danger_DANGER_CONFIRM, "erase": pluginv1.Danger_DANGER_CONFIRM, "program": pluginv1.Danger_DANGER_CONFIRM,
	}
	for _, m := range NewWith("/nonexistent", &fakeTool{}).Info().Methods {
		if w, ok := want[m.Name]; !ok || m.Danger != w {
			t.Errorf("%s: danger %v", m.Name, m.Danger)
		}
		delete(want, m.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing methods %v", want)
	}
}

func TestInfoAndMAC(t *testing.T) {
	ft := &fakeTool{out: realInfo}
	p := NewWith(sysfsWith(t, ccDebugger), ft)
	res, err := invoke(t, p, pluginv1.CapDebugProbe, "info", "90000003", nil)
	if err != nil {
		t.Fatal(err)
	}
	chip := res["chip"].(map[string]any)
	if chip["id"] != "0x2530" || res["target"] != "CC2530" {
		t.Fatalf("%v", res)
	}
	if got := strings.Join(ft.runs[0].args, " "); got != "-d 2:4 -t" {
		t.Fatalf("argv %q", got)
	}
	ft.out = realMAC
	res, err = invoke(t, p, pluginv1.CapDebugProbe, "read_mac", "90000003", map[string]any{"target": "CC2530"})
	if err != nil || res["primary"] != "00:12:4B:00:19:92:B8:DA" {
		t.Fatalf("%v %v", res, err)
	}
	if got := strings.Join(ft.runs[1].args, " "); got != "-d 2:4 -n CC2530 -a" {
		t.Fatalf("argv %q", got)
	}
}

func TestNotFoundAndMissingTool(t *testing.T) {
	p := NewWith(sysfsWith(t, ccDebugger), &fakeTool{})
	_, err := invoke(t, p, pluginv1.CapDebugProbe, "info", "nope", nil)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("%v", err)
	}
	p = NewWith(sysfsWith(t, ccDebugger), &fakeTool{err: errors.New("cc-tool not found: install it")})
	_, err = invoke(t, p, pluginv1.CapDebugProbe, "info", "90000003", nil)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "install") {
		t.Fatalf("%v", err)
	}
	p = NewWith(sysfsWith(t, ccDebugger), &fakeTool{out: "Programmer: CC Debugger\n  Target: CC2530\nSpecified target CC2541 not found\n"})
	_, err = invoke(t, p, pluginv1.CapDebugProbe, "info", "90000003", map[string]any{"target": "CC2541"})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "CC2541 not found") {
		t.Fatalf("%v", err)
	}
}

func TestReadFlashRefusesOverwriteAndHashes(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "flash.bin")
	ft := &fakeTool{out: "Reading flash...\nCompleted\n", write: func(args []string) {
		_ = os.WriteFile(args[len(args)-1], []byte("firmware"), 0o644)
	}}
	p := NewWith(sysfsWith(t, ccDebugger), ft)
	res, err := invoke(t, p, pluginv1.CapFlashProgramming, "read", "90000003", map[string]any{"path": dst})
	if err != nil {
		t.Fatal(err)
	}
	if res["bytes"].(float64) != 8 || len(res["sha256"].(string)) != 64 {
		t.Fatalf("%v", res)
	}
	if got := strings.Join(ft.runs[0].args, " "); got != "-d 2:4 -f -r "+dst {
		t.Fatalf("argv %q", got)
	}
	_, err = invoke(t, p, pluginv1.CapFlashProgramming, "read", "90000003", map[string]any{"path": dst})
	if status.Code(err) != codes.AlreadyExists || len(ft.runs) != 1 {
		t.Fatalf("overwrite not refused: %v", err)
	}
	if _, err = invoke(t, p, pluginv1.CapFlashProgramming, "read", "90000003", map[string]any{"path": dst, "overwrite": true}); err != nil {
		t.Fatal(err)
	}
}

func TestProgramArgv(t *testing.T) {
	dir := t.TempDir()
	fw := filepath.Join(dir, "fw.hex")
	_ = os.WriteFile(fw, []byte(":00000001FF\n"), 0o644)
	ft := &fakeTool{out: "Programming...\nCompleted\n"}
	p := NewWith(sysfsWith(t, ccDebugger), ft)
	if _, err := invoke(t, p, pluginv1.CapFlashProgramming, "program", "90000003", map[string]any{"path": fw}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ft.runs[0].args, " "); got != "-d 2:4 -e -w "+fw+" -v c -p --reset" {
		t.Fatalf("argv %q", got)
	}
	if _, err := invoke(t, p, pluginv1.CapFlashProgramming, "program", "90000003", map[string]any{"path": fw, "verify": "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("%v", err)
	}
	if _, err := invoke(t, p, pluginv1.CapFlashProgramming, "program", "90000003", map[string]any{"path": filepath.Join(dir, "missing.hex")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("%v", err)
	}
	if len(ft.runs) != 1 {
		t.Fatalf("invalid calls reached cc-tool: %d runs", len(ft.runs))
	}
}

func TestTailLinesDropsProgressSpam(t *testing.T) {
	out := "  Programmer: CC Debugger\n  Reading flash (256 KB)...\n  Progress: 0%  Progress: 1%\r  Progress: 99%  Progress: 100%                    Completed (26.45 s.)\n"
	got := tailLines(out, 8)
	if len(got) != 3 || got[2] != "Completed (26.45 s.)" {
		t.Fatalf("%q", got)
	}
}

func TestUnsupportedMethod(t *testing.T) {
	p := NewWith(sysfsWith(t, ccDebugger), &fakeTool{})
	for _, m := range []string{"halt", "memory"} {
		if _, err := invoke(t, p, pluginv1.CapDebugProbe, m, "90000003", nil); status.Code(err) != codes.Unimplemented {
			t.Errorf("%s: %v", m, err)
		}
	}
	if _, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{}, nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("capture: %v", err)
	}
}
