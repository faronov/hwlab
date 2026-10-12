//go:build linux

package uart

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/faronov/hwlab/internal/testutil"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// openPTY returns the master side of a pseudo terminal and the path of its
// slave. A pty is a real kernel tty, so it exercises the real port code
// (termios, flow control) without hardware. Skipped where /dev/ptmx is absent.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty support: %v", err)
	}
	t.Cleanup(func() { testutil.CheckError(t, m.Close()) })
	rc, err := m.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var path string
	var ioerr error
	if err := rc.Control(func(fd uintptr) {
		if ioerr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ioerr != nil {
			return
		}
		var n int
		if n, ioerr = unix.IoctlGetInt(int(fd), unix.TIOCGPTN); ioerr == nil {
			path = fmt.Sprintf("/dev/pts/%d", n)
		}
	}); err != nil || ioerr != nil {
		t.Skipf("pty setup: %v %v", err, ioerr)
	}
	return m, path
}

func portTermios(t *testing.T, p Port) *unix.Termios {
	t.Helper()
	fd, err := fdOf(p.(*realPort).Port)
	if err != nil {
		t.Fatal(err)
	}
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio
}

func TestRealPortFramingAndFlowControlOnPTY(t *testing.T) {
	_, slave := openPTY(t)
	m := DefaultMode()
	m.DataBits, m.Parity, m.StopBits, m.Flow = 7, "even", "2", FlowRTSCTS
	p, err := OpenPort(slave, m)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { testutil.CheckError(t, p.Close()) }()

	tio := portTermios(t, p)
	if tio.Cflag&unix.CRTSCTS == 0 {
		t.Error("RTS/CTS must be enabled")
	}
	// A pty forces 8N (the kernel clears CSIZE/PARENB for it), so only the
	// stop-bit flag proves that the framing request reached the tty.
	if tio.Cflag&unix.CSTOPB == 0 {
		t.Errorf("framing flags wrong: cflag=%#o", tio.Cflag)
	}

	m.Flow, m.Parity, m.StopBits, m.DataBits = FlowXonXoff, "none", "1", 8
	if err := p.SetMode(m); err != nil {
		t.Fatal(err)
	}
	tio = portTermios(t, p)
	if tio.Cflag&unix.CRTSCTS != 0 || tio.Iflag&unix.IXON == 0 || tio.Iflag&unix.IXOFF == 0 {
		t.Errorf("xon/xoff: cflag=%#o iflag=%#o", tio.Cflag, tio.Iflag)
	}
	if tio.Cflag&unix.CSTOPB != 0 {
		t.Errorf("reconfigured framing flags wrong: cflag=%#o", tio.Cflag)
	}

	m.Flow = FlowNone
	if err := p.SetMode(m); err != nil {
		t.Fatal(err)
	}
	tio = portTermios(t, p)
	if tio.Cflag&unix.CRTSCTS != 0 || tio.Iflag&(unix.IXON|unix.IXOFF) != 0 {
		t.Errorf("flow control must clear: cflag=%#o iflag=%#o", tio.Cflag, tio.Iflag)
	}
}

// TestPluginOverPTY runs the plugin with the real opener against a pty: the
// master side plays the attached device.
func TestPluginOverPTY(t *testing.T) {
	master, slave := openPTY(t)
	p := New()
	ctx := context.Background()
	if err := p.OpenDevice(ctx, "pty", map[string]any{"path": slave, "baud": 115200.0}); err != nil {
		t.Fatal(err)
	}
	defer func() { testutil.CheckError(t, p.CloseDevice(ctx, "pty")) }()

	cctx, cancel := context.WithCancel(ctx)
	sink := pluginsdk.NewTestSink(64)
	c, _, err := p.StartCapture(cctx, pluginsdk.CaptureRequest{DeviceID: "pty", Config: map[string]any{"lines": true, "idle_gap_ms": 20.0}, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := master.WriteString("boot ok\r\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, _, err := invoke(t, p, "pty", "write", map[string]any{"data": "ping"}, nil); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	_ = master.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := master.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("device side received %q err %v", buf[:n], err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	var rx, tx, lines []string
	for _, e := range sink.Drain() {
		switch pl := e.Payload.(type) {
		case *pluginv1.Event_Packet:
			if e.StreamId == StreamTX {
				tx = append(tx, string(pl.Packet.Data))
			} else {
				rx = append(rx, string(pl.Packet.Data))
			}
		case *pluginv1.Event_Log:
			lines = append(lines, pl.Log.Message)
		}
	}
	if strings.Join(rx, "") != "boot ok\r\n" || strings.Join(tx, "") != "ping" || strings.Join(lines, "|") != "boot ok" {
		t.Errorf("rx %q tx %q lines %q", rx, tx, lines)
	}
}
