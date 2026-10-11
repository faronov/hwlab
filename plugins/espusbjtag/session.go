package espusbjtag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/espusbjtag/espusb"
)

// Device-level settings.
var deviceKeys = []string{"interface", "clock_khz", "timeout_ms", "hart", "reconnect_timeout_ms",
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
	iface   int // -1 selects the vendor-class interface
	clock   int // kHz
	timeout time.Duration
	hart    uint32
	// reconnect bounds how long a lost device is waited for (rediscovery by
	// USB serial number and reopening).
	reconnect time.Duration
	perm      perms
}

func defaultOptions() options {
	return options{iface: -1, clock: 4000, timeout: 2 * time.Second, reconnect: 8 * time.Second}
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
	if v, ok, err := intIn("interface", 0, 15); err != nil {
		return o, err
	} else if ok {
		o.iface = v
	}
	if v, ok, err := intIn("clock_khz", 1, 40000); err != nil {
		return o, err
	} else if ok {
		o.clock = v
	}
	if v, ok, err := intIn("timeout_ms", 50, 60000); err != nil {
		return o, err
	} else if ok {
		o.timeout = time.Duration(v) * time.Millisecond
	}
	if v, ok, err := intIn("hart", 0, 7); err != nil {
		return o, err
	} else if ok {
		o.hart = uint32(v)
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

// session is an open device: one claimed vendor interface running the JTAG
// protocol, with the chain, DTM and DM discovered lazily.
type session struct {
	entry Entry
	o     options
	iface int
	probe *espusb.Probe
	drv   *jtag.Driver

	chain *jtag.Chain
	dtm   *riscv.DTM
	dm    *riscv.DM

	// identity is what this session first saw; a reconnect must match it.
	identity *identity
	// bps are the breakpoints this session installed (by address). They are
	// removed again when the session closes.
	bps map[bpKey]riscv.Breakpoint

	// persist marks a session opened with OpenDevice (kept across calls), as
	// opposed to the one-call session of an ad-hoc invoke.
	persist bool

	closed bool
	mu     sync.Mutex // serialises operations
}

// openSession claims only the JTAG interface and opens the transport. On any
// failure the interface is released again.
func openSession(ctx context.Context, e Entry, o options, open Opener) (*session, error) {
	ifnum := o.iface
	var ifc usb.Interface
	var ok bool
	if ifnum < 0 {
		if ifc, ok = jtagInterface(e.Dev); !ok {
			return nil, fmt.Errorf("%w: no vendor-class interface with bulk endpoints (is this an Espressif USB Serial/JTAG device?)", errInvalid)
		}
		ifnum = ifc.Number
	} else if ifc, ok = e.Dev.Iface(ifnum); !ok {
		return nil, fmt.Errorf("%w: device has no USB interface %d", errInvalid, ifnum)
	}
	in, out, _ := ifc.BulkEndpoints()
	if ifc.Class != 0xff || in == 0 || out == 0 {
		return nil, fmt.Errorf("%w: interface %d (class 0x%02x) is not the vendor JTAG interface; the CDC interfaces belong to the uart plugin", errInvalid, ifnum, ifc.Class)
	}
	// No kernel driver is ever detached: interface 0 and 1 keep cdc_acm and
	// the vendor interface has no driver. A bound driver means another user
	// owns it, which fails as busy.
	port, err := open(e, ifnum, usb.OpenOptions{})
	if err != nil {
		return nil, err
	}
	probe, err := espusb.Open(ctx, port, in, out, espusb.Config{ClockKHz: o.clock, Timeout: o.timeout})
	if err != nil {
		_ = port.Close()
		return nil, err
	}
	return &session{entry: e, o: o, iface: ifnum, probe: probe, drv: jtag.NewDriver(probe)}, nil
}

// ensureChain enumerates the chain once and checks it looks like a single
// RISC-V TAP.
func (s *session) ensureChain(ctx context.Context) (*jtag.Chain, error) {
	if s.chain != nil {
		return s.chain, nil
	}
	c, err := jtag.ScanChain(ctx, s.drv, jtag.ScanOptions{MaxDevices: 8, MeasureIR: true})
	if err != nil {
		return nil, err
	}
	s.chain = &c
	return s.chain, nil
}

// tapIRLen is the IR length of the only TAP in the chain.
func (s *session) tapIRLen(ctx context.Context) (int, error) {
	c, err := s.ensureChain(ctx)
	if err != nil {
		return 0, err
	}
	if c.Status != jtag.StatusOK || len(c.Devices) != 1 {
		return 0, fmt.Errorf("%w: expected one TAP in the chain, found %d (status %s)", errNoTarget, len(c.Devices), c.Status)
	}
	if c.TotalIR < 5 {
		return 0, fmt.Errorf("%w: IR length %d is too short for a RISC-V TAP", errNoTarget, c.TotalIR)
	}
	return c.TotalIR, nil
}

var errNoTarget = errors.New("no RISC-V target on the JTAG chain")

// ensureDM builds the DTM and DM once.
func (s *session) ensureDM(ctx context.Context) (*riscv.DM, error) {
	if s.dm != nil {
		return s.dm, nil
	}
	n, err := s.tapIRLen(ctx)
	if err != nil {
		return nil, err
	}
	dtm, err := riscv.NewDTM(ctx, s.drv, n)
	if err != nil {
		return nil, err
	}
	dm := riscv.NewDM(dtm)
	dm.Timeout = 2 * time.Second
	if s.o.hart != 0 {
		if err := dm.Select(ctx, s.o.hart); err != nil {
			return nil, err
		}
	}
	s.dtm, s.dm = dtm, dm
	return dm, nil
}

// close releases the debug module if this session activated it (never while a
// hart is halted) and then the USB interface.
func (s *session) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if s.dm != nil {
		s.dropBreakpoints(ctx)
		_ = s.dm.Release(ctx)
	}
	return s.probe.Close()
}

// transportErr makes USB failures readable.
func transportErr(err error) error {
	switch {
	case errors.Is(err, usb.ErrDisconnected):
		return fmt.Errorf("the device was unplugged or reset: %w", err)
	case errors.Is(err, usb.ErrTimeout):
		return fmt.Errorf("the device stopped answering: %w", err)
	}
	return err
}
