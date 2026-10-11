package ppk2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/faronov/hwlab/internal/usbserial"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/ppk2/protocol"
)

// batchSamples is the number of samples per emitted event (10 ms of data).
const batchSamples = 1000

var digitalNames = []string{"D0", "D1", "D2", "D3", "D4", "D5", "D6", "D7"}

// options are the per-capture settings.
type options struct {
	mode     protocol.Mode
	supplyMV int  // source mode regulator
	vddMV    int  // supply voltage used by the calibration formula
	dutPower bool // switch the DUT supply on when the capture starts
	// keepPower leaves the DUT supply on after the capture. By default a
	// capture that switched the supply on switches it off again.
	keepPower bool
}

func parseOptions(cfg map[string]any) (options, error) {
	o := options{mode: protocol.ModeAmpere, supplyMV: 3300, vddMV: 3300}
	if s, ok := cfg["mode"].(string); ok && s != "" {
		m, err := protocol.ParseMode(s)
		if err != nil {
			return o, err
		}
		o.mode = m
	}
	if v, ok := cfg["supply_mv"]; ok {
		mv, ok := toInt(v)
		if !ok || mv < protocol.VddMinMV || mv > protocol.VddMaxMV {
			return o, fmt.Errorf("supply_mv must be an integer in [%d,%d]", protocol.VddMinMV, protocol.VddMaxMV)
		}
		o.supplyMV = mv
	}
	o.vddMV = o.supplyMV
	if v, ok := cfg["vdd_mv"]; ok {
		mv, ok := toInt(v)
		if !ok || mv <= 0 {
			return o, errors.New("vdd_mv must be a positive integer")
		}
		o.vddMV = mv
	}
	o.dutPower = o.mode == protocol.ModeSource
	if v, ok := cfg["dut_power"].(bool); ok {
		o.dutPower = v
	}
	if v, ok := cfg["keep_dut_power"].(bool); ok {
		o.keepPower = v
	}
	return o, nil
}

type device struct {
	name     string
	port     usbserial.Port
	metadata *protocol.Metadata
	cfg      map[string]any

	wmu       sync.Mutex // serialises command writes
	capturing atomic.Bool
	broken    atomic.Bool // a read/write failed; the next use reconnects
}

func newDevice(port usbserial.Port, name string, cfg map[string]any) (*device, error) {
	if err := port.SetReadTimeout(100 * time.Millisecond); err != nil {
		return nil, err
	}
	d := &device{name: name, port: port, cfg: cfg}
	// A previous session may have left the device streaming.
	if err := d.write(protocol.CmdAverageStop); err != nil {
		return nil, err
	}
	drain(port, 200*time.Millisecond)
	if err := d.write(protocol.CmdGetMetaData); err != nil {
		return nil, err
	}
	md, err := readMetadata(port, metadataTimeout)
	if err != nil {
		return nil, err
	}
	d.metadata = md
	return d, nil
}

// drain discards pending input until the line is quiet.
func drain(p usbserial.Port, quiet time.Duration) {
	buf := make([]byte, 4096)
	deadline := time.Now().Add(quiet)
	for time.Now().Before(deadline) {
		if n, err := p.Read(buf); err != nil {
			return
		} else if n > 0 {
			deadline = time.Now().Add(quiet)
		}
	}
}

func readMetadata(p usbserial.Port, timeout time.Duration) (*protocol.Metadata, error) {
	var sb strings.Builder
	buf := make([]byte, 2048)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := p.Read(buf)
		if err != nil {
			return nil, err
		}
		sb.Write(buf[:n])
		if md, err := protocol.ParseMetadata(sb.String()); err == nil {
			return md, nil
		} else if !errors.Is(err, protocol.ErrIncomplete) {
			return nil, err
		}
	}
	return nil, errors.New("no metadata reply (not the PPK2 command port?)")
}

func (d *device) write(b ...byte) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := d.port.Write(b)
	if err != nil {
		d.broken.Store(true)
	}
	return err
}

func (d *device) setVoltage(mv int) error {
	return d.writeCmd(protocol.CmdSetRegulator(mv))
}

func (d *device) setDUTPower(on bool) error {
	return d.writeCmd(protocol.CmdDUTPower(on))
}

func (d *device) writeCmd(b []byte) error { return d.write(b...) }

func (d *device) close() error {
	if d.capturing.Load() {
		return errors.New("capture still running")
	}
	return d.port.Close()
}

func (d *device) startCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	merged := map[string]any{}
	for k, v := range d.cfg {
		merged[k] = v
	}
	for k, v := range req.Config {
		merged[k] = v
	}
	opts, err := parseOptions(merged)
	if err != nil {
		return nil, nil, err
	}
	if !d.capturing.CompareAndSwap(false, true) {
		return nil, nil, errors.New("capture already running on this device")
	}
	fail := func(err error) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
		d.capturing.Store(false)
		return nil, nil, err
	}
	if err := d.writeCmd(protocol.CmdPowerMode(opts.mode)); err != nil {
		return fail(err)
	}
	if opts.mode == protocol.ModeSource {
		if err := d.setVoltage(opts.supplyMV); err != nil {
			return fail(err)
		}
	}
	if opts.dutPower {
		if err := d.setDUTPower(true); err != nil {
			return fail(err)
		}
	}
	drain(d.port, 20*time.Millisecond)

	c := &capture{
		d: d, sink: sink, clock: req.Clock, duration: req.Duration,
		switchOff: opts.dutPower && !opts.keepPower,
		dec:       &protocol.Decoder{Cal: d.metadata.Calibration, VddMV: float64(opts.vddMV)},
	}
	writeNs := req.Clock.Now()
	if err := d.write(protocol.CmdAverageStart); err != nil {
		return fail(err)
	}
	// Sample 0 is assumed to be taken when AVERAGE_START was issued; the
	// device provides no per-sample timestamps (see ADR 0008).
	c.t0 = req.Clock.Now()
	c.bound = newStartBound(writeNs, c.t0)
	c.wg.Add(1)
	go c.run(ctx)
	attrs := map[string]string{
		"sample_period_ns": fmt.Sprint(protocol.SamplePeriod), "mode": string(opts.mode),
		"vdd_mv": fmt.Sprint(opts.vddMV), "port": d.name,
	}
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: "current", Kind: "power", Attributes: attrs}}}, nil
}

type capture struct {
	d        *device
	sink     *pluginsdk.Sink
	clock    *pluginsdk.Clock
	duration time.Duration
	dec      *protocol.Decoder
	t0       int64
	bound    *startBound
	// switchOff turns the DUT supply off when the capture ends.
	switchOff bool

	wg      sync.WaitGroup
	err     error
	emitted int64 // samples emitted so far
	events  uint64
	limit   int64 // 0 = unbounded
}

func (c *capture) Wait() error { c.wg.Wait(); return c.err }

func (c *capture) Result() *pluginv1.StopCaptureResponse {
	return &pluginv1.StopCaptureResponse{Stats: &pluginv1.CaptureStats{
		Events: c.events, Samples: uint64(c.emitted), DroppedBatches: c.sink.Dropped()},
		Clock: c.bound.clockInfo()}
}

func (c *capture) run(ctx context.Context) {
	defer c.wg.Done()
	defer c.d.capturing.Store(false)
	if c.duration > 0 {
		c.limit = int64((c.duration + protocol.SamplePeriod - 1) / protocol.SamplePeriod)
	}
	buf := make([]byte, 64*1024)
	var cur []int32
	var logic []uint32
	for ctx.Err() == nil && (c.limit == 0 || c.emitted+int64(len(cur)) < c.limit) {
		n, err := c.d.port.Read(buf)
		if err != nil {
			c.err = fmt.Errorf("ppk2 read: %w", err)
			c.d.broken.Store(true)
			break
		}
		arrive := c.clock.Now()
		cur, logic = c.dec.Decode(buf[:n], cur, logic)
		c.bound.observe(arrive, c.emitted+int64(len(cur)))
		for len(cur) >= batchSamples {
			c.emit(ctx, cur[:batchSamples], logic[:batchSamples], false)
			cur, logic = cur[batchSamples:], logic[batchSamples:]
		}
	}
	if err := c.d.write(protocol.CmdAverageStop); err != nil && c.err == nil {
		c.err = fmt.Errorf("ppk2 stop: %w", err)
	}
	if c.switchOff {
		// Best effort: the port may be gone. Power-off stays explicit in the
		// sense that only a capture that switched the supply on does this.
		_ = c.d.setDUTPower(false)
	}
	// Collect what is already in flight, then flush with a fresh context so
	// that a cancelled capture does not lose its tail.
	fctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if n, err := c.d.port.Read(buf); err == nil {
		cur, logic = c.dec.Decode(buf[:n], cur, logic)
	}
	if len(cur) > 0 {
		c.emit(fctx, cur, logic, true)
	}
	drain(c.d.port, 50*time.Millisecond)
}

// emit queues one batch. While streaming it never blocks (the device cannot
// be paused, and stalling the serial reader would lose data silently);
// overflow is counted as dropped batches instead. The final flush blocks.
func (c *capture) emit(ctx context.Context, cur []int32, logic []uint32, block bool) bool {
	if c.limit > 0 {
		if remain := c.limit - c.emitted; int64(len(cur)) > remain {
			cur, logic = cur[:remain], logic[:remain]
		}
	}
	if len(cur) == 0 {
		return true
	}
	ev := &pluginv1.Event{
		TimestampNs: c.t0 + c.emitted*protocol.SamplePeriod, StreamId: "current",
		Payload: &pluginv1.Event_Power{Power: &pluginv1.PowerBatch{
			SamplePeriodNs: protocol.SamplePeriod,
			CurrentNa:      append([]int32(nil), cur...),
			Digital:        append([]uint32(nil), logic...),
			DigitalNames:   digitalNames,
		}},
	}
	c.emitted += int64(len(cur)) // advances even for dropped batches so timestamps stay correct
	if block {
		if err := c.sink.Emit(ctx, ev); err != nil {
			return false
		}
	} else if !c.sink.TryEmit(ev) {
		return true
	}
	c.events++
	return true
}
