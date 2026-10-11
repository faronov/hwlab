package jlinknativeplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCleanupDoesNotReclassifyPrimaryError(t *testing.T) {
	for _, tc := range []struct {
		primary error
		code    codes.Code
		prefix  string
	}{
		{usb.ErrDisconnected, codes.Unavailable, DeviceGone},
		{context.Canceled, codes.Canceled, "context canceled"},
		{context.DeadlineExceeded, codes.DeadlineExceeded, "context deadline exceeded"},
		{jlerr.New(jlerr.ProtocolError, "verify", cortexm.ErrVerify), codes.FailedPrecondition, "PROTOCOL_ERROR"},
		{status.Error(codes.Unimplemented, "UNSUPPORTED"), codes.Unimplemented, "UNSUPPORTED"},
	} {
		err := classify(cortexm.WithCleanupError(tc.primary, usb.ErrDisconnected))
		msg := status.Convert(err).Message()
		if status.Code(err) != tc.code || !strings.HasPrefix(msg, tc.prefix) || !strings.Contains(msg, "cleanup failed") {
			t.Fatalf("primary=%v: code/message changed: %v", tc.primary, err)
		}
	}
}
