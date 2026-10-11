package ownedtemp

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Publish stages output in an exclusively created sibling file. Failed writes
// leave caller-owned output and neighboring reference files untouched.
func Publish(path string, overwrite bool, write func(io.Writer) error) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".hwlab-output-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		if e := os.Remove(name); e != nil && !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	werr := write(f)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return err
	}
	if overwrite {
		return os.Rename(name, path)
	}
	// Link publishes without the check-then-rename race over an existing file.
	return os.Link(name, path)
}

func WriteFile(path string, data []byte, overwrite bool) error {
	return Publish(path, overwrite, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}
