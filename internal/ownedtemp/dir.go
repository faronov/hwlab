// Package ownedtemp scopes temporary files to a directory created by this process.
package ownedtemp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Dir cannot be constructed from a caller-supplied existing path.
type Dir struct {
	path   string
	info   os.FileInfo
	mu     sync.Mutex
	closed bool
}

func New(prefix string) (*Dir, error) {
	path, err := os.MkdirTemp("", prefix)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(path))
	}
	return &Dir{path: path, info: info}, nil
}

func (d *Dir) Path() string { return d.path }

func (d *Dir) File(name string) string { return filepath.Join(d.path, name) }

// Close removes only the directory allocated by New, never neighboring files.
func (d *Dir) Close() error {
	if d == nil || d.path == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	info, err := os.Lstat(d.path)
	if errors.Is(err, os.ErrNotExist) {
		d.closed = true
		return nil
	}
	if err != nil {
		return err
	}
	if d.info == nil || !os.SameFile(d.info, info) {
		return fmt.Errorf("temporary directory %s was replaced; refusing cleanup", d.path)
	}
	if err := os.RemoveAll(d.path); err != nil {
		return err
	}
	d.closed = true
	return nil
}
