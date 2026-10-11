//go:build darwin && !cgo

package uart

import (
	"path/filepath"
	"sort"
)

// ListPorts is the cgo-free macOS listing: it globs the call-out device nodes
// and cannot read USB identity (VID/PID/serial), because that needs IOKit
// through cgo. Serial I/O itself is pure Go, so a port can still be opened by
// path; only discovery by VID/PID is lost. Build with cgo for full discovery.
func ListPorts() ([]Info, error) {
	var out []Info
	for _, pat := range []string{"/dev/cu.usbmodem*", "/dev/cu.usbserial*", "/dev/cu.SLAB*", "/dev/cu.wchusbserial*"} {
		m, _ := filepath.Glob(pat)
		for _, p := range m {
			out = append(out, Info{Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
