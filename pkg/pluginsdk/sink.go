package pluginsdk

import (
	"context"
	"errors"
	"sync/atomic"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// DefaultSinkDepth bounds the number of undelivered batches per capture.
const DefaultSinkDepth = 64

// Sink is the bounded event buffer between plugin workers and StreamEvents.
type Sink struct {
	ch      chan []*pluginv1.Event
	dropped atomic.Uint64
}

func newSink(depth int) *Sink {
	if depth <= 0 {
		depth = DefaultSinkDepth
	}
	return &Sink{ch: make(chan []*pluginv1.Event, depth)}
}

// Emit queues a batch, blocking (backpressure) while the buffer is full.
// It returns ctx.Err() if the context ends first.
func (s *Sink) Emit(ctx context.Context, events ...*pluginv1.Event) error {
	if len(events) == 0 {
		return nil
	}
	select {
	case s.ch <- events:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryEmit queues a batch without blocking. If the buffer is full the batch is
// dropped and counted; the core is told via EventBatch.dropped_batches.
// Hardware that cannot be paused (live streaming) should use this.
func (s *Sink) TryEmit(events ...*pluginv1.Event) bool {
	if len(events) == 0 {
		return true
	}
	select {
	case s.ch <- events:
		return true
	default:
		s.dropped.Add(1)
		return false
	}
}

// Dropped returns the total number of dropped batches.
func (s *Sink) Dropped() uint64 { return s.dropped.Load() }

func (s *Sink) close() { close(s.ch) }

var errCaptureExists = errors.New("capture id already in use")
