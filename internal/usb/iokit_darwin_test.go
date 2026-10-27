//go:build darwin

package usb

import (
	"errors"
	"strings"
	"testing"
)

func TestMapIOReturn(t *testing.T) {
	cases := []struct {
		kr   uint32
		want error
	}{
		{kIOUSBTransactionTimeout, ErrTimeout},
		{kIOReturnTimeout, ErrTimeout},
		{kIOUSBPipeStalled, ErrStall},
		{kIOReturnNoDevice, ErrDisconnected},
		{kIOReturnOffline, ErrDisconnected},
		{kIOReturnExclusiveAccess, ErrBusy},
		{kIOReturnBusy, ErrBusy},
		{kIOReturnNotPrivileged, ErrPermission},
		{kIOReturnNotPermitted, ErrPermission},
		{kIOReturnNotOpen, ErrClosed},
	}
	for _, c := range cases {
		if err := mapIOReturn("op", c.kr); !errors.Is(err, c.want) {
			t.Errorf("0x%08x: got %v, want %v", c.kr, err, c.want)
		}
	}
	if mapIOReturn("op", kIOReturnSuccess) != nil {
		t.Error("success must map to nil")
	}
	// Aborted and overrun stay generic but keep the raw code.
	err := mapIOReturn("bulk IN 0x85", kIOReturnOverrun)
	var kr ioReturnError
	if !errors.As(err, &kr) || uint32(kr) != kIOReturnOverrun || !strings.Contains(err.Error(), "kIOReturnOverrun (0xe00002e8)") {
		t.Errorf("overrun: %v", err)
	}
	for _, s := range []error{ErrTimeout, ErrStall, ErrDisconnected, ErrBusy, ErrPermission, ErrClosed} {
		if errors.Is(err, s) {
			t.Errorf("overrun must not map to %v", s)
		}
	}
	if !mayBeUnplug(mapIOReturn("x", kIOReturnAborted)) || !mayBeUnplug(mapIOReturn("x", kIOReturnNotResponding)) {
		t.Error("aborted/not responding can be an unplug")
	}
	if mayBeUnplug(mapIOReturn("x", kIOUSBPipeStalled)) || mayBeUnplug(errors.New("x")) {
		t.Error("a stall is not an unplug")
	}
}

func TestBusyErr(t *testing.T) {
	err := busyErr(1, "AppleUSBACMData", OpenOptions{})
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "interface 1 is held by AppleUSBACMData") || strings.Contains(err.Error(), "detach") {
		t.Errorf("%v", err)
	}
	err = busyErr(0, "", OpenOptions{DetachDriver: "ftdi_sio"})
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "cannot detach") {
		t.Errorf("%v", err)
	}
}

func TestLocationName(t *testing.T) {
	for loc, want := range map[uint32]string{
		0x14200000: "20-2",
		0x14210000: "20-2.1",
		0x14213400: "20-2.1.3.4",
		0x80400000: "128-4",
		0x14000000: "20",
	} {
		if got := locationName(loc); got != want {
			t.Errorf("0x%08x: %q, want %q", loc, got, want)
		}
	}
}

func TestSpeedMbps(t *testing.T) {
	for in, want := range map[int64]int{0: 1, 1: 12, 2: 480, 3: 5000, 4: 10000, 9: 0} {
		if got := speedMbps(in); got != want {
			t.Errorf("%d: %d, want %d", in, got, want)
		}
	}
}

func TestUUIDRegs(t *testing.T) {
	u, err := uuidBytes("8752663B-C07B-4BAE-9584-22032FAB9C5A")
	if err != nil {
		t.Fatal(err)
	}
	// CFUUIDBytes by value: byte 0 is the low byte of the first register.
	lo, hi := uuidRegs(u)
	if lo != 0xAE4B7BC03B665287 || hi != 0x5A9CAB2F03228495 {
		t.Errorf("lo %#x hi %#x", lo, hi)
	}
	for _, bad := range []string{"", "8752663B-C07B-4BAE-9584", "Z752663B-C07B-4BAE-9584-22032FAB9C5A"} {
		if _, err := uuidBytes(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestUsesIOKit(t *testing.T) {
	if !UsesIOKit("/sys") || !UsesIOKit("/sys/") || UsesIOKit(t.TempDir()) {
		t.Error("only the default root is served from the IORegistry")
	}
}

func TestIOThreadBlocksSIGURG(t *testing.T) {
	if err := loadIOKit(); err != nil {
		t.Skip(err)
	}
	th := newIOThread()
	defer th.stop()
	for i := 0; i < 3; i++ {
		var mask uint32
		if r := th.do(func() uint32 { return uint32(pthreadSigmask(sigBlock, nil, &mask)) }); r != 0 {
			t.Fatalf("pthread_sigmask: %d", r)
		}
		if mask&(1<<(sigURG-1)) == 0 {
			t.Fatalf("SIGURG is not blocked on the I/O thread: mask %#x", mask)
		}
	}
}
