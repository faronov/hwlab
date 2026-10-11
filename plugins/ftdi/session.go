package ftdiplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/ftdi/ftdi"
	"github.com/faronov/hwlab/plugins/ftdi/jtagport"
	"github.com/faronov/hwlab/plugins/ftdi/mpsse"
	"github.com/faronov/hwlab/plugins/ftdi/profiles"
)

// options are the device-level settings.
type options struct {
	profile      string // "" selects automatically
	iface        int    // -1 selects the profile's JTAG interface
	clockHz      uint32 // 0 selects the profile's nominal clock
	detach       bool
	latencyMs    int
	timeout      time.Duration
	maxDevices   int
	irLengths    []int
	measureIR    bool
	measureIRSet bool
}

func defaultOptions() options {
	return options{iface: -1, detach: true, timeout: time.Second, maxDevices: 32}
}

// session is an open adapter: one claimed interface running MPSSE.
type session struct {
	mu sync.Mutex // serialises operations

	entry   Entry
	o       options
	prof    profiles.Profile
	profSrc string // "configured" or "auto"
	dev     usb.Device
	iface   int
	conn    *ftdi.Conn
	eng     *mpsse.Engine
	drv     *jtag.Driver
	info    ChipFacts
	closed  bool
}

// ChipFacts is what open learned about the adapter.
type ChipFacts struct {
	Chip           string   `json:"chip"`
	BCD            string   `json:"bcd_device"`
	MaxPacket      int      `json:"max_packet"`
	EndpointIn     uint8    `json:"endpoint_in"`
	EndpointOut    uint8    `json:"endpoint_out"`
	LatencyMs      int      `json:"latency_ms,omitempty"`
	BaseClockHz    uint32   `json:"base_clock_hz"`
	RequestedHz    uint32   `json:"requested_clock_hz"`
	ClockHz        uint32   `json:"clock_hz"`
	ClockFixed     bool     `json:"clock_fixed_by_firmware"`
	DetachedDriver string   `json:"detached_kernel_driver,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

// serialDriver is the one kernel driver that may be detached from the JTAG
// interface.
const serialDriver = "ftdi_sio"

// errInvalid marks a failure caused by the caller's settings.
var errInvalid = errors.New("invalid setting")

func resolveProfile(e Entry, o options) (profiles.Profile, string, error) {
	if o.profile != "" {
		p, ok := profiles.ByName(o.profile)
		if !ok {
			return p, "", fmt.Errorf("%w: unknown profile %q (want one of %s)", errInvalid, o.profile, strings.Join(profiles.Names(), ", "))
		}
		return p, "configured", nil
	}
	d := e.Dev
	return profiles.Match(d.VID, d.PID, d.Manufacturer, d.Product), "auto", nil
}

// openSession claims only the profile's JTAG interface, puts it into MPSSE
// mode and brings the JTAG pins to their idle levels. On any failure the
// interface is released again (re-binding a detached kernel driver).
func openSession(ctx context.Context, e Entry, o options, open Opener) (*session, error) {
	prof, src, err := resolveProfile(e, o)
	if err != nil {
		return nil, err
	}
	if err := prof.Layout.Validate(); err != nil {
		return nil, fmt.Errorf("profile %s: %w", prof.Name, err)
	}
	ifnum := prof.JTAGInterface
	if o.iface >= 0 {
		ifnum = o.iface
	}
	ifc, ok := e.Dev.Iface(ifnum)
	if !ok {
		return nil, fmt.Errorf("%w: device has no USB interface %d", errInvalid, ifnum)
	}
	chip := ftdi.ChipFromBCD(e.Dev.BCD)
	if chip != ftdi.ChipUnknown && !chip.HasMPSSE(ifnum) {
		return nil, fmt.Errorf("%w: %s has no MPSSE engine on channel %d", errInvalid, chip, ifnum)
	}
	// Only the kernel serial driver is ever detached. An interface bound to
	// anything else (including usbfs, i.e. already claimed by another
	// session or process) is never taken over: the claim fails as busy.
	opt := usb.OpenOptions{}
	if o.detach && ifc.Driver == serialDriver {
		opt.DetachDriver = serialDriver
	}
	port, err := open(e, ifnum, opt)
	if err != nil {
		return nil, err
	}
	s := &session{entry: e, o: o, prof: prof, profSrc: src, dev: e.Dev, iface: ifnum}
	fail := func(err error) (*session, error) {
		if s.conn != nil {
			_ = s.conn.Close(context.Background())
		} else {
			_ = port.Close()
		}
		return nil, err
	}
	conn, err := ftdi.New(port, e.Dev, ftdi.Options{Channel: ifnum, Timeout: o.timeout, Quirks: ftdi.Quirks{OptionalControl: prof.Quirks.OptionalControl}})
	if err != nil {
		_ = port.Close()
		return nil, err
	}
	s.conn = conn
	in, out := conn.Endpoints()
	s.info = ChipFacts{
		Chip: conn.Chip().String(), BCD: fmt.Sprintf("0x%04x", e.Dev.BCD), MaxPacket: conn.MaxPacket(),
		EndpointIn: in, EndpointOut: out, DetachedDriver: conn.DetachedDriver(),
	}
	if err := s.initMPSSE(ctx); err != nil {
		return fail(err)
	}
	return s, nil
}

func (s *session) initMPSSE(ctx context.Context) error {
	c, p := s.conn, s.prof
	lat := p.LatencyMs
	if s.o.latencyMs > 0 {
		lat = s.o.latencyMs
	}
	if lat == 0 {
		lat = 16
	}
	steps := []struct {
		what string
		f    func() error
	}{
		{"reset", func() error { return c.Reset(ctx) }},
		{"disable event/error characters", func() error { return c.DisableSpecialChars(ctx) }},
		{"set latency timer", func() error { return c.SetLatency(ctx, lat) }},
		{"leave previous bit mode", func() error { return c.SetBitMode(ctx, 0, ftdi.BitModeReset) }},
		{"enter MPSSE mode", func() error { return c.SetBitMode(ctx, 0, ftdi.BitModeMPSSE) }},
		{"purge RX", func() error { return c.PurgeRX(ctx) }},
		{"purge TX", func() error { return c.PurgeTX(ctx) }},
	}
	for _, st := range steps {
		if err := st.f(); err != nil {
			return fmt.Errorf("%s: %w", st.what, err)
		}
	}
	s.info.LatencyMs = lat
	s.eng = mpsse.NewEngine(c, mpsse.Config{Timeout: s.o.timeout})
	if err := s.eng.Sync(ctx); err != nil {
		return fmt.Errorf("MPSSE did not answer the bad-command probe (is channel %d really in MPSSE mode?): %w", s.iface, err)
	}
	return s.configureClockAndPins(ctx)
}

func (s *session) configureClockAndPins(ctx context.Context) error {
	chipInfo, known := s.conn.Chip().Info()
	if !known {
		chipInfo, _ = ftdi.ChipFT2232C.Info()
		s.info.Notes = append(s.info.Notes, "unrecognised bcdDevice: assuming FT2232C/D timing")
	}
	s.info.BaseClockHz = chipInfo.BaseClockHz
	req := s.o.clockHz
	if req == 0 {
		req = s.prof.NominalClockHz
	}
	s.info.RequestedHz = req
	b := mpsse.NewBuilder()
	b.Loopback(false)
	if chipInfo.HighSpeed {
		b.DivideBy5(false)
		b.Adaptive(false)
		b.ThreePhase(false)
	}
	var actual uint32
	if !s.prof.Quirks.FixedClock {
		div, a, err := mpsse.Divisor(chipInfo.BaseClockHz, req)
		if err != nil {
			return fmt.Errorf("%w: %v", errInvalid, err)
		}
		actual = a
		b.SetDivisor(div)
	}
	l := s.prof.Layout
	b.SetLow(l.InitValue, l.InitDir)
	b.SetHigh(0x00, 0x00)
	if _, err := s.eng.Exec(ctx, b); err != nil {
		return fmt.Errorf("initialise MPSSE pins and clock: %w", err)
	}
	if s.prof.Quirks.FixedClock {
		s.info.ClockFixed = true
		s.info.ClockHz = s.prof.NominalClockHz
		s.info.Notes = append(s.info.Notes, fmt.Sprintf("this device ignores the clock divisor: TCK is fixed by firmware (nominal %d Hz); the %d Hz request is not applied", s.prof.NominalClockHz, req))
	} else {
		s.info.ClockHz = actual
	}
	if s.prof.Quirks.FakeGPIO {
		s.info.Notes = append(s.info.Notes, "GPIO opcodes are no-ops on this device: the pin layout is documented, not driven")
	}
	if err := s.eng.Sync(ctx); err != nil {
		return fmt.Errorf("MPSSE lost sync after initialisation: %w", err)
	}
	s.drv = jtag.NewDriver(jtagport.NewMPSSEPort(s.eng))
	return nil
}

// close releases the interface; a detached kernel driver is re-bound.
func (s *session) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.conn.Close(ctx)
}

// transportErr turns USB failures into a clear message and marks the session
// unusable after a disconnect.
func transportErr(err error) error {
	switch {
	case errors.Is(err, usb.ErrDisconnected):
		return fmt.Errorf("the adapter was unplugged or reset: %w", err)
	case errors.Is(err, usb.ErrTimeout):
		return fmt.Errorf("the adapter stopped answering: %w", err)
	}
	return err
}
