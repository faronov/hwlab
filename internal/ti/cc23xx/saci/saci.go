// Package saci implements the CC23xx Secure Application Command Interface
// mailbox (SWCU193 chapter 8.3) as seen from a debug probe: framing, response
// sequence tags, result decoding and the documented SACI commands.
//
// It is independent of any probe. It talks to a Mailbox, which the adiv5
// layer provides on the SEC-AP, so the XDS110 and CMSIS-DAP transports share
// every line of it. Only commands the TRM documents are implemented; the
// TI-internal command ids are deliberately absent.
package saci

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SEC-AP register offsets (AP register addresses).
const (
	RegTXD   = 0x00
	RegTXCTL = 0x04
	RegRXD   = 0x08
	RegRXCTL = 0x0C
)

const (
	txFull     = 1 << 0
	txCmdStart = 1 << 1
	rxFull     = 1 << 0
)

// RXCTL status bits reported in Response.Status.
const (
	StatusAborted = 1 << 1
	StatusWorking = 1 << 2
	StatusError   = 1 << 3
)

// Command ids documented in the TRM.
type CmdID uint8

const (
	CmdNop              CmdID = 0x01
	CmdGetDieID         CmdID = 0x03
	CmdGetCCFGUserRec   CmdID = 0x04
	CmdDebugReqPwdID    CmdID = 0x05
	CmdDebugSubmitAuth  CmdID = 0x06
	CmdExitSACIHalt     CmdID = 0x07
	CmdExitSACIShutdown CmdID = 0x08
	CmdFlashEraseChip   CmdID = 0x09
	CmdFlashProgCCFG    CmdID = 0x0C
	CmdFlashProgUserRec CmdID = 0x0D
	CmdFlashProgMain    CmdID = 0x0E
	CmdFlashProgPipe    CmdID = 0x0F
	CmdFlashVerifyMain  CmdID = 0x10
	CmdFlashVerifyCCFG  CmdID = 0x11
	CmdResetDevice      CmdID = 0x14
	CmdExitSACIRun      CmdID = 0x15
	CmdModeReqTools     CmdID = 0x17
)

var cmdNames = map[CmdID]string{
	CmdNop: "NO_OPERATION", CmdGetDieID: "GET_DIE_ID", CmdGetCCFGUserRec: "GET_CCFG_USER_REC",
	CmdDebugReqPwdID: "DEBUG_REQ_PWD_ID", CmdDebugSubmitAuth: "DEBUG_SUBMIT_AUTH",
	CmdExitSACIHalt: "DEBUG_EXIT_SACI_HALT", CmdExitSACIShutdown: "DEBUG_EXIT_SACI_SHUTDOWN",
	CmdFlashEraseChip: "FLASH_ERASE_CHIP", CmdFlashProgCCFG: "FLASH_PROG_CCFG_SECTOR",
	CmdFlashProgUserRec: "FLASH_PROG_CCFG_USER_REC", CmdFlashProgMain: "FLASH_PROG_MAIN_SECTOR",
	CmdFlashProgPipe: "FLASH_PROG_MAIN_PIPELINED", CmdFlashVerifyMain: "FLASH_VERIFY_MAIN_SECTORS",
	CmdFlashVerifyCCFG: "FLASH_VERIFY_CCFG_SECTOR", CmdResetDevice: "BLDR_APP_RESET_DEVICE",
	CmdExitSACIRun: "BLDR_APP_EXIT_SACI_RUN", CmdModeReqTools: "MODE_REQ_TOOLS_CLIENT",
}

func (c CmdID) String() string {
	if n, ok := cmdNames[c]; ok {
		return n
	}
	return fmt.Sprintf("CMD_0x%02X", uint8(c))
}

// Result is the result field of a response header.
type Result uint8

// Results from the TRM. Unknown values stay visible as their number.
const (
	ResSuccess            Result = 0x00
	ResInvalidCmdID       Result = 0x80
	ResInvalidAddress     Result = 0x81
	ResInvalidSize        Result = 0x82
	ResInvalidKey         Result = 0x83
	ResFlashFSMError      Result = 0x84
	ResParamBufferOverrun Result = 0x85
	ResNotAllowed         Result = 0x86
	ResCRC32Mismatch      Result = 0x87
	ResInvalidPwd         Result = 0x88
	ResBlankCheckFailed   Result = 0x89
	ResCmdFailed          Result = 0xFF
)

func (r Result) String() string {
	switch r {
	case ResSuccess:
		return "SUCCESS"
	case ResInvalidCmdID:
		return "INVALID_CMD_ID"
	case ResInvalidAddress:
		return "INVALID_ADDRESS_PARAM"
	case ResInvalidSize:
		return "INVALID_SIZE_PARAM"
	case ResInvalidKey:
		return "INVALID_KEY_PARAM"
	case ResFlashFSMError:
		return "FLASH_FSM_ERROR"
	case ResParamBufferOverrun:
		return "PARAM_BUFFER_OVERFLOW"
	case ResNotAllowed:
		return "NOT_ALLOWED"
	case ResCRC32Mismatch:
		return "CRC32_MISMATCH"
	case ResInvalidPwd:
		return "INVALID_PWD_PARAM"
	case ResBlankCheckFailed:
		return "BLANK_CHECK_FAILED"
	case ResCmdFailed:
		return "CMD_FAILED"
	}
	return fmt.Sprintf("RESULT_0x%02X", uint8(r))
}

// FlashKey is the documented flash operation key (a magic number, not a secret).
const FlashKey uint32 = 0xB7E3A08F

// Mailbox is the probe-independent access to the SEC-AP registers.
type Mailbox interface {
	ReadReg(reg byte) (uint32, error)
	WriteReg(reg byte, v uint32) error
	// WriteWords writes data to one register back to back (the TXD FIFO).
	WriteWords(reg byte, data []uint32) error
}

// Response is one decoded response header with its data words.
type Response struct {
	Cmd    CmdID
	Seq    uint8
	Result Result
	Data   []uint32
	Status uint32 // RXCTL when the header was read
}

// ResultError is returned when a command ends with a result other than SUCCESS.
type ResultError struct {
	Cmd    CmdID
	Result Result
}

func (e *ResultError) Error() string { return fmt.Sprintf("SACI %s: %s", e.Cmd, e.Result) }

// ErrTimeout is wrapped by errors for a mailbox that did not answer in time.
var ErrTimeout = errors.New("SACI mailbox timeout")

// Client runs SACI commands. It is not safe for concurrent use.
type Client struct {
	mb  Mailbox
	seq uint8
	// Timeout bounds one mailbox wait (TXD free, response ready).
	Timeout time.Duration
	// Poll is the pause between RXCTL polls.
	Poll time.Duration
}

// New returns a client over mb.
func New(mb Mailbox) *Client {
	return &Client{mb: mb, Timeout: 2 * time.Second, Poll: time.Millisecond}
}

func (c *Client) nextSeq() uint8 {
	c.seq++
	if c.seq == 0 {
		c.seq = 1
	}
	return c.seq
}

func (c *Client) waitTX(ctx context.Context) error {
	dl := time.Now().Add(c.Timeout)
	for {
		v, err := c.mb.ReadReg(RegTXCTL)
		if err != nil {
			return err
		}
		if v&txFull == 0 {
			return nil
		}
		if err := c.pause(ctx, dl, fmt.Sprintf("TXD_FULL (TXCTL 0x%08x)", v)); err != nil {
			return err
		}
	}
}

func (c *Client) pause(ctx context.Context, dl time.Time, what string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if time.Now().After(dl) {
		return fmt.Errorf("%w waiting for %s", ErrTimeout, what)
	}
	time.Sleep(c.Poll)
	return nil
}

// Send writes one command: word0 (id, sequence tag, command specific bits
// 31:16) and payload. It returns the sequence tag the response will echo.
func (c *Client) Send(ctx context.Context, id CmdID, spec uint16, payload ...uint32) (uint8, error) {
	seq := c.nextSeq()
	return seq, c.sendWord0(ctx, uint32(id)|uint32(seq)<<8|uint32(spec)<<16, payload)
}

func (c *Client) sendWord0(ctx context.Context, w0 uint32, payload []uint32) error {
	if err := c.waitTX(ctx); err != nil {
		return err
	}
	if err := c.mb.WriteReg(RegTXCTL, txCmdStart); err != nil {
		return err
	}
	if err := c.mb.WriteReg(RegTXD, w0); err != nil {
		return err
	}
	if len(payload) == 0 {
		if err := c.mb.WriteReg(RegTXCTL, 0); err != nil {
			return err
		}
		return c.waitTX(ctx)
	}
	if err := c.waitTX(ctx); err != nil {
		return err
	}
	if err := c.mb.WriteReg(RegTXCTL, 0); err != nil {
		return err
	}
	if err := c.mb.WriteReg(RegTXD, payload[0]); err != nil {
		return err
	}
	if err := c.waitTX(ctx); err != nil {
		return err
	}
	if rest := payload[1:]; len(rest) > 0 {
		if err := c.mb.WriteWords(RegTXD, rest); err != nil {
			return err
		}
	}
	return c.waitTX(ctx)
}

func (c *Client) readWord(ctx context.Context, timeout time.Duration, what string) (uint32, uint32, error) {
	dl := time.Now().Add(timeout)
	for {
		st, err := c.mb.ReadReg(RegRXCTL)
		if err != nil {
			return 0, 0, err
		}
		if st&rxFull != 0 {
			v, err := c.mb.ReadReg(RegRXD)
			return v, st, err
		}
		if err := c.pause(ctx, dl, what); err != nil {
			return 0, st, err
		}
	}
}

// Recv reads one response header and its data words, waiting up to timeout.
func (c *Client) Recv(ctx context.Context, timeout time.Duration) (Response, error) {
	h, st, err := c.readWord(ctx, timeout, "response")
	if err != nil {
		return Response{}, err
	}
	r := Response{Cmd: CmdID(h), Seq: uint8(h >> 8), Result: Result(h >> 16), Status: st}
	for n := int(h >> 24); n > 0; n-- {
		w, _, err := c.readWord(ctx, c.Timeout, "response data")
		if err != nil {
			return r, err
		}
		r.Data = append(r.Data, w)
	}
	return r, nil
}

// Exec sends a command and waits up to timeout for its response. A result
// other than SUCCESS is returned as *ResultError together with the response.
func (c *Client) Exec(ctx context.Context, timeout time.Duration, id CmdID, spec uint16, payload ...uint32) (Response, error) {
	seq, err := c.Send(ctx, id, spec, payload...)
	if err != nil {
		return Response{}, fmt.Errorf("SACI %s send: %w", id, err)
	}
	return c.finish(ctx, timeout, id, seq)
}

func (c *Client) finish(ctx context.Context, timeout time.Duration, id CmdID, seq uint8) (Response, error) {
	r, err := c.Recv(ctx, timeout)
	if err != nil {
		return r, fmt.Errorf("SACI %s: %w", id, err)
	}
	if r.Cmd != id || r.Seq != seq {
		return r, fmt.Errorf("SACI %s: response is for %s seq %d, expected seq %d", id, r.Cmd, r.Seq, seq)
	}
	if r.Result != ResSuccess {
		return r, &ResultError{Cmd: id, Result: r.Result}
	}
	return r, nil
}

// Nop sends NO_OPERATION, which also disables the SACI inactivity timeout.
func (c *Client) Nop(ctx context.Context) error {
	_, err := c.Exec(ctx, c.Timeout, CmdNop, 0)
	return err
}

// DieID returns the 4 die id words. They identify one chip: callers must not
// log them.
func (c *Client) DieID(ctx context.Context) ([]uint32, error) {
	r, err := c.Exec(ctx, c.Timeout, CmdGetDieID, 0)
	return r.Data, err
}

// CCFGUserRecord returns the 32 user record words (128 bytes). Needs a valid
// CCFG. The content is application data: callers must not log it.
func (c *Client) CCFGUserRecord(ctx context.Context) ([]uint32, error) {
	r, err := c.Exec(ctx, c.Timeout, CmdGetCCFGUserRec, 0)
	return r.Data, err
}

// PwdRequirement is the answer of DEBUG_REQ_PWD_ID.
type PwdRequirement struct {
	Needed bool
	// PwdID are the 2 password id words when a password is needed; they
	// are an identifier, not the password.
	PwdID []uint32
}

// RequestPasswordID asks whether debug needs a password.
func (c *Client) RequestPasswordID(ctx context.Context) (PwdRequirement, error) {
	r, err := c.Exec(ctx, c.Timeout, CmdDebugReqPwdID, 0)
	if err != nil {
		return PwdRequirement{}, err
	}
	return PwdRequirement{Needed: len(r.Data) > 0, PwdID: r.Data}, nil
}

// SubmitAuth sends a debug password (3..16 words). The words are never
// logged by this package.
func (c *Client) SubmitAuth(ctx context.Context, pwd []uint32) error {
	if len(pwd) < 3 || len(pwd) > 16 {
		return fmt.Errorf("SACI password must be 3..16 words, got %d", len(pwd))
	}
	_, err := c.Exec(ctx, c.Timeout, CmdDebugSubmitAuth, uint16(len(pwd)), pwd...)
	return err
}

// ExitHalt leaves SACI so that the CPU waits in a boot ROM loop for the probe
// (BOOTSTA 0x81 or 0xC1). Not allowed after a chip erase or CCFG program in
// the same session.
func (c *Client) ExitHalt(ctx context.Context) error {
	_, err := c.Exec(ctx, c.Timeout, CmdExitSACIHalt, 0)
	return err
}

// ExitRun leaves SACI and lets the boot flow run the application.
func (c *Client) ExitRun(ctx context.Context) error {
	_, err := c.Exec(ctx, c.Timeout, CmdExitSACIRun, 0)
	return err
}

// ExitShutdown leaves SACI into shutdown.
func (c *Client) ExitShutdown(ctx context.Context) error {
	_, err := c.Exec(ctx, c.Timeout, CmdExitSACIShutdown, 0)
	return err
}

// ResetDevice resets the device. With waitDisconnect the device answers and
// then waits for the SWD disconnect; without it the device resets at once and
// re-enters SACI, and no response is read.
func (c *Client) ResetDevice(ctx context.Context, waitDisconnect bool) error {
	var spec uint16
	if waitDisconnect {
		spec = 1
	}
	if !waitDisconnect {
		_, err := c.Send(ctx, CmdResetDevice, spec)
		return err
	}
	_, err := c.Exec(ctx, c.Timeout, CmdResetDevice, spec)
	return err
}
