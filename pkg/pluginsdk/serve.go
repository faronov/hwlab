package pluginsdk

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// Main serves p and exits the process. Typical plugin main():
//
//	func main() { pluginsdk.Main(myPlugin{}) }
func Main(p Plugin) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := Serve(context.Background(), p, log); err != nil {
		log.Error("plugin exited with error", "err", err)
		os.Exit(1)
	}
}

// Serve runs the plugin until the core disconnects, Shutdown is called, the
// context ends or a termination signal arrives.
func Serve(ctx context.Context, p Plugin, log *slog.Logger) error {
	sockPath := os.Getenv(pluginv1.EnvSocket)
	if sockPath == "" {
		dir, err := os.MkdirTemp("", "hwl")
		if err != nil {
			return fmt.Errorf("create socket dir: %w", err)
		}
		defer os.RemoveAll(dir)
		sockPath = filepath.Join(dir, "p.sock")
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", sockPath, err)
	}
	defer os.Remove(sockPath)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := newServer(ctx, p, log)
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(recoverUnary(log)),
		grpc.ChainStreamInterceptor(recoverStream(log)),
		grpc.MaxRecvMsgSize(64<<20), grpc.MaxSendMsgSize(64<<20),
	)
	pluginv1.RegisterPluginServer(gs, srv)

	if os.Getenv(pluginv1.EnvWatchStdin) != "" {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin) // returns on EOF: core is gone
			log.Info("stdin closed, shutting down")
			stop()
		}()
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-srv.shutdown:
		}
		srv.closeAll()
		done := make(chan struct{})
		go func() { gs.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			gs.Stop()
		}
	}()

	// The core waits for exactly this line.
	fmt.Fprintf(os.Stdout, "%s|%d|unix|%s\n", pluginv1.HandshakePrefix, pluginv1.ProtocolVersion, sockPath)
	if err := gs.Serve(lis); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

type captureState struct {
	cancel context.CancelFunc
	sink   *Sink
	done   chan struct{}
	result *pluginv1.StopCaptureResponse
	err    error
}

type server struct {
	pluginv1.UnimplementedPluginServer
	p        Plugin
	log      *slog.Logger
	baseCtx  context.Context
	shutdown chan struct{}
	once     sync.Once

	mu       sync.Mutex
	captures map[string]*captureState
}

func newServer(ctx context.Context, p Plugin, log *slog.Logger) *server {
	return &server{p: p, log: log, baseCtx: ctx, shutdown: make(chan struct{}), captures: map[string]*captureState{}}
}

func (s *server) closeAll() {
	s.mu.Lock()
	caps := make([]*captureState, 0, len(s.captures))
	for _, c := range s.captures {
		caps = append(caps, c)
	}
	s.mu.Unlock()
	for _, c := range caps {
		c.cancel()
		<-c.done
	}
}

func (s *server) Handshake(_ context.Context, req *pluginv1.HandshakeRequest) (*pluginv1.HandshakeResponse, error) {
	info := proto.Clone(s.p.Info()).(*pluginv1.PluginInfo)
	info.ProtocolVersion = pluginv1.ProtocolVersion
	return &pluginv1.HandshakeResponse{Info: info}, nil
}

func (s *server) ListDevices(ctx context.Context, _ *pluginv1.ListDevicesRequest) (*pluginv1.ListDevicesResponse, error) {
	devs, err := s.p.ListDevices(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pluginv1.ListDevicesResponse{Devices: devs}, nil
}

func (s *server) OpenDevice(ctx context.Context, req *pluginv1.OpenDeviceRequest) (*pluginv1.OpenDeviceResponse, error) {
	if err := s.p.OpenDevice(ctx, req.GetDeviceId(), structToMap(req.GetConfig())); err != nil {
		return nil, toStatus(err)
	}
	return &pluginv1.OpenDeviceResponse{}, nil
}

func (s *server) CloseDevice(ctx context.Context, req *pluginv1.CloseDeviceRequest) (*pluginv1.CloseDeviceResponse, error) {
	if err := s.p.CloseDevice(ctx, req.GetDeviceId()); err != nil {
		return nil, toStatus(err)
	}
	return &pluginv1.CloseDeviceResponse{}, nil
}

func (s *server) StartCapture(_ context.Context, req *pluginv1.StartCaptureRequest) (*pluginv1.StartCaptureResponse, error) {
	clock := NewClock(req.GetSessionTimeNs())
	id := req.GetCaptureId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "capture_id required")
	}
	s.mu.Lock()
	if _, ok := s.captures[id]; ok {
		s.mu.Unlock()
		return nil, status.Error(codes.AlreadyExists, errCaptureExists.Error())
	}
	s.mu.Unlock()

	cctx, cancel := context.WithCancel(s.baseCtx)
	sink := newSink(DefaultSinkDepth)
	cr := CaptureRequest{
		DeviceID: req.GetDeviceId(), CaptureID: id, Config: structToMap(req.GetConfig()),
		Duration: time.Duration(req.GetDurationNs()), ArtifactDir: req.GetArtifactDir(), Clock: clock,
	}
	capture, start, err := s.p.StartCapture(cctx, cr, sink)
	if err != nil {
		cancel()
		return nil, toStatus(err)
	}
	st := &captureState{cancel: cancel, sink: sink, done: make(chan struct{})}
	s.mu.Lock()
	s.captures[id] = st
	s.mu.Unlock()

	if cr.Duration > 0 {
		timer := time.AfterFunc(cr.Duration, cancel)
		go func() { <-st.done; timer.Stop() }()
	}
	go func() {
		defer close(st.done)
		st.err = capture.Wait()
		st.result = capture.Result()
		sink.close()
	}()

	resp := &pluginv1.StartCaptureResponse{StartedSessionNs: clock.Now()}
	if start != nil {
		resp.Streams = start.Streams
	}
	return resp, nil
}

func (s *server) StopCapture(ctx context.Context, req *pluginv1.StopCaptureRequest) (*pluginv1.StopCaptureResponse, error) {
	s.mu.Lock()
	st, ok := s.captures[req.GetCaptureId()]
	s.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "capture %q not found", req.GetCaptureId())
	}
	st.cancel()
	select {
	case <-st.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	delete(s.captures, req.GetCaptureId())
	s.mu.Unlock()
	if st.err != nil && ctx.Err() == nil && st.result == nil {
		return nil, toStatus(st.err)
	}
	res := st.result
	if res == nil {
		res = &pluginv1.StopCaptureResponse{}
	}
	if res.Stats == nil {
		res.Stats = &pluginv1.CaptureStats{}
	}
	res.Stats.DroppedBatches = st.sink.Dropped()
	return res, nil
}

func (s *server) StreamEvents(req *pluginv1.StreamEventsRequest, stream grpc.ServerStreamingServer[pluginv1.EventBatch]) error {
	s.mu.Lock()
	st, ok := s.captures[req.GetCaptureId()]
	s.mu.Unlock()
	if !ok {
		return status.Errorf(codes.NotFound, "capture %q not found", req.GetCaptureId())
	}
	var lastDropped uint64
	for {
		select {
		case evs, open := <-st.sink.ch:
			if !open {
				// Final message carries any outstanding drop count.
				if d := st.sink.Dropped(); d != lastDropped {
					return stream.Send(&pluginv1.EventBatch{DroppedBatches: d - lastDropped})
				}
				return nil
			}
			d := st.sink.Dropped()
			if err := stream.Send(&pluginv1.EventBatch{Events: evs, DroppedBatches: d - lastDropped}); err != nil {
				return err
			}
			lastDropped = d
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

func (s *server) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	resp, err := s.p.Invoke(ctx, req)
	if err != nil {
		return nil, toStatus(err)
	}
	return resp, nil
}

func (s *server) Shutdown(context.Context, *pluginv1.ShutdownRequest) (*pluginv1.ShutdownResponse, error) {
	s.once.Do(func() { close(s.shutdown) })
	return &pluginv1.ShutdownResponse{}, nil
}

func toStatus(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Unknown, err.Error())
}

func recoverUnary(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in handler", "method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Errorf(codes.Internal, "plugin panic: %v", r)
			}
		}()
		return h(ctx, req)
	}
}

func recoverStream(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in stream handler", "method", info.FullMethod, "panic", r)
				err = status.Errorf(codes.Internal, "plugin panic: %v", r)
			}
		}()
		return h(srv, ss)
	}
}
