package xds

import (
	"encoding/binary"
	"fmt"
)

// XDS110 JTAG state ids used by GOTO_STATE and JTAG_SCAN.
const (
	StReset     uint32 = 1
	StIdle      uint32 = 2
	StShiftDR   uint32 = 3
	StShiftIR   uint32 = 4
	StPauseDR   uint32 = 5
	StPauseIR   uint32 = 6
	StExit1DR   uint32 = 8
	StExit1IR   uint32 = 9
	StExit2DR   uint32 = 10
	StExit2IR   uint32 = 11
	StSelectDR  uint32 = 12
	StSelectIR  uint32 = 13
	StUpdateDR  uint32 = 14
	StUpdateIR  uint32 = 15
	StCaptureDR uint32 = 16
	StCaptureIR uint32 = 17
)

// transitQuickest routes between states by the shortest TMS path.
const transitQuickest = 1

// MaxScanBits is the largest single scan this driver issues; longer scans are
// rejected rather than split because a split would pass through Update.
const MaxScanBits = 8 * 1024

// GotoState moves the TAP to a stable state by the quickest route.
func (p *Probe) GotoState(state uint32) error {
	args := append(le32(state), le32(transitQuickest)...)
	_, err := p.Do(CmdGotoState, args)
	return err
}

// Scan shifts nbits through the IR (ir true) or DR from the current TAP state
// and finishes in end. data is LSB first; the captured bits come back in the
// same layout.
func (p *Probe) Scan(ir bool, data []byte, nbits int, end uint32) ([]byte, error) {
	if nbits < 1 || nbits > MaxScanBits {
		return nil, fmt.Errorf("xds110: scan of %d bits (1..%d supported)", nbits, MaxScanBits)
	}
	n := (nbits + 7) / 8
	buf := make([]byte, n)
	copy(buf, data)
	path := byte(StShiftDR)
	if ir {
		path = byte(StShiftIR)
	}
	a := make([]byte, 18, 18+n)
	binary.LittleEndian.PutUint16(a[0:], uint16(nbits))
	a[2] = path
	a[3] = transitQuickest
	a[4] = byte(end)
	a[5] = transitQuickest
	// preamble, postamble, delay: 0
	binary.LittleEndian.PutUint16(a[12:], 1) // repeat
	binary.LittleEndian.PutUint16(a[14:], uint16(n))
	binary.LittleEndian.PutUint16(a[16:], uint16(n))
	d, err := p.Do(CmdJTAGScan, append(a, buf...))
	if err != nil {
		return nil, err
	}
	if err := want(d, n, "JTAG_SCAN"); err != nil {
		return nil, err
	}
	return d[:n], nil
}
