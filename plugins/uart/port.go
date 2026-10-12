package uart

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.bug.st/serial"
)

// Port is an open serial line. It is an interface so the plugin logic can be
// tested without a device.
type Port interface {
	io.ReadWriteCloser
	// SetReadTimeout bounds each Read; a timed-out Read returns 0, nil.
	SetReadTimeout(d time.Duration) error
	SetMode(m Mode) error
	SetDTR(on bool) error
	SetRTS(on bool) error
	// ResetInput discards received bytes that were not read yet.
	ResetInput() error
}

// Info describes one serial port found on the host.
type Info struct {
	Path    string
	VID     string
	PID     string
	Serial  string
	Product string
	USB     bool
	// Alias replaces the base name as device id (virtual ports, see
	// ExtraPortsEnv).
	Alias string
}

// ID is the stable plugin-local device id: the port's alias, else its base name.
func (i Info) ID() string {
	if i.Alias != "" {
		return i.Alias
	}
	return filepath.Base(i.Path)
}

// ExtraPortsEnv lists ports the OS does not enumerate (pseudo terminals,
// socat or rfcomm links), as comma-separated name=path pairs, e.g.
// "pty0=/dev/pts/5". They are offered as devices named uart/<name>.
const ExtraPortsEnv = "HWLAB_UART_EXTRA_PORTS"

// parseExtraPorts parses the value of ExtraPortsEnv; malformed entries are
// ignored.
func parseExtraPorts(v string) []Info {
	var out []Info
	for _, kv := range strings.Split(v, ",") {
		name, path, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && name != "" && path != "" {
			out = append(out, Info{Alias: name, Path: path})
		}
	}
	return out
}

// ListWithExtra enumerates the host's serial ports plus those named in
// ExtraPortsEnv.
func ListWithExtra() ([]Info, error) {
	infos, err := ListPorts()
	extra := parseExtraPorts(os.Getenv(ExtraPortsEnv))
	if err != nil && len(extra) == 0 {
		return nil, err
	}
	return append(infos, extra...), nil
}

// Lister enumerates serial ports.
type Lister func() ([]Info, error)

// Opener opens the serial port at path.
type Opener func(path string, m Mode) (Port, error)

type realPort struct {
	serial.Port
	path string
}

// OpenPort opens a real serial port.
func OpenPort(path string, m Mode) (Port, error) {
	if m.Flow != FlowNone && !flowSupported() {
		return nil, fmt.Errorf("flow_control %s is not supported on this operating system", m.Flow)
	}
	sm, err := toSerialMode(m)
	if err != nil {
		return nil, err
	}
	p, err := serial.Open(path, sm)
	if err != nil {
		return nil, err
	}
	rp := &realPort{Port: p, path: path}
	if m.Flow != FlowNone {
		if err := applyFlow(p, m.Flow); err != nil {
			return nil, errors.Join(err, p.Close())
		}
	}
	return rp, nil
}

func (p *realPort) SetMode(m Mode) error {
	sm, err := toSerialMode(m)
	if err != nil {
		return err
	}
	if err := p.Port.SetMode(sm); err != nil {
		return err
	}
	if flowSupported() {
		return applyFlow(p.Port, m.Flow)
	}
	if m.Flow != FlowNone {
		return fmt.Errorf("flow_control %s is not supported on this operating system", m.Flow)
	}
	return nil
}

func (p *realPort) ResetInput() error { return p.ResetInputBuffer() }

func toSerialMode(m Mode) (*serial.Mode, error) {
	sm := &serial.Mode{BaudRate: m.Baud, DataBits: m.DataBits}
	switch m.Parity {
	case "none":
		sm.Parity = serial.NoParity
	case "even":
		sm.Parity = serial.EvenParity
	case "odd":
		sm.Parity = serial.OddParity
	case "mark":
		sm.Parity = serial.MarkParity
	case "space":
		sm.Parity = serial.SpaceParity
	default:
		return nil, fmt.Errorf("parity %q", m.Parity)
	}
	switch m.StopBits {
	case "1":
		sm.StopBits = serial.OneStopBit
	case "1.5":
		sm.StopBits = serial.OnePointFiveStopBits
	case "2":
		sm.StopBits = serial.TwoStopBits
	default:
		return nil, fmt.Errorf("stop_bits %q", m.StopBits)
	}
	if m.DTR != nil || m.RTS != nil {
		bits := &serial.ModemOutputBits{DTR: true, RTS: true}
		if m.DTR != nil {
			bits.DTR = *m.DTR
		}
		if m.RTS != nil {
			bits.RTS = *m.RTS
		}
		sm.InitialStatusBits = bits
	}
	return sm, nil
}
