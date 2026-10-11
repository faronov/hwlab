package jlinknativeplugin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	jusb "github.com/faronov/hwlab/plugins/jlinknative/usb"
)

// The wire fixture is a REAL recording: the exact USB command/reply byte
// pairs a SAM-ICE class J-Link (J-Link V9, HW 9.70) exchanged with an
// EFR32MG1P while the script below ran. Replaying it drives the whole native
// stack (protocol, SWD sequences, adiv5, cortexm) from those bytes with no
// probe attached. It carries no USB serial, bus address, RTT payload or
// firmware content: the memory read is the CPUID register.
//
// Replay is strict: a command that differs from the recorded one fails the
// test, so the fixture must be re-recorded (HWLAB_JLINKNATIVE_RECORD, see
// hardware_test.go) when the wire behaviour of the native stack changes on
// purpose.
const wireFixture = "testdata/real-jlink-native/wire.json"

type wireStep struct {
	Cmd   string `json:"cmd"`
	Reply string `json:"reply"`
}

type wireFile struct {
	Note   string     `json:"note"`
	Probe  string     `json:"probe"`
	Target string     `json:"target"`
	Steps  []wireStep `json:"steps"`
}

type scriptCall struct {
	cap, method string
	args        map[string]any
}

// wireScript is what is recorded and replayed. The run state is left as found.
var wireScript = []scriptCall{
	{"DebugProbe", "info", nil},
	{"DebugProbe", "connect", nil},
	{"DebugProbe", "ap_scan", nil},
	{"DebugProbe", "status", nil},
	{"DebugProbe", "halt", nil},
	{"DebugProbe", "read_registers", nil},
	{"MemoryAccess", "read", map[string]any{"address": float64(0xE000ED00), "length": float64(4)}},
	{"DebugProbe", "resume", nil},
	{"DebugProbe", "status", nil},
	{"DebugProbe", "disconnect", nil},
}

// replayTransport serves recorded exchanges in order.
type replayTransport struct {
	mu    sync.Mutex
	steps []wireStep
	next  int
}

func (r *replayTransport) Exchange(_ context.Context, cmd []byte, replyLen int) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next >= len(r.steps) {
		return nil, fmt.Errorf("replay: exchange %d is past the %d recorded", r.next, len(r.steps))
	}
	s := r.steps[r.next]
	if got := hex.EncodeToString(cmd); got != s.Cmd {
		return nil, fmt.Errorf("replay: exchange %d sent %s, recorded %s", r.next, abbrev(got), abbrev(s.Cmd))
	}
	rep, err := hex.DecodeString(s.Reply)
	if err != nil || len(rep) != replyLen {
		return nil, fmt.Errorf("replay: exchange %d reply %d bytes, expected %d (%v)", r.next, len(rep), replyLen, err)
	}
	r.next++
	return rep, nil
}

func (r *replayTransport) Describe() string { return "replay" }
func (r *replayTransport) Close() error     { return nil }

func abbrev(s string) string {
	if len(s) > 48 {
		return s[:48] + "..."
	}
	return s
}

func loadWire(t *testing.T) wireFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", wireFixture))
	if err != nil {
		t.Fatal(err)
	}
	var w wireFile
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestRealWireFixtureReplaysWithoutHardware(t *testing.T) {
	w := loadWire(t)
	if len(w.Steps) < 50 {
		t.Fatalf("fixture has only %d exchanges", len(w.Steps))
	}
	rt := &replayTransport{steps: w.Steps}
	const id = "jlink-native:REPLAY0001"
	info := &jusb.Info{Serial: "REPLAY0001", Interface: 2, In: 0x83, Out: 0x03, MaxPacket: 64}
	p := NewWith(func() []Entry { return []Entry{{ID: id, USB: info}} },
		func(Entry) (jusb.Transport, error) { return rt, nil })
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	var results []map[string]any
	for _, c := range wireScript {
		r, err := invoke(t, p, id, c.cap, c.method, c.args)
		if err != nil {
			t.Fatalf("%s.%s: %v", c.cap, c.method, err)
		}
		results = append(results, r)
	}
	if err := p.CloseDevice(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if rt.next != len(w.Steps) {
		t.Errorf("replayed %d of %d recorded exchanges", rt.next, len(w.Steps))
	}

	if fw, _ := results[0]["firmware"].(string); !strings.HasPrefix(fw, "J-Link V9") {
		t.Errorf("firmware %q", fw)
	}
	if hw, _ := results[0]["hardware_version"].(string); !strings.Contains(hw, "9.70") {
		t.Errorf("hardware version %q", hw)
	}
	c := results[1]
	if c["dpidr"] != "0x2ba01477" || c["core"] != "Cortex-M4" {
		t.Errorf("connect %v", c)
	}
	if ap, _ := json.Marshal(results[2]); !strings.Contains(string(ap), "0x24770011") {
		t.Errorf("ap_scan lacks the MEM-AP IDR: %s", ap)
	}
	if results[6]["hex"] != "41c20f41" {
		t.Errorf("CPUID bytes %v", results[6]["hex"])
	}
	if results[4]["halted"] != true || results[7]["halted"] == true {
		t.Errorf("halt/resume %v / %v", results[4], results[7])
	}
}

// The committed fixture must not carry the probe's USB serial or a private
// path, and must contain nothing but hex exchanges.
func TestWireFixtureIsSanitized(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", wireFixture))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, bad := range []string{"90000001", "/home/", "usb/"} {
		if strings.Contains(s, bad) {
			t.Errorf("fixture contains %q", bad)
		}
	}
}
