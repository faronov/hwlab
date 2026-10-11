package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/faronov/hwlab/internal/testutil"
)

// The protocol tests launch the REAL `hwlab mcp --stdio` process. The binary
// and the plugins it needs are built once per test run.
var (
	buildMu  sync.Mutex
	buildDir string
	built    = map[string]bool{}
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func goBin() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// binDir returns a directory holding bin/hwlab and the named plugins.
func binDir(t testing.TB, plugins ...string) string {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if buildDir == "" {
		d, err := os.MkdirTemp("", "hwlab-mcp-bin-")
		if err != nil {
			t.Fatal(err)
		}
		buildDir = d
	}
	root := testutil.ModuleRoot(t)
	build := func(out, pkg string) {
		if built[out] {
			return
		}
		cmd := exec.Command(goBin(), "build", "-o", filepath.Join(buildDir, out), pkg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
		built[out] = true
	}
	build("hwlab", "./cmd/hwlab")
	for _, p := range plugins {
		build("hwlab-plugin-"+p, "./plugins/"+p+"/cmd/hwlab-plugin-"+p)
	}
	return buildDir
}

type serverOpts struct {
	Home    string
	Plugins []string
	Args    []string // extra args after mcp --stdio
	Env     []string
}

func (o serverOpts) cmd(t testing.TB) *exec.Cmd {
	t.Helper()
	dir := binDir(t, o.Plugins...)
	if o.Home == "" {
		o.Home = t.TempDir()
	}
	args := append([]string{"mcp", "--stdio"}, o.Args...)
	cmd := exec.Command(filepath.Join(dir, "hwlab"), args...)
	cmd.Env = append(os.Environ(), "HWLAB_HOME="+o.Home, "HWLAB_PLUGIN_PATH="+dir)
	cmd.Env = append(cmd.Env, o.Env...)
	return cmd
}

// startServer launches the real server process and connects a real MCP
// client over its stdio.
func startServer(t testing.TB, o serverOpts) *sdk.ClientSession {
	t.Helper()
	cmd := o.cmd(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "hwlab-test-client", Version: "0"}, nil).
		Connect(ctx, &sdk.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
	if err != nil {
		t.Fatalf("connect to hwlab mcp: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callTool calls a tool and returns the result plus its structured content.
func callTool(t testing.TB, cs *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &out)
	}
	return res, out
}

// errCode returns the structured error code of a failed tool call.
func errCode(t testing.TB, res *sdk.CallToolResult, out map[string]any) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("expected isError, got success: %v", out)
	}
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	if code == "" || e["message"] == nil {
		t.Fatalf("error without structured code/message: %v", out)
	}
	return code
}

// raw is a bare JSON-RPC peer for malformed-input tests.
type raw struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan string
	done  chan error
}

func startRaw(t testing.TB, o serverOpts) *raw {
	t.Helper()
	cmd := o.cmd(t)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &raw{cmd: cmd, in: in, lines: make(chan string, 64), done: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			r.lines <- sc.Text()
		}
		close(r.lines)
	}()
	go func() { r.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = in.Close()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	return r
}

func (r *raw) send(s string) {
	_, _ = io.WriteString(r.in, s+"\n")
}

// recv returns the next JSON message from the server.
func (r *raw) recv(t testing.TB) map[string]any {
	t.Helper()
	select {
	case l, ok := <-r.lines:
		if !ok {
			t.Fatal("server closed stdout")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("server wrote a non-JSON line on stdout: %q", l)
		}
		return m
	case <-time.After(30 * time.Second):
		t.Fatal("no message from the server within 30s")
	}
	return nil
}

func (r *raw) initialize(t testing.TB) map[string]any {
	t.Helper()
	r.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`)
	m := r.recv(t)
	r.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return m
}

// exited waits for the process to end and returns its error.
func (r *raw) exited(t testing.TB, within time.Duration) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(within):
		t.Fatalf("server still running %s after the input ended", within)
	}
	return nil
}

func rpcErrCode(m map[string]any) float64 {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

var _ = fmt.Sprintf
