package nrf802154

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/usbserial"
	"github.com/faronov/hwlab/plugins/nrf802154/dfu"
)

// dfuPID is the Nordic Open DFU bootloader's USB product id (nRF52840
// dongle PCA10059: hold the side RESET button, the red LED pulses).
const dfuPID = "521f"

// dfuConfirm must be passed as confirm_token to dfu_program.
const dfuConfirm = "program-application"

func isBootloader(i usbserial.Info) bool {
	return strings.EqualFold(i.VID, nordicVID) && strings.EqualFold(i.PID, dfuPID)
}

// dfuProgram implements FlashProgramming.dfu_program on an Open DFU
// bootloader device.
func (p *Plugin) dfuProgram(ctx context.Context, deviceID string, args map[string]any) (map[string]any, error) {
	if tok, _ := args["confirm_token"].(string); tok != dfuConfirm {
		return nil, status.Errorf(codes.FailedPrecondition,
			"dfu_program replaces the application on the dongle; pass confirm_token=%q (and confirm=true)", dfuConfirm)
	}
	path, _ := args["path"].(string)
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "path to an application .hex or nrfutil .zip is required")
	}
	ver := uint32(1)
	if v, ok := args["application_version"]; ok {
		f, ok := v.(float64)
		if !ok || f < 0 || f > 0xFFFFFFFF || f != float64(uint32(f)) {
			return nil, status.Error(codes.InvalidArgument, "application_version must be an unsigned 32-bit integer")
		}
		ver = uint32(f)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "read %s: %v", path, err)
	}
	img, err := dfu.Load(path, data, ver)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	infos, err := p.list()
	if err != nil {
		return nil, err
	}
	var port string
	for _, i := range infos {
		if i.SerialNumber == deviceID || i.Name == deviceID || filepath.Base(i.Name) == deviceID {
			if !isBootloader(i) {
				return nil, status.Errorf(codes.FailedPrecondition,
					"%s (%s:%s %q) is not the Open DFU bootloader (1915:521f); press the dongle's side RESET button so the red LED pulses, then use the bootloader device", deviceID, i.VID, i.PID, i.Product)
			}
			port = i.Name
		}
	}
	if port == "" {
		return nil, status.Errorf(codes.NotFound, "bootloader %q not found; press the dongle's side RESET button (red LED pulses) and list devices again", deviceID)
	}
	release, err := p.acquire(port)
	if err != nil {
		return nil, err
	}
	defer release()
	sp, err := p.open(port)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open %s: %v", port, err)
	}
	defer func() { _ = sp.Close() }()
	if err := sp.SetReadTimeout(20 * time.Millisecond); err != nil {
		return nil, status.Errorf(codes.Unavailable, "%s: %v", port, err)
	}
	if d, ok := sp.(interface{ SetDTR(bool) error }); ok {
		_ = d.SetDTR(true)
	}
	sum := sha256.Sum256(img.Bin)
	start := time.Now()
	c := &dfu.Client{Port: sp, Timeout: p.dfuTimeout}
	res, err := c.Program(ctx, img)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "DFU on %s failed after %s: %v (the bootloader keeps the previous application unless the transfer completed; retry from the bootloader)",
			port, time.Since(start).Round(time.Millisecond), err)
	}
	return map[string]any{
		"port": port, "image": img.Source, "bytes": float64(len(img.Bin)), "sha256": hex.EncodeToString(sum[:]),
		"application_version": float64(ver), "mtu": float64(res.MTU), "objects": float64(res.Objects),
		"activated_ack": res.Activated, "seconds": time.Since(start).Seconds(),
		"note": fmt.Sprintf("the bootloader validates the image and starts it; the application re-enumerates (e.g. 1915:%s) within a few seconds", snifferPID),
	}, nil
}
