package protocol

import (
	"context"
	"fmt"
	"strings"
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

func (d *Device) write(cmd string) error {
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
