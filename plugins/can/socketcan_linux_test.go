//go:build linux

package canplugin

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/faronov/hwlab/internal/can"
)

// socketPair returns a bus on one end of a datagram socketpair, standing in
// for a CAN raw socket, and the raw peer fd.
func socketPair(t *testing.T, o Options) (*scBus, int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Skipf("socketpair: %v", err)
	}
	_ = unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1)
	b, err := newSocketBus(fds[0], "can-test", o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(); unix.Close(fds[1]) })
	return b, fds[1]
}

func TestSocketBusReceivesClassicAndFD(t *testing.T) {
	b, peer := socketPair(t, Options{FD: true})
	classic := can.Frame{ID: 0x123, Data: []byte{1, 2, 3}}
	fd := can.Frame{ID: 0x1ABCDE00, Extended: true, FD: true, BRS: true, Data: make([]byte, 24)}
	rtr := can.Frame{ID: 0x7, RTR: true, DLC: 5}
	for _, f := range []can.Frame{classic, fd, rtr} {
		if _, err := unix.Write(peer, f.KernelBytes()); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []can.Frame{classic, fd, rtr} {
		rx, err := b.Recv(time.Second)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		got := rx.Frame
		if got.ID != want.ID || got.Extended != want.Extended || got.FD != want.FD || got.RTR != want.RTR || got.Len() != want.Len() || got.Interface != "can-test" || got.Own {
			t.Errorf("frame %d: got %+v want %+v", i, got, want)
		}
		if rx.At.IsZero() {
			t.Errorf("frame %d: no timestamp", i)
		}
		if !rx.Kernel {
			t.Logf("frame %d: no kernel timestamp on a unix socketpair (host-stamped)", i)
		}
	}
}

func TestSocketBusTimeoutSendCloseUnblocks(t *testing.T) {
	b, peer := socketPair(t, Options{})
	start := time.Now()
	if _, err := b.Recv(60 * time.Millisecond); !errors.Is(err, ErrTimeout) || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("timeout: %v after %v", err, time.Since(start))
	}
	if err := b.Send(can.Frame{ID: 0x55, Data: []byte{0xAA}}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	n, err := unix.Read(peer, buf)
	if err != nil || n != can.KernelClassicSize {
		t.Fatalf("peer read %d err=%v", n, err)
	}
	f, err := can.FromKernel(buf[:n])
	if err != nil || f.ID != 0x55 || f.Data[0] != 0xAA {
		t.Errorf("sent frame: %+v %v", f, err)
	}
	if err := b.Send(can.Frame{ID: 1, FD: true, Data: []byte{1}}); !errors.Is(err, errFDDisabled) {
		t.Errorf("FD send without fd: %v", err)
	}
	if err := b.Send(can.Frame{ID: 0x800}); err == nil {
		t.Error("invalid frame was written")
	}

	done := make(chan error, 1)
	go func() { _, err := b.Recv(10 * time.Second); done <- err }()
	time.Sleep(50 * time.Millisecond)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("blocked Recv after Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not interrupt a blocked Recv")
	}
	if err := b.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	if err := b.Send(can.Frame{ID: 1}); !errors.Is(err, ErrClosed) {
		t.Errorf("send after close: %v", err)
	}
}

func TestSocketBusRejectsGarbage(t *testing.T) {
	b, peer := socketPair(t, Options{})
	if _, err := unix.Write(peer, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Recv(time.Second); err == nil {
		t.Error("a 3-byte datagram was accepted as a CAN frame")
	}
}

func writeIface(t *testing.T, root, name string, typ int, mtu int, flags string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, v := range map[string]string{"type": strconv.Itoa(typ), "mtu": strconv.Itoa(mtu), "flags": flags} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListSocketCAN(t *testing.T) {
	root := t.TempDir()
	writeIface(t, root, "eth0", 1, 1500, "0x1003")
	writeIface(t, root, "can0", 280, 16, "0x1003")
	writeIface(t, root, "can1", 280, 72, "0x1002")
	writeIface(t, root, "vcan0", 280, 72, "0x1243")
	infos, err := listSocketCAN(root)
	if err != nil || len(infos) != 3 {
		t.Fatalf("infos %+v err=%v", infos, err)
	}
	byName := map[string]IfaceInfo{}
	for _, i := range infos {
		byName[i.Name] = i
	}
	if c := byName["can0"]; c.FD || !c.Up || c.Virtual || c.Backend != "socketcan" {
		t.Errorf("can0: %+v", c)
	}
	if c := byName["can1"]; !c.FD || c.Up {
		t.Errorf("can1: %+v", c)
	}
	if c := byName["vcan0"]; !c.Virtual || !c.FD {
		t.Errorf("vcan0: %+v", c)
	}
	if infos[0].Name != "can0" {
		t.Error("not sorted")
	}
	if got, err := listSocketCAN(filepath.Join(root, "missing")); err != nil || got != nil {
		t.Errorf("missing sysfs: %v %v", got, err)
	}
}

func TestOpenSocketCANErrors(t *testing.T) {
	if _, err := OpenSocketCAN("hwlab-no-such-if", Options{}); err == nil {
		t.Error("opened a missing interface")
	}
	if _, err := OpenSocketCAN("lo", Options{}); err == nil {
		t.Error("lo is not a CAN interface")
	}
}
