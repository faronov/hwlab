// Package sws is a native Go client for the Telink SWire (SWS) programmer
// protocol: a CRC16-framed request/response protocol that a Telink SWire
// master adapter speaks over a serial port and that drives the single-wire
// debug interface of the target (TLSR825x family).
//
// PROVENANCE. The wire format, command numbers, register addresses and the
// debug sequences were read from the public pvvx/TLSRPGM host tool
// (TlsrPgm.py, permission notice: free use, modification and distribution) and
// the zigbee-rs lab workflow that uses it. This is a clean Go implementation of
// that protocol; no code or tables were copied (the CRC is computed, and was
// checked against the reference's table).
//
// STATUS: framing, CRC, version, chip id, JEDEC id, flash read and SRAM/SWire
// reads were validated read-only against one real programmer and target (see
// frames_test.go: real captured frames). Erase, program, memory write, step,
// breakpoints and attach are only tested against Sim, a SYNTHETIC programmer
// written from the same reading of the reference.
package sws

import "errors"

// Command numbers (first byte of every request and response).
const (
	cmdFuncs      = 0
	cmdFlashRead  = 1
	cmdFlashWrite = 2
	cmdSectErase  = 3
	cmdFlashJEDEC = 5
	cmdFlashStat  = 6
	cmdSWRead     = 7
	cmdSWWrite    = 8
	cmdFlashWrRd  = 13
	cmdFlashCRC   = 14
)

// Sub-functions of cmdFuncs (second request byte).
const (
	fnVersion  = 0
	fnReset    = 3 // external power / reset pin
	fnActivate = 4
)

// Programmer error codes (second response byte).
const (
	ErrFunc = 1 // function number error
	ErrLen  = 2 // data length error
	ErrRead = 3 // SWire read timeout: the target did not answer
	ErrBusy = 4 // timeout flag while reading an analog register
	ErrCRC  = 5 // request CRC error
	ErrBaud = 6 // invalid baud rate number
)

// ErrorText names a programmer error code.
func ErrorText(code byte) string {
	switch code {
	case ErrFunc:
		return "function number error"
	case ErrLen:
		return "data length error"
	case ErrRead:
		return "SWire read timeout (the target did not answer: check the SWM-SWS connection or the reset/activation time)"
	case ErrBusy:
		return "timeout reading an analog register"
	case ErrCRC:
		return "the programmer reported a request CRC error"
	case ErrBaud:
		return "invalid baud rate number"
	}
	return "unknown programmer error"
}

// Error is an error response from the programmer.
type Error struct {
	Cmd  byte
	Code byte
}

func (e *Error) Error() string {
	return "programmer error " + itoa(int(e.Code)) + " for command " + itoa(int(e.Cmd)) + ": " + ErrorText(e.Code)
}

var (
	// ErrTimeout means the programmer did not answer in time.
	ErrTimeout = errors.New("no answer from the SWire programmer (wrong port, baud rate or not a programmer?)")
	// ErrBadResponse means an answer failed its CRC or did not match the request.
	ErrBadResponse = errors.New("malformed answer from the SWire programmer")
)

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// CRC16 is CRC-16/MODBUS (reflected polynomial 0xA001, initial value 0xFFFF).
// Frames carry it little-endian after the last byte.
func CRC16(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, v := range b {
		crc ^= uint16(v)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// withCRC appends the little-endian CRC of b.
func withCRC(b []byte) []byte {
	c := CRC16(b)
	return append(b, byte(c), byte(c>>8))
}

// crcOK reports whether the last two bytes of a frame are the CRC of the rest.
func crcOK(frame []byte) bool {
	if len(frame) < 3 {
		return false
	}
	n := len(frame) - 2
	c := CRC16(frame[:n])
	return frame[n] == byte(c) && frame[n+1] == byte(c>>8)
}
