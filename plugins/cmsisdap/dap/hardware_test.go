//go:build hardware && linux

package dap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/usb"
)

// Real CMSIS-DAP v2 hardware tests, opt-in: HWLAB_CMSISDAP_HW=1. The probe is
// chosen by USB serial (HWLAB_CMSISDAP_SERIAL) or, when only one CMSIS-DAP
// probe is attached, found automatically. Tests that need a target skip when
// no SWD target answers.

func hwDevice(t *testing.T) UsbDevice {
	t.Helper()
	if os.Getenv("HWLAB_CMSISDAP_HW") != "1" {
		t.Skip("set HWLAB_CMSISDAP_HW=1 to run against a real CMSIS-DAP probe")
	}
	want := os.Getenv("HWLAB_CMSISDAP_SERIAL")
	var found []UsbDevice
	for _, d := range Scan("/sys") {
		if want == "" || d.Serial == want {
			found = append(found, d)
		}
	}
	switch len(found) {
	case 0:
		t.Skip("no CMSIS-DAP probe attached")
	case 1:
		return found[0]
	}
	t.Fatalf("%d CMSIS-DAP probes found; select one with HWLAB_CMSISDAP_SERIAL", len(found))
	return UsbDevice{}
}

func hwProbe(t *testing.T) *Probe {
	t.Helper()
	d := hwDevice(t)
	tr, err := OpenUSB(d)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p, err := NewProbe(tr)
	if err != nil {
		tr.Close()
		t.Fatalf("NewProbe: %v", err)
	}
	t.Cleanup(func() { _ = p.Disconnect(); _ = p.Close() })
	return p
}

func TestHWDiscoverAndInfo(t *testing.T) {
	d := hwDevice(t)
	t.Logf("usb %s:%s serial=%q manufacturer=%q product=%q if=%d epOut=0x%02x epIn=0x%02x max=%d v%d id=%s",
		d.VID, d.PID, d.Serial, d.Manufacturer, d.Product, d.Interface, d.EPOut, d.EPIn, d.MaxPacket, d.Version, d.ID())
	if d.Version != V2 {
		t.Fatalf("expected a v2 bulk interface, got v%d", d.Version)
	}
	p := hwProbe(t)
	i := p.Info()
	t.Logf("info %+v", i)
	for _, id := range []byte{InfoVendor, InfoProduct, InfoSerial, InfoProtocolVer, InfoTargetVend, InfoTargetName, InfoBoardVend, InfoBoardName, InfoProductFW, InfoCaps, InfoPacketCount, InfoPacketSize, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0xFD} {
		p.mu.Lock()
		raw, ok, err := p.infoRaw(id)
		p.mu.Unlock()
		t.Logf("DAP_Info 0x%02X ok=%v err=%v len=%d % x %q", id, ok, err, len(raw), raw, raw)
	}
}

func hwFacts(t *testing.T, p *Probe) {
	t.Helper()
	for _, hz := range []uint32{100_000, 400_000, 1_000_000, 2_000_000, 4_000_000, 10_000_000} {
		start := time.Now()
		err := p.SetClock(hz)
		t.Logf("DAP_SWJ_Clock %d Hz -> err=%v (%s)", hz, err, time.Since(start))
	}
}

func TestHWClockProbe(t *testing.T) {
	p := hwProbe(t)
	if _, err := p.Connect(PortSWD); err != nil {
		t.Fatal(err)
	}
	hwFacts(t, p)
}

func TestHWSWDNoTargetIsExplicit(t *testing.T) {
	p := hwProbe(t)
	tg, err := Attach(p, AttachOptions{ClockHz: 100_000})
	if err == nil {
		t.Logf("a target answered: DPIDR 0x%08x APIDR 0x%08x", tg.DPIDR, tg.APIDR)
		return
	}
	t.Logf("attach error: %v (no-ack=%v)", err, errors.Is(err, ErrNoACK))
	if strings.Contains(err.Error(), "success") {
		t.Fatal("misleading error")
	}
}

func TestHWCapabilitiesAndPacketLimits(t *testing.T) {
	p := hwProbe(t)
	c, err := p.Caps()
	if err != nil {
		t.Fatal(err)
	}
	i := p.Info()
	t.Logf("caps %+v protocol=%q packet size=%d count=%d", c, i.ProtocolVersion, i.PacketSize, i.PacketCount)
	if !i.SupportsSWD() {
		t.Fatalf("a CMSIS-DAP probe that cannot do SWD: caps 0x%x", i.Capabilities)
	}
	if i.PacketSize < 64 || i.PacketCount < 1 {
		t.Fatalf("implausible limits: size %d count %d", i.PacketSize, i.PacketCount)
	}
	// a request larger than the reported packet size is refused host-side,
	// before anything reaches the probe
	big := make([]byte, i.PacketSize+1)
	big[0] = cmdDelay
	if _, err := p.cmd(big); err == nil || !strings.Contains(err.Error(), "packet size") {
		t.Fatalf("oversized request: %v", err)
	}
	if err := p.Delay(10); err != nil {
		t.Fatalf("the probe must still work after a refused request: %v", err)
	}
}

// The BL702-DAPLink firmware does not answer an undefined command at all (a
// strict CMSIS-DAP probe would send 0xFF). hwlab must report a timeout rather
// than hang or fabricate success, and the next command must still be matched
// with its own response.
func TestHWUndefinedCommandTimesOutAndRecovers(t *testing.T) {
	p := hwProbe(t)
	p.SetTimeout(300 * time.Millisecond)
	start := time.Now()
	_, err := p.cmd([]byte{0x7E})
	t.Logf("undefined command 0x7E after %s: %v", time.Since(start), err)
	if err == nil {
		t.Fatal("an undefined command must not succeed")
	}
	if !errors.Is(err, ErrUnsupported) && !errors.Is(err, usb.ErrTimeout) {
		t.Fatalf("want unsupported or timeout, got %v", err)
	}
	p.SetTimeout(defaultTimeout)
	// A late reply to the undefined command may be seen as one explicit
	// out-of-sync error; it must never be taken for an answer, and the stream
	// must be back in step afterwards.
	for i := 0; i < 6; i++ {
		raw, ok, err := p.InfoRaw(InfoPacketSize)
		t.Logf("after the undefined command, round %d: ok=%v err=%v raw=% x", i, ok, err, raw)
		if err != nil && (i > 0 || !strings.Contains(err.Error(), "out of sync")) {
			t.Fatalf("round %d: %v", i, err)
		}
		if err == nil && (!ok || len(raw) != 2) {
			t.Fatalf("round %d: bad answer % x", i, raw)
		}
	}
	// a probe left with stale replies by a previous owner is usable on a fresh open
	if err := p.Delay(10); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.InfoRaw(InfoCaps); err != nil || !ok {
		t.Fatalf("probe unusable after an unsupported command: ok=%v err=%v", ok, err)
	}
}

func TestHWConnectDisconnectSequencesAndPins(t *testing.T) {
	p := hwProbe(t)
	port, err := p.Connect(PortSWD)
	if err != nil || port != PortSWD {
		t.Fatalf("connect SWD: port %d err %v", port, err)
	}
	if err := p.SetClock(400_000); err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(0, 64, 0); err != nil {
		t.Fatalf("DAP_TransferConfigure: %v", err)
	}
	if err := p.SWDConfigure(0); err != nil {
		t.Fatalf("DAP_SWD_Configure: %v", err)
	}
	if err := p.SWJSequence(51, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x07}); err != nil {
		t.Fatalf("DAP_SWJ_Sequence: %v", err)
	}
	// read-only: select=0 never drives a pin
	lv, err := p.Pins(0, 0, 0)
	t.Logf("DAP_SWJ_Pins levels 0x%02x err=%v", lv, err)
	if err != nil {
		t.Fatalf("DAP_SWJ_Pins: %v", err)
	}
	ex, err := p.ResetTarget()
	t.Logf("DAP_ResetTarget executed=%v err=%v", ex, err)
	if err != nil {
		t.Fatalf("DAP_ResetTarget: %v", err)
	}
	if err := p.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if port, err := p.Connect(PortJTAG); err != nil || port != PortJTAG {
		t.Fatalf("connect JTAG: port %d err %v", port, err)
	}
	if err := p.Disconnect(); err != nil {
		t.Fatal(err)
	}
}

func TestHWNoTargetTransferStatusIsDecoded(t *testing.T) {
	p := hwProbe(t)
	if _, err := p.Connect(PortSWD); err != nil {
		t.Fatal(err)
	}
	if err := p.SetClock(100_000); err != nil {
		t.Fatal(err)
	}
	if err := p.SWJSequence(51, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x07}); err != nil {
		t.Fatal(err)
	}
	if err := p.SWJSequence(16, []byte{0x9E, 0xE7}); err != nil {
		t.Fatal(err)
	}
	if err := p.SWJSequence(51, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x07}); err != nil {
		t.Fatal(err)
	}
	if err := p.SWJSequence(8, []byte{0}); err != nil {
		t.Fatal(err)
	}
	_, err := p.Transfer([]Op{{Reg: 0x00, Read: true}})
	t.Logf("DP IDCODE read with nothing attached: %v", err)
	if err == nil {
		t.Fatal("a DP read succeeded with no target: stale or fabricated success")
	}
	var te *TransferError
	if !errors.As(err, &te) && !errors.Is(err, ErrNoACK) {
		t.Fatalf("the failure is not decoded as a DAP status: %v", err)
	}
}

func TestHWRepeatedPacketsAndRoundTrip(t *testing.T) {
	p := hwProbe(t)
	const n = 2000
	var worst time.Duration
	start := time.Now()
	for i := 0; i < n; i++ {
		t0 := time.Now()
		raw, ok, err := p.InfoRaw(InfoPacketSize)
		if err != nil || !ok || len(raw) != 2 {
			t.Fatalf("round %d: ok=%v err=%v raw=% x", i, ok, err, raw)
		}
		if d := time.Since(t0); d > worst {
			worst = d
		}
	}
	el := time.Since(start)
	t.Logf("DAP_Info round trip: %d in %s = %s avg, worst %s", n, el, el/n, worst)
}

func TestHWOpenCloseRepeatedAndBusy(t *testing.T) {
	d := hwDevice(t)
	for i := 0; i < 25; i++ {
		tr, err := OpenUSB(d)
		if err != nil {
			t.Fatalf("round %d: open: %v", i, err)
		}
		p, err := NewProbe(tr)
		if err != nil {
			tr.Close()
			t.Fatalf("round %d: probe: %v", i, err)
		}
		if _, err := p.Connect(PortSWD); err != nil {
			t.Fatalf("round %d: connect: %v", i, err)
		}
		_ = p.Disconnect()
		if err := p.Close(); err != nil {
			t.Fatalf("round %d: close: %v", i, err)
		}
	}
	first, err := OpenUSB(d)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenUSB(d)
	if err == nil {
		second.Close()
		t.Fatal("a second concurrent open succeeded")
	}
	if !errors.Is(err, usb.ErrBusy) {
		t.Fatalf("want BUSY, got %v", err)
	}
	t.Logf("second open: %v", err)
}

func TestHWInvalidSerialFindsNothing(t *testing.T) {
	hwDevice(t)
	for _, d := range Scan("/sys") {
		if d.Serial == "NO-SUCH-SERIAL" {
			t.Fatalf("matched %+v", d)
		}
	}
}

func TestHWCDCCoexistence(t *testing.T) {
	d := hwDevice(t)
	sys := "/sys/bus/usb/devices/" + d.SysName
	var cdc []string
	ifs, _ := os.ReadDir(sys)
	for _, e := range ifs {
		if !strings.HasPrefix(e.Name(), d.SysName+":") {
			continue
		}
		num := strings.TrimPrefix(e.Name(), d.SysName+":1.")
		if num == fmt.Sprint(d.Interface) {
			continue
		}
		drv, _ := os.Readlink(filepath.Join(sys, e.Name(), "driver"))
		cdc = append(cdc, e.Name()+"="+filepath.Base(drv))
	}
	if len(cdc) == 0 {
		t.Skip("probe has no other interfaces")
	}
	before := append([]string(nil), cdc...)

	tr, err := OpenUSB(d)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProbe(tr)
	if err != nil {
		tr.Close()
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Connect(PortSWD); err != nil {
		t.Fatal(err)
	}

	var after []string
	var tty string
	for _, e := range ifs {
		if !strings.HasPrefix(e.Name(), d.SysName+":") || strings.HasSuffix(e.Name(), "."+fmt.Sprint(d.Interface)) {
			continue
		}
		drv, _ := os.Readlink(filepath.Join(sys, e.Name(), "driver"))
		after = append(after, e.Name()+"="+filepath.Base(drv))
		m, _ := filepath.Glob(filepath.Join(sys, e.Name(), "tty", "ttyACM*"))
		if len(m) > 0 {
			tty = "/dev/" + filepath.Base(m[0])
		}
	}
	t.Logf("other interfaces before=%v after=%v tty=%s", before, after, tty)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("hwlab changed a sibling interface binding: %v -> %v", before, after)
	}
	for _, b := range after {
		if !strings.HasSuffix(b, "=cdc_acm") {
			t.Fatalf("a CDC interface lost its driver: %s", b)
		}
	}
	if tty == "" {
		t.Fatal("no ttyACM device exists while hwlab owns the DAP interface")
	}
	f, err := os.OpenFile(tty, os.O_RDWR|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("tty node exists and is bound but this user cannot open it: %v", err)
		}
		t.Fatalf("serial port cannot be opened while the DAP interface is held: %v", err)
	}
	f.Close()
	if raw, ok, err := p.InfoRaw(InfoPacketSize); err != nil || !ok || len(raw) != 2 {
		t.Fatalf("DAP stopped working after the tty was opened: ok=%v err=%v", ok, err)
	}
}

// TestHWDelayZeroWedgesFirmware documents (and only runs with
// HWLAB_CMSISDAP_WEDGE=1) that DAP_Delay(0) stops the BL702-DAPLink firmware
// from answering; it then needs a physical replug.
func TestHWDelayZeroWedgesFirmware(t *testing.T) {
	if os.Getenv("HWLAB_CMSISDAP_WEDGE") != "1" {
		t.Skip("destructive to the probe's firmware state: set HWLAB_CMSISDAP_WEDGE=1 (needs a replug afterwards)")
	}
	p := hwProbe(t)
	r, err := p.t.Exchange([]byte{cmdDelay, 0, 0}, 500*time.Millisecond)
	t.Logf("raw DAP_Delay(0): % x err=%v", r, err)
	r, err = p.t.Exchange([]byte{cmdInfo, InfoPacketSize}, 500*time.Millisecond)
	t.Logf("DAP_Info right after: % x err=%v", r, err)
}
