// Package testutil holds helpers shared by hwlab tests.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// CheckError reports cleanup failures without stopping remaining cleanup.
func CheckError(tb testing.TB, err error) {
	tb.Helper()
	if err != nil {
		tb.Error(err)
	}
}

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
	// Every test starts a freshly built binary; on a loaded host (the whole
	// module tested in parallel) or one that scans new executables on their
	// first start, the default 15 s handshake limit is not enough.
	// internal/plugin reads it (LaunchTimeoutEnv; not imported: cycle).
	if os.Getenv("HWLAB_PLUGIN_LAUNCH_TIMEOUT") == "" {
		_ = os.Setenv("HWLAB_PLUGIN_LAUNCH_TIMEOUT", "90s")
	}
	dir := tb.TempDir()
	goBin, err := exec.LookPath("go")
	if err != nil {
		tb.Fatalf("locate Go tool: %v", err)
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
