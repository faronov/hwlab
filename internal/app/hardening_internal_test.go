package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/apperr"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/policy"
	"github.com/faronov/hwlab/internal/usb"
)

func internalApp(t *testing.T) *App {
	t.Helper()
	cfg := config.Config{Home: t.TempDir()}
	cfg.SessionsDir = filepath.Join(cfg.Home, "sessions")
	a := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { a.Close(context.Background()) })
	return a
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"apperr", apperr.Newf(apperr.PathDenied, "x"), apperr.PathDenied},
		{"wrapped-apperr", fmt.Errorf("ctx: %w", apperr.Newf(apperr.BadAddress, "x")), apperr.BadAddress},
		{"cancel", context.Canceled, apperr.Cancelled},
		{"deadline", context.DeadlineExceeded, apperr.Timeout},
		{"usb-timeout", usb.ErrTimeout, apperr.Timeout},
		{"usb-gone", fmt.Errorf("read: %w", usb.ErrDisconnected), apperr.DeviceGone},
		{"usb-busy", usb.ErrBusy, apperr.Busy},
		{"usb-perm", usb.ErrPermission, apperr.PermissionDenied},
		{"swd-noack", adiv5.ErrNoACK, apperr.SWDNoACK},
		{"swd-wait", adiv5.ErrWait, apperr.SWDWait},
		{"swd-fault", adiv5.ErrFault, apperr.SWDFault},
		{"swd-proto", adiv5.ErrProtocol, apperr.SWDProtocol},
		{"not-halted", cortexm.ErrNotHalted, apperr.TargetNotHalted},
		{"verify", cortexm.ErrVerify, apperr.VerifyFailed},
		{"grpc-gone", status.Error(codes.Unavailable, "DEVICE_GONE: unplugged"), apperr.DeviceGone},
		{"grpc-busy", status.Error(codes.FailedPrecondition, "BUSY: owned"), apperr.Busy},
		{"grpc-usb-busy", status.Error(codes.FailedPrecondition, "stlink open device: "+usb.ErrBusy.Error()+": interface 0 is claimed"), apperr.Busy},
		{"grpc-usb-gone", status.Error(codes.Unknown, "read: "+usb.ErrDisconnected.Error()), apperr.DeviceGone},
		{"grpc-unsupported-hw", status.Error(codes.FailedPrecondition, "UNSUPPORTED_BY_ATTACHED_HW: no target"), apperr.UnsupportedByAttachedHW},
		{"grpc-protected", status.Error(codes.PermissionDenied, "flash is read protected"), apperr.ProtectedTarget},
		{"grpc-perm", status.Error(codes.PermissionDenied, "allow_flash not set"), apperr.PermissionDenied},
		{"grpc-invalid", status.Error(codes.InvalidArgument, "bad"), apperr.InvalidArgument},
		{"grpc-range", status.Error(codes.OutOfRange, "bad"), apperr.BadAddress},
		{"grpc-notfound", status.Error(codes.NotFound, "x"), apperr.NotFound},
		{"grpc-unimpl", status.Error(codes.Unimplemented, "x"), apperr.Unsupported},
		{"grpc-precondition", status.Error(codes.FailedPrecondition, "backup /x: no such file"), apperr.PreconditionFailed},
		{"grpc-dataloss", status.Error(codes.DataLoss, "x"), apperr.VerifyFailed},
		{"text-only-swd", status.Error(codes.Unknown, "dp read: "+adiv5.ErrNoACK.Error()), apperr.SWDNoACK},
		{"plain", errors.New("boom"), apperr.Internal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.err)
			if got.Code != c.want {
				t.Fatalf("code %s, want %s (%+v)", got.Code, c.want, got)
			}
			if got.Message == "" {
				t.Error("empty message")
			}
			if strings.Contains(got.Message, "rpc error:") {
				t.Errorf("transport prefix leaked: %q", got.Message)
			}
			if _, ok := ErrorCodes[got.Code]; !ok {
				t.Errorf("code %s missing from ErrorCodes", got.Code)
			}
		})
	}
	if c := Classify(&policy.ConfirmationRequired{}); c.Code != apperr.ConfirmationRequired {
		t.Errorf("policy error classified as %s", c.Code)
	}
	if !Classify(usb.ErrBusy).Retryable || Classify(apperr.Newf(apperr.InvalidArgument, "x")).Retryable {
		t.Error("retryable flag wrong")
	}
}

func TestValidateArgs(t *testing.T) {
	bad := []struct {
		name string
		args map[string]any
		code string
	}{
		{"nan", map[string]any{"v": math.NaN()}, apperr.InvalidArgument},
		{"inf", map[string]any{"v": math.Inf(1)}, apperr.InvalidArgument},
		{"2^53", map[string]any{"address": float64(1 << 53)}, ""},
		{"negative-address", map[string]any{"address": float64(-1)}, ""},
		{"negative-length", map[string]any{"length": -4}, ""},
		{"bad-hex", map[string]any{"address": "0xZZ"}, ""},
		{"hex-too-wide", map[string]any{"address": "0x1ffffffffffffffff"}, ""},
		{"overflow", map[string]any{"address": "0xfffffffffffffff0", "length": float64(0x100)}, apperr.BadAddress},
		{"nested", map[string]any{"a": []any{map[string]any{"address": "nope"}}}, ""},
		{"huge-string", map[string]any{"blob": strings.Repeat("x", 3<<20)}, apperr.InvalidArgument},
	}
	for _, c := range bad {
		t.Run("bad/"+c.name, func(t *testing.T) {
			err := ValidateArgs(c.args)
			if err == nil {
				t.Fatal("accepted")
			}
			code, _ := apperr.CodeOf(err)
			if code != apperr.InvalidArgument && code != apperr.BadAddress {
				t.Errorf("code %q", code)
			}
			if c.code != "" && code != c.code {
				t.Errorf("code %q, want %q", code, c.code)
			}
		})
	}
	good := []map[string]any{
		nil,
		{"address": "0x20000000", "length": 16},
		{"address": "536870912", "length": "0x10"},
		{"address": float64(0x20000000), "length": float64(4)},
		{"address": "0xfffffffffffffff0", "length": 16},
		{"address": uint64(0xfffffffffffffff0), "length": 16},
		{"path": "x.bin", "note": "hello"},
	}
	for i, args := range good {
		if err := ValidateArgs(args); err != nil {
			t.Errorf("good[%d] %v: %v", i, args, err)
		}
	}
}

func TestParseUint64(t *testing.T) {
	for in, want := range map[string]uint64{"0": 0, "16": 16, "0x10": 16, "0XFF": 255, "0xffffffffffffffff": math.MaxUint64} {
		got, err := ParseUint64(in)
		if err != nil || got != want {
			t.Errorf("%q -> %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "-1", "0x", "0x10000000000000000", "abc", "1.5"} {
		if _, err := ParseUint64(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestConfineFiles(t *testing.T) {
	a := internalApp(t)
	root := a.ArtifactsRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	denied := []string{"../../x.bin", "a/../../../x.bin", "/etc/passwd", filepath.Join(outside, "x.bin"), "/proc/self/environ", filepath.Join(root, "escape", "x.bin"), filepath.Join(root, "..", "evil")}
	for _, p := range denied {
		for _, key := range []string{"path", "backup", "backup_path", "dir", "file"} {
			_, err := a.confineFiles(map[string]any{key: p})
			if code, _ := apperr.CodeOf(err); code != apperr.PathDenied {
				t.Errorf("%s=%q: %v", key, p, err)
			}
		}
	}

	out, err := a.confineFiles(map[string]any{"path": "bk/fw.bin", "address": "0x10"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := out["path"].(string)
	if !strings.HasPrefix(p, root+string(filepath.Separator)) {
		t.Errorf("relative path not placed under artifacts: %q", p)
	}
	if out["address"] != "0x10" {
		t.Error("non-file argument modified")
	}
	in := filepath.Join(root, "inside.bin")
	out, err = a.confineFiles(map[string]any{"path": in})
	if err != nil || out["path"] != in {
		t.Errorf("absolute inside path: %v %v", out, err)
	}
}

func TestCheckExperimentPath(t *testing.T) {
	a := internalApp(t)
	ok := filepath.Join(a.Cfg.Home, "e.yaml")
	if err := os.WriteFile(ok, []byte("name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CheckExperimentPath(ok); err != nil {
		t.Errorf("inside home: %v", err)
	}
	for _, p := range []string{"/etc/passwd", "/etc/hosts.yaml", filepath.Join(a.Cfg.Home, "e.txt")} {
		if _, err := a.CheckExperimentPath(p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
	other := filepath.Join(t.TempDir(), "o.yaml")
	if err := os.WriteFile(other, []byte("name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CheckExperimentPath(other); err == nil {
		t.Error("yaml outside home and cwd accepted")
	}
}

func TestArtifactsBoundedAndConfined(t *testing.T) {
	a := internalApp(t)
	root := a.ArtifactsRoot()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 100<<10)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "big.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	list, err := a.ListArtifacts("")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range list {
		if it.Ref == "file/sub/big.bin" {
			found = true
		}
		if it.Name == "link" {
			t.Error("symlink listed")
		}
	}
	if !found {
		t.Fatalf("big.bin not listed: %+v", list)
	}

	info, err := a.ArtifactInfo("file/sub/big.bin")
	if err != nil || info.Size != int64(len(data)) || len(info.SHA256) != 64 {
		t.Fatalf("info: %+v %v", info, err)
	}
	ch, err := a.ReadArtifact("file/sub/big.bin", 10, 16, "hex")
	if err != nil || ch.Length != 16 || ch.EOF || ch.Data != "0a0b0c0d0e0f10111213141516171819" {
		t.Fatalf("chunk: %+v %v", ch, err)
	}
	ch, err = a.ReadArtifact("file/sub/big.bin", int64(len(data))-4, 64, "hex")
	if err != nil || ch.Length != 4 || !ch.EOF {
		t.Fatalf("tail: %+v %v", ch, err)
	}
	if _, err := a.ReadArtifact("file/sub/big.bin", 0, MaxArtifactRead+1, "hex"); err == nil {
		t.Error("read above the limit accepted")
	}
	if _, err := a.ReadArtifact("file/sub/big.bin", -1, 4, "hex"); err == nil {
		t.Error("negative offset accepted")
	}
	if _, err := a.ReadArtifact("file/sub/big.bin", 0, 4, "base64"); err == nil {
		t.Error("unknown encoding accepted")
	}
	for _, ref := range []string{"file/../x", "file/../../etc/passwd", "file//etc/passwd", "file/link", "file/sub", "file/", "session/", "session/x", "nothing"} {
		if _, err := a.ReadArtifact(ref, 0, 4, "hex"); err == nil {
			t.Errorf("ref %q readable", ref)
		}
	}
}
