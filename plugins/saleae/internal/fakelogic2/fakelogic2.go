// Package fakelogic2 is an in-process stand-in for the Saleae Logic 2
// automation server. It speaks the real gRPC API but fabricates the files
// Logic 2 would write, so the saleae plugin can be tested without Logic 2 or
// hardware. It is NOT evidence about Logic 2's real file formats: those are
// only verified by the opt-in hardware test.
package fakelogic2

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/saleae/internal/saleaepb"
)

// Default fabricated exports: a short SPI-like burst on CH0 (CLK), CH1 (MOSI), CH2 (IRQ).
const (
	DefaultDigital = "Time [s], Channel 0, Channel 1, Channel 2\n" +
		"0.000000000,0,0,0\n" +
		"0.000100000,0,0,1\n" +
		"0.000200000,1,1,1\n" +
		"0.000200500,0,1,1\n" +
		"0.000201000,1,0,1\n" +
		"0.000300000,1,0,0\n"
	DefaultAnalog = "Time [s], Channel 0\n" +
		"0.000000000,0.100\n0.000010000,0.200\n0.000020000,0.300\n0.000030000,0.400\n"
	DefaultTable = "name,type,start_time,duration,mosi,miso\n" +
		"SPI,result,0.000200000,0.000100000,0xA5,0x00\n" +
		"SPI,result,0.000250000,0.000040000,0x5A,0x01\n"
)

type capState struct {
	req    *saleaepb.StartCaptureRequest
	stop   chan struct{}
	once   sync.Once
	manual bool
}

func (c *capState) end() { c.once.Do(func() { close(c.stop) }) }

// Server is the fake Manager service.
type Server struct {
	saleaepb.UnimplementedManagerServer

	Devices []*saleaepb.Device
	Digital string
	Analog  string
	Table   string
	// FailExport makes ExportRawDataCsv fail.
	FailExport bool
	// FailTable makes ExportDataTableCsv fail.
	FailTable bool

	mu        sync.Mutex
	next      uint64
	caps      map[uint64]*capState
	analyzers map[uint64]*saleaepb.AddAnalyzerRequest
	calls     []string
}

// Start serves a Server on a free localhost port and returns its address.
func Start(tb testing.TB) (*Server, string) {
	tb.Helper()
	s := &Server{
		Devices: []*saleaepb.Device{{DeviceId: "F4241", DeviceType: saleaepb.DeviceType_DEVICE_TYPE_LOGIC_PRO_16, IsSimulation: false}},
		Digital: DefaultDigital, Analog: DefaultAnalog, Table: DefaultTable,
		caps: map[uint64]*capState{}, analyzers: map[uint64]*saleaepb.AddAnalyzerRequest{},
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	gs := grpc.NewServer()
	saleaepb.RegisterManagerServer(gs, s)
	go gs.Serve(lis)
	tb.Cleanup(gs.Stop)
	return s, lis.Addr().String()
}

func (s *Server) record(name string) {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	s.mu.Unlock()
}

// Calls returns the RPC names received, in order.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// StartRequest returns the StartCapture request that created capture id.
func (s *Server) StartRequest(id uint64) *saleaepb.StartCaptureRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.caps[id]; c != nil {
		return c.req
	}
	return nil
}

// Analyzers returns the AddAnalyzer requests received.
func (s *Server) Analyzers() []*saleaepb.AddAnalyzerRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*saleaepb.AddAnalyzerRequest
	for i := uint64(1); i <= uint64(len(s.analyzers)); i++ {
		out = append(out, s.analyzers[i])
	}
	return out
}

func (s *Server) GetAppInfo(context.Context, *saleaepb.GetAppInfoRequest) (*saleaepb.GetAppInfoReply, error) {
	s.record("GetAppInfo")
	return &saleaepb.GetAppInfoReply{AppInfo: &saleaepb.AppInfo{ApplicationVersion: "fake-2.4.0",
		ApiVersion: &saleaepb.Version{Major: 1, Minor: 0, Patch: 0}, LaunchPid: 4242}}, nil
}

func (s *Server) GetDevices(context.Context, *saleaepb.GetDevicesRequest) (*saleaepb.GetDevicesReply, error) {
	s.record("GetDevices")
	return &saleaepb.GetDevicesReply{Devices: s.Devices}, nil
}

func (s *Server) StartCapture(_ context.Context, r *saleaepb.StartCaptureRequest) (*saleaepb.StartCaptureReply, error) {
	s.record("StartCapture")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.caps {
		select {
		case <-c.stop:
		default:
			return nil, status.Error(codes.FailedPrecondition, "device busy")
		}
	}
	s.next++
	id := s.next
	c := &capState{req: r, stop: make(chan struct{})}
	if t := r.GetCaptureConfiguration().GetTimedCaptureMode(); t != nil {
		time.AfterFunc(time.Duration(t.GetDurationSeconds()*float64(time.Second)), c.end)
	} else {
		c.manual = true
	}
	s.caps[id] = c
	return &saleaepb.StartCaptureReply{CaptureInfo: &saleaepb.CaptureInfo{CaptureId: id}}, nil
}

func (s *Server) get(id uint64) (*capState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.caps[id]
	if c == nil {
		return nil, status.Errorf(codes.NotFound, "no capture %d", id)
	}
	return c, nil
}

func (s *Server) StopCapture(_ context.Context, r *saleaepb.StopCaptureRequest) (*saleaepb.StopCaptureReply, error) {
	s.record("StopCapture")
	c, err := s.get(r.GetCaptureId())
	if err != nil {
		return nil, err
	}
	c.end()
	return &saleaepb.StopCaptureReply{}, nil
}

func (s *Server) WaitCapture(ctx context.Context, r *saleaepb.WaitCaptureRequest) (*saleaepb.WaitCaptureReply, error) {
	s.record("WaitCapture")
	c, err := s.get(r.GetCaptureId())
	if err != nil {
		return nil, err
	}
	if c.manual {
		return nil, status.Error(codes.FailedPrecondition, "WaitCapture cannot be used with a manual capture")
	}
	select {
	case <-c.stop:
		return &saleaepb.WaitCaptureReply{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func write(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func (s *Server) SaveCapture(_ context.Context, r *saleaepb.SaveCaptureRequest) (*saleaepb.SaveCaptureReply, error) {
	s.record("SaveCapture")
	if _, err := s.get(r.GetCaptureId()); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(r.GetFilepath()) {
		return nil, status.Error(codes.InvalidArgument, "filepath must be absolute")
	}
	return &saleaepb.SaveCaptureReply{}, write(r.GetFilepath(), "FAKE-SAL")
}

func (s *Server) CloseCapture(_ context.Context, r *saleaepb.CloseCaptureRequest) (*saleaepb.CloseCaptureReply, error) {
	s.record("CloseCapture")
	c, err := s.get(r.GetCaptureId())
	if err != nil {
		return nil, err
	}
	c.end()
	return &saleaepb.CloseCaptureReply{}, nil
}

func (s *Server) ExportRawDataCsv(_ context.Context, r *saleaepb.ExportRawDataCsvRequest) (*saleaepb.ExportRawDataCsvReply, error) {
	s.record("ExportRawDataCsv")
	if s.FailExport {
		return nil, status.Error(codes.Internal, "export failed")
	}
	if _, err := s.get(r.GetCaptureId()); err != nil {
		return nil, err
	}
	ch := r.GetLogicChannels()
	if len(ch.GetDigitalChannels()) > 0 {
		if err := write(filepath.Join(r.GetDirectory(), "digital.csv"), s.Digital); err != nil {
			return nil, err
		}
	}
	if len(ch.GetAnalogChannels()) > 0 {
		if err := write(filepath.Join(r.GetDirectory(), "analog.csv"), s.Analog); err != nil {
			return nil, err
		}
	}
	return &saleaepb.ExportRawDataCsvReply{}, nil
}

func (s *Server) AddAnalyzer(_ context.Context, r *saleaepb.AddAnalyzerRequest) (*saleaepb.AddAnalyzerReply, error) {
	s.record("AddAnalyzer")
	if _, err := s.get(r.GetCaptureId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uint64(len(s.analyzers) + 1)
	s.analyzers[id] = r
	return &saleaepb.AddAnalyzerReply{AnalyzerId: id}, nil
}

func (s *Server) ExportDataTableCsv(_ context.Context, r *saleaepb.ExportDataTableCsvRequest) (*saleaepb.ExportDataTableCsvReply, error) {
	s.record("ExportDataTableCsv")
	if s.FailTable {
		return nil, status.Error(codes.Internal, "table export failed")
	}
	s.mu.Lock()
	n := len(s.analyzers)
	s.mu.Unlock()
	for _, a := range r.GetAnalyzers() {
		if a.GetAnalyzerId() == 0 || int(a.GetAnalyzerId()) > n {
			return nil, status.Error(codes.NotFound, fmt.Sprintf("analyzer %d not found", a.GetAnalyzerId()))
		}
	}
	return &saleaepb.ExportDataTableCsvReply{}, write(r.GetFilepath(), s.Table)
}
