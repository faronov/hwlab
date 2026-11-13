package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/usbserial"
)

const (
	readTimeout  = 50 * time.Millisecond
	quietPeriod  = 150 * time.Millisecond
	drainMax     = 1500 * time.Millisecond
	flushTimeout = 2 * time.Second
)

// Device is an open sniffer serial port.
type Device struct {
	port usbserial.Port
	lb   LineBuffer

	// Quiet is how long the device must be silent before the next command is
	// sent; defaults to 150 ms.
	Quiet time.Duration

	Lines     uint64 // non-empty lines read while receiving
	Frames    uint64
	Malformed uint64

	// Firmware is the result of the version probe done by Start or Probe.
	Firmware Firmware
	// OnTx, when set before Run, receives transmit output lines (see
	// radiolab.go) read while streaming, with their host receive time.
	OnTx func(TxEvent, time.Time)

	wmu sync.Mutex
}

// Open opens the serial port. baud is irrelevant for USB CDC but must be set.
func Open(open usbserial.Opener, name string) (*Device, error) {
	p, err := open(name)
	if err != nil {
		return nil, err
	}
	if err := p.SetReadTimeout(readTimeout); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("set read timeout: %w", err)
	}
	if d, ok := p.(interface{ SetDTR(bool) error }); ok {
		// The firmware's CDC ACM UART only transmits once the host asserts DTR.
		_ = d.SetDTR(true)
	}
	return &Device{port: p, Quiet: quietPeriod}, nil
}

// Start puts the radio to sleep, selects channel and enters receive mode. The
// commands are sent one at a time with a quiet period in between, because the
// shell echoes and frames interleave with typed input while streaming.
func (d *Device) Start(ctx context.Context, channel int) error {
	if channel < MinChannel || channel > MaxChannel {
		return fmt.Errorf("channel %d out of range [%d,%d]", channel, MinChannel, MaxChannel)
	}
	d.lb = LineBuffer{}
	// A previous run may have left the radio receiving; wake the shell, then stop it.
	if _, err := d.command(ctx, ""); err != nil {
		return err
	}
	if _, err := d.command(ctx, "sleep"); err != nil {
		return err
	}
	if _, err := d.command(ctx, "shell echo off"); err != nil {
		return err
	}
	if _, err := d.Probe(ctx); err != nil {
		return err
	}
	resp, err := d.command(ctx, fmt.Sprintf("channel %d", channel))
	if err != nil {
		return err
	}
	if low := strings.ToLower(resp); strings.Contains(low, "not found") || strings.Contains(low, "error") || strings.Contains(low, "invalid") {
		return fmt.Errorf("device rejected channel %d: %s", channel, strings.TrimSpace(resp))
	}
	// No drain: frames start streaming immediately and belong to the capture.
	return d.write("receive")
}

// Retune switches an already started device to another channel, discarding
// frames from the previous one. It is cheaper than Start (used by scans).
func (d *Device) Retune(ctx context.Context, channel int) error {
	if channel < MinChannel || channel > MaxChannel {
		return fmt.Errorf("channel %d out of range [%d,%d]", channel, MinChannel, MaxChannel)
	}
	d.lb = LineBuffer{}
	if _, err := d.command(ctx, "sleep"); err != nil {
		return err
	}
	if _, err := d.command(ctx, fmt.Sprintf("channel %d", channel)); err != nil {
		return err
	}
	return d.write("receive")
}

// Probe asks the firmware for its version and capabilities ("version");
// Nordic's sniffer answers "command not found" and is receive-only. Call
// with the radio asleep and echo off (Start does).
func (d *Device) Probe(ctx context.Context) (Firmware, error) {
	resp, err := d.command(ctx, "version")
	if err != nil {
		return Firmware{}, err
	}
	d.Firmware = ParseVersion(resp)
	return d.Firmware, nil
}

// Identify wakes the shell, puts the radio to sleep, turns echo off and
// probes the firmware, leaving the device idle.
func (d *Device) Identify(ctx context.Context) error {
	d.lb = LineBuffer{}
	for _, cmd := range []string{"", "sleep", "shell echo off"} {
		if _, err := d.command(ctx, cmd); err != nil {
			return err
		}
	}
	_, err := d.Probe(ctx)
	return err
}

// Setup prepares a device for commands without receiving: Identify, then
// select channel.
func (d *Device) Setup(ctx context.Context, channel int) error {
	if channel < MinChannel || channel > MaxChannel {
		return fmt.Errorf("channel %d out of range [%d,%d]", channel, MinChannel, MaxChannel)
	}
	if err := d.Identify(ctx); err != nil {
		return err
	}
	return d.checked(ctx, fmt.Sprintf("channel %d", channel))
}

// SetPower sets the transmit power (radio lab firmware only).
func (d *Device) SetPower(ctx context.Context, dbm int) error {
	return d.checked(ctx, fmt.Sprintf("power %d", dbm))
}

func (d *Device) checked(ctx context.Context, cmd string) error {
	resp, err := d.command(ctx, cmd)
	if err != nil {
		return err
	}
	if low := strings.ToLower(resp); strings.Contains(low, "not found") || strings.Contains(low, "error") || strings.Contains(low, "invalid") {
		return fmt.Errorf("device rejected %q: %s", cmd, strings.TrimSpace(resp))
	}
	return nil
}

// ErrNoTxResult is returned when the firmware did not finish a transmit
// within the deadline.
var ErrNoTxResult = errors.New("no tx_done from the firmware before the deadline")

// Transmit sends one transmit command on a device that is not streaming
// (after Setup) and returns every transmit line up to and including the
// final "tx_done:" or "error:". It gives up after timeout.
func (d *Device) Transmit(ctx context.Context, req TxRequest, timeout time.Duration) ([]TxEvent, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if err := d.write(req.Command()); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var evs []TxEvent
	done := false
	buf := make([]byte, 4096)
	for !done {
		if ctx.Err() != nil {
			return evs, ErrNoTxResult
		}
		n, err := d.port.Read(buf)
		if n > 0 {
			d.lb.Feed(buf[:n], func(line string) {
				if done {
					return
				}
				if ev, ok := ParseTxLine(line); ok {
					evs = append(evs, ev)
					done = ev.Kind == TxDone || ev.Kind == TxError
				}
			})
		}
		if err != nil {
			return evs, fmt.Errorf("read: %w", err)
		}
	}
	return evs, nil
}

// Send writes one command while Run is streaming; its output arrives
// through OnTx (transmit lines) or is ignored.
func (d *Device) Send(cmd string) error { return d.write(cmd) }

func (d *Device) write(cmd string) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.port.Write([]byte(cmd + "\r\n"))
	if err != nil {
		return fmt.Errorf("write %q: %w", cmd, err)
	}
	return nil
}

// command sends one command and returns whatever the device printed until it
// had been quiet for quietPeriod (bounded by drainMax).
func (d *Device) command(ctx context.Context, cmd string) (string, error) {
	if err := d.write(cmd); err != nil {
		return "", err
	}
	var out strings.Builder
	buf := make([]byte, 1024)
	start := time.Now()
	last := start
	for {
		if err := ctx.Err(); err != nil {
			return out.String(), err
		}
		n, err := d.port.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			last = time.Now()
		}
		if err != nil {
			return out.String(), fmt.Errorf("read: %w", err)
		}
		if now := time.Now(); now.Sub(last) >= d.Quiet || now.Sub(start) >= drainMax {
			return out.String(), nil
		}
	}
}

// Run reads frames until ctx is cancelled or the port fails. onFrame is called
// with the time the bytes containing the frame were read; onTick is called at
// least every read timeout (50 ms) so callers can flush batches. A cancelled
// context is a normal end and returns nil.
func (d *Device) Run(ctx context.Context, onFrame func(Frame, time.Time), onTick func(time.Time)) error {
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := d.port.Read(buf)
		rx := time.Now()
		if n > 0 {
			d.lb.Feed(buf[:n], func(line string) {
				d.Lines++
				f, kind := ParseLine(line)
				switch kind {
				case FrameLine:
					d.Frames++
					onFrame(f, rx)
				case Malformed:
					d.Malformed++
				case Noise:
					if d.OnTx != nil {
						if ev, ok := ParseTxLine(line); ok {
							d.OnTx(ev, rx)
						}
					}
				}
			})
		}
		if onTick != nil {
			onTick(rx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("sniffer read: %w", err)
		}
	}
}

// Close stops the radio (best effort) and releases the port. Closing right
// after writing "sleep" loses the command on real hardware, so it waits until
// the stream has gone quiet, retrying once. It never blocks longer than
// flushTimeout.
func (d *Device) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for attempt := 0; attempt < 2; attempt++ {
			start := time.Now()
			if _, err := d.command(ctx, "sleep"); err != nil {
				return
			}
			if time.Since(start) < drainMax {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(flushTimeout + 500*time.Millisecond):
	}
	return d.port.Close()
}
