package stlinkplugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DeviceGone prefixes the error of every call that failed because the probe
// vanished (unplugged, or its USB port reset and not yet back).
const DeviceGone = "DEVICE_GONE"

func goneErr(format string, a ...any) error {
	return status.Errorf(codes.Unavailable, DeviceGone+": "+format, a...)
}

// IsDeviceGone classifies an error returned by the plugin.
func IsDeviceGone(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.Unavailable && len(st.Message()) >= len(DeviceGone) && st.Message()[:len(DeviceGone)] == DeviceGone
}

// identity is what a session learned about the probe and the target behind
// it. A probe that reappears under the same USB serial must show the same
// identity before a session is continued on it.
type identity struct {
	USBSerial string
	DPIDCode  uint32
	CPUID     uint32
	// Vendor is board/vendor knowledge read from vendor registers; it is a
	// hint, never derived from the core. VendorWord is the raw register.
	Vendor     string
	VendorWord uint32
}

func (a identity) mismatch(b identity) string {
	switch {
	case a.USBSerial != b.USBSerial:
		return fmt.Sprintf("probe serial %q != %q", a.USBSerial, b.USBSerial)
	case a.DPIDCode != b.DPIDCode:
		return fmt.Sprintf("DP IDCODE 0x%08x != 0x%08x", a.DPIDCode, b.DPIDCode)
	case a.CPUID != b.CPUID:
		return fmt.Sprintf("CPUID 0x%08x != 0x%08x", a.CPUID, b.CPUID)
	case a.Vendor != b.Vendor || a.VendorWord != b.VendorWord:
		return fmt.Sprintf("vendor identity %q (0x%08x) != %q (0x%08x)", a.Vendor, a.VendorWord, b.Vendor, b.VendorWord)
	}
	return ""
}

// captureIdentity reads the identity of the target behind the session. The
// reads are all side-effect free.
func (s *session) captureIdentity(ctx context.Context) (identity, error) {
	id := identity{USBSerial: s.entry.Serial, DPIDCode: s.info.DPIDCode}
	ci, err := s.core.ReadIdentity(ctx)
	if err != nil {
		return id, err
	}
	id.CPUID = ci.CPUID
	id.Vendor, id.VendorWord, s.chip = s.vendorHint(ctx)
	return id, nil
}

// known remembers how a probe was opened, so that a session lost to an
// unplug can be rebuilt on the same probe.
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

// reopen waits (bounded) for the probe with this USB serial to be enumerated,
// opens it again, re-enters debug mode and checks the identity against want.
// The USB bus/device number is never used.
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
		return nil, time.Since(t0), goneErr("probe %s did not come back within %s: %v", id, o.reconnect, transportErr(lastErr))
	}
	return s, time.Since(t0), nil
}

// reconnect closes the session (if any) and rebuilds it on the probe that
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
	if (s != nil || k != nil) && len(over) > 0 {
		return nil, argErr(errors.New("device settings cannot change on an open device"))
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
		"usb_address": fmt.Sprintf("%d:%d", ns.entry.Dev.Bus, ns.entry.Dev.Address),
		"note":        "the probe is found by USB serial number; the USB bus/device number is informational only and may differ from before",
	}
	if want == nil {
		res["note"] = "no earlier identity was known for this probe, so nothing could be compared; this only proves the probe answers"
	}
	if persistent {
		ns.persist = true
		p.mu.Lock()
		p.devs[id] = ns
		p.last[id] = &known{opts: o, id: ns.identity}
		p.mu.Unlock()
	} else {
		defer func() { _ = ns.close() }()
	}
	return ns.mark(res), nil
}
