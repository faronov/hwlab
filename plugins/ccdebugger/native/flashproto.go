package native

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/faronov/hwlab/plugins/ccdebugger/native/flashplan"
	"github.com/faronov/hwlab/plugins/ccdebugger/native/target"
)

// Confirmation tokens. A destructive operation must be given its exact token
// in addition to the matching permission, so a generic "yes" cannot start it.
const (
	ConfirmChipErase  = "erase-entire-chip"
	ConfirmErasePages = "erase-flash-pages"
	ConfirmProgram    = "program-flash"
	ConfirmConfig     = "write-device-config"
	ConfirmDebugLock  = "lock-debug-interface"
)

// Flash-specific failure classes, all wrapping [ErrTransport] unless noted.
var (
	// ErrTimeout: an erase or write did not finish in time; flash state is unknown.
	ErrTimeout = errors.New("flash operation timed out")
	// ErrDisconnected: the debugger vanished mid-operation; flash state is unknown.
	ErrDisconnected = errors.New("debugger disconnected")
	// ErrTargetLocked: the target's debug interface or a page is locked.
	ErrTargetLocked = errors.New("target locked")
	// ErrEraseFailed: erase completed with an error indication or left data behind.
	ErrEraseFailed = errors.New("erase failed")
	// ErrWriteFailed: a flash write was aborted or the flash controller misbehaved.
	ErrWriteFailed = errors.New("flash write failed")
	// ErrVerify: readback or CRC verification found a difference.
	ErrVerify = errors.New("verification failed")
	// errPreflight marks refusals made before any state-changing exchange.
	errPreflight = fmt.Errorf("%w: preflight", ErrPolicy)
)

func errPreflightf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errPreflight, fmt.Sprintf(format, a...))
}

// classify tags a transport error as a disconnect when the OS says the device
// is gone.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ENODEV), errors.Is(err, syscall.ESHUTDOWN), errors.Is(err, syscall.ENOENT):
		return fmt.Errorf("%w: %w: %v", ErrTransport, ErrDisconnected, err)
	}
	return err
}

// Adapter and target constants used by the programmer.
const (
	cmdChipErase  = 0x14
	cmdGetChipID  = 0x68
	burstPrefix   = 0xEE
	burstMax      = 1024
	streamChunk   = 64
	flashChunk    = 4096
	regFCTL       = 0x6270
	regFADDRL     = 0x6271
	regDBGDATA    = 0x6260 // DMA source of the debug-interface burst write
	regFWDATA     = 0x6273
	regRNDH       = 0x70BD // SFR 0xBD through the XDATA SFR mirror
	sfrMEMCTR     = 0xC7
	sfrFMAP       = 0x9F
	sfrRNDL       = 0xBC
	sfrRNDH       = 0xBD
	sfrDMAIRQ     = 0xD1
	sfrDMA1CFGL   = 0xD2
	sfrDMA1CFGH   = 0xD3
	sfrDMA0CFGL   = 0xD4
	sfrDMA0CFGH   = 0xD5
	sfrDMAARM     = 0xD6
	sfrDMAREQ     = 0xD7
	fctlBusy      = 0x80
	fctlAbort     = 0x20
	fctlErase     = 0x01
	fctlWrite     = 0x06 // WRITE with the reset-default cache mode
	dmaTrigFlash  = 18
	dmaTrigDBG    = 31
	scratchBuf    = 0x0400 // RAM staging buffer (one burst block)
	scratchDesc   = 0x0C00 // DMA descriptors: ch1 +0, ch2 +8, ch0 +0x20
	crcBlock      = 4096
	groupBlocks   = 16 // program blocks per bounded operation
	groupPages    = 8  // erased pages per bounded operation
	pollMaxIter   = 4000
	chipEraseWait = 5 * time.Millisecond
)

// ChipInfo is the identity read from the hardware.
type ChipInfo struct {
	target.Geometry
	AdapterTargetID uint16 `json:"adapter_target_id"`
	CHIPINFO0       uint8  `json:"chipinfo0"`
	CHIPINFO1       uint8  `json:"chipinfo1"`
}

// IEEEAddresses are the primary (info page) and secondary (flash) addresses,
// stored LSB first as on the chip.
type IEEEAddresses struct {
	Primary          []byte
	PrimarySource    string
	Secondary        []byte
	SecondarySource  string
	PrimaryPresent   bool
	SecondaryPresent bool
}

// VerifyMode selects how programmed data is verified.
type VerifyMode string

// Verification modes. VerifyNone must be requested explicitly.
const (
	VerifyReadback VerifyMode = "readback"
	VerifyCRC      VerifyMode = "crc"
	VerifyBoth     VerifyMode = "both"
	VerifyNone     VerifyMode = "none"
)

// ParseVerify validates a verify mode; empty means readback.
func ParseVerify(s string) (VerifyMode, error) {
	switch v := VerifyMode(s); v {
	case VerifyReadback, VerifyCRC, VerifyBoth, VerifyNone:
		return v, nil
	case "":
		return VerifyReadback, nil
	}
	return "", invalidf("verify %q: want readback, crc, both or none", s)
}

// Progress receives coarse progress; it must not call back into the session.
type Progress func(stage string, done, total int)

// ChipEraseOptions controls ChipErase.
type ChipEraseOptions struct {
	Confirm string // must equal ConfirmChipErase
}

// ChipEraseResult reports a chip erase.
type ChipEraseResult struct {
	StatusBefore  Status
	StatusAfter   Status
	LockedBefore  bool
	Elapsed       time.Duration
	PollCount     int
	ReattachFirst bool // the debug session must be re-established before further use
}

// ErasePagesOptions controls ErasePages.
type ErasePagesOptions struct {
	Mode     flashplan.Mode
	Confirm  string // ConfirmErasePages
	Progress Progress
}

// ErasePagesResult reports page erasure.
type ErasePagesResult struct {
	Pages      []int
	BlankCheck bool
}

// ProgramOptions controls Program.
type ProgramOptions struct {
	Verify        VerifyMode
	Confirm       string // ConfirmProgram
	ConfirmConfig string // ConfirmConfig, required when the plan touches the lock-bit page
	Progress      Progress
}

// ProgramResult reports a program run.
type ProgramResult struct {
	Chip             string
	Mode             flashplan.Mode
	ErasedPages      []int
	BlocksProgrammed int
	BytesProgrammed  int
	SkippedBlocks    int
	Verify           VerifyMode // what was actually used
	Verified         bool
	CRCs             int
	Elapsed          time.Duration
	ResetRequired    bool
}

// FlashSink receives streamed flash data.
type FlashSink = io.Writer
