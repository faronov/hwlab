package espusbjtag

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// DeviceGone prefixes the error of every call that failed because the USB
// device vanished (unplugged, or reset and not yet back).
const DeviceGone = "DEVICE_GONE"

func goneErr(format string, a ...any) error {
	return status.Errorf(codes.Unavailable, DeviceGone+": "+format, a...)
}

// IsDeviceGone classifies an error returned by the plugin.
func IsDeviceGone(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.Unavailable && len(st.Message()) >= len(DeviceGone) && st.Message()[:len(DeviceGone)] == DeviceGone
}

// identity is what a session learned about the attached target. A device that
// reappears under the same USB serial number must show the same identity
// before a session is continued on it.
type identity struct {
	USBSerial string
	IDCode    uint32
	ROMMagic  uint32
	Harts     int
}

func (a identity) mismatch(b identity) string {
	switch {
	case a.USBSerial != b.USBSerial:
		return fmt.Sprintf("USB serial %q != %q", a.USBSerial, b.USBSerial)
	case a.IDCode != b.IDCode:
		return fmt.Sprintf("JTAG IDCODE 0x%08x != 0x%08x", a.IDCode, b.IDCode)
	case a.ROMMagic != b.ROMMagic:
		return fmt.Sprintf("ROM chip-detect word 0x%08x != 0x%08x", a.ROMMagic, b.ROMMagic)
	}
	return ""
}

// captureIdentity reads the identity of the target behind the session.
func (s *session) captureIdentity(ctx context.Context) (identity, error) {
	dm, err := s.ensureDM(ctx)
	if err != nil {
		return identity{}, err
	}
	id := identity{USBSerial: s.entry.Dev.Serial}
	if c := s.chain; c != nil && len(c.Devices) == 1 && c.Devices[0].IDCode != nil {
		id.IDCode = c.Devices[0].IDCode.Raw
	}
	if ev, err := esp.ReadEvidence(ctx, dm); err == nil {
		id.ROMMagic = ev.ROMMagic
	}
	if n, err := dm.HartCount(ctx, 8); err == nil {
		id.Harts = n
	}
	return id, nil
}

// known remembers how a device was opened, so that a session lost to an
// unplug or a reset can be rebuilt on the same device.
type known struct {
	opts options
	id   *identity
}

func (p *Plugin) present(id string) bool {
	for _, e := range p.list() {
		if e.ID == id {
			return true
		}
	}
	return false
}

// pollUntil calls f every step until it returns true or the deadline passes.
func pollUntil(ctx context.Context, d, step time.Duration, f func() bool) (time.Duration, bool) {
	t0 := time.Now()
	for {
		if f() {
			return time.Since(t0), true
		}
		if time.Since(t0) >= d {
			return time.Since(t0), false
		}
		select {
		case <-ctx.Done():
			return time.Since(t0), false
		case <-time.After(step):
		}
	}
}

// reopen waits (bounded) for the device with this USB serial to be enumerated,
// opens the vendor interface again, rebuilds the TAP/DTM/DM state and checks
// the target identity against want. The USB bus/device number is never used.
func (p *Plugin) reopen(ctx context.Context, id string, o options, want *identity) (*session, time.Duration, error) {
	t0 := time.Now()
	var lastErr error
	var s *session
	_, ok := pollUntil(ctx, o.reconnect, 50*time.Millisecond, func() bool {
		e, err := p.find(id)
		if err != nil {
			lastErr = err
			return false
		}
		ns, err := openSession(ctx, e, o, p.open)
		if err != nil {
			lastErr = err
			return false
		}
		// Enumerated devices can answer before the debug unit is ready: the
		// whole rebuild is retried inside the deadline.
		got, err := ns.captureIdentity(ctx)
		if err != nil {
			lastErr = err
			_ = ns.close()
			return false
		}
		if want != nil {
			if why := want.mismatch(got); why != "" {
				lastErr = fmt.Errorf("identity changed: %s", why)
				_ = ns.close()
				return false
			}
		}
		ns.identity = &got
		s = ns
		return true
	})
	if !ok {
		if lastErr == nil {
			lastErr = errors.New("not enumerated")
		}
		return nil, time.Since(t0), goneErr("device %s did not come back within %s: %v", id, o.reconnect, transportErr(lastErr))
	}
	return s, time.Since(t0), nil
}

// soc performs a SoC reset through the RTC watchdog and brings the session
// back up on the re-enumerated device. The caller holds no locks.
func (p *Plugin) soc(ctx context.Context, id string, s *session, persistent bool, args map[string]any) (map[string]any, error) {
	halt, err := boolArg(args, "halt", false)
	if err != nil {
		return nil, err
	}
	if tok, _ := strArg(args, "confirm"); tok != "reset-soc" {
		return nil, denied(`a SoC reset restarts the whole chip and re-enumerates USB: pass confirm="reset-soc"`)
	}
	s.mu.Lock()
	if !s.o.perm.reset {
		s.mu.Unlock()
		return nil, denied("reset needs allow_reset=true in the device configuration")
	}
	dm, err := s.dmFor(ctx, args)
	if err == nil && s.identity == nil {
		var got identity
		if got, err = s.captureIdentity(ctx); err == nil {
			s.identity = &got
		}
	}
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	old := *s.identity
	st, _ := dm.Status(ctx)
	armErr := s.withHaltedLocked(ctx, dm, func() error { return esp.ArmSoCReset(ctx, dm) })
	wasHalted := st.State == riscv.HartHalted
	if armErr == nil && wasHalted {
		_, armErr = dm.Resume(ctx) // the watchdog only matters while the CPU runs the clock domain
	}
	opts := s.o
	s.mu.Unlock()
	// The chip may reset while we are still resuming it: that is the success path.
	if armErr != nil && !errors.Is(armErr, usb.ErrDisconnected) {
		return nil, armErr
	}

	// 1. The old handle must really die; do not report success before that.
	// A hart that was halted (typically parked in ROM after a reset-halt) goes
	// on to run boot code that disables the watchdog again, so it is re-armed
	// through the system bus (no halt needed) whenever it is found disarmed.
	wait := 5 * time.Second
	if wasHalted {
		wait = 12 * time.Second
	}
	gone, ok := pollUntil(ctx, wait, 10*time.Millisecond, func() bool {
		if !p.present(id) {
			return true
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		_, err := s.dtm.Read(ctx, 0x11)
		if errors.Is(err, usb.ErrDisconnected) {
			return true
		}
		if wasHalted && err == nil {
			if armed, e := esp.SoCResetArmed(ctx, dm); e == nil && !armed {
				_ = esp.ArmSoCReset(ctx, dm)
			}
		}
		return false
	})
	p.dropAny(id, s, persistent)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "the SoC reset was armed but the device never disappeared within %s: reset NOT observed", wait)
	}
	// 2. Rediscover by serial, reopen interface, rebuild chain/DTM/DM, check identity.
	ns, back, err := p.reopen(ctx, id, opts, &old)
	if err != nil {
		return nil, err
	}
	res := map[string]any{
		"kind": "soc", "usb_dropped": true, "gone_after_ms": float64(gone.Milliseconds()), "back_after_ms": float64((gone + back).Milliseconds()),
		"reenumerated": true, "identity_verified": true, "usb_serial": old.USBSerial,
		"identity_checked": "USB serial, JTAG IDCODE and ROM chip-detect word equal before and after",
		"note":             "the SoC reset restarts the firmware; the USB bus/device number may differ and is never used as identity",
	}
	ns.mu.Lock()
	dm2, err := ns.dmFor(ctx, args)
	if err == nil && halt {
		var hs riscv.Status
		if hs, err = dm2.Halt(ctx); err == nil {
			for k, v := range ns.stateResult(ctx, dm2, hs) {
				res[k] = v
			}
		}
	} else if err == nil {
		if hs, e := dm2.Status(ctx); e == nil {
			for k, v := range ns.stateResult(ctx, dm2, hs) {
				res[k] = v
			}
		}
	}
	ns.mu.Unlock()
	if err != nil {
		_ = ns.close()
		return nil, err
	}
	if persistent {
		p.mu.Lock()
		p.devs[id] = ns
		p.mu.Unlock()
	} else {
		_ = ns.close()
	}
	return ns.mark(res, true), nil
}

func (p *Plugin) dropAny(id string, s *session, persistent bool) {
	if persistent {
		p.drop(id, s)
	}
}

// reconnect closes the session (if any) and rebuilds it on the device that
// carries the same USB serial number, proving it is the same target.
func (p *Plugin) reconnect(ctx context.Context, id string, args map[string]any) (map[string]any, error) {
	over := map[string]any{}
	for _, k := range deviceKeys {
		if v, ok := args[k]; ok {
			over[k] = v
		}
	}
	p.mu.Lock()
	s := p.devs[id]
	k := p.last[id]
	p.mu.Unlock()
	if s != nil || k != nil {
		delete(over, "hart")
	}
	var o options
	var want *identity
	persistent := s != nil || k != nil
	switch {
	case s != nil:
		o = s.o
		s.mu.Lock()
		if s.identity == nil {
			if got, err := s.captureIdentity(ctx); err == nil {
				s.identity = &got
			}
		}
		want = s.identity
		s.mu.Unlock()
	case k != nil:
		o, want = k.opts, k.id
	default:
		var err error
		if o, err = parseOptions(defaultOptions(), over); err != nil {
			return nil, argErr(err)
		}
	}
	if (s != nil || k != nil) && len(over) > 0 {
		return nil, argErr(errors.New("device settings cannot change on an open device"))
	}
	if s != nil {
		p.drop(id, s)
	}
	rctx, cancel := context.WithTimeout(ctx, o.reconnect+30*time.Second)
	defer cancel()
	ns, took, err := p.reopen(rctx, id, o, want)
	if err != nil {
		return nil, err
	}
	res := map[string]any{
		"reconnected": true, "took_ms": float64(took.Milliseconds()), "identity_verified": want != nil,
		"idcode": fmt.Sprintf("0x%08x", ns.identity.IDCode), "rom_chip_word": fmt.Sprintf("0x%08x", ns.identity.ROMMagic), "harts": float64(ns.identity.Harts),
		"usb_address": fmt.Sprintf("%d:%d", ns.entry.Dev.Bus, ns.entry.Dev.Address),
		"note":        "the device is found by USB serial number; the USB bus/device number is informational only and may differ from before",
	}
	if want == nil {
		res["note"] = "no earlier identity was known for this device, so nothing could be compared; this only proves the device answers"
	}
	if persistent {
		ns.persist = true
		p.mu.Lock()
		p.devs[id] = ns
		p.last[id] = &known{opts: o, id: ns.identity}
		p.mu.Unlock()
	} else {
		_ = ns.close()
	}
	return ns.mark(res, true), nil
}
