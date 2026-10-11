//go:build !darwin || cgo

package uart

import (
	"fmt"
	"sort"

	"go.bug.st/serial/enumerator"
)

// ListPorts enumerates the serial ports of the host, sorted by path, with USB
// identification. On macOS this goes through IOKit and therefore needs cgo;
// see enum_darwin_nocgo.go for the cgo-free build.
func ListPorts() ([]Info, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, fmt.Errorf("enumerate serial ports: %w", err)
	}
	out := make([]Info, 0, len(ports))
	for _, p := range ports {
		out = append(out, Info{Path: p.Name, VID: p.VID, PID: p.PID, Serial: p.SerialNumber, Product: p.Product, USB: p.IsUSB})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
