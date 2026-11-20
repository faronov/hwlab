package xds

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/faronov/hwlab/internal/jtag"
)

// OCD_SCAN_REQUEST entry types.
const (
	scanReqIR      = 1
	scanReqDR      = 2
	scanReqRunTest = 3
)

// maxScanBlock is the largest request and result block of one
// OCD_SCAN_REQUEST (the probe's 4 KiB data block).
const maxScanBlock = 4096

// ScanEngine runs batched IR/DR scans with OCD_SCAN_REQUEST (firmware 2.3.0.17
// and later): every scan walks from the current stable state to Shift-IR/DR
// and ends in the requested stable state, so no Test-Logic-Reset is ever
// clocked. It implements jtag.Scanner and works in every scan format the
// probe was connected with, including the two-pin cJTAG formats.
type ScanEngine struct {
	P *Probe
}

var _ jtag.Scanner = (*ScanEngine)(nil)

func xdsEnd(s jtag.State) (byte, error) {
	switch s {
	case jtag.RunTestIdle:
		return byte(StIdle), nil
	case jtag.PauseDR:
		return byte(StPauseDR), nil
	case jtag.PauseIR:
		return byte(StPauseIR), nil
	}
	return 0, fmt.Errorf("xds110: scan end state %v is not supported (Run-Test/Idle, Pause-DR or Pause-IR)", s)
}

// Scans implements jtag.Scanner.
func (e *ScanEngine) Scans(ctx context.Context, ops []jtag.ScanOp) ([][]byte, error) {
	if fw := e.P.Info.Firmware; fw != 0 && fw < FWOptimizedDAP {
		return nil, fmt.Errorf("XDS110 firmware %s has no OCD_SCAN_REQUEST (needs %s)", e.P.Info.FirmwareText, FirmwareString(FWOptimizedDAP))
	}
	var out [][]byte
	var req []byte
	var sizes []int
	resBytes := 0
	flush := func() error {
		if len(req) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		d, err := e.P.Do(CmdScanRequest, append(req, 0))
		if err != nil {
			return err
		}
		if err := want(d, resBytes, "OCD_SCAN_REQUEST results"); err != nil {
			return err
		}
		off := 0
		for _, n := range sizes {
			out = append(out, append([]byte(nil), d[off:off+n]...))
			off += n
		}
		req, sizes, resBytes = req[:0], sizes[:0], 0
		return nil
	}
	for _, op := range ops {
		end, err := xdsEnd(op.End)
		if err != nil {
			return nil, err
		}
		var entry []byte
		n := 0
		if op.Bits == 0 {
			if op.Idle < 0 {
				return nil, fmt.Errorf("xds110: negative run-test count %d", op.Idle)
			}
			entry = binary.LittleEndian.AppendUint32([]byte{scanReqRunTest}, uint32(op.Idle))
			entry = append(entry, end)
		} else {
			if op.Bits < 0 || op.Bits > 0xFFFF {
				return nil, fmt.Errorf("xds110: scan of %d bits", op.Bits)
			}
			n = (op.Bits + 7) / 8
			kind := byte(scanReqDR)
			if op.IR {
				kind = scanReqIR
			}
			entry = append(entry, kind, end, byte(op.Bits), byte(op.Bits>>8))
			data := make([]byte, n)
			copy(data, op.Out)
			entry = append(entry, data...)
		}
		if len(entry)+1 > maxScanBlock || n > maxScanBlock {
			return nil, fmt.Errorf("xds110: scan of %d bits does not fit one request", op.Bits)
		}
		if len(req)+len(entry)+1 > maxScanBlock || resBytes+n > maxScanBlock || len(sizes) >= maxScanBlock/4 {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		req = append(req, entry...)
		if op.Bits > 0 {
			sizes = append(sizes, n)
			resBytes += n
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	for i, j := 0, 0; i < len(ops); i++ {
		if ops[i].Bits == 0 {
			continue
		}
		if r := ops[i].Bits % 8; r != 0 {
			out[j][len(out[j])-1] &= byte(1<<r) - 1
		}
		j++
	}
	return out, nil
}
