package jlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// LogSource is the Log.source of RTT lines in the timeline.
const LogSource = "jlink-rtt"

const (
	rttStream  = "rtt"
	rttBatch   = 128
	maxLineLen = 64 << 10
)

type rttConfig struct {
	host           string
	port           int
	launch         bool
	disabled       bool // rtt: false - the device is only driven by calls, no Commander session is held
	connectTimeout time.Duration
	controlBlock   uint64 // RTT control block address; 0 = let the J-Link software search
	target         Target
}

// rttCapture streams RTT terminal output (up channel 0) into the timeline.
// It reads the telnet server of the J-Link software; a Commander session is
// launched for that unless rtt_launch is false (another J-Link program, e.g.
// a GDB server, already serves RTT).
type rttCapture struct {
	cfg    rttConfig
	serial string
	cmd    Commander
	clock  *pluginsdk.Clock
	sink   *pluginsdk.Sink
	wg     sync.WaitGroup

	lines uint64
	bytes uint64
	err   error
	stop  func()
	// release frees the reserved telnet port; shutdown calls it once.
	release func()
	res     *pluginv1.StopCaptureResponse
}

func (c *rttCapture) begin(ctx context.Context) error {
	if c.cfg.disabled {
		return nil
	}
	if c.cfg.launch {
		stop, err := c.cmd.Start(ctx, c.serial, c.cfg.target, c.cfg.port, c.cfg.controlBlock)
		if err != nil {
			return err
		}
		c.stop = stop
	}
	addr := net.JoinHostPort(c.cfg.host, fmt.Sprint(c.cfg.port))
	deadline := time.Now().Add(c.cfg.connectTimeout)
	var conn net.Conn
	var err error
	for {
		d := net.Dialer{Timeout: time.Second}
		conn, err = d.DialContext(ctx, "tcp", addr)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			c.shutdown()
			return fmt.Errorf("RTT telnet server %s not reachable after %s: %v "+
				"(is the target connected, and is the J-Link software running with RTT enabled?)", addr, c.cfg.connectTimeout, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(150 * time.Millisecond):
		}
	}
	c.wg.Add(1)
	go c.read(ctx, conn)
	return nil
}

func (c *rttCapture) shutdown() {
	if c.stop != nil {
		c.stop()
		c.stop = nil
	}
	if c.release != nil {
		c.release()
		c.release = nil
	}
}

func (c *rttCapture) emit(ctx context.Context, evs []*pluginv1.Event) {
	if len(evs) == 0 {
		return
	}
	if err := c.sink.Emit(ctx, evs...); err != nil && c.err == nil && ctx.Err() == nil {
		c.err = err
	}
}

func (c *rttCapture) read(ctx context.Context, conn net.Conn) {
	defer c.wg.Done()
	defer c.shutdown()
	go func() {
		<-ctx.Done()
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Warn("close RTT connection", "err", err)
		}
	}()

	var (
		pending []byte
		batch   []*pluginv1.Event
		first   = true
		buf     = make([]byte, 16<<10)
	)
	flush := func() {
		bctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c.emit(bctx, batch)
		batch = nil
	}
	defer func() {
		if len(pending) > 0 {
			batch = append(batch, c.line(string(pending), c.clock.Now(), &first))
		}
		flush()
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := conn.Read(buf)
		at := c.clock.Now()
		if n > 0 {
			c.bytes += uint64(n)
			pending = append(pending, buf[:n]...)
			for {
				i := indexNL(pending)
				if i < 0 {
					break
				}
				batch = append(batch, c.line(string(pending[:i]), at, &first))
				pending = pending[i+1:]
			}
			if len(pending) > maxLineLen { // a stream without newlines must not grow unbounded
				batch = append(batch, c.line(string(pending), at, &first))
				pending = nil
			}
		}
		if len(batch) >= rttBatch || (err != nil && len(batch) > 0) || n == 0 {
			flush()
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if ctx.Err() == nil && c.err == nil && !isEOF(err) {
				c.err = fmt.Errorf("RTT connection: %w", err)
			}
			return
		}
	}
}

func indexNL(b []byte) int {
	for i, ch := range b {
		if ch == '\n' {
			return i
		}
	}
	return -1
}

func isEOF(err error) bool { return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) }

// line builds the timeline event for one RTT line. The J-Link software prints
// a short banner (first line "SEGGER J-Link V...") before the target output;
// it is kept, labelled debug, so nothing is hidden.
func (c *rttCapture) line(raw string, at int64, first *bool) *pluginv1.Event {
	level, msg := classifyLine(strings.TrimRight(raw, "\r"))
	c.lines++
	src := LogSource
	if strings.HasPrefix(msg, "SEGGER J-Link V") || (!*first && c.lines <= 4 && (strings.HasPrefix(msg, "Process:") || strings.HasPrefix(msg, "J-Link ") || strings.HasPrefix(msg, "SEGGER J-Link "))) {
		level, src = "debug", "jlink"
	}
	*first = false
	return &pluginv1.Event{TimestampNs: at, StreamId: rttStream,
		Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{Level: level, Source: src, Message: msg,
			Attributes: map[string]string{"channel": "0"}}}}
}

// Wait implements pluginsdk.Capture.
func (c *rttCapture) Wait() error {
	c.wg.Wait()
	c.shutdown()
	c.res = &pluginv1.StopCaptureResponse{
		Stats: &pluginv1.CaptureStats{Events: c.lines},
		Clock: clockInfo(c.lines, c.bytes),
	}
	return c.err
}

// Result implements pluginsdk.Capture.
func (c *rttCapture) Result() *pluginv1.StopCaptureResponse { return c.res }

// clockInfo states the timing quality honestly: a line is stamped when the
// host receives it, which is after the target wrote it into the RTT buffer
// and after the J-Link polled it. Lines are never early; the delay is unknown.
func clockInfo(lines, bytes uint64) *pluginv1.ClockInfo {
	return &pluginv1.ClockInfo{
		Source:          "host clock at RTT telnet receive (target writes are polled by the probe)",
		ResolutionNs:    1_000_000,
		ErrorMinNs:      0,
		ErrorMinBounded: true,
		Note: fmt.Sprintf("the stamp is late by the unknown probe polling + USB + TCP latency and never early; "+
			"lines are not stamped by the target; %d lines, %d bytes", lines, bytes),
	}
}
