package dfu

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/internal/usb"
)

// DFU class requests.
const (
	ReqDetach    = 0
	ReqDnload    = 1
	ReqUpload    = 2
	ReqGetStatus = 3
	ReqClrStatus = 4
	ReqGetState  = 5
	ReqAbort     = 6

	typeOut = 0x21 // class, interface, host-to-device
	typeIn  = 0xA1 // class, interface, device-to-host
)

// DfuSe commands sent as DNLOAD block 0.
const (
	CmdGetCommands   = 0x00
	CmdSetAddress    = 0x21
	CmdErase         = 0x41
	CmdReadUnprotect = 0x92
)

// State is bState of GETSTATUS.
type State uint8

// DFU states.
const (
	StateAppIdle           State = 0
	StateAppDetach         State = 1
	StateIdle              State = 2
	StateDnloadSync        State = 3
	StateDnBusy            State = 4
	StateDnloadIdle        State = 5
	StateManifestSync      State = 6
	StateManifest          State = 7
	StateManifestWaitReset State = 8
	StateUploadIdle        State = 9
	StateError             State = 10
)

var stateNames = []string{"appIDLE", "appDETACH", "dfuIDLE", "dfuDNLOAD-SYNC", "dfuDNBUSY", "dfuDNLOAD-IDLE",
	"dfuMANIFEST-SYNC", "dfuMANIFEST", "dfuMANIFEST-WAIT-RESET", "dfuUPLOAD-IDLE", "dfuERROR"}

func (s State) String() string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return fmt.Sprintf("state(%d)", uint8(s))
}

var statusNames = []string{"OK", "errTARGET", "errFILE", "errWRITE", "errERASE", "errCHECK_ERASED", "errPROG", "errVERIFY",
	"errADDRESS", "errNOTDONE", "errFIRMWARE", "errVENDOR", "errUSBR", "errPOR", "errUNKNOWN", "errSTALLEDPKT"}

// StatusName names a bStatus code.
func StatusName(c uint8) string {
	if int(c) < len(statusNames) {
		return statusNames[c]
	}
	return fmt.Sprintf("status(0x%02x)", c)
}

// Status is a decoded GETSTATUS answer.
type Status struct {
	Code        uint8
	PollTimeout time.Duration
	State       State
	IString     uint8
}

// Errors.
var (
	// ErrRefused marks a download the guard refused; nothing was sent.
	ErrRefused = errors.New("dfu: refused by the hwlab guard")
	// ErrDevice marks a DFU error status reported by the device.
	ErrDevice = errors.New("dfu: device reported an error")
	// ErrProtocol marks an answer that does not follow DFU.
	ErrProtocol = errors.New("dfu: protocol violation")
)

// Client drives one claimed DFU interface. It is not safe for concurrent
// use; the plugin serialises calls per session.
type Client struct {
	Port         usb.Port
	Iface        uint16
	TransferSize int
	// CmdTimeout bounds one command (DNLOAD + status polling);
	// EraseTimeout bounds a page erase; MaxPoll caps one bwPollTimeout.
	CmdTimeout, EraseTimeout, MaxPoll time.Duration
	// Sleep is time.Sleep unless replaced (tests).
	Sleep func(context.Context, time.Duration) error

	alt      uint8
	altKnown bool
	ptr      uint32
	ptrKnown bool

	flashAlt    uint8
	flashAltSet bool
	winLo       uint32
	winHi       uint32
	leaving     bool
	// Downloads counts the DNLOAD requests actually sent.
	Downloads int
}

// New returns a client with conservative bounds.
func New(p usb.Port, iface uint16, transferSize int) *Client {
	return &Client{Port: p, Iface: iface, TransferSize: transferSize, CmdTimeout: 3 * time.Second,
		EraseTimeout: 10 * time.Second, MaxPoll: 5 * time.Second, Sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SetWriteWindow allows page erases and data downloads on the given flash
// alternate setting inside [lo, hi). lo >= hi closes the window.
func (c *Client) SetWriteWindow(flashAlt uint8, lo, hi uint32) {
	c.flashAlt, c.flashAltSet, c.winLo, c.winHi = flashAlt, true, lo, hi
}

// CloseWriteWindow refuses every erase and data download again.
func (c *Client) CloseWriteWindow() { c.flashAltSet, c.winLo, c.winHi = false, 0, 0 }

// Window reports the current write window.
func (c *Client) Window() (alt uint8, lo, hi uint32, open bool) {
	return c.flashAlt, c.winLo, c.winHi, c.flashAltSet && c.winLo < c.winHi
}

func refused(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRefused}, a...)...)
}

// checkDownload is the single choke point every DNLOAD passes.
func (c *Client) checkDownload(block uint16, data []byte) error {
	inWindow := func(addr uint32, n int) bool {
		return c.flashAltSet && c.winLo < c.winHi && c.altKnown && c.alt == c.flashAlt &&
			addr >= c.winLo && uint64(addr)+uint64(n) <= uint64(c.winHi)
	}
	switch {
	case len(data) == 0:
		if !c.leaving || block != 2 {
			return refused("a zero-length download is only sent by Leave")
		}
		return nil
	case block == 0:
		if !c.altKnown {
			return refused("no alternate setting selected")
		}
		switch {
		case data[0] == CmdSetAddress && len(data) == 5:
			return nil
		case data[0] == CmdErase && len(data) == 5:
			a := binary.LittleEndian.Uint32(data[1:])
			if !inWindow(a, 1) {
				return refused("page erase at 0x%08x is outside the write window %s", a, c.windowString())
			}
			return nil
		case data[0] == CmdErase:
			return refused("mass erase (0x41 without an address) is never sent")
		case data[0] == CmdReadUnprotect:
			return refused("read unprotect (0x92) is never sent: it mass-erases the flash")
		}
		return refused("DfuSe command 0x%02x (%d bytes) is not one hwlab sends", data[0], len(data))
	case block == 1:
		return refused("block 1 is reserved")
	}
	if !c.ptrKnown {
		return refused("no address pointer was set")
	}
	addr := uint64(c.ptr) + uint64(block-2)*uint64(c.TransferSize)
	if addr > 0xFFFFFFFF || !inWindow(uint32(addr), len(data)) {
		return refused("data download at 0x%08x+%d (alt %d) is outside the write window %s", addr, len(data), c.alt, c.windowString())
	}
	return nil
}

func (c *Client) windowString() string {
	if !c.flashAltSet || c.winLo >= c.winHi {
		return "(closed: the secure boundary is not established)"
	}
	return fmt.Sprintf("[0x%08x, 0x%08x) on alt %d", c.winLo, c.winHi, c.flashAlt)
}

// GetStatus issues DFU_GETSTATUS.
func (c *Client) GetStatus(ctx context.Context) (Status, error) {
	b, err := c.Port.ControlIn(ctx, typeIn, ReqGetStatus, 0, c.Iface, 6)
	if err != nil {
		return Status{}, err
	}
	if len(b) < 6 {
		return Status{}, fmt.Errorf("%w: GETSTATUS returned %d bytes", ErrProtocol, len(b))
	}
	pt := time.Duration(uint32(b[1])|uint32(b[2])<<8|uint32(b[3])<<16) * time.Millisecond
	return Status{Code: b[0], PollTimeout: pt, State: State(b[4]), IString: b[5]}, nil
}

// GetState issues DFU_GETSTATE.
func (c *Client) GetState(ctx context.Context) (State, error) {
	b, err := c.Port.ControlIn(ctx, typeIn, ReqGetState, 0, c.Iface, 1)
	if err != nil {
		return 0, err
	}
	if len(b) < 1 {
		return 0, fmt.Errorf("%w: GETSTATE returned nothing", ErrProtocol)
	}
	return State(b[0]), nil
}

// ClrStatus issues DFU_CLRSTATUS.
func (c *Client) ClrStatus(ctx context.Context) error {
	return c.Port.ControlOut(ctx, typeOut, ReqClrStatus, 0, c.Iface, nil)
}

// Abort issues DFU_ABORT.
func (c *Client) Abort(ctx context.Context) error {
	return c.Port.ControlOut(ctx, typeOut, ReqAbort, 0, c.Iface, nil)
}

// EnsureIdle brings the interface to dfuIDLE: CLRSTATUS from dfuERROR,
// ABORT from the idle download/upload states. Bounded.
func (c *Client) EnsureIdle(ctx context.Context) error {
	var last State
	for i := 0; i < 4; i++ {
		st, err := c.GetStatus(ctx)
		if err != nil {
			return err
		}
		last = st.State
		switch st.State {
		case StateIdle:
			return nil
		case StateError:
			err = c.ClrStatus(ctx)
		case StateDnloadIdle, StateUploadIdle:
			err = c.Abort(ctx)
		case StateDnBusy, StateDnloadSync:
			err = c.Sleep(ctx, min(max(st.PollTimeout, time.Millisecond), c.MaxPoll))
		default:
			return fmt.Errorf("%w: the interface is in %s and cannot be brought to dfuIDLE", ErrProtocol, st.State)
		}
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("%w: still %s after recovery attempts", ErrProtocol, last)
}

// SelectAlt selects an alternate setting of the DFU interface.
func (c *Client) SelectAlt(ctx context.Context, alt uint8) error {
	if c.altKnown && c.alt == alt {
		return nil
	}
	c.altKnown, c.ptrKnown = false, false
	var err error
	if as, ok := c.Port.(usb.AltSetter); ok {
		err = as.SetAltSetting(ctx, alt)
	} else {
		err = c.Port.ControlOut(ctx, 0x01, 0x0B, uint16(alt), c.Iface, nil)
	}
	if err != nil {
		return fmt.Errorf("select alternate setting %d: %w", alt, err)
	}
	c.alt, c.altKnown = alt, true
	return nil
}

// Alt reports the selected alternate setting.
func (c *Client) Alt() (uint8, bool) { return c.alt, c.altKnown }

// dnload sends one DNLOAD after the guard and waits for it to complete.
func (c *Client) dnload(ctx context.Context, block uint16, data []byte, bound time.Duration) error {
	if err := c.checkDownload(block, data); err != nil {
		return err
	}
	if len(data) > c.TransferSize {
		return fmt.Errorf("%w: %d bytes exceed wTransferSize %d", ErrProtocol, len(data), c.TransferSize)
	}
	c.Downloads++
	if err := c.Port.ControlOut(ctx, typeOut, ReqDnload, block, c.Iface, data); err != nil {
		return err
	}
	return c.wait(ctx, bound, StateDnloadIdle)
}

// wait polls GETSTATUS, honouring bwPollTimeout, until the device leaves
// the busy states; a non-OK status is cleared and returned as ErrDevice.
func (c *Client) wait(ctx context.Context, bound time.Duration, want State) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	for {
		st, err := c.GetStatus(ctx)
		if err != nil {
			return err
		}
		if st.Code != 0 || st.State == StateError {
			cerr := c.ClrStatus(ctx)
			return errors.Join(fmt.Errorf("%w: %s in %s", ErrDevice, StatusName(st.Code), st.State), cerr)
		}
		switch st.State {
		case want:
			return nil
		case StateDnBusy, StateDnloadSync:
			if err := c.Sleep(ctx, min(max(st.PollTimeout, time.Millisecond), c.MaxPoll)); err != nil {
				return fmt.Errorf("device still busy: %w", err)
			}
		default:
			return fmt.Errorf("%w: unexpected %s waiting for %s", ErrProtocol, st.State, want)
		}
	}
}

// SetAddress sets the DfuSe address pointer.
func (c *Client) SetAddress(ctx context.Context, addr uint32) error {
	c.ptrKnown = false
	cmd := []byte{CmdSetAddress, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(cmd[1:], addr)
	if err := c.dnload(ctx, 0, cmd, c.CmdTimeout); err != nil {
		return fmt.Errorf("set address 0x%08x: %w", addr, err)
	}
	c.ptr, c.ptrKnown = addr, true
	return nil
}

// ErasePage erases the page holding addr (the 5-byte form; mass erase is
// never sent).
func (c *Client) ErasePage(ctx context.Context, addr uint32) error {
	cmd := []byte{CmdErase, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(cmd[1:], addr)
	if err := c.dnload(ctx, 0, cmd, c.EraseTimeout); err != nil {
		return fmt.Errorf("erase page 0x%08x: %w", addr, err)
	}
	return c.Abort(ctx)
}

// GetCommands returns the DfuSe command list (UPLOAD block 0).
func (c *Client) GetCommands(ctx context.Context) ([]byte, error) {
	if err := c.EnsureIdle(ctx); err != nil {
		return nil, err
	}
	b, err := c.Port.ControlIn(ctx, typeIn, ReqUpload, 0, c.Iface, 16)
	if err != nil {
		return nil, err
	}
	return b, c.EnsureIdle(ctx)
}

// readWindow is the most read after one set-address.
const readWindow = 32 << 10

// Read uploads n bytes from addr on the selected alternate setting.
func (c *Client) Read(ctx context.Context, addr uint32, n int) ([]byte, error) {
	if c.TransferSize < 1 {
		return nil, fmt.Errorf("%w: wTransferSize is 0", ErrProtocol)
	}
	if uint64(addr)+uint64(n) > 1<<32 {
		return nil, errors.New("dfu: read passes the end of the address space")
	}
	out := make([]byte, 0, n)
	for off := 0; off < n; off += readWindow {
		w := min(readWindow, n-off)
		b, err := c.readOnce(ctx, addr+uint32(off), w)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

func (c *Client) readOnce(ctx context.Context, addr uint32, n int) ([]byte, error) {
	if err := c.EnsureIdle(ctx); err != nil {
		return nil, err
	}
	if err := c.SetAddress(ctx, addr); err != nil {
		return nil, err
	}
	if err := c.Abort(ctx); err != nil {
		return nil, err
	}
	out := make([]byte, 0, n)
	for blk := 2; len(out) < n; blk++ {
		want := min(c.TransferSize, n-len(out))
		b, err := c.Port.ControlIn(ctx, typeIn, ReqUpload, uint16(blk), c.Iface, want)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("upload 0x%08x: %w", addr+uint32(len(out)), err), c.recover(ctx))
		}
		if len(b) != want {
			return nil, errors.Join(fmt.Errorf("%w: upload at 0x%08x returned %d of %d bytes", ErrDevice, addr+uint32(len(out)), len(b), want), c.recover(ctx))
		}
		out = append(out, b...)
	}
	return out, c.Abort(ctx)
}

// recover tries to leave the interface in dfuIDLE after a failure.
func (c *Client) recover(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := c.EnsureIdle(rctx); err != nil {
		return fmt.Errorf("recovery to dfuIDLE: %w", err)
	}
	return nil
}

// Write downloads data at addr on the selected alternate setting. The
// target pages must already be erased.
func (c *Client) Write(ctx context.Context, addr uint32, data []byte) error {
	if c.TransferSize < 1 {
		return fmt.Errorf("%w: wTransferSize is 0", ErrProtocol)
	}
	if err := c.EnsureIdle(ctx); err != nil {
		return err
	}
	// Keep each window's block numbers small and the pointer explicit.
	win := c.TransferSize * 32
	for off := 0; off < len(data); off += win {
		chunk := data[off:min(off+win, len(data))]
		if err := c.SetAddress(ctx, addr+uint32(off)); err != nil {
			return errors.Join(err, c.recover(ctx))
		}
		for i, blk := 0, uint16(2); i < len(chunk); i, blk = i+c.TransferSize, blk+1 {
			b := chunk[i:min(i+c.TransferSize, len(chunk))]
			if err := c.dnload(ctx, blk, b, c.CmdTimeout); err != nil {
				return errors.Join(fmt.Errorf("download 0x%08x: %w", addr+uint32(off+i), err), c.recover(ctx))
			}
		}
		if err := c.Abort(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Leave makes the bootloader leave DFU and jump to addr: set the address
// pointer, a zero-length DNLOAD, then GETSTATUS starts the manifestation.
// The device drops off the bus; a disconnect at that point is success.
func (c *Client) Leave(ctx context.Context, addr uint32) error {
	if err := c.EnsureIdle(ctx); err != nil {
		return err
	}
	if err := c.SetAddress(ctx, addr); err != nil {
		return err
	}
	if err := c.checkLeave(); err != nil {
		return err
	}
	c.leaving = true
	defer func() { c.leaving = false }()
	if err := c.checkDownload(2, nil); err != nil {
		return err
	}
	c.Downloads++
	if err := c.Port.ControlOut(ctx, typeOut, ReqDnload, 2, c.Iface, nil); err != nil {
		if gone(err) {
			return nil
		}
		return err
	}
	st, err := c.GetStatus(ctx)
	if err != nil {
		if gone(err) {
			return nil
		}
		return err
	}
	if st.Code != 0 {
		return errors.Join(fmt.Errorf("%w: leave: %s in %s", ErrDevice, StatusName(st.Code), st.State), c.ClrStatus(ctx))
	}
	if st.State != StateManifest && st.State != StateManifestSync && st.State != StateManifestWaitReset && st.State != StateDnBusy {
		return fmt.Errorf("%w: leave: unexpected %s after the zero-length download", ErrProtocol, st.State)
	}
	return nil
}

func (c *Client) checkLeave() error {
	if !c.altKnown || !c.ptrKnown {
		return refused("leave needs a selected alternate setting and address pointer")
	}
	return nil
}

func gone(err error) bool {
	return errors.Is(err, usb.ErrDisconnected) || errors.Is(err, usb.ErrStall) || errors.Is(err, usb.ErrTimeout)
}
