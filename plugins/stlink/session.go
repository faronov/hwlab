package stlinkplugin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/stlink/stlink"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

// Device-level settings.
var deviceKeys = []string{"transport", "clock_khz", "timeout_ms", "reconnect_timeout_ms",
	"allow_memory_write", "allow_reset", "allow_flash", "allow_erase", "allow_chip_erase"}

// permKeys are the settings that widen what the session may do to the target.
// They are all declared as dangerous configuration.
var permKeys = []string{"allow_memory_write", "allow_reset", "allow_flash", "allow_erase", "allow_chip_erase"}

// perms are the explicit permissions of one session. Everything defaults to
// false: a session can inspect and debug but never write memory, reset or
// touch flash unless its configuration says so.
type perms struct {
	memWrite, reset, flash, erase, chipErase bool
}

type options struct {
	transport string // swd or jtag
	clock     int    // requested kHz
	timeout   time.Duration
	reconnect time.Duration
	perm      perms
}

func defaultOptions() options {
	return options{transport: "swd", clock: 1800, timeout: 2 * time.Second, reconnect: 8 * time.Second}
}

// errInvalid marks a failure caused by the caller's settings.
var errInvalid = errors.New("invalid setting")

func isErr(err, target error) bool { return errors.Is(err, target) }

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
	intIn := func(key string, lo, hi float64) (int, bool, error) {
		v, ok, err := optNumber(cfg, key)
		if err != nil || !ok {
			return 0, false, err
		}
		if v < lo || v > hi || v != math.Trunc(v) {
			return 0, false, fmt.Errorf("%s %v: want an integer %v..%v", key, v, lo, hi)
		}
		return int(v), true, nil
	}
	if v, ok := cfg["transport"]; ok && v != nil {
		t, isStr := v.(string)
		if !isStr || (t != "swd" && t != "jtag") {
			return o, fmt.Errorf("transport %v: want \"swd\" or \"jtag\"", v)
		}
		o.transport = t
	}
	if v, ok, err := intIn("clock_khz", 1, 4000); err != nil {
		return o, err
	} else if ok {
		o.clock = v
	}
	if v, ok, err := intIn("timeout_ms", 50, 60000); err != nil {
		return o, err
	} else if ok {
		o.timeout = time.Duration(v) * time.Millisecond
	}
	if v, ok, err := intIn("reconnect_timeout_ms", 0, 60000); err != nil {
		return o, err
	} else if ok {
		o.reconnect = time.Duration(v) * time.Millisecond
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
		case "allow_memory_write":
			o.perm.memWrite = b
		case "allow_reset":
			o.perm.reset = b
		case "allow_flash":
			o.perm.flash = b
		case "allow_erase":
			o.perm.erase = b
		case "allow_chip_erase":
			o.perm.chipErase = b
		}
	}
	return o, nil
}

// session is one open probe: one claimed interface, a connected debug port
// and the Cortex-M core behind it.
type session struct {
	entry Entry
	o     options
	iface int
	port  usb.Port
	conn  *stlink.Conn
	info  stlink.ConnectInfo
	// serialChecked is "" when the serial from the string descriptor equals
	// the one from sysfs, otherwise a description of why it could not be
	// confirmed.
	serialChecked string
	serialDesc    string
	actualKHz     int

	core *cortexm.Core
	fpb  *cortexm.FPB
	dwt  *cortexm.DWT

	chip *stm32.Profile // non-nil only when an STM32 was positively recognised

	// identity is what this session first saw; a reconnect must match it.
	identity *identity
	// bps and wps are what this session installed. They are removed again
	// when the session closes.
	bps map[uint32]cortexm.Breakpoint
	// What the comparators held before this session touched them.
	fpbSaved *cortexm.FPBState

	// persist marks a session opened with OpenDevice (kept across calls), as
	// opposed to the one-call session of an ad-hoc invoke.
	persist bool

	closed bool
	mu     sync.Mutex // serialises operations
}

// stringDescriptorSerial reads the serial string descriptor directly from
// the probe and returns its canonical form. Some ST-Link clones return raw
// bytes as UTF-16 units; sysfs mangles those into UTF-8, so the descriptor
// is the authoritative source.
func stringDescriptorSerial(ctx context.Context, port usb.Port, idx uint16) (string, error) {
	b, err := port.ControlIn(ctx, 0x80, 6, 0x0300|idx, 0x0409, 64)
	if err != nil {
		return "", err
	}
	if len(b) < 2 || b[1] != 3 {
		return "", errors.New("not a string descriptor")
	}
	n := int(b[0])
	if n > len(b) {
		n = len(b)
	}
	var raw []rune
	for i := 2; i+1 < n; i += 2 {
		raw = append(raw, rune(uint16(b[i])|uint16(b[i+1])<<8))
	}
	return CanonicalSerial(string(raw)), nil
}

// openSession claims only the debug interface, enters debug mode and reads
// the core identity. On any failure everything is released again.
func openSession(ctx context.Context, e Entry, o options, open Opener) (_ *session, err error) {
	ifc, ok := debugInterface(e.Dev)
	if !ok {
		return nil, fmt.Errorf("%w: no vendor-class interface with a bulk OUT+IN pair (is this an ST-Link?)", errInvalid)
	}
	// No kernel driver is ever detached: the debug interface has none, and a
	// bound driver means another user owns the probe, which fails as busy.
	port, err := open(e, ifc.Number, usb.OpenOptions{})
	if err != nil {
		return nil, err
	}
	s := &session{entry: e, o: o, iface: ifc.Number, port: port, bps: map[uint32]cortexm.Breakpoint{}}
	defer func() {
		if err != nil {
			_ = port.Close()
		}
	}()
	if got, derr := stringDescriptorSerial(ctx, port, 3); derr == nil {
		s.serialDesc = got
		if e.Serial != "" && got != e.Serial {
			return nil, fmt.Errorf("%w: the probe's own serial descriptor says %q but it was selected as %q: refusing to continue on a different probe", errInvalid, got, e.Serial)
		}
	} else if errors.Is(derr, usb.ErrDisconnected) {
		return nil, derr
	} else {
		s.serialChecked = "serial descriptor unreadable: " + derr.Error()
	}
	s.conn, err = stlink.Open(ctx, port, ifc, o.timeout)
	if err != nil {
		return nil, err
	}
	s.info, err = s.conn.Connect(ctx, stlink.ConnectConfig{Transport: o.transport, KHz: o.clock})
	if err != nil {
		return nil, err
	}
	s.actualKHz = s.info.ActualKHz
	s.core = cortexm.New(s.conn)
	s.core.Timeout = o.timeout
	return s, nil
}

// close removes this session's breakpoints and watchpoints, leaves debug
// mode (the core keeps its run state) and releases the USB interface.
func (s *session) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if s.conn != nil {
		err := s.dropTriggers(ctx)
		return errors.Join(err, s.conn.Close(ctx), s.port.Close())
	}
	return s.port.Close()
}

// transportErr makes USB failures readable.
func transportErr(err error) error {
	switch {
	case errors.Is(err, usb.ErrDisconnected):
		return fmt.Errorf("the probe was unplugged: %w", err)
	case errors.Is(err, usb.ErrTimeout):
		return fmt.Errorf("the probe stopped answering: %w", err)
	}
	return err
}
