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

// DFU bStatus codes AN3156 gives a DfuSe meaning.
const (
	StatusOK     = 0x00
	StatusTarget = 0x01 // errTARGET: the address is not allowed
	StatusVendor = 0x0B // errVENDOR: read protection is active
)

// Errors.
var (
	// ErrRefused marks a download the guard refused; nothing was sent.
	ErrRefused = errors.New("dfu: refused by the hwlab guard")
	// ErrDevice marks a DFU error status reported by the device.
	ErrDevice = errors.New("dfu: device reported an error")
	// ErrTarget is errTARGET: the bootloader does not allow the address
	// (AN3156). It wraps ErrDevice.
	ErrTarget = fmt.Errorf("%w: errTARGET, the address is not allowed", ErrDevice)
	// ErrProtected is errVENDOR: read protection is active (AN3156). It
	// wraps ErrDevice.
	ErrProtected = fmt.Errorf("%w: errVENDOR, read protection is active", ErrDevice)
	// ErrProtocol marks an answer that does not follow DFU.
	ErrProtocol = errors.New("dfu: protocol violation")
)

// MaxXfer is the AN3156 bound of one UPLOAD or data DNLOAD; MinXfer the
// smallest one.
const (
	MinXfer = 2
	MaxXfer = 2048
)

// The bootloader's own SRAM (AN3156: it uses RAM from 0x20000000; hwlab
// reserves the first 16 KiB). No download may land there.
const (
	BootRAMLo = 0x20000000
	BootRAMHi = 0x20004000
)

// deviceErr maps a non-OK GETSTATUS to ErrTarget, ErrProtected or
// ErrDevice.
func deviceErr(what string, st Status) error {
	kind := ErrDevice
	switch st.Code {
	case StatusTarget:
		kind = ErrTarget
	case StatusVendor:
		kind = ErrProtected
	}
	return fmt.Errorf("%w (%s: %s in %s)", kind, what, StatusName(st.Code), st.State)
}

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
	ramAlt      uint8
	ramSet      bool
	ramLo       uint32
	ramHi       uint32
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
func (c *Client) CloseWriteWindow() {
	c.flashAltSet, c.winLo, c.winHi = false, 0, 0
	c.CloseRAMWindow()
}

// SetRAMWindow allows data downloads (never erases) on the given alternate
// setting inside [lo, hi), clipped so that it never reaches the
// bootloader's own RAM below BootRAMHi. lo >= hi closes it.
func (c *Client) SetRAMWindow(alt uint8, lo, hi uint32) {
	lo = max(lo, BootRAMHi)
	c.ramAlt, c.ramSet, c.ramLo, c.ramHi = alt, lo < hi, lo, hi
}

// CloseRAMWindow refuses RAM downloads again.
func (c *Client) CloseRAMWindow() { c.ramSet, c.ramLo, c.ramHi = false, 0, 0 }

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
	inRAM := func(addr uint32, n int) bool {
		return c.ramSet && c.altKnown && c.alt == c.ramAlt && addr >= c.ramLo && addr >= BootRAMHi &&
			uint64(addr)+uint64(n) <= uint64(c.ramHi)
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
	case block != 2:
		// AN3156 computes the address from wBlockNum and a transfer size it
		// defines ambiguously; hwlab sets the pointer before every block.
		return refused("data block %d: hwlab sends every data download as block 2 after an explicit set-address", block)
	case len(data) < MinXfer || len(data) > MaxXfer:
		return refused("a data download is %d..%d bytes (AN3156), not %d", MinXfer, MaxXfer, len(data))
	}
	if !c.ptrKnown {
		return refused("no address pointer was set")
	}
	addr := c.ptr
	if overlapsBootRAM(addr, len(data)) {
		return refused("data download at 0x%08x+%d touches the bootloader's own RAM 0x%08x..0x%08x", addr, len(data), uint32(BootRAMLo), uint32(BootRAMHi-1))
	}
	if !inWindow(addr, len(data)) && !inRAM(addr, len(data)) {
		return refused("data download at 0x%08x+%d (alt %d) is outside the write window %s", addr, len(data), c.alt, c.windowString())
	}
	return nil
}

func overlapsBootRAM(addr uint32, n int) bool {
	return uint64(addr) < BootRAMHi && uint64(addr)+uint64(n) > BootRAMLo
}

func (c *Client) windowString() string {
	w := "flash (closed: the secure boundary is not established)"
	if c.flashAltSet && c.winLo < c.winHi {
		w = fmt.Sprintf("flash [0x%08x, 0x%08x) on alt %d", c.winLo, c.winHi, c.flashAlt)
	}
	if c.ramSet {
		w += fmt.Sprintf(", RAM [0x%08x, 0x%08x) on alt %d", c.ramLo, c.ramHi, c.ramAlt)
	}
	return w
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

// dnload sends one DNLOAD after the guard and completes it with the
// AN3156 GETSTATUS sequence.
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
	return c.wait(ctx, bound)
}

func (c *Client) poll(st Status) time.Duration {
	return min(max(st.PollTimeout, time.Millisecond), c.MaxPoll)
}

// wait completes a DNLOAD as AN3156 describes: the first GETSTATUS starts
// the command and reports dfuDNBUSY with bwPollTimeout (figure 6 also
// accepts dfuDNLOAD-IDLE); after waiting bwPollTimeout a second GETSTATUS
// must report dfuDNLOAD-IDLE. dfuDNBUSY again is polled within the bound
// (USB DFU 1.1). An error status is cleared and returned as ErrTarget,
// ErrProtected or ErrDevice.
func (c *Client) wait(ctx context.Context, bound time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	status := func(n int) (Status, error) {
		st, err := c.GetStatus(ctx)
		if err != nil {
			return st, err
		}
		if st.Code != StatusOK || st.State == StateError {
			return st, errors.Join(deviceErr(fmt.Sprintf("GETSTATUS %d", n), st), c.ClrStatus(ctx))
		}
		return st, nil
	}
	st, err := status(1)
	if err != nil {
		return err
	}
	if st.State != StateDnBusy && st.State != StateDnloadIdle {
		return fmt.Errorf("%w: the first GETSTATUS after a download reported %s, not dfuDNBUSY", ErrProtocol, st.State)
	}
	for n := 2; ; n++ {
		if err := c.Sleep(ctx, c.poll(st)); err != nil {
			return fmt.Errorf("device still busy: %w", err)
		}
		if st, err = status(n); err != nil {
			return err
		}
		switch st.State {
		case StateDnloadIdle:
			return nil
		case StateDnBusy, StateDnloadSync:
		default:
			return fmt.Errorf("%w: GETSTATUS %d after a download reported %s, not dfuDNLOAD-IDLE", ErrProtocol, n, st.State)
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
		return nil, errors.Join(c.uploadErr(ctx, "get commands", err), c.recover(ctx))
	}
	return b, c.EnsureIdle(ctx)
}

// CommandName names a DfuSe command byte from the Get command list.
func CommandName(b byte) string {
	switch b {
	case CmdGetCommands:
		return "get-command"
	case CmdSetAddress:
		return "set-address-pointer"
	case CmdErase:
		return "erase"
	case CmdReadUnprotect:
		return "read-unprotect"
	}
	return fmt.Sprintf("0x%02x", b)
}

// uploadErr classifies a failed UPLOAD: a stall leaves the reason in
// GETSTATUS (errTARGET for an address that is not allowed, errVENDOR with
// read protection active).
func (c *Client) uploadErr(ctx context.Context, what string, err error) error {
	if !errors.Is(err, usb.ErrStall) || ctx.Err() != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	st, serr := c.GetStatus(ctx)
	if serr != nil || st.Code == StatusOK {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: %w: %w", what, deviceErr("UPLOAD", st), err)
}

// readWindow is the most read after one set-address.
const readWindow = 32 << 10

// chunk is the size of one UPLOAD or data DNLOAD.
func (c *Client) chunk() int { return min(c.TransferSize, MaxXfer) }

// Read uploads n bytes (at least MinXfer) from addr on the selected
// alternate setting.
func (c *Client) Read(ctx context.Context, addr uint32, n int) ([]byte, error) {
	if c.TransferSize < MinXfer {
		return nil, fmt.Errorf("%w: wTransferSize is %d", ErrProtocol, c.TransferSize)
	}
	if n < MinXfer {
		return nil, fmt.Errorf("%w: an upload is %d..%d bytes (AN3156), not %d", ErrProtocol, MinXfer, MaxXfer, n)
	}
	if uint64(addr)+uint64(n) > 1<<32 {
		return nil, errors.New("dfu: read passes the end of the address space")
	}
	out := make([]byte, 0, n)
	for off := 0; off < n; {
		w := min(readWindow, n-off)
		if r := n - off - w; r > 0 && r < MinXfer {
			w -= MinXfer // never leave a tail shorter than one upload
		}
		b, err := c.readOnce(ctx, addr+uint32(off), w)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
		off += w
	}
	return out, nil
}

// readOnce reads n >= MinXfer bytes. Transfers of exactly wTransferSize
// follow one set-address as blocks 2, 3, ... (the AN3156 address formula is
// unambiguous for them); anything shorter is block 2 after its own
// set-address. A 1-byte remainder is read as the last two bytes of the
// range, so nothing outside it is read.
func (c *Client) readOnce(ctx context.Context, addr uint32, n int) ([]byte, error) {
	if err := c.EnsureIdle(ctx); err != nil {
		return nil, err
	}
	out := make([]byte, 0, n)
	seq := c.chunk() == c.TransferSize
	for len(out) < n {
		rem := n - len(out)
		from, skip := addr+uint32(len(out)), 0
		if rem < MinXfer {
			from, skip = from-1, 1
		}
		// DNLOAD (set-address) needs dfuIDLE or dfuDNLOAD-IDLE and UPLOAD
		// needs dfuIDLE or dfuUPLOAD-IDLE: ABORT between them.
		if len(out) > 0 {
			if err := c.Abort(ctx); err != nil {
				return nil, err
			}
		}
		if err := c.SetAddress(ctx, from); err != nil {
			return nil, errors.Join(err, c.recover(ctx))
		}
		if err := c.Abort(ctx); err != nil {
			return nil, err
		}
		blocks := 1
		if seq && rem >= c.chunk() {
			blocks = rem / c.chunk()
		}
		for i := 0; i < blocks; i++ {
			want := min(c.chunk(), rem+skip)
			b, err := c.Port.ControlIn(ctx, typeIn, ReqUpload, uint16(2+i), c.Iface, want)
			if err != nil {
				return nil, errors.Join(c.uploadErr(ctx, fmt.Sprintf("upload 0x%08x", from), err), c.recover(ctx))
			}
			if len(b) != want {
				return nil, errors.Join(fmt.Errorf("%w: upload at 0x%08x returned %d of %d bytes", ErrDevice, from, len(b), want), c.recover(ctx))
			}
			out = append(out, b[skip:]...)
			rem -= want - skip
			from += uint32(want)
			skip = 0
		}
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

// Write downloads data (at least MinXfer bytes) at addr on the selected
// alternate setting. Flash pages must already be erased. Every block is
// sent as block 2 after its own set-address: AN3156 section 5.5 says the
// bootloader keeps the pointer of the last download, and its address
// formula is ambiguous for blocks above 2. A 1-byte remainder is written
// as the last two bytes of data.
func (c *Client) Write(ctx context.Context, addr uint32, data []byte) error {
	if c.TransferSize < MinXfer {
		return fmt.Errorf("%w: wTransferSize is %d", ErrProtocol, c.TransferSize)
	}
	if len(data) < MinXfer {
		return fmt.Errorf("%w: a download is %d..%d bytes (AN3156), not %d", ErrRefused, MinXfer, MaxXfer, len(data))
	}
	if err := c.EnsureIdle(ctx); err != nil {
		return err
	}
	for off := 0; off < len(data); {
		n := min(c.chunk(), len(data)-off)
		if n < MinXfer {
			off, n = len(data)-MinXfer, MinXfer
		}
		at := addr + uint32(off)
		if err := c.SetAddress(ctx, at); err != nil {
			return errors.Join(err, c.recover(ctx))
		}
		err := c.dnload(ctx, 2, data[off:off+n], c.CmdTimeout)
		c.ptrKnown = false
		if err != nil {
			return errors.Join(fmt.Errorf("download 0x%08x: %w", at, err), c.recover(ctx))
		}
		off += n
	}
	return c.Abort(ctx)
}

// LeaveResult is how the manifestation ended.
type LeaveResult struct {
	// Manifest is true when GETSTATUS reported dfuMANIFEST (AN3156); false
	// when the device dropped off the bus before answering it.
	Manifest bool
	// Note explains an unconfirmed leave.
	Note string
}

// Leave makes the bootloader leave DFU and jump to the vector table at
// addr (AN3156 "Leave DFU mode"): set the address pointer, a zero-length
// DNLOAD, then GETSTATUS must report dfuMANIFEST. The bootloader is not
// manifestation tolerant: it disconnects and jumps, so a USB error after
// that status is the expected outcome.
func (c *Client) Leave(ctx context.Context, addr uint32) (LeaveResult, error) {
	if err := c.EnsureIdle(ctx); err != nil {
		return LeaveResult{}, err
	}
	if err := c.SetAddress(ctx, addr); err != nil {
		return LeaveResult{}, err
	}
	if err := c.checkLeave(); err != nil {
		return LeaveResult{}, err
	}
	c.leaving = true
	defer func() { c.leaving = false }()
	// AN3156 leaves wValue open; block 2 (the pointer itself) is what
	// dfu-util sends.
	if err := c.checkDownload(2, nil); err != nil {
		return LeaveResult{}, err
	}
	c.Downloads++
	if err := c.Port.ControlOut(ctx, typeOut, ReqDnload, 2, c.Iface, nil); err != nil {
		return LeaveResult{}, fmt.Errorf("leave: the zero-length download failed: %w", err)
	}
	st, err := c.GetStatus(ctx)
	if err != nil {
		if errors.Is(err, usb.ErrDisconnected) || errors.Is(err, usb.ErrTimeout) {
			return LeaveResult{Note: "the device dropped off USB before answering the manifestation GETSTATUS (" + err.Error() + "): dfuMANIFEST was not observed"}, nil
		}
		return LeaveResult{}, fmt.Errorf("leave: GETSTATUS: %w", err)
	}
	if st.Code != StatusOK || st.State == StateError {
		return LeaveResult{}, errors.Join(deviceErr("leave", st), c.ClrStatus(ctx))
	}
	if st.State != StateManifest {
		return LeaveResult{}, fmt.Errorf("%w: leave: GETSTATUS reported %s after the zero-length download, not dfuMANIFEST", ErrProtocol, st.State)
	}
	return LeaveResult{Manifest: true}, nil
}

func (c *Client) checkLeave() error {
	if !c.altKnown || !c.ptrKnown {
		return refused("leave needs a selected alternate setting and address pointer")
	}
	return nil
}
