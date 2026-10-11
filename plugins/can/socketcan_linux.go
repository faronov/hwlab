//go:build linux

package canplugin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/faronov/hwlab/internal/can"
)

// SocketCANSupported reports that this build can talk to Linux SocketCAN.
const SocketCANSupported = true

// Raw CAN socket options (linux/can/raw.h); x/sys does not export them all.
const (
	canRawFilter     = 1
	canRawErrFilter  = 2
	canRawRecvOwn    = 4
	canRawFDFrames   = 5
	canErrMaskAll    = 0x1FFFFFFF
	arphrdCAN        = 280
	ifFlagUp         = 0x1
	oobSpace         = 128
	maxSocketRecvLen = can.KernelFDSize
)

// scBus is a SocketCAN raw socket (or, in tests, any datagram socket that
// carries kernel-format frames).
type scBus struct {
	name string
	opt  Options

	fd   int
	wake int // eventfd that interrupts a blocked Recv on Close

	mu     sync.RWMutex // held for reading by Recv, for writing by Close
	closed bool
}

// OpenSocketCAN opens a SocketCAN raw socket bound to the interface. It never
// changes the interface: bit rate and up/down state are left to `ip link`.
func OpenSocketCAN(name string, o Options) (Bus, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("CAN interface %q: %w", name, err)
	}
	if ifi.Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("CAN interface %q is down: bring it up with `ip link set %s up type can bitrate ...` first", name, name)
	}
	fd, err := unix.Socket(unix.AF_CAN, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.CAN_RAW)
	if err != nil {
		if errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EPROTONOSUPPORT) {
			return nil, errors.New("this kernel has no SocketCAN raw support (load can and can_raw)")
		}
		return nil, fmt.Errorf("socket(AF_CAN): %w", err)
	}
	if err := configureSocket(fd, o); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrCAN{Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("bind %s: %w", name, err)
	}
	b, err := newSocketBus(fd, name, o)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	return b, nil
}

func configureSocket(fd int, o Options) error {
	set := func(opt, v int, what string) error {
		if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, opt, v); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	if err := set(canRawFDFrames, b2i(o.FD), "enable CAN FD frames (is the interface FD capable?)"); err != nil {
		return err
	}
	if err := set(canRawRecvOwn, b2i(o.Own), "receive own frames"); err != nil {
		return err
	}
	errMask := 0
	if o.ErrorFrames {
		errMask = canErrMaskAll
	}
	if err := set(canRawErrFilter, errMask, "error frame filter"); err != nil {
		return err
	}
	if len(o.Filters) > 0 {
		raw := make([]byte, 0, 8*len(o.Filters))
		for _, f := range o.Filters {
			id, mask := f.KernelPair()
			raw = binary.NativeEndian.AppendUint32(raw, id)
			raw = binary.NativeEndian.AppendUint32(raw, mask)
		}
		if err := unix.SetsockoptString(fd, unix.SOL_CAN_RAW, canRawFilter, string(raw)); err != nil {
			return fmt.Errorf("receive filters: %w", err)
		}
	}
	// Best effort: without it Recv falls back to the time the read returned.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1)
	return nil
}

// newSocketBus wraps an already configured socket fd and takes ownership of it.
func newSocketBus(fd int, name string, o Options) (*scBus, error) {
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("eventfd: %w", err)
	}
	return &scBus{name: name, opt: o, fd: fd, wake: wake}, nil
}

func (b *scBus) Recv(timeout time.Duration) (Rx, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return Rx{}, ErrClosed
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, maxSocketRecvLen+8)
	oob := make([]byte, oobSpace)
	for {
		left := time.Until(deadline)
		if left < 0 {
			left = 0
		}
		ms := int((left + time.Millisecond - 1) / time.Millisecond)
		fds := []unix.PollFd{{Fd: int32(b.fd), Events: unix.POLLIN}, {Fd: int32(b.wake), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, ms)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return Rx{}, fmt.Errorf("poll: %w", err)
		}
		if fds[1].Revents != 0 {
			return Rx{}, ErrClosed
		}
		if n == 0 || fds[0].Revents&unix.POLLIN == 0 {
			if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return Rx{}, b.sockErr()
			}
			if !time.Now().Before(deadline) {
				return Rx{}, ErrTimeout
			}
			continue
		}
		got, oobn, flags, _, err := unix.Recvmsg(b.fd, buf, oob, unix.MSG_DONTWAIT)
		now := time.Now()
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.ENETDOWN) || errors.Is(err, unix.ENODEV) {
				return Rx{}, fmt.Errorf("CAN interface %s went away or down: %w", b.name, err)
			}
			return Rx{}, fmt.Errorf("recvmsg: %w", err)
		}
		f, err := can.FromKernel(buf[:got])
		if err != nil {
			return Rx{}, err
		}
		f.Interface = b.name
		f.Own = flags&unix.MSG_CONFIRM != 0
		rx := Rx{Frame: f, At: now}
		if ts, ok := kernelTimestamp(oob[:oobn]); ok {
			// Convert the wall-clock receive time to the monotonic clock via
			// the age of the frame, so later clock steps cannot skew it.
			if age := now.Sub(ts); age >= 0 && age < time.Second {
				rx.At, rx.Kernel = now.Add(-age), true
			}
		}
		return rx, nil
	}
}

func (b *scBus) sockErr() error {
	if e, err := unix.GetsockoptInt(b.fd, unix.SOL_SOCKET, unix.SO_ERROR); err == nil && e != 0 {
		return fmt.Errorf("CAN socket error: %w", unix.Errno(e))
	}
	return errors.New("CAN socket closed by the kernel")
}

// kernelTimestamp extracts SCM_TIMESTAMPNS from the control messages.
func kernelTimestamp(oob []byte) (time.Time, bool) {
	if len(oob) == 0 {
		return time.Time{}, false
	}
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return time.Time{}, false
	}
	for _, m := range msgs {
		if m.Header.Level != unix.SOL_SOCKET || m.Header.Type != unix.SCM_TIMESTAMPNS {
			continue
		}
		switch len(m.Data) {
		case 16:
			return time.Unix(int64(binary.NativeEndian.Uint64(m.Data)), int64(binary.NativeEndian.Uint64(m.Data[8:]))), true
		case 8:
			return time.Unix(int64(int32(binary.NativeEndian.Uint32(m.Data))), int64(int32(binary.NativeEndian.Uint32(m.Data[4:])))), true
		}
	}
	return time.Time{}, false
}

func (b *scBus) Send(f can.Frame) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if f.FD && !b.opt.FD {
		return errFDDisabled
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return ErrClosed
	}
	raw := f.KernelBytes()
	for {
		n, err := unix.Write(b.fd, raw)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			switch {
			case errors.Is(err, unix.ENOBUFS), errors.Is(err, unix.EAGAIN):
				return fmt.Errorf("transmit queue of %s is full (no other node, bus-off or no ACK?): %w", b.name, err)
			case errors.Is(err, unix.ENETDOWN):
				return fmt.Errorf("CAN interface %s is down: %w", b.name, err)
			}
			return fmt.Errorf("write %s: %w", b.name, err)
		}
		if n != len(raw) {
			return fmt.Errorf("short CAN write: %d of %d bytes", n, len(raw))
		}
		return nil
	}
}

func (b *scBus) Close() error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return nil
	}
	// Interrupt a blocked Recv, then wait for readers to leave before closing.
	_, _ = unix.Write(b.wake, []byte{1, 0, 0, 0, 0, 0, 0, 0})
	b.mu.RUnlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	err := unix.Close(b.fd)
	_ = unix.Close(b.wake)
	return err
}

// ListSocketCAN enumerates the CAN network interfaces of the host.
func ListSocketCAN() ([]IfaceInfo, error) { return listSocketCAN("/sys/class/net") }

func listSocketCAN(root string) ([]IfaceInfo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []IfaceInfo
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		typ, err := readInt(filepath.Join(dir, "type"), 10)
		if err != nil || typ != arphrdCAN {
			continue
		}
		info := IfaceInfo{Name: e.Name(), Backend: "socketcan"}
		if v, err := readInt(filepath.Join(dir, "mtu"), 10); err == nil {
			info.MTU = int(v)
			info.FD = info.MTU >= can.KernelFDSize
		}
		if v, err := readInt(filepath.Join(dir, "flags"), 0); err == nil {
			info.Up = v&ifFlagUp != 0
		}
		info.Virtual = strings.HasPrefix(e.Name(), "vcan") || strings.HasPrefix(e.Name(), "vxcan")
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func readInt(path string, base int) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(raw)), base, 64)
}
