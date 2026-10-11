package ownedtemp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishNeverDeletesReferences(t *testing.T) {
	for _, operationErr := range []error{nil, errors.New("write failed"), context.Canceled, errors.New("DEVICE_GONE")} {
		t.Run("result-"+errorName(operationErr), func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "result.bin")
			neighbor := output + ".part"
			for _, path := range []string{output, neighbor} {
				if err := os.WriteFile(path, []byte("reference"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := Publish(output, true, func(w io.Writer) error {
				if _, err := w.Write([]byte("owned")); err != nil {
					return err
				}
				return operationErr
			})
			if !errors.Is(err, operationErr) {
				t.Fatalf("lost primary error: %v", err)
			}
			want := "owned"
			if operationErr != nil {
				want = "reference"
			}
			for path, contents := range map[string]string{output: want, neighbor: "reference"} {
				b, err := os.ReadFile(path)
				if err != nil || string(b) != contents {
					t.Fatalf("%s: %q, %v", path, b, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 2 {
				t.Fatalf("staging file leaked: %v, %v", entries, err)
			}
			if err := WriteFile(output, []byte("must not overwrite"), false); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing output not refused: %v", err)
			}
		})
	}
}

func errorName(err error) string {
	if err == nil {
		return "success"
	}
	return err.Error()
}
