package pluginsdk

import pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"

// NewTestSink returns a sink for plugin unit tests; read it with Drain.
func NewTestSink(depth int) *Sink { return newSink(depth) }

// Drain closes the sink and returns every queued event in order. Call it
// after the capture's workers have finished.
func (s *Sink) Drain() []*pluginv1.Event {
	s.close()
	var out []*pluginv1.Event
	for batch := range s.ch {
		out = append(out, batch...)
	}
	return out
}
