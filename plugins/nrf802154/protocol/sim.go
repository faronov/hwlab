package protocol

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
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
	// RadioLab makes the simulator answer like the zigbee-rs radio lab
	// firmware (version, power, transmit) instead of Nordic's sniffer.
	RadioLab bool
	// AckStatus is the status of "transmit ... ack" attempts (default no_ack).
	AckStatus string

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
	power   int
	txs     []string
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

// Transmits returns the transmit commands accepted so far.
func (s *Sim) Transmits() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.txs...)
}

// Power returns the transmit power last set by the host.
func (s *Sim) Power() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.power
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
	case s.RadioLab && cmd == "version":
		s.emit([]byte("version: " + RadioLabName + " 0.1.0 protocol: 1 caps: receive,transmit,cca,ack-wait,repeat,power,stats\r\n"))
	case s.RadioLab && cmd == "power":
		s.mu.Lock()
		pw := s.power
		s.mu.Unlock()
		s.emit(fmt.Appendf(nil, "power: %d\r\n", pw))
	case s.RadioLab && strings.HasPrefix(cmd, "power "):
		var pw int
		if _, err := fmt.Sscanf(cmd, "power %d", &pw); err != nil || !slices.Contains(TxPowerLevels, pw) {
			s.emit([]byte("error: unsupported power level\r\n"))
			return
		}
		s.mu.Lock()
		s.power = pw
		s.mu.Unlock()
	case s.RadioLab && strings.HasPrefix(cmd, "transmit "):
		s.transmit(cmd)
	default:
		s.emit([]byte(strings.Fields(cmd)[0] + ": command not found\r\n"))
	}
}

// transmit emulates the radio lab's transmit command, including its output.
func (s *Sim) transmit(cmd string) {
	f := strings.Fields(cmd)
	req := TxRequest{Repeat: 1}
	psdu, err := hex.DecodeString(f[1])
	if err != nil {
		s.emit([]byte("error: frame must be hex\r\n"))
		return
	}
	req.PSDU = psdu
	for i := 2; i < len(f); i++ {
		switch f[i] {
		case "cca":
			req.CCA = true
		case "ack":
			req.AckWait = true
		case "repeat":
			if i+2 >= len(f) {
				s.emit([]byte("error: transmit: wrong parameter count\r\n"))
				return
			}
			req.Repeat, _ = strconv.Atoi(f[i+1])
			req.IntervalMs, _ = strconv.Atoi(f[i+2])
			i += 2
		default:
			s.emit([]byte("error: unknown transmit option\r\n"))
			return
		}
	}
	if err := req.Validate(); err != nil {
		s.emit([]byte("error: " + err.Error() + "\r\n"))
		return
	}
	s.mu.Lock()
	s.txs = append(s.txs, cmd)
	ch, pw := s.channel, s.power
	status := "sent"
	if req.AckWait {
		status = "no_ack"
		if s.AckStatus != "" {
			status = s.AckStatus
		}
	}
	s.mu.Unlock()
	onAir := append(append([]byte(nil), psdu...), FCS(psdu)...)
	sum := TxSummary{Result: "ok"}
	for a := 1; a <= req.Repeat; a++ {
		if a > 1 {
			time.Sleep(time.Duration(req.IntervalMs) * time.Millisecond)
		}
		us := s.devUs()
		sum.Attempts++
		switch status {
		case "acked":
			sum.Sent++
			sum.Acked++
		case "no_ack":
			sum.Sent++
			sum.NoAck++
		case "sent":
			sum.Sent++
		}
		s.emit(fmt.Appendf(nil, "transmitted: %s channel: %d power: %d time: %d\r\n", hex2(onAir), ch, pw, us))
		s.emit(fmt.Appendf(nil, "tx: %d/%d status: %s time: %d\r\n", a, req.Repeat, status, us))
	}
	s.emit(fmt.Appendf(nil, "tx_done: attempts: %d sent: %d acked: %d no_ack: %d cca_busy: 0 error: 0 result: ok\r\n",
		sum.Attempts, sum.Sent, sum.Acked, sum.NoAck))
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
