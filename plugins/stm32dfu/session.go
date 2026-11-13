package stm32dfuplugin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/stm32dfu/dfu"
)

// Device-level settings.
var deviceKeys = []string{"timeout_ms", "allow_flash", "allow_erase", "allow_reset"}

// permKeys widen what a session may do; all are dangerous configuration.
var permKeys = []string{"allow_flash", "allow_erase", "allow_reset"}

type perms struct{ flash, erase, reset bool }

type options struct {
	timeout time.Duration // one DFU command (DNLOAD + status polling)
	perm    perms
}

func defaultOptions() options { return options{timeout: 3 * time.Second} }

var errInvalid = errors.New("invalid setting")

func optNumber(a map[string]any, key string) (float64, bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) {
		return 0, false, fmt.Errorf("%s: want a number, got %v", key, v)
	}
	return f, true, nil
}

func parseOptions(base options, cfg map[string]any) (options, error) {
	o := base
	for k := range cfg {
		known := false
		for _, s := range deviceKeys {
			known = known || s == k
		}
		if !known {
			return o, fmt.Errorf("unknown setting %q (want %s)", k, strings.Join(deviceKeys, ", "))
		}
	}
	if v, ok, err := optNumber(cfg, "timeout_ms"); err != nil {
		return o, err
	} else if ok {
		if v < 100 || v > 60000 || v != math.Trunc(v) {
			return o, fmt.Errorf("timeout_ms %v: want an integer 100..60000", v)
		}
		o.timeout = time.Duration(v) * time.Millisecond
	}
	for _, k := range permKeys {
		v, ok := cfg[k]
		if !ok || v == nil {
			continue
		}
		b, isBool := v.(bool)
		if !isBool {
			return o, fmt.Errorf("%s: want true or false, got %v", k, v)
		}
		switch k {
		case "allow_flash":
			o.perm.flash = b
		case "allow_erase":
			o.perm.erase = b
		case "allow_reset":
			o.perm.reset = b
		}
	}
	return o, nil
}

// session is one claimed DFU interface.
type session struct {
	entry Entry
	o     options
	iface int
	port  usb.Port
	c     *dfu.Client

	devDesc    []byte
	serialDesc string
	serialNote string
	cfg        dfu.ConfigInfo
	commands   []byte
	left       bool
	tgt        *target

	mu      sync.Mutex
	closed  bool
	persist bool
}

func openCode(err error) codes.Code {
	switch {
	case errors.Is(err, errInvalid):
		return codes.InvalidArgument
	case errors.Is(err, usb.ErrPermission):
		return codes.PermissionDenied
	case errors.Is(err, usb.ErrBusy):
		return codes.FailedPrecondition
	}
	return codes.Unavailable
}

func getString(ctx context.Context, p usb.Port, idx uint8) (string, error) {
	if idx == 0 {
		return "", errors.New("no string")
	}
	b, err := p.ControlIn(ctx, 0x80, 6, 0x0300|uint16(idx), 0x0409, 255)
	if err != nil {
		return "", err
	}
	return dfu.DecodeString(b)
}

// openSession claims the DFU interface, reads the descriptors and brings
// the interface to dfuIDLE. Nothing is downloaded.
func openSession(ctx context.Context, e Entry, o options, open Opener) (_ *session, err error) {
	ifc, ok := dfuInterface(e.Dev)
	if !ok {
		return nil, fmt.Errorf("%w: no DFU interface (class 0xFE) on %04x:%04x", errInvalid, e.Dev.VID, e.Dev.PID)
	}
	port, err := open(e, ifc.Number, usb.OpenOptions{})
	if err != nil {
		return nil, err
	}
	s := &session{entry: e, o: o, iface: ifc.Number, port: port}
	defer func() {
		if err != nil {
			_ = port.Close()
		}
	}()
	dd, err := port.ControlIn(ctx, 0x80, 6, 0x0100, 0, 18)
	if err != nil {
		return nil, fmt.Errorf("device descriptor: %w", err)
	}
	if len(dd) < 18 || dd[1] != 1 {
		return nil, fmt.Errorf("%w: bad device descriptor", dfu.ErrProtocol)
	}
	s.devDesc = dd
	if got, derr := getString(ctx, port, dd[16]); derr == nil {
		s.serialDesc = strings.TrimSpace(got)
		if e.Serial != "" && s.serialDesc != e.Serial && canonicalSerial(usb.Device{Serial: got}) != e.Serial {
			return nil, fmt.Errorf("%w: the device's serial descriptor says %q but it was selected as %q: refusing to continue on a different device", errInvalid, got, e.Serial)
		}
	} else if errors.Is(derr, usb.ErrDisconnected) {
		return nil, derr
	} else {
		s.serialNote = "serial descriptor unreadable: " + derr.Error()
	}
	cd, err := port.ControlIn(ctx, 0x80, 6, 0x0200, 0, 9)
	if err != nil {
		return nil, fmt.Errorf("configuration descriptor: %w", err)
	}
	if len(cd) >= 4 {
		total := int(cd[2]) | int(cd[3])<<8
		if cd, err = port.ControlIn(ctx, 0x80, 6, 0x0200, 0, min(max(total, 9), 1024)); err != nil {
			return nil, fmt.Errorf("configuration descriptor: %w", err)
		}
	}
	s.cfg, err = dfu.ParseConfig(cd)
	if err != nil {
		return nil, err
	}
	if s.cfg.Functional == nil || s.cfg.Functional.TransferSize < 8 {
		return nil, fmt.Errorf("%w: no usable DFU functional descriptor (wTransferSize)", dfu.ErrProtocol)
	}
	for i := range s.cfg.Alts {
		a := &s.cfg.Alts[i]
		if int(a.Interface) != ifc.Number {
			continue
		}
		name, serr := getString(ctx, port, a.IString)
		if serr != nil {
			a.ParseErr = "string descriptor: " + serr.Error()
			continue
		}
		a.Name = name
		if a.Memory, serr = dfu.ParseMemory(name); serr != nil {
			a.ParseErr = serr.Error()
		}
	}
	s.c = dfu.New(port, uint16(ifc.Number), int(s.cfg.Functional.TransferSize))
	s.c.CmdTimeout = o.timeout
	if err := s.c.SelectAlt(ctx, 0); err != nil {
		return nil, err
	}
	if err := s.c.EnsureIdle(ctx); err != nil {
		return nil, fmt.Errorf("bringing the DFU interface to dfuIDLE: %w", err)
	}
	return s, nil
}

func (s *session) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.c != nil && !s.left {
		s.c.CloseWriteWindow()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.c.EnsureIdle(ctx)
		cancel()
	}
	return s.port.Close()
}

// alt returns the alternate setting by number.
func (s *session) altByNum(n uint8) *dfu.AltSetting {
	for i := range s.cfg.Alts {
		if s.cfg.Alts[i].Alt == n && int(s.cfg.Alts[i].Interface) == s.iface {
			return &s.cfg.Alts[i]
		}
	}
	return nil
}

// altCovering returns the alternate setting whose memory map covers the
// range, preferring the first one.
func (s *session) altCovering(addr uint32, n int) *dfu.AltSetting {
	for i := range s.cfg.Alts {
		a := &s.cfg.Alts[i]
		if int(a.Interface) == s.iface && a.Memory != nil && a.Memory.Covers(addr, n) {
			return a
		}
	}
	return nil
}
