// Package scpi is a vendor-independent SCPI client: transports, IEEE 488.2
// framing, a command safety classifier, an oscilloscope semantic layer and the
// per-vendor profiles (Rigol, Siglent) that map it onto command strings.
package scpi

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// ErrTimeout is returned by Transport.Read when nothing arrived in time.
var ErrTimeout = errors.New("scpi: timeout waiting for the instrument")

// Transport moves raw bytes to and from an instrument.
type Transport interface {
	Write(p []byte) error
	// Read waits at most timeout for at least one byte.
	Read(p []byte, timeout time.Duration) (int, error)
	Close() error
	// Describe names the endpoint for messages and device properties.
	Describe() string
}

// DefaultPort is the raw SCPI socket port of LAN instruments.
const DefaultPort = "5025"

// Address kinds.
const (
	KindTCP    = "tcp"
	KindSerial = "serial"
	KindUSBTMC = "usbtmc"
)

// Addr is a parsed instrument address.
type Addr struct {
	Kind   string
	Target string // host:port or a device path
	Baud   int    // serial only
}

// String renders the address in the form ParseAddr accepts.
func (a Addr) String() string {
	switch a.Kind {
	case KindSerial:
		return fmt.Sprintf("serial:%s@%d", a.Target, a.Baud)
	case KindUSBTMC:
		return "usbtmc:" + a.Target
	default:
		return "tcp://" + a.Target
	}
}

// ParseAddr accepts tcp://host[:port], host[:port] (raw socket, port 5025),
// serial:/dev/ttyUSB0[@baud] and usbtmc:/dev/usbtmc0.
func ParseAddr(s string) (Addr, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Addr{}, errors.New("empty instrument address")
	case strings.HasPrefix(s, "serial:"):
		path := strings.TrimPrefix(s, "serial:")
		a := Addr{Kind: KindSerial, Target: path, Baud: 9600}
		if i := strings.LastIndex(path, "@"); i >= 0 {
			var baud int
			if _, err := fmt.Sscanf(path[i+1:], "%d", &baud); err != nil || baud <= 0 {
				return Addr{}, fmt.Errorf("serial address %q: bad baud rate", s)
			}
			a.Target, a.Baud = path[:i], baud
		}
		if a.Target == "" {
			return Addr{}, fmt.Errorf("serial address %q has no port", s)
		}
		return a, nil
	case strings.HasPrefix(s, "usbtmc:"):
		p := strings.TrimPrefix(s, "usbtmc:")
		if p == "" {
			return Addr{}, fmt.Errorf("usbtmc address %q has no device node", s)
		}
		return Addr{Kind: KindUSBTMC, Target: p}, nil
	}
	host := strings.TrimPrefix(s, "tcp://")
	if strings.Contains(host, "/") || host == "" {
		return Addr{}, fmt.Errorf("address %q: want tcp://host[:port], serial:<port>[@baud] or usbtmc:<node>", s)
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(strings.Trim(host, "[]"), DefaultPort)
	}
	return Addr{Kind: KindTCP, Target: host}, nil
}

type tcpTransport struct {
	c    net.Conn
	desc string
}

// DialTCP connects a raw SCPI socket.
func DialTCP(hostport string, timeout time.Duration) (Transport, error) {
	c, err := net.DialTimeout("tcp", hostport, timeout)
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return &tcpTransport{c: c, desc: "tcp://" + hostport}, nil
}

func (t *tcpTransport) Write(p []byte) error {
	_ = t.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := t.c.Write(p)
	return err
}

func (t *tcpTransport) Read(p []byte, timeout time.Duration) (int, error) {
	_ = t.c.SetReadDeadline(time.Now().Add(timeout))
	n, err := t.c.Read(p)
	var ne net.Error
	if n == 0 && errors.As(err, &ne) && ne.Timeout() {
		return 0, ErrTimeout
	}
	if n > 0 {
		return n, nil
	}
	return 0, err
}

func (t *tcpTransport) Close() error     { return t.c.Close() }
func (t *tcpTransport) Describe() string { return t.desc }

type fileTransport struct {
	f    *os.File
	desc string
}

// OpenUSBTMC opens a Linux usbtmc device node. The driver applies its own
// read timeout, so Read ignores the requested one when the node cannot set
// deadlines.
func OpenUSBTMC(path string) (Transport, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &fileTransport{f: f, desc: "usbtmc:" + path}, nil
}

func (t *fileTransport) Write(p []byte) error {
	_, err := t.f.Write(p)
	return err
}

func (t *fileTransport) Read(p []byte, timeout time.Duration) (int, error) {
	_ = t.f.SetReadDeadline(time.Now().Add(timeout))
	n, err := t.f.Read(p)
	if n > 0 {
		return n, nil
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return 0, ErrTimeout
	}
	return 0, err
}

func (t *fileTransport) Close() error     { return t.f.Close() }
func (t *fileTransport) Describe() string { return t.desc }
