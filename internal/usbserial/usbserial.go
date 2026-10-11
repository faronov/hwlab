// Package usbserial is the small serial-port layer shared by hardware
// plugins: USB CDC-ACM discovery by VID/PID and an abstract Port so protocol
// code can be tested against simulated devices.
//
// Note: on macOS the discovery uses IOKit through cgo (a limitation of
// go.bug.st/serial/enumerator), so plugin binaries that import this package
// must be built natively on macOS. The hwlab core and the fake plugin stay
// cgo-free. See docs/adr/0009-hardware-plugin-build.md.
package usbserial

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// Info describes one USB serial port.
type Info struct {
	Name         string
	VID, PID     string // hex as reported by the OS, compare case-insensitively
	SerialNumber string
	Product      string
}

// Port is a byte stream with a read timeout.
type Port interface {
	io.ReadWriteCloser
	// SetReadTimeout bounds each Read; a timed-out Read returns 0, nil.
	SetReadTimeout(d time.Duration) error
}

// Lister enumerates serial ports.
type Lister func() ([]Info, error)

// Opener opens a port by name.
type Opener func(name string) (Port, error)

// List enumerates all USB serial ports, sorted by name.
func List() ([]Info, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, fmt.Errorf("enumerate serial ports: %w", err)
	}
	var out []Info
	for _, p := range ports {
		if !p.IsUSB {
			continue
		}
		out = append(out, Info{Name: p.Name, VID: p.VID, PID: p.PID, SerialNumber: p.SerialNumber, Product: p.Product})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Match filters infos by VID and, when pid is non-empty, PID (hex, any case).
func Match(infos []Info, vid, pid string) []Info {
	var out []Info
	for _, i := range infos {
		if strings.EqualFold(i.VID, vid) && (pid == "" || strings.EqualFold(i.PID, pid)) {
			out = append(out, i)
		}
	}
	return out
}

// Open returns an Opener for 8N1 serial at the given baud rate.
func Open(baud int) Opener {
	return func(name string) (Port, error) {
		p, err := serial.Open(name, &serial.Mode{BaudRate: baud})
		if err != nil {
			return nil, err
		}
		return p, nil
	}
}
