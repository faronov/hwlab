// Package isp speaks the Bouffalo Lab BL702 boot-ROM ISP protocol and the
// command set of the vendor eflash_loader helper that the ROM loads into RAM.
// It works over a USB CDC port (the ROM's own USB device, VID:PID ffff:ffff,
// entered by holding BOOT at power-up) or a UART.
//
// Frames are [cmd, checksum, len_lo, len_hi, data...] with checksum the low
// byte of len_lo+len_hi+sum(data). Answers are "OK" (followed by a 2-byte
// length and data for commands that return data), "PD" (pending, erase) or
// "FL" with a 2-byte error code.
//
// Three behaviours of the chip shape this package (all measured on a BL702
// over its native USB, see docs/notes/bflb-bl702-usb-isp-results.md):
//   - A handshake is a short run of 0x55 bytes. The reference tool sends
//     0.003*baud/10 of them (600 at 2 Mbaud); over USB they arrive as one
//     packet and the excess corrupts the next command (FL0103). 12 suffice.
//   - Bytes other than 0x55 are ignored while the chip waits for a handshake,
//     but 0x55 bytes sent once it is in command mode are parsed as a frame
//     and desynchronise it. Connect therefore probes with get_boot_info first
//     and only handshakes when nothing answers.
//   - The helper's USB receive path loses frames that need a full 64-byte USB
//     packet (anything of 64 bytes or more): no answer, and later frames may
//     be lost too until the port is reopened. The ROM is not affected. Over
//     USB, flash writes therefore go in USBWriteChunk pieces (63-byte frames).
package isp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"
)

// Link is the serial line to the chip.
type Link interface {
	io.ReadWriter
	// SetReadTimeout bounds each Read; a timed-out Read returns 0, nil.
	SetReadTimeout(d time.Duration) error
	// ResetInput discards received bytes not read yet.
	ResetInput() error
}

// ROM commands.
const (
	cmdGetBootInfo    = 0x10
	cmdLoadBootHeader = 0x11
	cmdLoadSegHeader  = 0x17
	cmdLoadSegData    = 0x18
	cmdCheckImage     = 0x19
	cmdMemWrite       = 0x50
)

// Helper (eflash_loader) commands.
const (
	cmdReset      = 0x21
	cmdErase      = 0x30
	cmdWrite      = 0x31
	cmdRead       = 0x32
	cmdReadJEDEC  = 0x36
	cmdWriteCheck = 0x3a
	cmdReadSHA    = 0x3d
	cmdReadMAC    = 0x42
)

const (
	// DefaultSyncBytes is the handshake length used over USB CDC.
	DefaultSyncBytes = 12
	// SegChunk is the largest load_seg_data payload the ROM accepts.
	SegChunk = 4080
	// IOChunk is the largest flash read/write payload of the helper.
	IOChunk = 2048
	// USBWriteChunk keeps a write frame (8 bytes of header and address plus
	// data) below one full USB packet.
	USBWriteChunk = 55
	// Sector is the flash erase granule.
	Sector = 4096

	probeTimeout     = 300 * time.Millisecond
	handshakeTimeout = 300 * time.Millisecond
	settle           = 30 * time.Millisecond
	drainTimeout     = 20 * time.Millisecond
	helperStart      = 150 * time.Millisecond
	transientRetries = 2
)

// SyncBytesFor is the handshake length for a real UART at baud: what the
// reference tool sends (3 ms of 0x55 for the ROM's auto-baud), at least
// DefaultSyncBytes.
func SyncBytesFor(baud int) int {
	n := baud * 3 / 10000
	if n < DefaultSyncBytes {
		n = DefaultSyncBytes
	}
	return n
}

// Stats counts link recoveries of a client.
type Stats struct {
	Handshakes int
	Retries    int
}

// Client talks to one chip. It is not safe for concurrent use.
type Client struct {
	link      Link
	ctx       context.Context
	SyncBytes int
	// Timeout bounds every answer except erase.
	Timeout time.Duration
	// EraseTimeout bounds an erase (the helper keeps sending PD meanwhile).
	EraseTimeout time.Duration
	// WriteChunk is the flash write payload per frame; 0 means IOChunk.
	WriteChunk int
	// Reopen, when set, reopens the port if it failed after the helper
	// started (a USB device that re-enumerates).
	Reopen func() (Link, error)
	stats  Stats
}

// New returns a client with the USB CDC defaults.
func New(l Link) *Client {
	return &Client{link: l, ctx: context.Background(), SyncBytes: DefaultSyncBytes, Timeout: 2 * time.Second, EraseTimeout: 60 * time.Second}
}

// SetContext makes long operations cancellable.
func (c *Client) SetContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ctx = ctx
}

// Stats returns the cumulative recoveries.
func (c *Client) Stats() Stats { return c.stats }

func (c *Client) readFull(n int, timeout time.Duration) ([]byte, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	deadline := time.Now().Add(timeout)
	step := timeout
	if step > 100*time.Millisecond {
		step = 100 * time.Millisecond
	}
	if err := c.link.SetReadTimeout(step); err != nil {
		return nil, err
	}
	for len(out) < n {
		if err := c.ctx.Err(); err != nil {
			return out, err
		}
		k, err := c.link.Read(buf[:n-len(out)])
		out = append(out, buf[:k]...)
		if err != nil {
			return out, err
		}
		if k == 0 && time.Now().After(deadline) {
			if len(out) == 0 {
				return out, ErrNoAnswer
			}
			return out, fmt.Errorf("answer truncated after %d of %d bytes", len(out), n)
		}
	}
	return out, nil
}

// drain discards whatever the chip still sends.
func (c *Client) drain() {
	_ = c.link.SetReadTimeout(drainTimeout)
	buf := make([]byte, 256)
	for i := 0; i < 64; i++ {
		if k, err := c.link.Read(buf); err != nil || k == 0 {
			return
		}
	}
}

func frame(cmd byte, data []byte) []byte {
	n := len(data)
	sum := n&0xff + n>>8
	for _, b := range data {
		sum += int(b)
	}
	return append([]byte{cmd, byte(sum), byte(n), byte(n >> 8)}, data...)
}

// status reads a 2-byte answer status; for FL also the error code.
func (c *Client) status(cmd byte, timeout time.Duration) (string, error) {
	st, err := c.readFull(2, timeout)
	if err != nil {
		return "", fmt.Errorf("command 0x%02x: %w", cmd, err)
	}
	switch string(st) {
	case "OK", "PD":
		return string(st), nil
	case "FL":
		code, err := c.readFull(2, timeout)
		if err != nil {
			return "", fmt.Errorf("command 0x%02x: FL without an error code: %w", cmd, err)
		}
		return "", &ChipError{Cmd: cmd, Code: binary.LittleEndian.Uint16(code)}
	}
	return "", fmt.Errorf("command 0x%02x: unexpected answer % x", cmd, st)
}

// exec sends one command and reads its answer. withData reads the
// length-prefixed payload after OK. Framing errors are retried.
func (c *Client) exec(cmd byte, data []byte, withData bool, timeout time.Duration) ([]byte, error) {
	f := frame(cmd, data)
	for attempt := 0; ; attempt++ {
		if err := c.ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := c.link.Write(f); err != nil {
			return nil, fmt.Errorf("command 0x%02x: write: %w", cmd, err)
		}
		st, err := c.status(cmd, timeout)
		var ce *ChipError
		if errors.As(err, &ce) && ce.Transient() && attempt < transientRetries {
			c.stats.Retries++
			c.drain()
			continue
		}
		if err != nil {
			return nil, err
		}
		if st != "OK" {
			return nil, fmt.Errorf("command 0x%02x: unexpected %s", cmd, st)
		}
		if !withData {
			return nil, nil
		}
		hdr, err := c.readFull(2, timeout)
		if err != nil {
			return nil, fmt.Errorf("command 0x%02x: length: %w", cmd, err)
		}
		n := int(binary.LittleEndian.Uint16(hdr))
		if n == 0 {
			return []byte{}, nil
		}
		out, err := c.readFull(n, timeout)
		if err != nil {
			return nil, fmt.Errorf("command 0x%02x: data: %w", cmd, err)
		}
		return out, nil
	}
}

// Handshake sends the 0x55 sync and waits for OK. Only use it when the chip
// waits for a handshake (see the package comment).
func (c *Client) Handshake() error {
	sync := bytes.Repeat([]byte{0x55}, c.SyncBytes)
	var last error = ErrNoAnswer
	for attempt := 0; attempt < 3; attempt++ {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		_ = c.link.ResetInput()
		if _, err := c.link.Write(sync); err != nil {
			return fmt.Errorf("handshake: write: %w", err)
		}
		got, err := c.readUntilOK(handshakeTimeout)
		if err == nil {
			c.stats.Handshakes++
			time.Sleep(settle)
			c.drain()
			return nil
		}
		last = fmt.Errorf("%w (received % x)", err, got)
	}
	return fmt.Errorf("handshake: %w", last)
}

func (c *Client) readUntilOK(timeout time.Duration) ([]byte, error) {
	var got []byte
	buf := make([]byte, 64)
	_ = c.link.SetReadTimeout(20 * time.Millisecond)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		k, err := c.link.Read(buf)
		if err != nil {
			return got, err
		}
		got = append(got, buf[:k]...)
		if bytes.Contains(got, []byte("OK")) {
			return got, nil
		}
	}
	return got, ErrNoAnswer
}

// BootInfo is the answer to get_boot_info.
type BootInfo struct {
	Raw []byte
	// ROMVersion is 0xffffffff when the eflash_loader helper answers instead
	// of the ROM.
	ROMVersion uint32
	Sign       byte
	Encrypt    byte
	ChipID     []byte // ROM only: bytes 16..24
}

// HelperRunning reports whether the helper, not the ROM, answered.
func (b BootInfo) HelperRunning() bool { return b.ROMVersion == 0xffffffff }

func parseBootInfo(b []byte) (BootInfo, error) {
	if len(b) < 8 {
		return BootInfo{}, fmt.Errorf("boot info is %d bytes", len(b))
	}
	bi := BootInfo{Raw: b, ROMVersion: binary.LittleEndian.Uint32(b), Sign: b[4], Encrypt: b[5]}
	if !bi.HelperRunning() && len(b) >= 24 {
		bi.ChipID = b[16:24]
	}
	return bi, nil
}

// BootInfo reads get_boot_info (ROM and helper both answer it).
func (c *Client) BootInfo() (BootInfo, error) {
	b, err := c.exec(cmdGetBootInfo, nil, true, c.Timeout)
	if err != nil {
		return BootInfo{}, err
	}
	return parseBootInfo(b)
}

// Connect brings the chip into command mode without desynchronising it: it
// first probes with get_boot_info (ignored while the chip waits for a
// handshake) and handshakes only if nothing answered.
func (c *Client) Connect() (BootInfo, error) {
	_ = c.link.ResetInput()
	b, err := c.exec(cmdGetBootInfo, nil, true, probeTimeout)
	if err == nil {
		return parseBootInfo(b)
	}
	if !errors.Is(err, ErrNoAnswer) {
		// In command mode but out of step: let a partial frame time out.
		time.Sleep(probeTimeout)
		c.drain()
		if b, err2 := c.exec(cmdGetBootInfo, nil, true, probeTimeout); err2 == nil {
			return parseBootInfo(b)
		}
	}
	if err := c.Handshake(); err != nil {
		return BootInfo{}, fmt.Errorf("the chip does not answer as a Bouffalo boot ROM (is it in ISP mode: BOOT held while powering up?): %w", err)
	}
	return c.BootInfo()
}

// runHelperPredata starts a checked RAM image on the BL702: the reference
// tool's replacement for run_image. Four memory writes: the HBN magic
// "EHBN" at 0x4000f100, the entry 0x22010000 at 0x4000f104, then
// 0x40000018 = 0 and = 2 (software CPU reset into the loaded image).
var runHelperPredata = []byte{
	0x50, 0x00, 0x08, 0x00, 0x00, 0xf1, 0x00, 0x40, 0x45, 0x48, 0x42, 0x4e,
	0x50, 0x00, 0x08, 0x00, 0x04, 0xf1, 0x00, 0x40, 0x00, 0x00, 0x01, 0x22,
	0x50, 0x00, 0x08, 0x00, 0x18, 0x00, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00,
	0x50, 0x00, 0x08, 0x00, 0x18, 0x00, 0x00, 0x40, 0x02, 0x00, 0x00, 0x00,
}

// HelperEntry is the RAM address the BL702 run sequence jumps to.
const HelperEntry = 0x22010000

// LoadHelper loads a RAM boot image (the eflash_loader) through the ROM,
// starts it and handshakes with it. bi is the ROM's boot info from Connect.
func (c *Client) LoadHelper(img *Image, bi BootInfo) (BootInfo, error) {
	if bi.HelperRunning() {
		return bi, nil
	}
	if bi.Sign != 0 || bi.Encrypt != 0 {
		return BootInfo{}, fmt.Errorf("the chip requires signed (%d) or encrypted (%d) images; loading the unsigned helper is not supported", bi.Sign, bi.Encrypt)
	}
	if len(img.Segments) == 0 || img.Segments[0].Dest != HelperEntry {
		return BootInfo{}, fmt.Errorf("helper image does not start at 0x%08x", HelperEntry)
	}
	if _, err := c.exec(cmdLoadBootHeader, img.Header, false, c.Timeout); err != nil {
		return BootInfo{}, fmt.Errorf("load boot header: %w", err)
	}
	for i, s := range img.Segments {
		h, err := c.exec(cmdLoadSegHeader, s.Header, true, c.Timeout)
		if err != nil {
			return BootInfo{}, fmt.Errorf("load segment %d header: %w", i, err)
		}
		if len(h) < 8 || int(binary.LittleEndian.Uint32(h[4:])) != len(s.Data) {
			return BootInfo{}, fmt.Errorf("segment %d: the ROM reports length % x, the image has %d bytes", i, h, len(s.Data))
		}
		for off := 0; off < len(s.Data); off += SegChunk {
			end := min(off+SegChunk, len(s.Data))
			if _, err := c.exec(cmdLoadSegData, s.Data[off:end], false, c.Timeout); err != nil {
				return BootInfo{}, fmt.Errorf("load segment %d data at %d: %w", i, off, err)
			}
		}
	}
	if _, err := c.exec(cmdCheckImage, nil, false, c.Timeout); err != nil {
		return BootInfo{}, fmt.Errorf("check image: %w", err)
	}
	if _, err := c.link.Write(runHelperPredata); err != nil {
		return BootInfo{}, fmt.Errorf("run helper: %w", err)
	}
	// The reference tool reads one ack; the reset may cut the rest short.
	_, _ = c.readFull(2, c.Timeout)
	time.Sleep(helperStart)
	c.drain()
	if err := c.Handshake(); err != nil {
		if c.Reopen == nil {
			return BootInfo{}, fmt.Errorf("the helper did not answer after start: %w", err)
		}
		l, rerr := c.reopen()
		if rerr != nil {
			return BootInfo{}, fmt.Errorf("the helper did not answer after start (%v) and the port could not be reopened: %w", err, rerr)
		}
		c.link = l
		if err := c.Handshake(); err != nil {
			return BootInfo{}, fmt.Errorf("the helper did not answer after start and reopen: %w", err)
		}
	}
	got, err := c.BootInfo()
	if err != nil {
		return BootInfo{}, err
	}
	if !got.HelperRunning() {
		return BootInfo{}, fmt.Errorf("after loading the helper the ROM still answers (boot info % x)", got.Raw)
	}
	return got, nil
}

func (c *Client) reopen() (Link, error) {
	var last error
	for i := 0; i < 20; i++ {
		l, err := c.Reopen()
		if err == nil {
			return l, nil
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, last
}

// MAC reads the 8-byte efuse MAC (helper only) and checks its CRC32.
func (c *Client) MAC() ([]byte, error) {
	b, err := c.exec(cmdReadMAC, nil, true, c.Timeout)
	if err != nil {
		return nil, err
	}
	if len(b) != 12 {
		return nil, fmt.Errorf("read MAC: %d bytes, want 12", len(b))
	}
	if crc32.ChecksumIEEE(b[:8]) != binary.LittleEndian.Uint32(b[8:]) {
		return nil, fmt.Errorf("read MAC: CRC mismatch (% x)", b)
	}
	return b[:8], nil
}

// JEDEC reads the flash JEDEC id (helper only) and derives the size from
// the capacity byte.
func (c *Client) JEDEC() (id uint32, size int, err error) {
	b, err := c.exec(cmdReadJEDEC, nil, true, c.Timeout)
	if err != nil {
		return 0, 0, err
	}
	if len(b) < 3 {
		return 0, 0, fmt.Errorf("read JEDEC: %d bytes", len(b))
	}
	id = uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	if b[2] < 0x10 || b[2] > 0x19 {
		return id, 0, fmt.Errorf("flash JEDEC id 0x%06x: capacity byte 0x%02x is not plausible (no flash answered?)", id, b[2])
	}
	return id, 1 << b[2], nil
}

func le32(vs ...uint32) []byte {
	out := make([]byte, 0, 4*len(vs))
	for _, v := range vs {
		out = binary.LittleEndian.AppendUint32(out, v)
	}
	return out
}

// Read reads n bytes of flash.
func (c *Client) Read(addr uint32, n int) ([]byte, error) {
	out := make([]byte, 0, n)
	for len(out) < n {
		k := min(n-len(out), IOChunk)
		a := addr + uint32(len(out))
		b, err := c.exec(cmdRead, le32(a, uint32(k)), true, c.Timeout)
		if err != nil {
			return out, fmt.Errorf("read 0x%06x: %w", a, err)
		}
		if len(b) != k {
			return out, fmt.Errorf("read 0x%06x: got %d bytes, want %d", a, len(b), k)
		}
		out = append(out, b...)
	}
	return out, nil
}

// Erase erases the sectors covering [start, end] (end inclusive).
func (c *Client) Erase(start, end uint32) error {
	if _, err := c.link.Write(frame(cmdErase, le32(start, end))); err != nil {
		return fmt.Errorf("erase: write: %w", err)
	}
	deadline := time.Now().Add(c.EraseTimeout)
	for {
		st, err := c.status(cmdErase, c.Timeout)
		if err != nil {
			return fmt.Errorf("erase 0x%06x-0x%06x: %w", start, end, err)
		}
		if st == "OK" {
			c.drain()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("erase 0x%06x-0x%06x: still pending after %s", start, end, c.EraseTimeout)
		}
	}
}

// Write programs erased flash.
func (c *Client) Write(addr uint32, data []byte) error {
	chunk := c.WriteChunk
	if chunk <= 0 || chunk > IOChunk {
		chunk = IOChunk
	}
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		a := addr + uint32(off)
		if _, err := c.exec(cmdWrite, append(le32(a), data[off:end]...), false, c.Timeout); err != nil {
			return fmt.Errorf("write 0x%06x: %w", a, err)
		}
	}
	return nil
}

// WriteCheck asks the helper whether all writes completed.
func (c *Client) WriteCheck() error {
	_, err := c.exec(cmdWriteCheck, nil, false, c.Timeout)
	return err
}

// SHA256 has the helper hash n bytes of flash.
func (c *Client) SHA256(addr uint32, n int) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	b, err := c.exec(cmdReadSHA, le32(addr, uint32(n)), true, c.Timeout+time.Duration(n/(64<<10))*100*time.Millisecond)
	if err != nil {
		return out, err
	}
	if len(b) != sha256.Size {
		return out, fmt.Errorf("sha256: %d bytes", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// Reset restarts the chip. With BOOT released it boots from flash. The
// helper may reset before its answer is sent: no answer is not an error.
func (c *Client) Reset() error {
	if _, err := c.link.Write(frame(cmdReset, nil)); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	_, err := c.status(cmdReset, 500*time.Millisecond)
	var ce *ChipError
	if errors.As(err, &ce) {
		return err
	}
	return nil
}
