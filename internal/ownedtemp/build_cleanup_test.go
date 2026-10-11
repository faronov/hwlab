package ownedtemp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBuildCleanupUsesOwnershipMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Makefile cleaner is a POSIX shell script")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("Makefile cleaner requires sha256sum")
	}
	_, file, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(file), "../../scripts/build-ownership.sh")
	dir := t.TempDir()
	for _, name := range []string{"created.out", "modified.bin", "neighbor.out"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("sh", append([]string{script}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("build cleaner: %v, %s", err, out)
		}
	}
	run("record", dir, "created.out", "modified.bin")
	if err := os.WriteFile(filepath.Join(dir, "modified.bin"), []byte("user replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("clean", dir)
	if _, err := os.Stat(filepath.Join(dir, "created.out")); !os.IsNotExist(err) {
		t.Fatalf("owned output survived: %v", err)
	}
	for name, want := range map[string]string{"neighbor.out": "neighbor.out", "modified.bin": "user replacement"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("unrelated/modified output changed: %s %q, %v", name, got, err)
		}
	}
	// No manifest means no ownership, even for familiar file extensions.
	run("clean", dir)
	if b, err := os.ReadFile(filepath.Join(dir, "neighbor.out")); err != nil || string(b) != "neighbor.out" {
		t.Fatalf("manifest-free clean changed sentinel: %q, %v", b, err)
	}
}
