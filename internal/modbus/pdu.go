package modbus

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Request is one application-layer request to a slave.
type Request struct {
	Unit     byte
	Function byte

	Address  uint16
	Quantity uint16 // reads, write multiple

	Coils     []bool   // write single (one element) / multiple coils
	Registers []uint16 // write single (one element) / multiple registers, read-write write data

	AndMask, OrMask uint16 // mask write register

	ReadAddress, ReadQuantity uint16 // read-write registers: the read half

	// Raw is the whole PDU of a function this package does not model.
	Raw []byte
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }

func put16(dst []byte, v uint16) []byte { return binary.BigEndian.AppendUint16(dst, v) }

// Validate checks the request against the protocol's limits.
func (r *Request) Validate() error {
	rng := func(name string, q, max uint16) error {
		if q < 1 || q > max {
			return fmt.Errorf("%s: quantity %d is outside 1..%d", FunctionName(r.Function), q, max)
		}
		return nil
	}
	end := func(addr uint16, q uint16) error {
		if int(addr)+int(q) > 0x10000 {
			return fmt.Errorf("%s: address 0x%04x + quantity %d passes 0xffff", FunctionName(r.Function), addr, q)
		}
		return nil
	}
	switch r.Function {
	case FCReadCoils, FCReadDiscreteInputs:
		if err := rng("", r.Quantity, MaxReadBits); err != nil {
			return err
		}
		return end(r.Address, r.Quantity)
	case FCReadHoldingRegisters, FCReadInputRegisters:
		if err := rng("", r.Quantity, MaxReadRegisters); err != nil {
			return err
		}
		return end(r.Address, r.Quantity)
	case FCWriteSingleCoil:
		if len(r.Coils) != 1 {
			return fmt.Errorf("write_single_coil needs exactly one coil value")
		}
	case FCWriteSingleRegister:
		if len(r.Registers) != 1 {
			return fmt.Errorf("write_single_register needs exactly one value")
		}
	case FCWriteMultipleCoils:
		if err := rng("", uint16(len(r.Coils)), MaxWriteBits); err != nil {
			return err
		}
		return end(r.Address, uint16(len(r.Coils)))
	case FCWriteMultipleRegisters:
		if err := rng("", uint16(len(r.Registers)), MaxWriteRegs); err != nil {
			return err
		}
		return end(r.Address, uint16(len(r.Registers)))
	case FCMaskWriteRegister:
	case FCReadWriteRegisters:
		if err := rng("", r.ReadQuantity, MaxRWReadRegs); err != nil {
			return err
		}
		if err := rng("", uint16(len(r.Registers)), MaxRWWriteRegs); err != nil {
			return err
		}
		if err := end(r.ReadAddress, r.ReadQuantity); err != nil {
			return err
		}
		return end(r.Address, uint16(len(r.Registers)))
	default:
		if len(r.Raw) == 0 || r.Raw[0] != r.Function {
			return fmt.Errorf("function 0x%02x is not modelled: give the whole PDU as raw bytes starting with the function code", r.Function)
		}
		if len(r.Raw) > MaxPDU {
			return fmt.Errorf("raw PDU of %d bytes exceeds %d", len(r.Raw), MaxPDU)
		}
	}
	return nil
}

// PDU encodes the request's protocol data unit.
func (r *Request) PDU() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	b := []byte{r.Function}
	switch r.Function {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters:
		b = put16(put16(b, r.Address), r.Quantity)
	case FCWriteSingleCoil:
		v := uint16(0)
		if r.Coils[0] {
			v = 0xFF00
		}
		b = put16(put16(b, r.Address), v)
	case FCWriteSingleRegister:
		b = put16(put16(b, r.Address), r.Registers[0])
	case FCWriteMultipleCoils:
		data := packBits(r.Coils)
		b = put16(put16(b, r.Address), uint16(len(r.Coils)))
		b = append(append(b, byte(len(data))), data...)
	case FCWriteMultipleRegisters:
		b = put16(put16(b, r.Address), uint16(len(r.Registers)))
		b = append(b, byte(2*len(r.Registers)))
		for _, v := range r.Registers {
			b = put16(b, v)
		}
	case FCMaskWriteRegister:
		b = put16(put16(put16(b, r.Address), r.AndMask), r.OrMask)
	case FCReadWriteRegisters:
		b = put16(put16(b, r.ReadAddress), r.ReadQuantity)
		b = put16(put16(b, r.Address), uint16(len(r.Registers)))
		b = append(b, byte(2*len(r.Registers)))
		for _, v := range r.Registers {
			b = put16(b, v)
		}
	default:
		b = append([]byte(nil), r.Raw...)
	}
	return b, nil
}

// ParseRequest decodes a request PDU as a slave (or a sniffer) sees it.
func ParseRequest(unit byte, pdu []byte) (*Request, error) {
	if len(pdu) == 0 {
		return nil, ErrShort
	}
	r := &Request{Unit: unit, Function: pdu[0]}
	need := func(n int) error {
		if len(pdu) != n {
			return fmt.Errorf("%w: %s request is %d bytes, want %d", ErrProtocol, FunctionName(pdu[0]), len(pdu), n)
		}
		return nil
	}
	switch pdu[0] {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters:
		if err := need(5); err != nil {
			return nil, err
		}
		r.Address, r.Quantity = be16(pdu[1:]), be16(pdu[3:])
	case FCWriteSingleCoil:
		if err := need(5); err != nil {
			return nil, err
		}
		v := be16(pdu[3:])
		if v != 0xFF00 && v != 0 {
			return nil, fmt.Errorf("%w: coil value 0x%04x is neither 0xff00 nor 0x0000", ErrProtocol, v)
		}
		r.Address, r.Coils = be16(pdu[1:]), []bool{v == 0xFF00}
	case FCWriteSingleRegister:
		if err := need(5); err != nil {
			return nil, err
		}
		r.Address, r.Registers = be16(pdu[1:]), []uint16{be16(pdu[3:])}
	case FCWriteMultipleCoils:
		if len(pdu) < 6 {
			return nil, ErrShort
		}
		q, bc := int(be16(pdu[3:])), int(pdu[5])
		if bc != (q+7)/8 || len(pdu) != 6+bc || q < 1 {
			return nil, fmt.Errorf("%w: write_multiple_coils quantity %d with %d data bytes in a %d byte PDU", ErrProtocol, q, bc, len(pdu))
		}
		r.Address, r.Quantity, r.Coils = be16(pdu[1:]), uint16(q), unpackBits(pdu[6:], q)
	case FCWriteMultipleRegisters:
		if len(pdu) < 6 {
			return nil, ErrShort
		}
		q, bc := int(be16(pdu[3:])), int(pdu[5])
		if bc != 2*q || len(pdu) != 6+bc || q < 1 {
			return nil, fmt.Errorf("%w: write_multiple_registers quantity %d with %d data bytes in a %d byte PDU", ErrProtocol, q, bc, len(pdu))
		}
		r.Address, r.Quantity, r.Registers = be16(pdu[1:]), uint16(q), regsOf(pdu[6:])
	case FCMaskWriteRegister:
		if err := need(7); err != nil {
			return nil, err
		}
		r.Address, r.AndMask, r.OrMask = be16(pdu[1:]), be16(pdu[3:]), be16(pdu[5:])
	case FCReadWriteRegisters:
		if len(pdu) < 10 {
			return nil, ErrShort
		}
		wq, bc := int(be16(pdu[7:])), int(pdu[9])
		if bc != 2*wq || len(pdu) != 10+bc || wq < 1 {
			return nil, fmt.Errorf("%w: read_write_registers write quantity %d with %d data bytes in a %d byte PDU", ErrProtocol, wq, bc, len(pdu))
		}
		r.ReadAddress, r.ReadQuantity = be16(pdu[1:]), be16(pdu[3:])
		r.Address, r.Quantity, r.Registers = be16(pdu[5:]), uint16(wq), regsOf(pdu[10:])
	default:
		r.Raw = append([]byte(nil), pdu...)
	}
	return r, nil
}

func regsOf(b []byte) []uint16 {
	out := make([]uint16, len(b)/2)
	for i := range out {
		out[i] = be16(b[2*i:])
	}
	return out
}

// Response is a slave's answer.
type Response struct {
	Unit     byte
	Function byte // the request's function code (without the exception bit)

	// Exception is non-zero for an exception response.
	Exception byte

	Bits      []bool   // read coils / discrete inputs; whole bytes unless the request is known
	Registers []uint16 // read holding/input, read-write

	// Echoes of write functions.
	Address, Quantity, Value uint16
	AndMask, OrMask          uint16

	Raw []byte // PDU of a function this package does not model
}

// PDU encodes the response, for the simulated slave.
func (r *Response) PDU() []byte {
	if r.Exception != 0 {
		return []byte{r.Function | 0x80, r.Exception}
	}
	b := []byte{r.Function}
	switch r.Function {
	case FCReadCoils, FCReadDiscreteInputs:
		data := packBits(r.Bits)
		b = append(append(b, byte(len(data))), data...)
	case FCReadHoldingRegisters, FCReadInputRegisters, FCReadWriteRegisters:
		b = append(b, byte(2*len(r.Registers)))
		for _, v := range r.Registers {
			b = put16(b, v)
		}
	case FCWriteSingleCoil, FCWriteSingleRegister:
		b = put16(put16(b, r.Address), r.Value)
	case FCWriteMultipleCoils, FCWriteMultipleRegisters:
		b = put16(put16(b, r.Address), r.Quantity)
	case FCMaskWriteRegister:
		b = put16(put16(put16(b, r.Address), r.AndMask), r.OrMask)
	default:
		b = append([]byte(nil), r.Raw...)
	}
	return b
}

// ParseResponse decodes a response PDU. req may be nil (a sniffer that has not
// seen the request); with it the response is checked against the request and
// bit reads are trimmed to the requested quantity.
func ParseResponse(unit byte, pdu []byte, req *Request) (*Response, error) {
	if len(pdu) == 0 {
		return nil, ErrShort
	}
	fc := pdu[0]
	if fc&0x80 != 0 {
		if len(pdu) != 2 {
			return nil, fmt.Errorf("%w: exception response of %d bytes", ErrProtocol, len(pdu))
		}
		if req != nil && req.Function != fc&0x7F {
			return nil, fmt.Errorf("%w: exception for %s answers a %s request", ErrProtocol, FunctionName(fc), FunctionName(req.Function))
		}
		return &Response{Unit: unit, Function: fc & 0x7F, Exception: pdu[1]}, nil
	}
	if req != nil && req.Function != fc {
		return nil, fmt.Errorf("%w: %s response to a %s request", ErrProtocol, FunctionName(fc), FunctionName(req.Function))
	}
	r := &Response{Unit: unit, Function: fc}
	need := func(n int) error {
		if len(pdu) != n {
			return fmt.Errorf("%w: %s response is %d bytes, want %d", ErrProtocol, FunctionName(fc), len(pdu), n)
		}
		return nil
	}
	switch fc {
	case FCReadCoils, FCReadDiscreteInputs:
		if len(pdu) < 2 || int(pdu[1]) != len(pdu)-2 || pdu[1] == 0 {
			return nil, fmt.Errorf("%w: %s byte count does not match the PDU", ErrProtocol, FunctionName(fc))
		}
		n := int(pdu[1]) * 8
		if req != nil {
			if int(pdu[1]) != (int(req.Quantity)+7)/8 {
				return nil, fmt.Errorf("%w: %d data bytes for a request of %d bits", ErrProtocol, pdu[1], req.Quantity)
			}
			n = int(req.Quantity)
		}
		r.Bits = unpackBits(pdu[2:], n)
	case FCReadHoldingRegisters, FCReadInputRegisters, FCReadWriteRegisters:
		if len(pdu) < 2 || int(pdu[1]) != len(pdu)-2 || pdu[1] == 0 || pdu[1]%2 != 0 {
			return nil, fmt.Errorf("%w: %s byte count does not match the PDU", ErrProtocol, FunctionName(fc))
		}
		if req != nil {
			want := req.Quantity
			if fc == FCReadWriteRegisters {
				want = req.ReadQuantity
			}
			if int(pdu[1]) != 2*int(want) {
				return nil, fmt.Errorf("%w: %d data bytes for a request of %d registers", ErrProtocol, pdu[1], want)
			}
		}
		r.Registers = regsOf(pdu[2:])
	case FCWriteSingleCoil, FCWriteSingleRegister:
		if err := need(5); err != nil {
			return nil, err
		}
		r.Address, r.Value = be16(pdu[1:]), be16(pdu[3:])
	case FCWriteMultipleCoils, FCWriteMultipleRegisters:
		if err := need(5); err != nil {
			return nil, err
		}
		r.Address, r.Quantity = be16(pdu[1:]), be16(pdu[3:])
	case FCMaskWriteRegister:
		if err := need(7); err != nil {
			return nil, err
		}
		r.Address, r.AndMask, r.OrMask = be16(pdu[1:]), be16(pdu[3:]), be16(pdu[5:])
	default:
		r.Raw = append([]byte(nil), pdu...)
	}
	return r, nil
}

// Fields are the request's parameters as a plain map for events and results.
func (r *Request) Fields() map[string]any {
	m := map[string]any{"unit": int(r.Unit), "function": int(r.Function), "name": FunctionName(r.Function)}
	switch r.Function {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters:
		m["address"], m["quantity"] = int(r.Address), int(r.Quantity)
	case FCWriteSingleCoil:
		m["address"], m["value"] = int(r.Address), len(r.Coils) == 1 && r.Coils[0]
	case FCWriteSingleRegister:
		m["address"], m["value"] = int(r.Address), int(r.Registers[0])
	case FCWriteMultipleCoils:
		m["address"], m["quantity"], m["values"] = int(r.Address), len(r.Coils), boolsAny(r.Coils)
	case FCWriteMultipleRegisters:
		m["address"], m["quantity"], m["values"] = int(r.Address), len(r.Registers), regsAny(r.Registers)
	case FCMaskWriteRegister:
		m["address"], m["and_mask"], m["or_mask"] = int(r.Address), int(r.AndMask), int(r.OrMask)
	case FCReadWriteRegisters:
		m["read_address"], m["read_quantity"] = int(r.ReadAddress), int(r.ReadQuantity)
		m["address"], m["values"] = int(r.Address), regsAny(r.Registers)
	default:
		m["pdu_hex"] = hexOf(r.Raw)
	}
	return m
}

// Fields are the response's content as a plain map.
func (r *Response) Fields() map[string]any {
	m := map[string]any{"unit": int(r.Unit), "function": int(r.Function), "name": FunctionName(r.Function)}
	if r.Exception != 0 {
		m["exception"], m["exception_name"] = int(r.Exception), ExceptionName(r.Exception)
		return m
	}
	switch r.Function {
	case FCReadCoils, FCReadDiscreteInputs:
		m["bits"] = boolsAny(r.Bits)
	case FCReadHoldingRegisters, FCReadInputRegisters, FCReadWriteRegisters:
		m["registers"] = regsAny(r.Registers)
	case FCWriteSingleCoil, FCWriteSingleRegister:
		m["address"], m["value"] = int(r.Address), int(r.Value)
	case FCWriteMultipleCoils, FCWriteMultipleRegisters:
		m["address"], m["quantity"] = int(r.Address), int(r.Quantity)
	case FCMaskWriteRegister:
		m["address"], m["and_mask"], m["or_mask"] = int(r.Address), int(r.AndMask), int(r.OrMask)
	default:
		m["pdu_hex"] = hexOf(r.Raw)
	}
	return m
}

// Summary is a one-line description of the request.
func (r *Request) Summary() string {
	s := fmt.Sprintf("unit %d %s", r.Unit, FunctionName(r.Function))
	switch r.Function {
	case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters:
		return fmt.Sprintf("%s @0x%04x x%d", s, r.Address, r.Quantity)
	case FCWriteSingleCoil:
		return fmt.Sprintf("%s @0x%04x = %t", s, r.Address, len(r.Coils) == 1 && r.Coils[0])
	case FCWriteSingleRegister:
		return fmt.Sprintf("%s @0x%04x = %d", s, r.Address, r.Registers[0])
	case FCWriteMultipleCoils:
		return fmt.Sprintf("%s @0x%04x x%d", s, r.Address, len(r.Coils))
	case FCWriteMultipleRegisters:
		return fmt.Sprintf("%s @0x%04x x%d", s, r.Address, len(r.Registers))
	case FCMaskWriteRegister:
		return fmt.Sprintf("%s @0x%04x and=0x%04x or=0x%04x", s, r.Address, r.AndMask, r.OrMask)
	case FCReadWriteRegisters:
		return fmt.Sprintf("%s read @0x%04x x%d, write @0x%04x x%d", s, r.ReadAddress, r.ReadQuantity, r.Address, len(r.Registers))
	}
	return s
}

// Summary is a one-line description of the response.
func (r *Response) Summary() string {
	s := fmt.Sprintf("unit %d %s", r.Unit, FunctionName(r.Function))
	if r.Exception != 0 {
		return fmt.Sprintf("%s exception %d (%s)", s, r.Exception, ExceptionName(r.Exception))
	}
	switch r.Function {
	case FCReadCoils, FCReadDiscreteInputs:
		return fmt.Sprintf("%s %d bits", s, len(r.Bits))
	case FCReadHoldingRegisters, FCReadInputRegisters, FCReadWriteRegisters:
		return fmt.Sprintf("%s %s", s, regsPreview(r.Registers))
	}
	return s + " ok"
}

func boolsAny(v []bool) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

func regsAny(v []uint16) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = int(x)
	}
	return out
}

func regsPreview(v []uint16) string {
	const max = 6
	var sb strings.Builder
	sb.WriteByte('[')
	for i, x := range v {
		if i == max {
			fmt.Fprintf(&sb, " ... %d regs", len(v))
			break
		}
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprint(&sb, x)
	}
	sb.WriteByte(']')
	return sb.String()
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// Hex renders bytes as lowercase hex.
func Hex(b []byte) string { return hexOf(b) }
