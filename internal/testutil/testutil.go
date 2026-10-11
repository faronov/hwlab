// Package testutil holds helpers shared by hwlab tests.
package testutil

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// ModuleRoot returns the repository root (the directory holding go.mod).
func ModuleRoot(tb testing.TB) string {
	tb.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		tb.Fatal("cannot locate module root")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// BuildFakePlugin compiles the fake plugin into a fresh temporary directory
// and returns that directory, suitable for config.PluginPaths. The go build
// cache keeps repeated calls cheap.
func BuildFakePlugin(tb testing.TB) string {
	tb.Helper()
	return BuildPlugin(tb, "fake")
}

// BuildPlugin compiles plugins/<name>/cmd/hwlab-plugin-<name> into a fresh
// temporary directory and returns that directory.
func BuildPlugin(tb testing.TB, name string) string {
	tb.Helper()
	dir := tb.TempDir()
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(goBin, "build", "-o", filepath.Join(dir, "hwlab-plugin-"+name+exeSuffix()), "./plugins/"+name+"/cmd/hwlab-plugin-"+name)
	cmd.Dir = ModuleRoot(tb)
	if out, err := cmd.CombinedOutput(); err != nil {
		tb.Fatalf("build %s plugin: %v\n%s", name, err, out)
	}
	return dir
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
