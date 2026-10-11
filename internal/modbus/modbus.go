// Package modbus implements the Modbus application protocol (PDUs), the RTU and
// TCP framings, a client, a simulated slave and an offline stream analyzer. It
// has no plugin or serial-port dependency: RTU runs over any io.ReadWriter, so
// the generic uart transport supplies the bytes and nothing here opens a port.
package modbus

import (
	"errors"
	"fmt"
)

// Function codes implemented by the typed request/response layer. Other codes
// travel as raw PDUs.
const (
	FCReadCoils              = 0x01
	FCReadDiscreteInputs     = 0x02
	FCReadHoldingRegisters   = 0x03
	FCReadInputRegisters     = 0x04
	FCWriteSingleCoil        = 0x05
	FCWriteSingleRegister    = 0x06
	FCWriteMultipleCoils     = 0x0F
	FCWriteMultipleRegisters = 0x10
	FCMaskWriteRegister      = 0x16
	FCReadWriteRegisters     = 0x17
)

// Limits from the Modbus application protocol specification.
const (
	MaxReadBits      = 2000
	MaxReadRegisters = 125
	MaxWriteBits     = 1968
	MaxWriteRegs     = 123
	MaxRWReadRegs    = 125
	MaxRWWriteRegs   = 121
	MaxPDU           = 253
)

var functionNames = map[byte]string{
	FCReadCoils:              "read_coils",
	FCReadDiscreteInputs:     "read_discrete_inputs",
	FCReadHoldingRegisters:   "read_holding_registers",
	FCReadInputRegisters:     "read_input_registers",
	FCWriteSingleCoil:        "write_single_coil",
	FCWriteSingleRegister:    "write_single_register",
	FCWriteMultipleCoils:     "write_multiple_coils",
	FCWriteMultipleRegisters: "write_multiple_registers",
	FCMaskWriteRegister:      "mask_write_register",
	FCReadWriteRegisters:     "read_write_registers",
}

// FunctionName is the snake_case name of a function code, or "fcNN".
func FunctionName(fc byte) string {
	if n, ok := functionNames[fc&0x7F]; ok {
		return n
	}
	return fmt.Sprintf("fc%02x", fc&0x7F)
}

// FunctionByName is the inverse of FunctionName for the typed functions.
func FunctionByName(name string) (byte, bool) {
	for fc, n := range functionNames {
		if n == name {
			return fc, true
		}
	}
	return 0, false
}

// IsWrite reports whether a function changes the slave. Unknown functions are
// treated as writes because their effect is not known.
func IsWrite(fc byte) bool {
	switch fc & 0x7F {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters:
		return false
	}
	return true
}

var exceptionNames = map[byte]string{
	1:  "illegal_function",
	2:  "illegal_data_address",
	3:  "illegal_data_value",
	4:  "server_device_failure",
	5:  "acknowledge",
	6:  "server_device_busy",
	8:  "memory_parity_error",
	10: "gateway_path_unavailable",
	11: "gateway_target_failed_to_respond",
}

// ExceptionName names a Modbus exception code.
func ExceptionName(code byte) string {
	if n, ok := exceptionNames[code]; ok {
		return n
	}
	return fmt.Sprintf("exception_%02x", code)
}

// Exception codes a slave raises.
const (
	ExIllegalFunction    = 1
	ExIllegalDataAddress = 2
	ExIllegalDataValue   = 3
	ExDeviceFailure      = 4
)

// ExceptionError is a well-formed exception response from the slave.
type ExceptionError struct {
	Function byte
	Code     byte
}

func (e *ExceptionError) Error() string {
	return fmt.Sprintf("modbus exception %d (%s) for %s", e.Code, ExceptionName(e.Code), FunctionName(e.Function))
}

// Errors reported by the framing and client layers.
var (
	ErrCRC      = errors.New("modbus: CRC mismatch")
	ErrTimeout  = errors.New("modbus: no response before the timeout")
	ErrShort    = errors.New("modbus: frame too short")
	ErrProtocol = errors.New("modbus: protocol error")
)

// CRC16 is the Modbus RTU CRC (polynomial 0xA001, initial value 0xFFFF).
func CRC16(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, c := range b {
		crc ^= uint16(c)
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

func packBits(bits []bool) []byte {
	out := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		if b {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out
}

func unpackBits(b []byte, n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = b[i/8]>>(i%8)&1 == 1
	}
	return out
}
