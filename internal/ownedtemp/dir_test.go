package ownedtemp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupPreservesNeighbor(t *testing.T) {
	for _, name := range []string{"success", "error", "cancel", "DEVICE_GONE"} {
		t.Run(name, func(t *testing.T) {
			sentinel, err := os.CreateTemp("", "hwlab-unrelated-sentinel-*.out")
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				if err := os.Remove(sentinel.Name()); err != nil {
					t.Error(err)
				}
			})
			const contents = "unrelated reference output\n"
			if _, err := sentinel.WriteString(contents); err != nil {
				t.Fatal(err)
			}
			if err := sentinel.Close(); err != nil {
				t.Fatal(err)
			}
			var path string
			run := func() (err error) {
				dir, err := New("hwlab-jlink-native-")
				if err != nil {
					return err
				}
				defer func() { err = errors.Join(err, dir.Close()) }()
				path = dir.Path()
				if err := os.WriteFile(dir.File("owned.out"), []byte("owned"), 0o600); err != nil {
					return err
				}
				switch name {
				case "error":
					return errors.New("operation failed")
				case "cancel":
					return context.Canceled
				case "DEVICE_GONE":
					return errors.New("DEVICE_GONE")
				}
				return nil
			}
			if err := run(); (err == nil) != (name == "success") {
				t.Fatalf("run returned %v", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned directory survived: %v", err)
			}
			got, err := os.ReadFile(sentinel.Name())
			if err != nil || string(got) != contents {
				t.Fatalf("sentinel changed: %q, %v", got, err)
			}
		})
	}
}

func TestCleanupDoesNotAcquireReplacedDirectory(t *testing.T) {
	dir, err := New("hwlab-owned-")
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "original")
	if err := os.Rename(dir.Path(), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := dir.File("neighbor.out")
	t.Cleanup(func() {
		if err := os.Remove(sentinel); err != nil {
			t.Error(err)
		}
		if err := os.Remove(dir.Path()); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(sentinel, []byte("reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dir.Close(); err == nil {
		t.Fatal("replacement directory claimed as owned")
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "reference" {
		t.Fatalf("replacement sentinel changed: %q, %v", b, err)
	}
}
