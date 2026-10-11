package energytrace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/usb"
)

// Data stream location on the LP-XDS110ET (CONFIRMED on hardware): the second
// vendor interface, which has no kernel driver bound.
const (
	DataInterface = 6
	DataOut       = 0x05
	DataIn        = 0x87

	readLen     = 4 * 1024
	readTimeout = 500 * time.Millisecond
	// StallTimeout is how long the stream may stay silent before Run fails.
	StallTimeout = 5 * time.Second
)

// DataPort is the part of usb.Port the stream reader needs.
type DataPort interface {
	BulkIn(ctx context.Context, ep uint8, length int, timeout time.Duration) ([]byte, error)
}

// Batch is one data packet converted to nanoamps.
type Batch struct {
	Seq uint8
	// StartSample is the index of the first sample, counting samples of lost
	// packets, so StartSample*period is the time since the start of the
	// accepted data.
	StartSample uint64
	// Lost is the number of packets missing before this one.
	Lost      int
	CurrentNA []int32
	Arrival   time.Time
}

// Reader reads the stream. The first data packet is dropped like the vendor
// tool does (it is not in the oracle CSV); its content was not shown to be
// invalid, only unused.
type Reader struct {
	port    DataPort
	parser  Parser
	offset  float64
	skip    int
	sample  uint64
	dropped int
}

// NewReader makes a reader that converts with the range-0 offset.
func NewReader(p DataPort, cal Calibration) (*Reader, error) {
	off, err := cal.Offset()
	if err != nil {
		return nil, err
	}
	return &Reader{port: p, offset: off, skip: 1}, nil
}

// Stats returns the parser counters; Dropped packets are not in Samples of
// the emitted batches.
func (r *Reader) Stats() (Stats, int) { return r.parser.Stats(), r.dropped }

// ErrStalled reports that the probe stopped sending data.
var ErrStalled = errors.New("energytrace: no data from the probe")

// Run reads until ctx ends (returns nil) or the port fails (returns the
// error). idle bounds how long silence is tolerated; 0 means StallTimeout.
// emit is called for every packet in order; the Batch is only valid during
// the call.
func (r *Reader) Run(ctx context.Context, idle time.Duration, emit func(Batch)) error {
	if idle <= 0 {
		idle = StallTimeout
	}
	last := time.Now()
	for {
		if ctx.Err() != nil {
			return nil
		}
		b, err := r.port.BulkIn(ctx, DataIn, readLen, readTimeout)
		now := time.Now()
		switch {
		case err == nil:
		case errors.Is(err, usb.ErrTimeout):
			if now.Sub(last) > idle {
				return fmt.Errorf("%w for %s", ErrStalled, idle)
			}
			continue
		case ctx.Err() != nil:
			return nil
		default:
			return err
		}
		if len(b) > 0 {
			last = now
		}
		r.parser.Feed(b, func(pk Packet) {
			if r.skip > 0 {
				r.skip--
				r.dropped++
				return
			}
			r.sample += uint64(pk.Lost) * SamplesPerPacket
			emit(Batch{Seq: pk.Seq, StartSample: r.sample, Lost: pk.Lost, CurrentNA: ToCurrentNA(pk.Codes, r.offset), Arrival: now})
			r.sample += uint64(len(pk.Codes))
		})
	}
}
