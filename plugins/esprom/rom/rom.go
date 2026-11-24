// Package rom is a native client of the Espressif serial bootloader protocol
// spoken by the ROM download mode of ESP32-series chips and by the
// esp-flasher-stub RAM loader (third_party/esp-flasher-stub).
//
// It was written from Espressif's public "Serial Protocol" documentation
// (docs.espressif.com/projects/esptool, advanced-topics/serial-protocol),
// the chip technical reference manuals and ESP-IDF register headers
// (Apache-2.0), and the esp-flasher-stub sources (Apache-2.0 OR MIT) for the
// stub's behaviour. No esptool (GPL) code was copied or translated.
//
// Wire format: SLIP frames. A request is [0x00][cmd][size u16][checksum u32]
// [data]; a response is [0x01][cmd][size u16][value u32][data][status]. All
// numbers are little-endian. The checksum (0xEF XOR every payload byte) is
// only used by MEM_DATA, FLASH_DATA and FLASH_DEFL_DATA. The ROM appends 4
// status bytes (status, error, 2 reserved), the stub 2 (a 16-bit code sent
// high byte first, 0 = success).
package rom

import (
	"context"
	"crypto/md5" //nolint:gosec // SPI_FLASH_MD5 and READ_FLASH digests are MD5 by protocol
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// Command codes.
const (
	CmdFlashBegin      = 0x02
	CmdFlashData       = 0x03
	CmdFlashEnd        = 0x04
	CmdMemBegin        = 0x05
	CmdMemEnd          = 0x06
	CmdMemData         = 0x07
	CmdSync            = 0x08
	CmdWriteReg        = 0x09
	CmdReadReg         = 0x0A
	CmdSPISetParams    = 0x0B
	CmdSPIAttach       = 0x0D
	CmdChangeBaud      = 0x0F
	CmdFlashDeflBegin  = 0x10
	CmdFlashDeflData   = 0x11
	CmdFlashDeflEnd    = 0x12
	CmdSPIFlashMD5     = 0x13
	CmdGetSecurityInfo = 0x14
	// Stub only.
	CmdEraseFlash  = 0xD0
	CmdEraseRegion = 0xD1
	CmdReadFlash   = 0xD2
	CmdRunUserCode = 0xD3
)

// Sizes of the protocol.
const (
	// RAMBlock is the MEM_DATA payload size used for the stub upload.
	RAMBlock = 0x1800
	// StubFlashBlock is the FLASH_DATA payload size for the stub (its frame
	// buffer holds 0x4000 bytes of data).
	StubFlashBlock = 0x4000
	// ReadPacket is the READ_FLASH packet size (the stub's maximum).
	ReadPacket = 0x1000
	// SectorSize is the SPI flash erase sector.
	SectorSize = 0x1000
)

// Errors.
var (
	ErrTimeout  = errors.New("no answer from the ESP serial bootloader")
	ErrProtocol = errors.New("malformed answer from the ESP serial bootloader")
	ErrDigest   = errors.New("MD5 mismatch")
	ErrNoStub   = errors.New("this operation needs the flasher stub running")
)

// StatusError is a failure reported in a response's status bytes.
type StatusError struct {
	Cmd    byte
	Status byte
	Code   byte
	Stub   bool
}

func (e *StatusError) Error() string {
	if e.Stub {
		return fmt.Sprintf("command %s: stub error 0x%02X%02X (%s)", CmdName(e.Cmd), e.Status, e.Code, e.Name())
	}
	return fmt.Sprintf("command %s: ROM error 0x%02X (%s)", CmdName(e.Cmd), e.Code, e.Name())
}

var romErrors = map[byte]string{
	0x00: "undefined error", 0x01: "invalid input parameter", 0x02: "out of memory", 0x03: "failed to send message",
	0x04: "failed to receive message", 0x05: "invalid message format (unsupported command?)", 0x06: "message ok but the operation failed",
	0x07: "checksum error", 0x08: "flash write error (read-back CRC differs)", 0x09: "flash read error", 0x0A: "flash read length error",
	0x0B: "deflate error", 0x0C: "deflate Adler32 error", 0x0D: "deflate parameter error", 0x0E: "invalid RAM binary size",
	0x0F: "invalid RAM binary address", 0x64: "invalid parameter", 0x65: "invalid format", 0x66: "description too long",
	0x67: "bad encoding description", 0x69: "insufficient storage",
}

// stub codes, high byte (esp-flasher-stub commands.h esp_response_code).
var stubErrors = map[byte]string{
	0xC0: "bad data length", 0xC1: "bad data checksum", 0xC2: "bad block size", 0xC3: "invalid command",
	0xC4: "SPI flash operation failed", 0xC5: "SPI unlock failed", 0xC6: "not in flash mode", 0xC7: "inflate error",
	0xC8: "not enough data", 0xC9: "too much data", 0xCA: "NAND program failed", 0xCB: "NAND erase failed", 0xFF: "command not implemented",
}

// Name describes the error code.
func (e *StatusError) Name() string {
	if e.Stub {
		hi := e.Status
		if hi == 1 { // legacy form: status 1, code in the error byte
			hi = e.Code
		}
		if n, ok := stubErrors[hi]; ok {
			return n
		}
		return "undocumented"
	}
	if n, ok := romErrors[e.Code]; ok {
		return n
	}
	return "undocumented"
}

// Unsupported reports an error that means the loader does not know the command.
func (e *StatusError) Unsupported() bool {
	if e.Stub {
		return e.Status == 0xFF || e.Status == 0xC3 || (e.Status == 1 && (e.Code == 0xFF || e.Code == 0xC3))
	}
	return e.Code == 0x05
}

var cmdNames = map[byte]string{
	CmdFlashBegin: "FLASH_BEGIN", CmdFlashData: "FLASH_DATA", CmdFlashEnd: "FLASH_END", CmdMemBegin: "MEM_BEGIN",
	CmdMemEnd: "MEM_END", CmdMemData: "MEM_DATA", CmdSync: "SYNC", CmdWriteReg: "WRITE_REG", CmdReadReg: "READ_REG",
	CmdSPISetParams: "SPI_SET_PARAMS", CmdSPIAttach: "SPI_ATTACH", CmdChangeBaud: "CHANGE_BAUDRATE",
	CmdFlashDeflBegin: "FLASH_DEFL_BEGIN", CmdFlashDeflData: "FLASH_DEFL_DATA", CmdFlashDeflEnd: "FLASH_DEFL_END",
	CmdSPIFlashMD5: "SPI_FLASH_MD5", CmdGetSecurityInfo: "GET_SECURITY_INFO", CmdEraseFlash: "ERASE_FLASH",
	CmdEraseRegion: "ERASE_REGION", CmdReadFlash: "READ_FLASH", CmdRunUserCode: "RUN_USER_CODE",
}

// CmdName names a command code.
func CmdName(c byte) string {
	if n, ok := cmdNames[c]; ok {
		return n
	}
	return fmt.Sprintf("0x%02X", c)
}

// Link is the serial line.
type Link interface {
	io.ReadWriter
	// SetReadTimeout bounds each Read; a timed-out Read returns 0, nil.
	SetReadTimeout(d time.Duration) error
	// ResetInput discards bytes received and not read.
	ResetInput() error
}

// Stats counts link events.
type Stats struct {
	Commands int
	// Retries counts repeated commands and re-issued reads.
	Retries int
	// Stale counts frames that did not answer the pending command.
	Stale        int
	SyncAttempts int
}

// Client talks to the ROM loader or the stub on one link.
type Client struct {
	l   Link
	ctx context.Context
	dec SlipDecoder
	q   [][]byte
	buf []byte
	// Timeout bounds one ordinary command; long commands scale it.
	Timeout time.Duration
	// Baud is the current line rate (bounds FLASH_DATA answers); 0 = 115200.
	Baud int
	stub bool
	// statusLen is 4 for the ROM, 2 for the stub; learned from SYNC.
	statusLen int
	stats     Stats
	// Trace, when set, sees every frame sent (out=true) and received.
	Trace func(out bool, frame []byte)
}

// New returns a client on l that expects the ROM loader.
func New(l Link) *Client {
	return &Client{l: l, ctx: context.Background(), Timeout: 3 * time.Second, statusLen: 4, buf: make([]byte, 8192)}
}

// SetContext bounds every wait by ctx.
func (c *Client) SetContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ctx = ctx
}

// Stub reports whether the flasher stub answers.
func (c *Client) Stub() bool { return c.stub }

// Stats returns the link counters.
func (c *Client) Stats() Stats { return c.stats }

// Junk returns how many bytes arrived outside frames, and the last of them.
func (c *Client) Junk() (int, []byte) { return c.dec.Junk, c.dec.JunkText() }

// Flush discards buffered input and any partial frame.
func (c *Client) Flush() error {
	c.q = nil
	c.dec.Reset()
	return c.l.ResetInput()
}

func (c *Client) send(frame []byte) error {
	if c.Trace != nil {
		c.Trace(true, frame)
	}
	_, err := c.l.Write(SlipEncode(frame))
	return err
}

// readFrame returns the next frame received before deadline.
func (c *Client) readFrame(deadline time.Time) ([]byte, error) {
	for {
		if len(c.q) > 0 {
			f := c.q[0]
			c.q = c.q[1:]
			if c.Trace != nil {
				c.Trace(false, f)
			}
			return f, nil
		}
		if err := c.ctx.Err(); err != nil {
			return nil, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, ErrTimeout
		}
		if err := c.l.SetReadTimeout(min(left, 50*time.Millisecond)); err != nil {
			return nil, err
		}
		n, err := c.l.Read(c.buf)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if n > 0 {
			c.q = append(c.q, c.dec.Feed(c.buf[:n])...)
		}
	}
}

func le32(vs ...uint32) []byte {
	b := make([]byte, 0, 4*len(vs))
	for _, v := range vs {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	return b
}

// Checksum is the 8-bit XOR checksum of a _DATA payload, seeded with 0xEF.
func Checksum(data []byte) uint32 {
	s := byte(0xEF)
	for _, b := range data {
		s ^= b
	}
	return uint32(s)
}

func request(op byte, data []byte, chk uint32) []byte {
	f := make([]byte, 8, 8+len(data))
	f[0], f[1] = 0x00, op
	binary.LittleEndian.PutUint16(f[2:], uint16(len(data)))
	binary.LittleEndian.PutUint32(f[4:], chk)
	return append(f, data...)
}

// Command sends one request and waits for its response, skipping frames
// that answer something else. It returns the value field and the data
// before the status bytes.
func (c *Client) Command(op byte, data []byte, chk uint32, timeout time.Duration) (uint32, []byte, error) {
	if len(data) > 0xFFFF {
		return 0, nil, fmt.Errorf("%s: %d bytes of data is more than a packet holds", CmdName(op), len(data))
	}
	if timeout <= 0 {
		timeout = c.Timeout
	}
	c.stats.Commands++
	if err := c.send(request(op, data, chk)); err != nil {
		return 0, nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		f, err := c.readFrame(deadline)
		if err != nil {
			if errors.Is(err, ErrTimeout) {
				return 0, nil, fmt.Errorf("%s: %w", CmdName(op), ErrTimeout)
			}
			return 0, nil, err
		}
		if len(f) < 8 || f[0] != 0x01 || f[1] != op {
			c.stats.Stale++
			continue
		}
		return c.parse(op, f)
	}
}

func (c *Client) parse(op byte, f []byte) (uint32, []byte, error) {
	size := int(binary.LittleEndian.Uint16(f[2:]))
	val := binary.LittleEndian.Uint32(f[4:])
	payload := f[8:]
	if len(payload) < size || size < c.statusLen {
		return val, nil, fmt.Errorf("%s: %w: %d data bytes, size field %d, %d status bytes expected", CmdName(op), ErrProtocol, len(payload), size, c.statusLen)
	}
	payload = payload[:size]
	st := payload[size-c.statusLen:]
	if st[0] != 0 || (c.stub && st[1] != 0) {
		return val, nil, &StatusError{Cmd: op, Status: st[0], Code: st[1], Stub: c.stub}
	}
	return val, payload[:size-c.statusLen], nil
}

// syncPayload is SYNC's data: 07 07 12 20 and 32 x 0x55, which also lets the
// ROM measure the baud rate.
func syncPayload() []byte {
	b := []byte{0x07, 0x07, 0x12, 0x20}
	for i := 0; i < 32; i++ {
		b = append(b, 0x55)
	}
	return b
}

// SyncResult tells what answered SYNC.
type SyncResult struct {
	// Stub is true when the flasher stub answered (value 0), false for the ROM.
	Stub bool
	// Attempts is the number of SYNC packets sent.
	Attempts int
}

// Sync sends SYNC until a response arrives (at most attempts times, each
// waiting wait), then swallows the remaining repeated SYNC answers and
// flushes the input. The ROM answers with value 0x20120707 and 4 status
// bytes, the stub with value 0 and 2 status bytes.
func (c *Client) Sync(attempts int, wait time.Duration) (SyncResult, error) {
	var r SyncResult
	req := request(CmdSync, syncPayload(), 0)
	for r.Attempts < attempts {
		r.Attempts++
		c.stats.SyncAttempts++
		if err := c.ctx.Err(); err != nil {
			return r, err
		}
		if err := c.send(req); err != nil {
			return r, err
		}
		deadline := time.Now().Add(wait)
		for {
			f, err := c.readFrame(deadline)
			if errors.Is(err, ErrTimeout) {
				break
			}
			if err != nil {
				return r, err
			}
			if len(f) < 10 || f[0] != 0x01 || f[1] != CmdSync {
				continue
			}
			size := int(binary.LittleEndian.Uint16(f[2:]))
			if size != 2 && size != 4 || len(f) < 8+size || f[8] != 0 {
				continue
			}
			r.Stub = binary.LittleEndian.Uint32(f[4:]) == 0
			c.stub, c.statusLen = r.Stub, size
			// Swallow the remaining answers to this and earlier SYNCs.
			drain := time.Now().Add(100 * time.Millisecond)
			for {
				if _, err := c.readFrame(drain); err != nil {
					break
				}
			}
			return r, c.Flush()
		}
	}
	return r, fmt.Errorf("SYNC: %w after %d attempts", ErrTimeout, r.Attempts)
}

// ReadReg reads a 32-bit word.
func (c *Client) ReadReg(addr uint32) (uint32, error) {
	v, _, err := c.Command(CmdReadReg, le32(addr), 0, 0)
	return v, err
}

// WriteReg writes value under mask after delayUS microseconds.
func (c *Client) WriteReg(addr, value, mask, delayUS uint32) error {
	_, _, err := c.Command(CmdWriteReg, le32(addr, value, mask, delayUS), 0, 0)
	return err
}

// SecurityInfo is the GET_SECURITY_INFO answer.
type SecurityInfo struct {
	Flags         uint32
	FlashCryptCnt uint8
	KeyPurposes   [7]uint8
	// ChipID and ECO are absent (HasChipID false) on the ESP32-S2.
	ChipID    uint32
	ECO       uint32
	HasChipID bool
}

// Security flag bits (the ROM's GetSecurityInfoProc layout).
var SecurityFlags = []struct {
	Bit  uint32
	Name string
}{
	{1 << 0, "SECURE_BOOT_EN"}, {1 << 1, "SECURE_BOOT_AGGRESSIVE_REVOKE"}, {1 << 2, "SECURE_DOWNLOAD_ENABLE"},
	{1 << 3, "SECURE_BOOT_KEY_REVOKE0"}, {1 << 4, "SECURE_BOOT_KEY_REVOKE1"}, {1 << 5, "SECURE_BOOT_KEY_REVOKE2"},
	{1 << 6, "SOFT_DIS_JTAG"}, {1 << 7, "HARD_DIS_JTAG"}, {1 << 8, "DIS_USB"}, {1 << 9, "DIS_DOWNLOAD_DCACHE"},
	{1 << 10, "DIS_DOWNLOAD_ICACHE"},
}

// GetSecurityInfo asks for the security flags and, on newer chips, the chip id.
func (c *Client) GetSecurityInfo() (*SecurityInfo, error) {
	_, b, err := c.Command(CmdGetSecurityInfo, nil, 0, 0)
	if err != nil {
		return nil, err
	}
	if len(b) < 12 {
		return nil, fmt.Errorf("GET_SECURITY_INFO: %w: %d bytes", ErrProtocol, len(b))
	}
	s := &SecurityInfo{Flags: binary.LittleEndian.Uint32(b), FlashCryptCnt: b[4]}
	copy(s.KeyPurposes[:], b[5:12])
	if len(b) >= 20 {
		s.ChipID, s.ECO, s.HasChipID = binary.LittleEndian.Uint32(b[12:]), binary.LittleEndian.Uint32(b[16:]), true
	}
	return s, nil
}

// SPIAttach attaches the SPI flash; cfg 0 selects the default pins (or the
// SPI pad eFuses). The ROM takes a second, legacy-mode word; the stub not.
func (c *Client) SPIAttach(cfg uint32) error {
	data := le32(cfg)
	if !c.stub {
		data = le32(cfg, 0)
	}
	_, _, err := c.Command(CmdSPIAttach, data, 0, 0)
	return err
}

// SPISetParams tells the loader the flash geometry (id 0, total size,
// 64 KiB block, 4 KiB sector, 256 B page, status mask 0xFFFF).
func (c *Client) SPISetParams(size uint32) error {
	_, _, err := c.Command(CmdSPISetParams, le32(0, size, 0x10000, SectorSize, 0x100, 0xFFFF), 0, 0)
	return err
}

// ChangeBaud asks the loader to switch its UART to baud. The answer comes
// at the old rate; the caller then switches the host side. The ROM wants 0
// as the second word, the stub the current rate.
func (c *Client) ChangeBaud(baud, current int) error {
	old := uint32(current)
	if !c.stub {
		old = 0
	}
	_, _, err := c.Command(CmdChangeBaud, le32(uint32(baud), old), 0, 0)
	return err
}

// MemBegin, MemData and MemEnd download code into RAM.
func (c *Client) MemBegin(size, blocks, blockSize, addr uint32) error {
	_, _, err := c.Command(CmdMemBegin, le32(size, blocks, blockSize, addr), 0, 0)
	return err
}

// MemData sends one RAM block.
func (c *Client) MemData(seq uint32, data []byte) error {
	_, _, err := c.Command(CmdMemData, append(le32(uint32(len(data)), seq, 0, 0), data...), Checksum(data), 0)
	return err
}

// MemEnd ends the RAM download; with execute it jumps to entry.
func (c *Client) MemEnd(execute bool, entry uint32) error {
	flag := uint32(1)
	if execute {
		flag = 0
	}
	_, _, err := c.Command(CmdMemEnd, le32(flag, entry), 0, 0)
	return err
}

// md5Timeout scales a command's bound with the bytes the chip has to read.
func (c *Client) md5Timeout(n int) time.Duration {
	return c.Timeout + time.Duration(n/(1<<20)+1)*8*time.Second
}

// FlashMD5 has the loader compute the MD5 of flash [addr, addr+n). The ROM
// answers 32 hex characters, the stub 16 raw bytes.
func (c *Client) FlashMD5(addr uint32, n int) ([16]byte, error) {
	var sum [16]byte
	_, b, err := c.Command(CmdSPIFlashMD5, le32(addr, uint32(n), 0, 0), 0, c.md5Timeout(n))
	if err != nil {
		return sum, err
	}
	switch {
	case len(b) == 16:
		copy(sum[:], b)
	case len(b) >= 32:
		if _, err := hex.Decode(sum[:], b[:32]); err != nil {
			return sum, fmt.Errorf("SPI_FLASH_MD5: %w: %q", ErrProtocol, b[:32])
		}
	default:
		return sum, fmt.Errorf("SPI_FLASH_MD5: %w: %d bytes", ErrProtocol, len(b))
	}
	return sum, nil
}

// ReadFlash streams flash [addr, addr+n) through the stub's READ_FLASH: the
// stub sends packets of ReadPacket bytes, each acknowledged with the total
// received so far, then the MD5 of what it read, which must equal the MD5 of
// what arrived. The bytes go to w only as they arrive; on any error the
// caller must treat everything written as void.
func (c *Client) ReadFlash(addr uint32, n int, w io.Writer) ([16]byte, error) {
	var sum [16]byte
	if !c.stub {
		return sum, ErrNoStub
	}
	if _, _, err := c.Command(CmdReadFlash, le32(addr, uint32(n), ReadPacket, 64), 0, 0); err != nil {
		return sum, err
	}
	h := md5.New() //nolint:gosec
	got := 0
	for got < n {
		f, err := c.readFrame(time.Now().Add(c.Timeout))
		if err != nil {
			return sum, fmt.Errorf("READ_FLASH at 0x%08X (+%d of %d): %w", addr+uint32(got), got, n, err)
		}
		want := min(ReadPacket, n-got)
		if len(f) != want {
			return sum, fmt.Errorf("READ_FLASH at 0x%08X: %w: a %d-byte packet, want %d", addr+uint32(got), ErrProtocol, len(f), want)
		}
		h.Write(f)
		if _, err := w.Write(f); err != nil {
			return sum, err
		}
		got += len(f)
		if err := c.send(le32(uint32(got))); err != nil {
			return sum, err
		}
	}
	f, err := c.readFrame(time.Now().Add(c.Timeout))
	if err != nil {
		return sum, fmt.Errorf("READ_FLASH digest: %w", err)
	}
	if len(f) != 16 {
		return sum, fmt.Errorf("READ_FLASH digest: %w: %d bytes", ErrProtocol, len(f))
	}
	copy(sum[:], h.Sum(nil))
	if !equal16(sum[:], f) {
		return sum, fmt.Errorf("READ_FLASH 0x%08X+%d: %w: received %x, stub read %x", addr, n, ErrDigest, sum, f)
	}
	return sum, nil
}

func equal16(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// AbortRead ends a READ_FLASH the host gave up on: the stub leaves its
// streaming loop when an acknowledgement is not 4 bytes long, then sends its
// digest; everything pending is discarded and the link resynchronized.
func (c *Client) AbortRead() error {
	if err := c.send([]byte{0}); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	if err := c.Flush(); err != nil {
		return err
	}
	_, err := c.Sync(5, 300*time.Millisecond)
	return err
}

// FlashBegin starts a stub flash download of size bytes at addr in blocks
// of blockSize. The stub erases the 4 KiB sectors covering the range just in
// time (whole sectors: bytes outside the range in an edge sector are lost).
func (c *Client) FlashBegin(size, blocks, blockSize, addr uint32) error {
	if !c.stub {
		return ErrNoStub
	}
	_, _, err := c.Command(CmdFlashBegin, le32(size, blocks, blockSize, addr), 0, c.blockTimeout())
	return err
}

// FlashData sends one block. The stub answers before writing it; a write
// error is reported in the answer to the NEXT command.
func (c *Client) FlashData(seq uint32, data []byte) error {
	_, _, err := c.Command(CmdFlashData, append(le32(uint32(len(data)), seq, 0, 0), data...), Checksum(data), c.blockTimeout())
	return err
}

// blockTimeout bounds one FLASH_BEGIN/FLASH_DATA answer. The stub reads a
// block only after it erased (up to four non-blank 4 KiB sectors, or one
// 64 KiB block: about 2 s at most on common SPI NOR) and programmed (64
// pages, well under 1 s) the previous one; the block itself is on the wire
// for 16 KiB x 10 bits / baud (0.36 s at 460800), counted three times.
func (c *Client) blockTimeout() time.Duration {
	baud := c.Baud
	if baud <= 0 {
		baud = 115200
	}
	wire := time.Duration(StubFlashBlock*10) * time.Second / time.Duration(baud)
	return c.Timeout + EraseAllowance + 3*wire
}

// EraseAllowance is the erase/program time the stub may spend on the
// previous block before it reads the next (a variable so tests shorten it).
var EraseAllowance = 3 * time.Second

// writeFailure is a download that stopped at a block.
type writeFailure struct {
	offset int // bytes of the download before the failed block
	err    error
}

func (e *writeFailure) Error() string { return e.err.Error() }
func (e *writeFailure) Unwrap() error { return e.err }

// retryable: no answer at all (a frame lost in either direction) or a
// checksum error (a corrupted block that the stub refused). A stub error
// such as a failed SPI operation is not retried.
func retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Stub && (se.Status == 0xC1 || (se.Status == 1 && se.Code == 0xC1))
	}
	return errors.Is(err, ErrTimeout)
}

func (c *Client) download(addr uint32, data []byte) error {
	blocks := (len(data) + StubFlashBlock - 1) / StubFlashBlock
	if err := c.FlashBegin(uint32(len(data)), uint32(blocks), StubFlashBlock, addr); err != nil {
		return &writeFailure{0, err}
	}
	for i := 0; i < blocks; i++ {
		blk := data[i*StubFlashBlock : min(len(data), (i+1)*StubFlashBlock)]
		if err := c.FlashData(uint32(i), blk); err != nil {
			return &writeFailure{i * StubFlashBlock, fmt.Errorf("block %d of %d at 0x%08X: %w", i+1, blocks, addr+uint32(i*StubFlashBlock), err)}
		}
	}
	return nil
}

// WriteFlash writes data at addr through the stub, StubFlashBlock bytes per
// FLASH_DATA; addr and len(data) must be sector aligned (the stub erases
// whole sectors). When a block gets no answer, or a checksum error, the
// link is resynchronized and the download restarts AT THAT BLOCK with a new
// FLASH_BEGIN, at most attempts times. Resending the block inside the old
// download would be unsafe: the stub ignores sequence numbers, so a block it
// did receive (only its answer was lost) would be written again at the next
// offset. The new FLASH_BEGIN erases the remaining sectors again before
// writing, so a block that did land is rewritten in place. The result must
// be verified with FlashMD5, whose answer also carries a deferred write
// error of the last block. It returns the number of restarts.
func (c *Client) WriteFlash(addr uint32, data []byte, attempts int) (int, error) {
	if addr%SectorSize != 0 || len(data)%SectorSize != 0 || len(data) == 0 {
		return 0, fmt.Errorf("write 0x%08X+%d: not sector aligned", addr, len(data))
	}
	if !c.stub {
		return 0, ErrNoStub
	}
	retries, start := 0, 0
	for {
		err := c.download(addr+uint32(start), data[start:])
		if err == nil {
			return retries, nil
		}
		var wf *writeFailure
		if !errors.As(err, &wf) || !retryable(wf.err) || retries >= attempts {
			if retries > 0 {
				return retries, fmt.Errorf("%w (after %d restart(s))", err, retries)
			}
			return retries, err
		}
		retries++
		c.stats.Retries++
		start += wf.offset
		if rerr := c.resync(); rerr != nil {
			return retries, fmt.Errorf("%w; resynchronizing for restart %d: %w", err, retries, rerr)
		}
	}
}

// resync drops pending input and waits for the stub to answer SYNC; the
// stub answers only after it finished writing a block it received.
func (c *Client) resync() error {
	if err := c.Flush(); err != nil {
		return err
	}
	_, err := c.Sync(10, 500*time.Millisecond)
	return err
}

// EraseRegion erases whole sectors through the stub.
func (c *Client) EraseRegion(addr uint32, n int) error {
	if !c.stub {
		return ErrNoStub
	}
	t := c.Timeout + time.Duration(n/SectorSize+1)*150*time.Millisecond
	_, _, err := c.Command(CmdEraseRegion, le32(addr, uint32(n)), 0, t)
	return err
}

// EraseFlash erases the whole chip through the stub.
func (c *Client) EraseFlash() error {
	if !c.stub {
		return ErrNoStub
	}
	_, _, err := c.Command(CmdEraseFlash, nil, 0, 4*time.Minute)
	return err
}
