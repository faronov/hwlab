package plugin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

const (
	launchTimeout   = 15 * time.Second
	shutdownTimeout = 3 * time.Second
)

// process is a running plugin executable with its gRPC client.
type process struct {
	path string
	info *pluginv1.PluginInfo
	log  *slog.Logger

	cmd     *exec.Cmd
	stdin   interface{ Close() error }
	conn    *grpc.ClientConn
	cli     pluginv1.PluginClient
	sockDir string

	done    chan struct{}
	exitErr error
	once    sync.Once
}

// launch starts the executable, waits for the handshake line, connects and
// performs the Handshake RPC.
func launch(ctx context.Context, path, coreVersion string, log *slog.Logger) (*process, error) {
	dir, err := os.MkdirTemp("", "hwl")
	if err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	cleanupStartup := func(primary error) error {
		if err := os.RemoveAll(dir); err != nil {
			return errors.Join(primary, fmt.Errorf("remove plugin socket directory: %w", err))
		}
		return primary
	}
	sock := filepath.Join(dir, "p.sock")
	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(), pluginv1.EnvSocket+"="+sock, pluginv1.EnvWatchStdin+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, cleanupStartup(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, cleanupStartup(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, cleanupStartup(err)
	}
	if err := cmd.Start(); err != nil {
		return nil, cleanupStartup(fmt.Errorf("start %s: %w", path, err))
	}
	p := &process{path: path, log: log.With("plugin_exe", filepath.Base(path)), cmd: cmd, stdin: stdin, sockDir: dir, done: make(chan struct{})}

	go func() { // forward plugin stderr into structured logs
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			p.log.Debug("plugin stderr", "line", sc.Text())
		}
	}()
	lineCh := make(chan string, 1)
	go func() {
		br := bufio.NewReader(stdout)
		line, _ := br.ReadString('\n')
		lineCh <- strings.TrimSpace(line)
		for { // keep draining so the plugin never blocks on stdout
			if _, err := br.ReadString('\n'); err != nil {
				return
			}
		}
	}()
	go func() {
		p.exitErr = cmd.Wait()
		close(p.done)
	}()

	fail := func(err error) (*process, error) {
		p.kill()
		return nil, err
	}

	select {
	case line := <-lineCh:
		parts := strings.Split(line, "|")
		if len(parts) != 4 || parts[0] != pluginv1.HandshakePrefix || parts[2] != "unix" {
			return fail(fmt.Errorf("%s: invalid handshake line %q", filepath.Base(path), line))
		}
		if v, err := strconv.Atoi(parts[1]); err != nil || uint32(v) != pluginv1.ProtocolVersion {
			return fail(fmt.Errorf("%s: unsupported plugin protocol %q (core speaks %d)", filepath.Base(path), parts[1], pluginv1.ProtocolVersion))
		}
		sock = parts[3]
	case <-p.done:
		if p.exitErr != nil {
			return fail(fmt.Errorf("%s exited during startup: %w", filepath.Base(path), p.exitErr))
		}
		return fail(fmt.Errorf("%s exited during startup without a handshake", filepath.Base(path)))
	case <-time.After(launchTimeout):
		return fail(fmt.Errorf("%s: timeout waiting for handshake line", filepath.Base(path)))
	case <-ctx.Done():
		return fail(ctx.Err())
	}

	conn, err := grpc.NewClient("unix:"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)))
	if err != nil {
		return fail(fmt.Errorf("dial plugin: %w", err))
	}
	p.conn, p.cli = conn, pluginv1.NewPluginClient(conn)

	hctx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()
	resp, err := p.cli.Handshake(hctx, &pluginv1.HandshakeRequest{CoreProtocolVersion: pluginv1.ProtocolVersion, CoreVersion: coreVersion})
	if err != nil {
		return fail(fmt.Errorf("handshake with %s: %w", filepath.Base(path), err))
	}
	info := resp.GetInfo()
	if info.GetId() == "" {
		return fail(fmt.Errorf("%s: plugin returned empty id", filepath.Base(path)))
	}
	if info.GetProtocolVersion() != pluginv1.ProtocolVersion {
		return fail(fmt.Errorf("%s: plugin protocol %d != core %d", info.GetId(), info.GetProtocolVersion(), pluginv1.ProtocolVersion))
	}
	p.info = info
	p.log = p.log.With("plugin", info.GetId())
	return p, nil
}

func (p *process) kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	p.cleanup()
}

func (p *process) cleanup() {
	p.once.Do(func() {
		if p.conn != nil {
			if err := p.conn.Close(); err != nil {
				p.log.Warn("close plugin connection", "err", err)
			}
		}
		if err := os.RemoveAll(p.sockDir); err != nil {
			p.log.Warn("remove plugin socket directory", "err", err)
		}
	})
}

func (p *process) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	select {
	case <-p.done:
		if p.exitErr != nil {
			return fmt.Errorf("%s %s: %w (%w)", p.info.GetId(), op, ErrCrashed, p.exitErr)
		}
		return fmt.Errorf("%s %s: %w", p.info.GetId(), op, ErrCrashed)
	default:
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.Unavailable {
		return fmt.Errorf("%s %s: %w: %w", p.info.GetId(), op, ErrCrashed, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s %s: %w", p.info.GetId(), op, err)
}

// Backend implementation -----------------------------------------------------

func (p *process) Info() *pluginv1.PluginInfo { return p.info }
func (p *process) Done() <-chan struct{}      { return p.done }

func (p *process) ListDevices(ctx context.Context) ([]*pluginv1.Device, error) {
	r, err := p.cli.ListDevices(ctx, &pluginv1.ListDevicesRequest{})
	if err != nil {
		return nil, p.wrap("list devices", err)
	}
	return r.GetDevices(), nil
}

func (p *process) OpenDevice(ctx context.Context, id string, cfg map[string]any) error {
	s, err := structpb.NewStruct(cfg)
	if err != nil {
		return fmt.Errorf("encode device config: %w", err)
	}
	_, err = p.cli.OpenDevice(ctx, &pluginv1.OpenDeviceRequest{DeviceId: id, Config: s})
	return p.wrap("open device", err)
}

func (p *process) CloseDevice(ctx context.Context, id string) error {
	_, err := p.cli.CloseDevice(ctx, &pluginv1.CloseDeviceRequest{DeviceId: id})
	return p.wrap("close device", err)
}

func (p *process) StartCapture(ctx context.Context, req *pluginv1.StartCaptureRequest) (*pluginv1.StartCaptureResponse, error) {
	r, err := p.cli.StartCapture(ctx, req)
	return r, p.wrap("start capture", err)
}

func (p *process) StopCapture(ctx context.Context, id string) (*pluginv1.StopCaptureResponse, error) {
	r, err := p.cli.StopCapture(ctx, &pluginv1.StopCaptureRequest{CaptureId: id})
	return r, p.wrap("stop capture", err)
}

func (p *process) StreamEvents(ctx context.Context, id string, fn func(*pluginv1.EventBatch) error) error {
	st, err := p.cli.StreamEvents(ctx, &pluginv1.StreamEventsRequest{CaptureId: id})
	if err != nil {
		return p.wrap("stream events", err)
	}
	for {
		b, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return p.wrap("stream events", err)
		}
		if err := fn(b); err != nil {
			return err
		}
	}
}

func (p *process) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	r, err := p.cli.Invoke(ctx, req)
	return r, p.wrap("invoke", err)
}

func (p *process) Close(ctx context.Context) error {
	select {
	case <-p.done:
		p.cleanup()
		return nil
	default:
	}
	sctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	_, _ = p.cli.Shutdown(sctx, &pluginv1.ShutdownRequest{})
	cancel()
	_ = p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(shutdownTimeout):
		p.log.Warn("plugin did not exit, killing")
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	p.cleanup()
	return nil
}
