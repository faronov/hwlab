package protocol

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/usbserial"
)

// ErrSimClosed is returned by a closed simulated port.
var ErrSimClosed = errors.New("sim: port closed")

// SimFrame is one scripted frame for the simulator.
type SimFrame struct {
	Data []byte // MAC frame without FCS
	RSSI int
	LQI  int
}

// Sim is a simulated sniffer serial port speaking the real firmware's shell
// protocol, including echo, ANSI escapes and (optionally) garbled lines. It
// exists so plugin and capture tests run without hardware.
type Sim struct {
	// Frames are replayed in a loop while the radio is in receive mode.
	Frames []SimFrame
	// Interval between frames; defaults to 5 ms.
	Interval time.Duration
	// BootUs is the device timer value at construction; the timer then follows
	// real time scaled by (1 + DriftPPM*1e-6).
	BootUs   int64
	DriftPPM float64
	// Garble inserts a corrupted line before every Nth frame (0 = never).
	Garble int
	// Transit delays each frame by a pseudo-random 0..Jitter before delivery.
	Jitter time.Duration
	// FailAfter makes reads fail after that many frames were delivered (device unplug).
	FailAfter int

	mu      sync.Mutex
	out     chan []byte
	closed  chan struct{}
	timeout time.Duration
	rx      bool
	channel int
	cmds    []string
	t0      time.Time
	sent    int
	partial []byte
	once    sync.Once
	wg      sync.WaitGroup
	started bool
}

// NewSim returns a simulator with the given scripted frames.
func NewSim(frames ...SimFrame) *Sim {
	return &Sim{
		Frames: frames, Interval: 5 * time.Millisecond, BootUs: 100_000_000,
		out: make(chan []byte, 1024), closed: make(chan struct{}),
		timeout: 50 * time.Millisecond, t0: time.Now(),
	}
}

// Opener returns an Opener that always hands out this simulator.
func (s *Sim) Opener() usbserial.Opener {
	return func(string) (usbserial.Port, error) { return s, nil }
}

// Commands returns the commands received so far (without line endings).
func (s *Sim) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

// Channel returns the last channel selected by the host (0 if none).
func (s *Sim) Channel() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channel
}

// Receiving reports whether the radio is in receive mode.
func (s *Sim) Receiving() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rx
}

// SetReadTimeout implements usbserial.Port.
func (s *Sim) SetReadTimeout(d time.Duration) error {
	s.mu.Lock()
	s.timeout = d
	s.mu.Unlock()
	return nil
}

func (s *Sim) emit(b []byte) {
	select {
	case s.out <- b:
	case <-s.closed:
	}
}

// Write implements io.Writer: it parses CRLF-terminated shell commands.
func (s *Sim) Write(p []byte) (int, error) {
	select {
	case <-s.closed:
		return 0, ErrSimClosed
	default:
	}
	s.mu.Lock()
	s.partial = append(s.partial, p...)
	var lines []string
	for {
		i := strings.IndexByte(string(s.partial), '\n')
		if i < 0 {
			break
		}
		lines = append(lines, strings.TrimSpace(string(s.partial[:i])))
		s.partial = s.partial[i+1:]
	}
	s.mu.Unlock()
	for _, l := range lines {
		s.handle(l)
	}
	return len(p), nil
}

func (s *Sim) handle(cmd string) {
	s.mu.Lock()
	s.cmds = append(s.cmds, cmd)
	s.mu.Unlock()
	switch {
	case cmd == "":
		s.emit([]byte("\x1b[1;32muart:~$ \x1b[m"))
	case cmd == "sleep":
		s.setRx(false)
		s.emit([]byte("sleep\r\n\x1b[1;32muart:~$ \x1b[m"))
	case cmd == "shell echo off":
		s.emit([]byte("shell echo off\r\n"))
	case strings.HasPrefix(cmd, "channel "):
		var ch int
		if _, err := fmt.Sscanf(cmd, "channel %d", &ch); err != nil || ch < MinChannel || ch > MaxChannel {
			s.emit([]byte("channel: invalid channel\r\n"))
			return
		}
		s.mu.Lock()
		s.channel = ch
		s.mu.Unlock()
		s.emit([]byte("\x1b[1;32muart:~$ \x1b[m"))
	case cmd == "receive":
		s.setRx(true)
	default:
		s.emit([]byte(cmd + ": command not found\r\n"))
	}
}

func (s *Sim) setRx(on bool) {
	s.mu.Lock()
	was := s.rx
	s.rx = on
	start := on && !was && !s.started && len(s.Frames) > 0
	if start {
		s.started = true
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if start {
		go s.stream()
	}
}

func (s *Sim) devUs() int64 {
	el := float64(time.Since(s.t0).Microseconds())
	return s.BootUs + int64(el*(1+s.DriftPPM*1e-6))
}

func hex2(b []byte) string { return fmt.Sprintf("%x", b) }

func (s *Sim) stream() {
	defer s.wg.Done()
	i := 0
	tick := time.NewTicker(s.Interval)
	defer tick.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-tick.C:
		}
		s.mu.Lock()
		on := s.rx
		s.mu.Unlock()
		if !on {
			continue
		}
		f := s.Frames[i%len(s.Frames)]
		i++
		if s.Garble > 0 && i%s.Garble == 0 {
			s.emit([]byte("received: 0200 power: ds6 lqi: \x1b[J\r\nchchannsleep: command not found\r\n"))
		}
		raw := append(append([]byte(nil), f.Data...), byte(f.LQI/4), 0)
		line := fmt.Sprintf("received: %s power: %d lqi: %d time: %d\r\n", hex2(raw), f.RSSI, f.LQI, s.devUs())
		if s.Jitter > 0 {
			time.Sleep(time.Duration(i*7919) % s.Jitter)
		}
		s.emit([]byte(line))
		s.mu.Lock()
		s.sent++
		s.mu.Unlock()
	}
}

// Read implements io.Reader with the configured read timeout.
func (s *Sim) Read(p []byte) (int, error) {
	s.mu.Lock()
	to := s.timeout
	fail := s.FailAfter > 0 && s.sent >= s.FailAfter
	s.mu.Unlock()
	if fail {
		return 0, io.ErrUnexpectedEOF
	}
	select {
	case b := <-s.out:
		return copy(p, b), nil // lines are small; a chunk never exceeds len(p) in tests
	case <-s.closed:
		return 0, ErrSimClosed
	case <-time.After(to):
		return 0, nil
	}
}

// Close implements io.Closer and waits for the streaming goroutine.
func (s *Sim) Close() error {
	s.once.Do(func() { close(s.closed) })
	s.wg.Wait()
	return nil
}
