// Package stlink implements the ST-Link/V2 USB debug protocol natively over
// internal/usb (Linux usbfs, no libusb, no cgo).
//
// The command set and the quirks that matter (the DFU-exit step, stall
// recovery, the 16-byte command frame, the version word layout, the SWD
// divisor table) were established against a real probe and cross-checked with
// the BSD-3-Clause stlink-org/stlink project, which is used as a protocol
// reference only. See NOTICE.
//
// Generations: V2 and V2-1 share the same debug API and are supported. V1
// (SCSI-wrapped) and V3 (APIv3 frame sizes and frequency API) are recognised
// and refused with ErrUnsupportedGeneration rather than driven with V2
// semantics.
package stlink

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/usb"
)

// Top-level commands.
const (
	cmdGetVersion  = 0xF1
	cmdDebug       = 0xF2
	cmdDFU         = 0xF3
	cmdGetMode     = 0xF5
	cmdGetVoltage  = 0xF7
	dfuExit        = 0x07
	cmdVersionAPI3 = 0xFB
)

// Debug sub-commands (V2 API).
const (
	dbgGetStatus     = 0x01
	dbgExit          = 0x21
	dbgEnter         = 0x30
	dbgReadIDCodes   = 0x31
	dbgResetSys      = 0x32
	dbgReadReg       = 0x33
	dbgWriteReg      = 0x34
	dbgWriteDebug    = 0x35
	dbgReadDebug     = 0x36
	dbgReadAllRegs   = 0x3A
	dbgLastRW        = 0x3B
	dbgDriveNRST     = 0x3C
	dbgLastRW2       = 0x3E
	dbgSWDSetFreq    = 0x43
	dbgReadMem32     = 0x07
	dbgWriteMem32    = 0x08
	dbgWriteMem8     = 0x0D
	enterSWD         = 0xA3
	enterJTAGNoReset = 0xA4
)

// Status codes in byte 0 of a reply.
const (
	StatusOK            = 0x80
	StatusFault         = 0x81
	StatusJTAGWriteErr  = 0x0C
	StatusWriteVerify   = 0x0D
	StatusIDCodeErr     = 0x09
	StatusUnknownChain  = 0x04
	StatusAPWait        = 0x10
	StatusAPFault       = 0x11
	StatusAPError       = 0x12
	StatusAPParity      = 0x13
	StatusDPWait        = 0x14
	StatusDPFault       = 0x15
	StatusDPError       = 0x16
	StatusDPParity      = 0x17
	maxWaitRetries      = 3
	connectSettleDelay  = 50 * time.Millisecond
	defaultReadChunk    = 1024
	defaultWriteChunk   = 1024
	write8Chunk         = 64
	maxPacket           = 64
	defaultXferDeadline = 3 * time.Second
)

// StatusName names a status code; unknown codes are returned as hex so they
// stay visible in diagnostics.
func StatusName(c byte) string {
	switch c {
	case StatusOK:
		return "OK"
	case StatusFault:
		return "FAULT"
	case StatusJTAGWriteErr:
		return "JTAG_WRITE_ERROR"
	case StatusWriteVerify:
		return "WRITE_VERIFY_ERROR"
	case StatusIDCodeErr:
		return "GET_IDCODE_ERROR"
	case StatusUnknownChain:
		return "UNKNOWN_JTAG_CHAIN"
	case StatusAPWait:
		return "AP_WAIT"
	case StatusAPFault:
		return "AP_FAULT"
	case StatusAPError:
		return "AP_ERROR"
	case StatusAPParity:
		return "AP_PARITY_ERROR"
	case StatusDPWait:
		return "DP_WAIT"
	case StatusDPFault:
		return "DP_FAULT"
	case StatusDPError:
		return "DP_ERROR"
	case StatusDPParity:
		return "DP_PARITY_ERROR"
	}
	return fmt.Sprintf("UNKNOWN_0x%02X", c)
}

// Errors.
var (
	ErrShortReply            = errors.New("stlink: short reply")
	ErrUnsupportedGeneration = errors.New("stlink: unsupported probe generation")
	ErrUnsupportedFirmware   = errors.New("stlink: probe firmware lacks this feature")
	ErrNoTarget              = errors.New("stlink: no target answered on the debug port")
)

// StatusError is a reply whose status byte is not OK.
type StatusError struct {
	Op   string
	Code byte
	Addr uint32 // fault address when the probe reported one
	Have bool
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("stlink %s: status 0x%02X (%s)", e.Op, e.Code, StatusName(e.Code))
	if e.Have {
		s += fmt.Sprintf(" at 0x%08X", e.Addr)
	}
	return s
}

// IsWait reports an AP/DP WAIT status.
func (e *StatusError) IsWait() bool { return e.Code == StatusAPWait || e.Code == StatusDPWait }

// Version is the probe's own report.
type Version struct {
	Raw        []byte
	STLinkV    int
	JTAGV      int
	SWIMV      int
	VID, PID   uint16
	Generation string // "V1", "V2", "V2-1", "V3", "unknown"
	Name       string // e.g. "V2J39S7"
}

// Probe feature levels derived from the reported firmware.
func (v Version) HasSWDFreq() bool   { return v.STLinkV == 2 && v.JTAGV >= 22 }
func (v Version) HasRWStatus2() bool { return v.JTAGV >= 15 }
func (v Version) HasTrace() bool     { return v.JTAGV >= 13 }

// ParseVersion decodes a GET_VERSION reply. pid is the USB product id, used
// only to tell V2 from V2-1 (their GET_VERSION layout is identical).
func ParseVersion(b []byte, pid uint16) (Version, error) {
	if len(b) < 6 {
		return Version{}, fmt.Errorf("%w: GET_VERSION returned %d bytes, want 6", ErrShortReply, len(b))
	}
	v := Version{Raw: append([]byte(nil), b[:6]...)}
	word := uint16(b[0])<<8 | uint16(b[1])
	v.STLinkV = int(word >> 12 & 0xF)
	v.JTAGV = int(word >> 6 & 0x3F)
	v.SWIMV = int(word & 0x3F)
	v.VID = uint16(b[2]) | uint16(b[3])<<8
	v.PID = uint16(b[4]) | uint16(b[5])<<8
	switch v.STLinkV {
	case 1:
		v.Generation = "V1"
	case 2:
		v.Generation = "V2"
		if pid == 0x374b || pid == 0x3752 { // V2-1 is told from V2 by the USB PID only
			v.Generation = "V2-1"
		}
	case 3:
		v.Generation = "V3"
	default:
		v.Generation = "unknown"
	}
	v.Name = fmt.Sprintf("V%dJ%dS%d", v.STLinkV, v.JTAGV, v.SWIMV)
	return v, nil
}

// Mode is the probe's current operating mode.
type Mode int

// Probe modes (reply byte 0 of GET_CURRENT_MODE).
const (
	ModeDFU   Mode = 0
	ModeMass  Mode = 1
	ModeDebug Mode = 2
	ModeSWIM  Mode = 3
	ModeBoot  Mode = 4
)

func (m Mode) String() string {
	switch m {
	case ModeDFU:
		return "dfu"
	case ModeMass:
		return "mass-storage/idle"
	case ModeDebug:
		return "debug"
	case ModeSWIM:
		return "swim"
	case ModeBoot:
		return "bootloader"
	}
	return fmt.Sprintf("mode-%d", int(m))
}

// Conn is one ST-Link debug interface. It is not safe for concurrent use;
// the plugin serialises access.
type Conn struct {
	port    usb.Port
	out, in uint8
	trace   uint8
	timeout time.Duration

	Ver Version
	// AP is the MEM-AP the memory commands address.
	AP uint8
	// ReadChunk and WriteChunk bound one memory command's payload.
	ReadChunk, WriteChunk int

	// Stats count USB commands, for performance work and tests.
	Stats struct{ Commands, Recoveries int }

	connected bool
}

// Endpoints picks the request/reply/trace endpoints from the descriptor: the
// first bulk OUT is the request pipe, the first bulk IN the reply pipe and
// the second bulk IN the SWO trace pipe.
func Endpoints(i usb.Interface) (out, in, trace uint8, err error) {
	for _, e := range i.Endpoints {
		if e.Type != "Bulk" {
			continue
		}
		switch {
		case !e.In() && out == 0:
			out = e.Address
		case e.In() && in == 0:
			in = e.Address
		case e.In() && trace == 0:
			trace = e.Address
		}
	}
	if out == 0 || in == 0 {
		return 0, 0, 0, fmt.Errorf("interface %d has no bulk OUT+IN endpoint pair", i.Number)
	}
	return out, in, trace, nil
}

// Open prepares the interface: it clears any endpoint halt, drains stale
// replies left by an earlier owner and reads the version. It does not enter
// debug mode.
func Open(ctx context.Context, port usb.Port, ifc usb.Interface, timeout time.Duration) (*Conn, error) {
	out, in, trace, err := Endpoints(ifc)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = defaultXferDeadline
	}
	c := &Conn{port: port, out: out, in: in, trace: trace, timeout: timeout, ReadChunk: defaultReadChunk, WriteChunk: defaultWriteChunk}
	c.recover(ctx)
	var raw []byte
	for attempt := 0; ; attempt++ {
		raw, err = c.xfer(ctx, "GET_VERSION", []byte{cmdGetVersion}, 6, nil)
		if err == nil || attempt >= 1 || errors.Is(err, usb.ErrDisconnected) {
			break
		}
		c.recover(ctx)
	}
	if err != nil {
		return nil, err
	}
	pid := uint16(0)
	if len(raw) >= 6 {
		pid = uint16(raw[4]) | uint16(raw[5])<<8
	}
	if c.Ver, err = ParseVersion(raw, pid); err != nil {
		return nil, err
	}
	switch c.Ver.Generation {
	case "V2", "V2-1":
	default:
		return nil, fmt.Errorf("%w: the probe reports %s (%s); only V2 and V2-1 are implemented", ErrUnsupportedGeneration, c.Ver.Generation, c.Ver.Name)
	}
	return c, nil
}

// Close releases nothing on the wire: the caller owns the port. It leaves the
// probe out of debug mode if it was connected.
func (c *Conn) Close(ctx context.Context) error {
	if c.connected {
		return c.Disconnect(ctx)
	}
	return nil
}

// recover clears halts and drains stale reply bytes. Stale replies wedge the
// probe (OUT timeouts) on real hardware.
func (c *Conn) recover(ctx context.Context) {
	c.Stats.Recoveries++
	if hc, ok := c.port.(usb.HaltClearer); ok {
		_ = hc.ClearHalt(c.in)
		_ = hc.ClearHalt(c.out)
	}
	for i := 0; i < 32; i++ {
		if _, err := c.port.BulkIn(ctx, c.in, maxPacket, 30*time.Millisecond); err != nil {
			break
		}
	}
}

// xfer sends one 16-byte command frame (and optional data stage on the same
// pipe), then reads exactly rx reply bytes.
func (c *Conn) xfer(ctx context.Context, op string, cmd []byte, rx int, data []byte) ([]byte, error) {
	c.Stats.Commands++
	frame := make([]byte, 16)
	copy(frame, cmd)
	if _, err := c.port.BulkOut(ctx, c.out, frame, c.timeout); err != nil {
		return nil, c.wrapIO(ctx, op, "send", err)
	}
	if len(data) > 0 {
		if _, err := c.port.BulkOut(ctx, c.out, data, c.timeout); err != nil {
			return nil, c.wrapIO(ctx, op, "send data", err)
		}
	}
	if rx == 0 {
		return nil, nil
	}
	got := make([]byte, 0, rx)
	for len(got) < rx {
		b, err := c.port.BulkIn(ctx, c.in, rx-len(got), c.timeout)
		if err != nil {
			return nil, c.wrapIO(ctx, op, "read reply", err)
		}
		got = append(got, b...)
		if len(b) == 0 || (len(got) < rx && len(b)%maxPacket != 0) {
			return nil, fmt.Errorf("%w: %s returned %d of %d bytes", ErrShortReply, op, len(got), rx)
		}
	}
	return got, nil
}

func (c *Conn) wrapIO(ctx context.Context, op, stage string, err error) error {
	switch {
	case errors.Is(err, usb.ErrDisconnected), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("stlink %s (%s): %w", op, stage, err)
	case errors.Is(err, usb.ErrStall):
		c.recover(ctx)
		return fmt.Errorf("stlink %s (%s): the probe stalled the endpoint (unsupported command or wrong mode; recovered): %w", op, stage, err)
	case errors.Is(err, usb.ErrTimeout):
		c.recover(ctx)
		return fmt.Errorf("stlink %s (%s): the probe did not answer: %w", op, stage, err)
	}
	return fmt.Errorf("stlink %s (%s): %w", op, stage, err)
}

// check validates byte 0 of a reply, retrying nothing.
func check(op string, r []byte) error {
	if len(r) == 0 {
		return fmt.Errorf("%w: %s", ErrShortReply, op)
	}
	if r[0] != StatusOK {
		return &StatusError{Op: op, Code: r[0]}
	}
	return nil
}

// dbg runs a debug command with a status-checked reply, retrying AP/DP WAIT
// with a bounded back-off.
func (c *Conn) dbg(ctx context.Context, op string, rx int, args ...byte) ([]byte, error) {
	cmd := append([]byte{cmdDebug}, args...)
	for retry := 0; ; retry++ {
		r, err := c.xfer(ctx, op, cmd, rx, nil)
		if err != nil {
			return nil, err
		}
		err = check(op, r)
		if err == nil {
			return r, nil
		}
		var se *StatusError
		if errors.As(err, &se) && se.IsWait() && retry < maxWaitRetries {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Millisecond << retry):
			}
			continue
		}
		return r, err
	}
}

func le32(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
func rd32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// Mode reads the probe mode.
func (c *Conn) Mode(ctx context.Context) (Mode, error) {
	r, err := c.xfer(ctx, "GET_CURRENT_MODE", []byte{cmdGetMode}, 2, nil)
	if err != nil {
		return 0, err
	}
	return Mode(r[0]), nil
}

// TargetVoltage reads the VTref measurement in millivolts. A zero ADC factor
// is reported as an error rather than a made-up voltage.
func (c *Conn) TargetVoltage(ctx context.Context) (mv int, err error) {
	r, err := c.xfer(ctx, "GET_TARGET_VOLTAGE", []byte{cmdGetVoltage}, 8, nil)
	if err != nil {
		return 0, err
	}
	factor, reading := rd32(r), rd32(r[4:])
	if factor == 0 {
		return 0, fmt.Errorf("stlink GET_TARGET_VOLTAGE: the probe reported ADC factor 0")
	}
	return int(2400 * uint64(reading) / uint64(factor)), nil
}

// SWD divisors of the V2 firmware, kHz -> divisor, fastest first.
var swdDivisors = []struct{ KHz, Div int }{
	{4000, 0}, {1800, 1}, {1200, 2}, {950, 3}, {480, 7}, {240, 15}, {125, 31}, {100, 40}, {50, 79}, {25, 158}, {15, 265}, {5, 798},
}

// SupportedSWDKHz lists the SWD clocks this probe can be set to.
func (c *Conn) SupportedSWDKHz() []int {
	if !c.Ver.HasSWDFreq() {
		return nil
	}
	var out []int
	for _, d := range swdDivisors {
		out = append(out, d.KHz)
	}
	return out
}

// QuantizeSWD returns the supported clock nearest to khz.
func QuantizeSWD(khz int) (actual, divisor int) {
	best := swdDivisors[0]
	bestDiff := 1 << 30
	for _, d := range swdDivisors {
		diff := khz - d.KHz
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			best, bestDiff = d, diff
		}
	}
	return best.KHz, best.Div
}

// SetSWDClock programs the SWD clock and returns the actual clock the probe
// was set to. A probe whose firmware has no frequency command refuses.
func (c *Conn) SetSWDClock(ctx context.Context, khz int) (int, error) {
	if !c.Ver.HasSWDFreq() {
		return 0, fmt.Errorf("%w: SWD clock selection needs ST-Link/V2 firmware J22 or newer, this is %s", ErrUnsupportedFirmware, c.Ver.Name)
	}
	actual, div := QuantizeSWD(khz)
	if _, err := c.dbg(ctx, "SWD_SET_FREQ", 2, dbgSWDSetFreq, byte(div), byte(div>>8)); err != nil {
		return 0, err
	}
	return actual, nil
}

// ConnectConfig selects the transport.
type ConnectConfig struct {
	Transport string // "swd" (default) or "jtag"
	KHz       int    // 0 leaves the probe default
}

// ConnectInfo is what a successful connect observed.
type ConnectInfo struct {
	Transport   string
	ModeBefore  Mode
	ModeAfter   Mode
	DFUExit     bool
	ActualKHz   int
	DPIDCode    uint32
	IDCodesRaw  []byte
	VoltageMV   int
	VoltageOK   bool
	ConnectedNS int64
}

// Connect leaves DFU if needed, sets the clock, enters SWD or JTAG and reads
// the debug-port IDCODE. It never resets the target.
func (c *Conn) Connect(ctx context.Context, cfg ConnectConfig) (ConnectInfo, error) {
	t0 := time.Now()
	info := ConnectInfo{Transport: cfg.Transport}
	if info.Transport == "" {
		info.Transport = "swd"
	}
	if info.Transport != "swd" && info.Transport != "jtag" {
		return info, fmt.Errorf("unknown transport %q (swd, jtag)", cfg.Transport)
	}
	var err error
	if info.ModeBefore, err = c.Mode(ctx); err != nil {
		return info, err
	}
	switch info.ModeBefore {
	case ModeDebug:
		_ = c.exitDebug(ctx)
	case ModeDFU:
		// The probe powers up in its DFU/idle state and only accepts debug
		// commands after this exit.
		if _, err := c.xfer(ctx, "DFU_EXIT", []byte{cmdDFU, dfuExit}, 0, nil); err != nil {
			return info, err
		}
		info.DFUExit = true
		time.Sleep(connectSettleDelay)
	}
	if mv, err := c.TargetVoltage(ctx); err == nil {
		info.VoltageMV, info.VoltageOK = mv, true
	}
	if cfg.KHz > 0 && info.Transport == "swd" {
		if info.ActualKHz, err = c.SetSWDClock(ctx, cfg.KHz); err != nil {
			return info, err
		}
	}
	arg := byte(enterSWD)
	if info.Transport == "jtag" {
		arg = enterJTAGNoReset
	}
	if r, err := c.dbg(ctx, "ENTER_"+info.Transport, 2, dbgEnter, arg); err != nil {
		var se *StatusError
		if errors.As(err, &se) && (se.Code == StatusIDCodeErr || se.Code == StatusUnknownChain) {
			c.recover(ctx)
			return info, fmt.Errorf("%w: %v (check SWDIO, SWCLK, GND and target power)", ErrNoTarget, err)
		}
		_ = r
		return info, err
	}
	c.connected = true
	if info.ModeAfter, err = c.Mode(ctx); err != nil {
		return info, err
	}
	ids, err := c.dbg(ctx, "READ_IDCODES", 12, dbgReadIDCodes)
	if err != nil {
		return info, err
	}
	info.IDCodesRaw = ids
	info.DPIDCode = rd32(ids[4:])
	info.ConnectedNS = time.Since(t0).Nanoseconds()
	return info, nil
}

func (c *Conn) exitDebug(ctx context.Context) error {
	// The real probe sends no reply to DEBUG_EXIT; waiting for one costs a
	// full transfer timeout on every close.
	if _, err := c.xfer(ctx, "EXIT", []byte{cmdDebug, dbgExit}, 0, nil); err != nil {
		return err
	}
	return nil
}

// Disconnect leaves debug mode. It does not touch the core, so a running
// target keeps running.
func (c *Conn) Disconnect(ctx context.Context) error {
	c.connected = false
	return c.exitDebug(ctx)
}

// Connected reports whether Connect succeeded and Disconnect has not run.
func (c *Conn) Connected() bool { return c.connected }

// PinReset drives the NRST line low for lowFor, then releases it. The probe
// cannot tell whether NRST is wired; the caller must verify the effect.
func (c *Conn) PinReset(ctx context.Context, lowFor time.Duration) error {
	if _, err := c.dbg(ctx, "DRIVE_NRST_LOW", 2, dbgDriveNRST, 0); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		_, _ = c.dbg(context.Background(), "DRIVE_NRST_HIGH", 2, dbgDriveNRST, 1)
		return ctx.Err()
	case <-time.After(lowFor):
	}
	_, err := c.dbg(ctx, "DRIVE_NRST_HIGH", 2, dbgDriveNRST, 1)
	return err
}

// rwStatus reads the status of the last memory command.
func (c *Conn) rwStatus(ctx context.Context, op string) error {
	if !c.Ver.HasRWStatus2() {
		r, err := c.xfer(ctx, op+"/GETLASTRWSTATUS", []byte{cmdDebug, dbgLastRW}, 2, nil)
		if err != nil {
			return err
		}
		return check(op, r)
	}
	r, err := c.xfer(ctx, op+"/GETLASTRWSTATUS2", []byte{cmdDebug, dbgLastRW2}, 12, nil)
	if err != nil {
		return err
	}
	if r[0] == StatusOK {
		return nil
	}
	se := &StatusError{Op: op, Code: r[0]}
	if r[0] != 0 {
		se.Addr, se.Have = rd32(r[4:]), true
	}
	return se
}

// Read32 reads one word through READDEBUGREG, a single status-checked
// command.
func (c *Conn) Read32(ctx context.Context, addr uint32) (uint32, error) {
	r, err := c.dbg(ctx, "READDEBUGREG", 8, append([]byte{dbgReadDebug}, le32(addr)...)...)
	if err != nil {
		return 0, withAddr(err, addr)
	}
	return rd32(r[4:]), nil
}

// Write32 writes one word through WRITEDEBUGREG.
func (c *Conn) Write32(ctx context.Context, addr, v uint32) error {
	_, err := c.dbg(ctx, "WRITEDEBUGREG", 2, append(append([]byte{dbgWriteDebug}, le32(addr)...), le32(v)...)...)
	return withAddr(err, addr)
}

func withAddr(err error, addr uint32) error {
	var se *StatusError
	if errors.As(err, &se) && !se.Have {
		se.Addr, se.Have = addr, true
	}
	return err
}

// chunkEnd limits a transfer so that it does not cross a 1 KiB boundary (the
// MEM-AP auto-increment guarantee) or exceed max bytes.
func chunkEnd(addr uint32, n, max int) int {
	if n > max {
		n = max
	}
	if room := int(1024 - addr&1023); n > room {
		n = room
	}
	return n
}

// ReadBlock reads len(p) bytes (a multiple of 4) at a word-aligned address.
func (c *Conn) ReadBlock(ctx context.Context, addr uint32, p []byte) error {
	if addr&3 != 0 || len(p)&3 != 0 {
		return fmt.Errorf("stlink block read needs a word-aligned address and length, got 0x%08x/%d", addr, len(p))
	}
	for off := 0; off < len(p); {
		n := chunkEnd(addr+uint32(off), len(p)-off, c.ReadChunk)
		a := addr + uint32(off)
		var buf []byte
		var err error
		if n == 4 {
			var v uint32
			if v, err = c.Read32(ctx, a); err == nil {
				buf = le32(v)
			}
		} else {
			cmd := append(append([]byte{cmdDebug, dbgReadMem32}, le32(a)...), byte(n), byte(n>>8), c.AP)
			if buf, err = c.xfer(ctx, "READMEM_32BIT", cmd, n, nil); err == nil {
				err = c.rwStatus(ctx, "READMEM_32BIT")
				err = withAddr(err, a)
			}
		}
		if err != nil {
			return err
		}
		copy(p[off:], buf)
		off += n
	}
	return nil
}

// WriteBlock writes data (a multiple of 4 bytes) at a word-aligned address.
func (c *Conn) WriteBlock(ctx context.Context, addr uint32, data []byte) error {
	if addr&3 != 0 || len(data)&3 != 0 {
		return fmt.Errorf("stlink block write needs a word-aligned address and length, got 0x%08x/%d", addr, len(data))
	}
	for off := 0; off < len(data); {
		n := chunkEnd(addr+uint32(off), len(data)-off, c.WriteChunk)
		a := addr + uint32(off)
		var err error
		if n == 4 {
			err = c.Write32(ctx, a, rd32(data[off:]))
		} else {
			cmd := append(append([]byte{cmdDebug, dbgWriteMem32}, le32(a)...), byte(n), byte(n>>8), c.AP)
			if _, err = c.xfer(ctx, "WRITEMEM_32BIT", cmd, 0, data[off:off+n]); err == nil {
				err = withAddr(c.rwStatus(ctx, "WRITEMEM_32BIT"), a)
			}
		}
		if err != nil {
			return err
		}
		off += n
	}
	return nil
}

// WriteBytes writes 1..N bytes with the native 8-bit command (no neighbouring
// bytes are touched).
func (c *Conn) WriteBytes(ctx context.Context, addr uint32, data []byte) error {
	for off := 0; off < len(data); {
		n := len(data) - off
		if n > write8Chunk {
			n = write8Chunk
		}
		a := addr + uint32(off)
		cmd := append(append([]byte{cmdDebug, dbgWriteMem8}, le32(a)...), byte(n), byte(n>>8), c.AP)
		if _, err := c.xfer(ctx, "WRITEMEM_8BIT", cmd, 0, data[off:off+n]); err != nil {
			return err
		}
		if err := withAddr(c.rwStatus(ctx, "WRITEMEM_8BIT"), a); err != nil {
			return err
		}
		off += n
	}
	return nil
}

// ReadCoreReg reads one core register with READREG.
func (c *Conn) ReadCoreReg(ctx context.Context, id cortexm.RegisterID) (uint32, error) {
	r, err := c.dbg(ctx, "READREG", 8, dbgReadReg, byte(id))
	if err != nil {
		return 0, err
	}
	return rd32(r[4:]), nil
}

// WriteCoreReg writes one core register with WRITEREG.
func (c *Conn) WriteCoreReg(ctx context.Context, id cortexm.RegisterID, v uint32) error {
	_, err := c.dbg(ctx, "WRITEREG", 2, append([]byte{dbgWriteReg, byte(id)}, le32(v)...)...)
	return err
}

// ReadCoreRegs reads r0-r12, sp, lr, pc, xpsr, msp, psp with one command.
func (c *Conn) ReadCoreRegs(ctx context.Context) ([]uint32, error) {
	r, err := c.dbg(ctx, "READALLREGS", 88, dbgReadAllRegs)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, 19)
	for i := range out {
		out[i] = rd32(r[4+4*i:])
	}
	return out, nil
}

// Describe names the transport for diagnostics.
func (c *Conn) Describe() string {
	return fmt.Sprintf("ST-Link %s %s OUT 0x%02x IN 0x%02x", c.Ver.Generation, c.Ver.Name, c.out, c.in)
}

// RawRWStatus returns the 12-byte GETLASTRWSTATUS2 reply (diagnostics).
func (c *Conn) RawRWStatus(ctx context.Context) ([]byte, error) {
	return c.xfer(ctx, "GETLASTRWSTATUS2", []byte{cmdDebug, dbgLastRW2}, 12, nil)
}

var (
	_ cortexm.Bus        = (*Conn)(nil)
	_ cortexm.BlockBus   = (*Conn)(nil)
	_ cortexm.ByteWriter = (*Conn)(nil)
	_ cortexm.NativeRegs = (*Conn)(nil)
)
