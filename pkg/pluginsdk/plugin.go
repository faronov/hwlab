// Package pluginsdk is the plugin-side helper for the hwlab plugin protocol.
//
// A plugin implements [Plugin] and calls [Serve] from main. The SDK handles
// the process handshake, the gRPC server, capture lifecycle, bounded event
// buffering and clean shutdown.
package pluginsdk

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// CaptureRequest is the decoded StartCaptureRequest.
type CaptureRequest struct {
	DeviceID    string
	CaptureID   string
	Config      map[string]any
	Duration    time.Duration
	ArtifactDir string
	// Clock maps the plugin monotonic clock onto the session timeline.
	Clock *Clock
}

// CaptureStart is returned by Plugin.StartCapture.
type CaptureStart struct {
	Streams []*pluginv1.StreamInfo
}

// Capture is a running capture. Workers must stop when the context passed to
// StartCapture is cancelled.
type Capture interface {
	// Wait blocks until all workers have exited.
	Wait() error
	// Result is called once, after Wait, and reports artifacts and stats.
	Result() *pluginv1.StopCaptureResponse
}

// Plugin is implemented by every hwlab plugin.
type Plugin interface {
	Info() *pluginv1.PluginInfo
	ListDevices(ctx context.Context) ([]*pluginv1.Device, error)
	OpenDevice(ctx context.Context, deviceID string, cfg map[string]any) error
	CloseDevice(ctx context.Context, deviceID string) error
	// StartCapture arms the device and starts workers that emit into sink
	// until ctx is cancelled. ctx outlives the RPC.
	StartCapture(ctx context.Context, req CaptureRequest, sink *Sink) (Capture, *CaptureStart, error)
	Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error)
}

// Clock converts the plugin monotonic clock to session timeline nanoseconds.
// Go's time.Now carries a monotonic reading, so differences are immune to
// wall-clock steps.
type Clock struct {
	recv    time.Time
	session int64
}

// NewClock anchors the clock: sessionNs is the session time at the moment the
// start request was received.
func NewClock(sessionNs int64) *Clock {
	return &Clock{recv: time.Now(), session: sessionNs}
}

// Now returns the current session timeline time.
func (c *Clock) Now() int64 { return c.session + int64(time.Since(c.recv)) }

// At converts a monotonic time.Time (obtained from time.Now) to the timeline.
func (c *Clock) At(t time.Time) int64 { return c.session + int64(t.Sub(c.recv)) }

// Offset returns the timeline time of a time.Duration after the anchor.
func (c *Clock) Offset(d time.Duration) int64 { return c.session + int64(d) }

func structToMap(s *structpb.Struct) map[string]any {
	if s == nil {
		return nil
	}
	return s.AsMap()
}
