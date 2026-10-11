package ccdebugger

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPublishDoesNotOverwriteLateUserFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reference.bin")
	f, tmp, err := newFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("owned output")); err != nil {
		t.Fatal(err)
	}
	// A user file appears after the initial existence check.
	if err := os.WriteFile(path, []byte("reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publish(f, tmp, path, false); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("publish did not reject existing output: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "reference" {
		t.Fatalf("user file changed: %q, %v", b, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("owned staging file leaked: %v", err)
	}
}
