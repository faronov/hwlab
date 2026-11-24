package espromplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/esprom/rom"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// device is one open port and, once entered, the loader session behind it.
type device struct {
	p  *Plugin
	id string
	e  Entry
	o  options

	mu   sync.Mutex
	conn *Conn
	cl   *rom.Client
	ctx  context.Context

	// Loader session (valid while inROM).
	inROM    bool
	det      *rom.Detection
	facts    *rom.Facts
	factsErr error
	jedec    uint32
	size     int
	linkBaud int
	atomic   bool
	longHold bool
	syncs    int
	bootMsg  string
	stubNote string
	// Partition table, read once per session.
	ptRead bool
	parts  []esp.Partition
	ptErr  error
}

func (p *Plugin) newDevice(e Entry, o options) (*device, error) {
	c, err := p.open(e)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open %s (%s): %v", e.ID, e.Port, err)
	}
	return &device{p: p, id: e.ID, e: e, o: o, conn: c, ctx: context.Background(), linkBaud: romBaud}, nil
}

func (d *device) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil
	}
	err := d.conn.Close()
	d.conn = nil
	return err
}

func (d *device) lines(dtr, rts bool) (bool, error) { return d.conn.Lines(dtr, rts) }

// forget drops the loader session (after a reset or a lost link).
func (d *device) forget() {
	d.inROM, d.det, d.facts, d.factsErr, d.ptRead, d.parts, d.ptErr = false, nil, nil, nil, false, nil, nil
	d.cl = nil
}

// reopen closes the port and waits (bounded) for the same device to come
// back: a USB-Serial/JTAG chip reset can re-enumerate the CDC port.
func (d *device) reopen(ctx context.Context) error {
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn = nil
	}
	deadline := time.Now().Add(8 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e, err := d.p.find(d.id); err == nil {
			if c, err := d.p.open(e); err == nil {
				d.e.Port, d.conn, d.linkBaud = e.Port, c, romBaud
				return nil
			} else {
				last = err
			}
		} else {
			last = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return status.Errorf(codes.Unavailable, "%s did not come back after the reset: %v", d.id, last)
}

func (d *device) setBaud(b int) error {
	if d.conn.SetBaud == nil || b == d.linkBaud {
		return nil
	}
	if err := d.conn.SetBaud(b); err != nil {
		return err
	}
	d.linkBaud = b
	return nil
}

func isLinkGone(err error) bool {
	if err == nil {
		return false
	}
	var se *rom.StatusError
	if errors.As(err, &se) || errors.Is(err, rom.ErrTimeout) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// enterAndSync resets the chip into download mode (per entry) and syncs at
// the ROM rate. Classic wiring gets a second try with the long strap hold.
func (d *device) enterAndSync(ctx context.Context) error {
	if d.conn == nil {
		if err := d.reopen(ctx); err != nil {
			return err
		}
	}
	holds := []bool{false}
	if d.o.entry == rom.EntryClassic {
		holds = []bool{false, true}
	}
	var err error
	for _, long := range holds {
		if err = d.setBaud(romBaud); err != nil {
			return err
		}
		if d.o.entry.Wired() {
			if d.atomic, err = rom.EnterBootloader(ctx, d.lines, d.o.entry, long); err != nil {
				var le *rom.LineError
				if !errors.As(err, &le) || !le.AfterReset {
					return err
				}
				// The port vanished right after the reset: a USB-Serial/JTAG
				// chip reset re-enumerates it. Reopening sets (0,0), which
				// only clears the download flag.
				if err = d.reopen(ctx); err != nil {
					return err
				}
			}
		}
		d.longHold = long
		d.cl = rom.New(d.conn.Link)
		d.cl.Timeout = d.o.timeout
		d.cl.SetContext(ctx)
		var r rom.SyncResult
		r, err = d.cl.Sync(7, 100*time.Millisecond)
		if err != nil && isLinkGone(err) && d.o.entry == rom.EntryUSJ {
			// The USB-Serial/JTAG port re-enumerated after the reset.
			if err = d.reopen(ctx); err != nil {
				return err
			}
			d.cl = rom.New(d.conn.Link)
			d.cl.Timeout = d.o.timeout
			d.cl.SetContext(ctx)
			r, err = d.cl.Sync(7, 100*time.Millisecond)
		}
		d.syncs += r.Attempts
		if err == nil {
			_, junk := d.cl.Junk()
			d.bootMsg = printable(junk)
			return nil
		}
	}
	hint := "hold BOOT (GPIO0/GPIO9) while resetting the board, then retry with entry=none"
	switch d.o.entry {
	case rom.EntryClassic:
		hint = "the classic DTR/RTS sequence did not start the ROM loader: check that the board has the auto-reset circuit (RTS->EN, DTR->GPIO0), that nothing else holds the port, or put the chip into download mode by hand and use entry=none"
	case rom.EntryUSJ:
		hint = "the USB-Serial/JTAG download-mode sequence got no answer: the firmware may have disabled USB-Serial/JTAG or the download mode may be disabled by eFuse; hold BOOT while resetting and retry with entry=none"
	}
	return status.Errorf(codes.Unavailable, "%s: %v: %s", d.id, err, hint)
}

// printable keeps the text of a boot message.
func printable(b []byte) string {
	var sb strings.Builder
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		b = b[n:]
		switch {
		case r == '\n' || r == '\t':
			sb.WriteRune(r)
		case r == '\r':
		case r >= 0x20 && r != utf8.RuneError && r != 0x7F:
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}

// ensureROM brings up the loader session: entry, sync, chip detection, stub
// upload, baud switch, flash attach and identification.
func (d *device) ensureROM(ctx context.Context) error {
	if d.inROM {
		d.cl.SetContext(ctx)
		return nil
	}
	if s := tibslIdentities()[d.id]; s != "" && !d.o.allowOtherChip {
		return status.Errorf(codes.FailedPrecondition, "%s: the TI bootloader plugin identified %s behind this port; the ESP entry sequence would only reset it. Pass allow_other_chip=true if the board was changed", d.id, s)
	}
	d.syncs, d.stubNote = 0, ""
	if err := d.enterAndSync(ctx); err != nil {
		return err
	}
	det, err := d.cl.Detect()
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%s: chip detection: %v", d.id, err)
	}
	d.det = det
	switch {
	case !det.Chip.HasStub:
		d.stubNote = "no flasher stub is vendored for the " + det.Chip.Name + ": identify only"
	case d.cl.Stub():
		d.stubNote = "a flasher stub was already running"
	case !d.o.stub:
		d.stubNote = "stub=false: ROM loader only (no flash read/backup/program)"
	default:
		st, err := rom.StubFor(det.Chip.Key)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if err := d.cl.RunStub(st); err != nil {
			return status.Errorf(codes.Unavailable, "%s: %v", d.id, err)
		}
		d.stubNote = rom.StubRelease + " running"
	}
	if d.cl.Stub() && d.o.baud != 0 && d.o.baud != romBaud && d.o.entry != rom.EntryUSJ {
		if err := d.cl.ChangeBaud(d.o.baud, d.linkBaud); err != nil {
			return status.Errorf(codes.Unavailable, "%s: CHANGE_BAUDRATE %d: %v", d.id, d.o.baud, err)
		}
		if err := d.setBaud(d.o.baud); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
		_ = d.cl.Flush()
		if _, err := d.cl.ReadReg(rom.ChipDetectAddr); err != nil {
			return status.Errorf(codes.Unavailable, "%s: no answer after switching to %d Bd (%v): try a lower baud (the bridge or adapter may not support it)", d.id, d.o.baud, err)
		}
	}
	if det.Chip.SPI.Base != 0 {
		if err := d.cl.SPIAttach(0); err != nil {
			return status.Errorf(codes.Unavailable, "%s: SPI_ATTACH: %v", d.id, err)
		}
		if d.jedec, err = d.cl.FlashID(det.Chip); err != nil {
			return status.Errorf(codes.Unavailable, "%s: flash JEDEC id: %v", d.id, err)
		}
		d.size = rom.FlashSize(d.jedec)
		if d.jedec == 0 || d.jedec == 0xFFFFFF {
			d.size = 0
		}
		if d.size > 0 {
			if err := d.cl.SPISetParams(uint32(d.size)); err != nil {
				return status.Errorf(codes.Unavailable, "%s: SPI_SET_PARAMS: %v", d.id, err)
			}
		}
	}
	d.facts, d.factsErr = d.cl.ReadFacts(det)
	d.inROM = true
	id := identity{Chip: det.Chip.Name, JEDEC: rom.JEDECString(d.jedec), FlashBytes: d.size, Port: d.e.Port}
	if d.facts != nil {
		id.MAC, id.Revision = d.facts.MACString(), d.facts.Revision()
	}
	if d.e.Sim == nil || d.e.Candidate {
		d.p.cache.store(d.id, id)
	}
	return nil
}

// needStub refuses flash access without the stub and a known flash size.
func (d *device) needStub() error {
	if !d.cl.Stub() {
		return status.Errorf(codes.FailedPrecondition, "%s: flash access needs the flasher stub (%s)", d.det.Chip.Name, d.stubNote)
	}
	if d.size == 0 {
		return status.Errorf(codes.FailedPrecondition, "the flash size is unknown (JEDEC id %s): refusing flash access", rom.JEDECString(d.jedec))
	}
	return nil
}

// writable refuses modification of flash that is encrypted or under secure
// boot: plaintext writes would brick it.
func (d *device) writable() error {
	if d.facts == nil {
		return status.Errorf(codes.FailedPrecondition, "the eFuse security state could not be read (%v): modification refused", d.factsErr)
	}
	switch {
	case d.facts.FlashEncrypted:
		return status.Error(codes.FailedPrecondition, "flash encryption is enabled (eFuse): plaintext writes would brick the chip; modification refused")
	case d.facts.SecureBoot:
		return status.Error(codes.FailedPrecondition, "secure boot is enabled (eFuse): modification refused")
	}
	return nil
}

// resetTo drives the lines: mode run boots the application, bootloader
// leaves the chip in ROM download mode (checked by SYNC and detection).
func (d *device) resetTo(ctx context.Context, mode string, consoleMS int) (map[string]any, error) {
	if !d.o.entry.Wired() {
		return nil, status.Errorf(codes.FailedPrecondition, "%s: entry=%s drives no reset line: reset the board by hand", d.id, d.o.entry)
	}
	if d.conn == nil {
		if err := d.reopen(ctx); err != nil {
			return nil, err
		}
	}
	res := map[string]any{"mode": mode, "entry": string(d.o.entry), "transport": string(d.e.Kind)}
	if mode == "bootloader" {
		d.forget()
		if err := d.enterAndSync(ctx); err != nil {
			return nil, err
		}
		det, err := d.cl.Detect()
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "in download mode, but chip detection failed: %v", err)
		}
		res["chip"], res["lines_atomic"], res["sync_attempts"] = det.Chip.Name, d.atomic, float64(d.syncs)
		if d.bootMsg != "" {
			res["rom_boot_message"] = d.bootMsg
		}
		res["note"] = "the chip waits in the ROM loader (no stub loaded) until the next reset"
		d.cl = nil
		return res, nil
	}
	d.forget()
	if err := d.setBaud(romBaud); err != nil {
		return nil, err
	}
	atomic, err := rom.HardReset(ctx, d.lines, d.o.entry)
	if err != nil {
		var le *rom.LineError
		if !errors.As(err, &le) || !le.AfterReset {
			return nil, err
		}
		// The reset happened and the port re-enumerated (USB-Serial/JTAG).
		res["port_reenumerated"] = true
		if err := d.reopen(ctx); err != nil {
			return nil, err
		}
	}
	res["lines_atomic"] = atomic
	res["sequence"] = describeSteps(rom.RunSteps(d.o.entry))
	if consoleMS > 0 {
		txt, err := d.console(ctx, time.Duration(consoleMS)*time.Millisecond)
		if err != nil {
			res["console_error"] = err.Error()
		}
		res["console"], res["console_ms"] = txt, float64(consoleMS)
	}
	res["note"] = "the chip was reset into its application; the next ROM operation enters the bootloader again"
	return res, nil
}

func describeSteps(st []rom.Step) string {
	var l []string
	for _, s := range st {
		x := fmt.Sprintf("DTR=%d RTS=%d", b2i(s.DTR), b2i(s.RTS))
		if s.Hold > 0 {
			x += fmt.Sprintf(" %dms", s.Hold.Milliseconds())
		}
		l = append(l, x)
	}
	return strings.Join(l, " -> ")
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// maxConsole bounds the captured console text.
const maxConsole = 8192

// console reads what the application prints at 115200 for a while.
func (d *device) console(ctx context.Context, dur time.Duration) (string, error) {
	var out []byte
	buf := make([]byte, 1024)
	deadline := time.Now().Add(dur)
	reopened := false
	for time.Now().Before(deadline) && len(out) < maxConsole {
		if err := ctx.Err(); err != nil {
			return printable(out), err
		}
		if err := d.conn.Link.SetReadTimeout(50 * time.Millisecond); err != nil {
			return printable(out), err
		}
		n, err := d.conn.Link.Read(buf)
		if err != nil {
			if reopened || d.o.entry != rom.EntryUSJ {
				return printable(out), err
			}
			if err := d.reopen(ctx); err != nil {
				return printable(out), err
			}
			reopened = true
			continue
		}
		out = append(out, buf[:n]...)
	}
	return printable(out[:min(len(out), maxConsole)]), nil
}
