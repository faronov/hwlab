// Package plugin discovers, launches and talks to hwlab plugins.
package plugin

import (
	"context"
	"errors"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// ErrCrashed is returned when the plugin process died.
var ErrCrashed = errors.New("plugin process crashed")

// Backend is the core-side view of a plugin. It is implemented by the gRPC
// client (separate process) and by in-process backends such as replay.
type Backend interface {
	Info() *pluginv1.PluginInfo
	ListDevices(ctx context.Context) ([]*pluginv1.Device, error)
	OpenDevice(ctx context.Context, deviceID string, cfg map[string]any) error
	CloseDevice(ctx context.Context, deviceID string) error
	StartCapture(ctx context.Context, req *pluginv1.StartCaptureRequest) (*pluginv1.StartCaptureResponse, error)
	StopCapture(ctx context.Context, captureID string) (*pluginv1.StopCaptureResponse, error)
	// StreamEvents blocks until the capture stream ends, calling fn for every
	// batch. fn must not retain the batch after returning.
	StreamEvents(ctx context.Context, captureID string, fn func(*pluginv1.EventBatch) error) error
	Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error)
	// Done is closed when the backend is no longer usable (process exit).
	Done() <-chan struct{}
	// Close releases the backend (graceful shutdown of the process).
	Close(ctx context.Context) error
}
