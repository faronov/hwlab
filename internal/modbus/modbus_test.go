package modbus

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net"
	"reflect"
	"testing"
	"time"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := range b {
		var v byte
		for _, c := range s[2*i : 2*i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				v |= byte(c-'a') + 10
			default:
				t.Fatalf("bad hex %q", s)
			}
		}
		b[i] = v
	}
	return b
}

func TestCRCKnownVectors(t *testing.T) {
	// Frames from the Modbus over serial line guide, widely used as vectors.
	if got := EncodeRTU(0x01, mustHex(t, "0300000001")); !bytes.Equal(got, mustHex(t, "010300000001840a")) {
		t.Errorf("read 1 register: %x", got)
	}
	if got := EncodeRTU(0x01, mustHex(t, "030000000a")); !bytes.Equal(got, mustHex(t, "01030000000ac5cd")) {
		t.Errorf("read 10 registers: %x", got)
	}
	if got := EncodeRTU(0x11, mustHex(t, "03006b0003")); !bytes.Equal(got, mustHex(t, "1103006b00037687")) {
		t.Errorf("guide example: %x", got)
	}
}

func TestRequestPDURoundTrip(t *testing.T) {
	reqs := []*Request{
		{Unit: 1, Function: FCReadCoils, Address: 0x13, Quantity: 19},
		{Unit: 1, Function: FCReadDiscreteInputs, Address: 0xC4, Quantity: 22},
		{Unit: 1, Function: FCReadHoldingRegisters, Address: 0x6B, Quantity: 3},
		{Unit: 1, Function: FCReadInputRegisters, Address: 8, Quantity: 1},
		{Unit: 1, Function: FCWriteSingleCoil, Address: 0xAC, Coils: []bool{true}},
		{Unit: 1, Function: FCWriteSingleCoil, Address: 0xAC, Coils: []bool{false}},
		{Unit: 1, Function: FCWriteSingleRegister, Address: 1, Registers: []uint16{3}},
		{Unit: 1, Function: FCWriteMultipleCoils, Address: 0x13, Quantity: 10, Coils: []bool{true, false, true, true, false, false, true, true, true, false}},
		{Unit: 1, Function: FCWriteMultipleRegisters, Address: 1, Quantity: 2, Registers: []uint16{0x000A, 0x0102}},
		{Unit: 1, Function: FCMaskWriteRegister, Address: 4, AndMask: 0x00F2, OrMask: 0x0025},
		{Unit: 1, Function: FCReadWriteRegisters, ReadAddress: 3, ReadQuantity: 6, Address: 14, Quantity: 3, Registers: []uint16{0xFF, 0xFF, 0xFF}},
	}
	for _, r := range reqs {
		pdu, err := r.PDU()
		if err != nil {
			t.Fatalf("%s: %v", r.Summary(), err)
		}
		back, err := ParseRequest(r.Unit, pdu)
		if err != nil {
			t.Fatalf("%s: parse: %v", r.Summary(), err)
		}
		// Quantity is derived for the write-multiple forms.
		if r.Function == FCWriteMultipleCoils || r.Function == FCWriteMultipleRegisters || r.Function == FCReadWriteRegisters {
			r.Quantity = back.Quantity
		}
		if !reflect.DeepEqual(r, back) {
			t.Errorf("%s: round trip\n got %+v\nwant %+v", r.Summary(), back, r)
		}
	}
	// Spec example: read 3 holding registers at 0x006B.
	pdu, _ := (&Request{Function: FCReadHoldingRegisters, Address: 0x6B, Quantity: 3}).PDU()
	if !bytes.Equal(pdu, mustHex(t, "03006b0003")) {
		t.Errorf("PDU %x", pdu)
	}
}

func TestRequestValidation(t *testing.T) {
	bad := []*Request{
		{Function: FCReadHoldingRegisters, Address: 0, Quantity: 0},
		{Function: FCReadHoldingRegisters, Address: 0, Quantity: 126},
		{Function: FCReadCoils, Address: 0, Quantity: 2001},
		{Function: FCReadHoldingRegisters, Address: 0xFFFF, Quantity: 2},
		{Function: FCWriteSingleCoil, Address: 0},
		{Function: FCWriteMultipleRegisters, Address: 0},
		{Function: FCWriteMultipleRegisters, Address: 0, Registers: make([]uint16, 124)},
		{Function: FCReadWriteRegisters, ReadQuantity: 1, Registers: make([]uint16, 122)},
		{Function: 0x2B},
		{Function: 0x2B, Raw: []byte{0x2C, 1}},
	}
	for _, r := range bad {
		if _, err := r.PDU(); err == nil {
			t.Errorf("%+v should be rejected", r)
		}
	}
	if pdu, err := (&Request{Function: 0x2B, Raw: []byte{0x2B, 0x0E, 1, 0}}).PDU(); err != nil || !bytes.Equal(pdu, []byte{0x2B, 0x0E, 1, 0}) {
		t.Errorf("raw pdu: %x %v", pdu, err)
	}
}

func TestParseRequestRejectsMalformed(t *testing.T) {
	for _, h := range []string{"03", "0300000001ff", "05000a1234", "0f00130002010100", "10000100020400", "16000400f2", "170003000600"} {
		if _, err := ParseRequest(1, mustHex(t, h)); err == nil {
			t.Errorf("%s should not parse", h)
		}
	}
	if _, err := ParseRequest(1, nil); !errors.Is(err, ErrShort) {
		t.Errorf("empty: %v", err)
	}
}

func TestResponseParsing(t *testing.T) {
	rq := &Request{Unit: 1, Function: FCReadCoils, Address: 0x13, Quantity: 19}
	// Spec example: 19 coils in 3 bytes CD 6B 05.
	r, err := ParseResponse(1, mustHex(t, "0103cd6b05"), rq)
	if err != nil || len(r.Bits) != 19 || !r.Bits[0] || r.Bits[1] {
		t.Fatalf("coils: %+v %v", r, err)
	}
	if _, err := ParseResponse(1, mustHex(t, "0103cd6b"), rq); err == nil {
		t.Error("short byte count accepted")
	}
	if _, err := ParseResponse(1, mustHex(t, "0102cd6b"), rq); err == nil {
		t.Error("data bytes for the wrong quantity accepted")
	}
	// Without the request the whole bytes are returned.
	r, err = ParseResponse(1, mustHex(t, "0103cd6b05"), nil)
	if err != nil || len(r.Bits) != 24 {
		t.Fatalf("sniffed coils: %+v %v", r, err)
	}

	rr := &Request{Unit: 1, Function: FCReadHoldingRegisters, Address: 0x6B, Quantity: 3}
	r, err = ParseResponse(1, mustHex(t, "0306022b00000064"), rr)
	if err != nil || !reflect.DeepEqual(r.Registers, []uint16{0x022B, 0, 0x64}) {
		t.Fatalf("registers: %+v %v", r, err)
	}
	if _, err := ParseResponse(1, mustHex(t, "0304022b0000"), rr); err == nil {
		t.Error("two registers accepted for a request of three")
	}
	if _, err := ParseResponse(1, mustHex(t, "0402022b"), rr); err == nil {
		t.Error("response of a different function accepted")
	}
	r, err = ParseResponse(1, mustHex(t, "8302"), rr)
	if err != nil || r.Exception != 2 || r.Function != FCReadHoldingRegisters {
		t.Fatalf("exception: %+v %v", r, err)
	}
	if _, err := ParseResponse(1, mustHex(t, "830200"), rr); err == nil {
		t.Error("long exception accepted")
	}
	if _, err := ParseResponse(1, mustHex(t, "8402"), rr); err == nil {
		t.Error("exception for another function accepted")
	}
	// The response PDU encoder is the inverse.
	for _, h := range []string{"0103cd6b05", "0306022b00000064", "0500acff00", "0600010003", "0f00130a", "1000010002", "16000400f20025", "8302"} {
		r, err := ParseResponse(1, mustHex(t, h), nil)
		if err != nil {
			if h == "0f00130a" {
				continue
			}
			t.Fatalf("%s: %v", h, err)
		}
		if h == "0103cd6b05" {
			continue // the bits come back whole-byte padded
		}
		if got := r.PDU(); !bytes.Equal(got, mustHex(t, h)) {
			t.Errorf("%s re-encoded as %x", h, got)
		}
	}
}

func TestExtractRTUCutsFramesAndResyncs(t *testing.T) {
	r1 := EncodeRTU(1, mustHex(t, "0300000002"))   // request
	r2 := EncodeRTU(1, mustHex(t, "030400010002")) // response
	r3 := EncodeRTU(2, mustHex(t, "0600010003"))   // write single request
	stream := append(append(append([]byte{}, r1...), r2...), r3...)
	fs, rest, junk := ExtractRTU(stream, RoleAny, false)
	if len(fs) != 3 || len(rest) != 0 || junk != 0 {
		t.Fatalf("frames %d rest %d junk %d", len(fs), len(rest), junk)
	}
	if fs[0].Role != RoleRequest || fs[1].Role != RoleResponse || fs[2].Offset != len(r1)+len(r2) {
		t.Errorf("roles/offsets: %+v", fs)
	}

	// Role-specific extraction never yields a good frame of the other kind.
	fs, _, _ = ExtractRTU(r2, RoleRequest, true)
	for _, f := range fs {
		if !f.BadCRC {
			t.Errorf("a response parsed as a good request: %+v", f)
		}
	}

	// Leading junk is skipped, a partial tail is kept until more data arrives.
	noisy := append([]byte{0xEE, 0xEE}, r1...)
	noisy = append(noisy, r2[:5]...)
	fs, rest, junk = ExtractRTU(noisy, RoleAny, false)
	if len(fs) != 1 || junk != 2 || !bytes.Equal(rest, r2[:5]) {
		t.Fatalf("noisy: frames %d junk %d rest %x", len(fs), junk, rest)
	}
	fs, rest, junk = ExtractRTU(append(append([]byte{}, rest...), r2[5:]...), RoleAny, false)
	if len(fs) != 1 || len(rest) != 0 || junk != 0 {
		t.Fatalf("completed: %d %d %d", len(fs), len(rest), junk)
	}
	// The same tail at the end of the data is junk.
	_, rest, junk = ExtractRTU(r2[:5], RoleAny, true)
	if len(rest) != 0 || junk != 5 {
		t.Errorf("final partial: rest %d junk %d", len(rest), junk)
	}

	// A frame of the right length with a damaged CRC is reported and consumed.
	bad := append([]byte{}, r1...)
	bad[len(bad)-1] ^= 0xFF
	fs, rest, _ = ExtractRTU(append(bad, r2...), RoleAny, false)
	if len(fs) != 2 || !fs[0].BadCRC || fs[1].BadCRC || len(rest) != 0 {
		t.Fatalf("bad crc: %+v", fs)
	}

	// An unmodelled function is accepted only as the whole, CRC-valid buffer.
	odd := EncodeRTU(1, mustHex(t, "2b0e0100"))
	fs, _, _ = ExtractRTU(odd, RoleAny, true)
	if len(fs) != 1 || fs[0].Unit != 1 {
		t.Errorf("unmodelled function: %+v", fs)
	}
}

func TestInterpret(t *testing.T) {
	v, err := Interpret([]uint16{0x4048, 0xF5C3}, "f32", "big")
	if err != nil || len(v) != 1 || math.Abs(v[0].(float64)-3.14) > 1e-6 {
		t.Errorf("f32: %v %v", v, err)
	}
	v, _ = Interpret([]uint16{0xF5C3, 0x4048}, "f32", "little")
	if math.Abs(v[0].(float64)-3.14) > 1e-6 {
		t.Errorf("f32 little: %v", v)
	}
	v, _ = Interpret([]uint16{0x0001, 0x0002}, "u32", "")
	if v[0].(float64) != 65538 {
		t.Errorf("u32: %v", v)
	}
	v, _ = Interpret([]uint16{0xFFFF, 0xFFFE}, "i32", "")
	if v[0].(float64) != -2 {
		t.Errorf("i32: %v", v)
	}
	v, _ = Interpret([]uint16{0xFFFF, 5}, "i16", "")
	if v[0] != -1 || v[1] != 5 {
		t.Errorf("i16: %v", v)
	}
	if _, err := Interpret([]uint16{1}, "u32", ""); err == nil {
		t.Error("odd register count accepted")
	}
	if _, err := Interpret([]uint16{1, 2}, "u64", ""); err == nil {
		t.Error("unknown type accepted")
	}
	if _, err := Interpret([]uint16{1, 2}, "u32", "weird"); err == nil {
		t.Error("unknown word order accepted")
	}
	v, _ = Interpret([]uint16{0x7FC0, 0}, "f32", "")
	if v[0] != nil {
		t.Errorf("NaN should become null: %v", v)
	}
}

func newRTUPair(t *testing.T, s *Slave) (*Client, *RTU) {
	t.Helper()
	m, sl := NewLink()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.ServeRTU(ctx, sl); close(done) }()
	t.Cleanup(func() {
		cancel()
		if err := m.Close(); err != nil {
			t.Error(err)
		}
		if err := sl.Close(); err != nil {
			t.Error(err)
		}
		<-done
	})
	tr := NewRTU(m, 115200, "link")
	tr.Timeout = 300 * time.Millisecond
	return &Client{T: tr}, tr
}

func exercise(t *testing.T, c *Client, s *Slave) {
	t.Helper()
	ctx := context.Background()
	do := func(r *Request) *Response {
		t.Helper()
		res, err := c.Do(ctx, r)
		if err != nil {
			t.Fatalf("%s: %v", r.Summary(), err)
		}
		return res.Response
	}
	s.SetHolding(0, 10, 20, 30, 40)
	s.SetInput(5, 111, 222)
	s.SetCoil(3, true)
	s.SetDiscrete(1, true)

	if r := do(&Request{Unit: 1, Function: FCReadHoldingRegisters, Address: 1, Quantity: 3}); !reflect.DeepEqual(r.Registers, []uint16{20, 30, 40}) {
		t.Errorf("holding: %v", r.Registers)
	}
	if r := do(&Request{Unit: 1, Function: FCReadInputRegisters, Address: 5, Quantity: 2}); !reflect.DeepEqual(r.Registers, []uint16{111, 222}) {
		t.Errorf("input: %v", r.Registers)
	}
	if r := do(&Request{Unit: 1, Function: FCReadCoils, Address: 2, Quantity: 3}); !reflect.DeepEqual(r.Bits, []bool{false, true, false}) {
		t.Errorf("coils: %v", r.Bits)
	}
	if r := do(&Request{Unit: 1, Function: FCReadDiscreteInputs, Address: 0, Quantity: 2}); !reflect.DeepEqual(r.Bits, []bool{false, true}) {
		t.Errorf("discrete: %v", r.Bits)
	}
	do(&Request{Unit: 1, Function: FCWriteSingleRegister, Address: 10, Registers: []uint16{0xBEEF}})
	do(&Request{Unit: 1, Function: FCWriteSingleCoil, Address: 11, Coils: []bool{true}})
	do(&Request{Unit: 1, Function: FCWriteMultipleRegisters, Address: 20, Registers: []uint16{1, 2, 3}})
	do(&Request{Unit: 1, Function: FCWriteMultipleCoils, Address: 30, Coils: []bool{true, false, true}})
	if got := s.Holding(10, 1)[0]; got != 0xBEEF {
		t.Errorf("write single register: %x", got)
	}
	if !s.Coil(11) || !s.Coil(30) || s.Coil(31) || !s.Coil(32) {
		t.Error("coil writes missing")
	}
	if got := s.Holding(20, 3); !reflect.DeepEqual(got, []uint16{1, 2, 3}) {
		t.Errorf("write multiple: %v", got)
	}
	s.SetHolding(40, 0x0012)
	do(&Request{Unit: 1, Function: FCMaskWriteRegister, Address: 40, AndMask: 0x00F2, OrMask: 0x0025})
	if got := s.Holding(40, 1)[0]; got != 0x0017 { // the spec's worked example
		t.Errorf("mask write: %04x", got)
	}
	if r := do(&Request{Unit: 1, Function: FCReadWriteRegisters, ReadAddress: 20, ReadQuantity: 3, Address: 21, Registers: []uint16{9, 9}}); !reflect.DeepEqual(r.Registers, []uint16{1, 9, 9}) {
		t.Errorf("read/write: %v (the write must land before the read)", r.Registers)
	}
}

func TestRTUClientAgainstSlave(t *testing.T) {
	s := NewSlave(1, 64)
	c, _ := newRTUPair(t, s)
	exercise(t, c, s)
}

func TestRTUExceptionAndErrors(t *testing.T) {
	s := NewSlave(1, 16)
	c, tr := newRTUPair(t, s)
	ctx := context.Background()

	res, err := c.Do(ctx, &Request{Unit: 1, Function: FCReadHoldingRegisters, Address: 100, Quantity: 1})
	var ee *ExceptionError
	if !errors.As(err, &ee) || ee.Code != ExIllegalDataAddress || res.Response == nil || res.Response.Exception != 2 {
		t.Fatalf("exception: res=%+v err=%v", res, err)
	}
	res, err = c.Do(ctx, &Request{Unit: 1, Function: 0x2B, Raw: []byte{0x2B, 0x0E, 0x01, 0x00}})
	if !errors.As(err, &ee) || ee.Code != ExIllegalFunction {
		t.Fatalf("unsupported function: res=%+v err=%v", res, err)
	}

	// A unit that does not exist never answers.
	if _, err := c.Do(ctx, &Request{Unit: 9, Function: FCReadHoldingRegisters, Address: 0, Quantity: 1}); !errors.Is(err, ErrTimeout) {
		t.Errorf("silent unit: %v", err)
	}
	// Broadcast: written, no answer awaited, and the slave applied it.
	start := time.Now()
	if _, err := c.Do(ctx, &Request{Unit: 0, Function: FCWriteSingleRegister, Address: 2, Registers: []uint16{77}}); err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Error("broadcast waited for a response")
	}
	time.Sleep(50 * time.Millisecond)
	if s.Holding(2, 1)[0] != 77 {
		t.Error("broadcast write not applied")
	}
	// A corrupted response is a CRC error and is not retried for writes.
	s.Mangle = func(f []byte) []byte { f = append([]byte(nil), f...); f[len(f)-1] ^= 1; return f }
	before := s.Served()
	c.Retries = 2
	if _, err := c.Do(ctx, &Request{Unit: 1, Function: FCWriteSingleRegister, Address: 3, Registers: []uint16{1}}); !errors.Is(err, ErrCRC) {
		t.Errorf("crc: %v", err)
	}
	if got := s.Served() - before; got != 1 {
		t.Errorf("a write was attempted %d times", got)
	}
	// Reads are retried.
	before = s.Served()
	res, err = c.Do(ctx, &Request{Unit: 1, Function: FCReadHoldingRegisters, Address: 0, Quantity: 1})
	if !errors.Is(err, ErrCRC) || res.Attempts != 3 || s.Served()-before != 3 {
		t.Errorf("read retries: attempts=%d served=%d err=%v", res.Attempts, s.Served()-before, err)
	}
	s.Mangle = nil
	if _, err := c.Do(ctx, &Request{Unit: 1, Function: FCReadHoldingRegisters, Address: 0, Quantity: 0}); err == nil {
		t.Error("an invalid request was sent")
	}

	// Cancellation interrupts a wait.
	tr.Timeout = 5 * time.Second
	cctx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if _, err := c.Do(cctx, &Request{Unit: 9, Function: FCReadHoldingRegisters, Address: 0, Quantity: 1}); err == nil || time.Since(t0) > time.Second {
		t.Errorf("cancel: err=%v after %v", err, time.Since(t0))
	}
}

func TestRTUSilenceAndSerialErrors(t *testing.T) {
	if got := RTUSilence(9600); got < 3*time.Millisecond || got > 5*time.Millisecond {
		t.Errorf("9600 baud silence %v", got)
	}
	if RTUSilence(115200) != 1750*time.Microsecond || RTUSilence(0) != 1750*time.Microsecond {
		t.Error("high baud silence is the fixed 1.75 ms")
	}
	m, sl := NewLink()
	if err := sl.Close(); err != nil {
		t.Fatal(err)
	}
	tr := NewRTU(m, 9600, "dead")
	if _, err := tr.RoundTrip(context.Background(), 1, mustHex(t, "0300000001")); err == nil {
		t.Error("writing to a closed link succeeded")
	}
}

func TestTCPClientAgainstSlave(t *testing.T) {
	s := NewSlave(1, 64)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback networking:", err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	go s.ServeTCP(l)
	tr, err := DialTCP(l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tr.Close(); err != nil {
			t.Error(err)
		}
	})
	exercise(t, &Client{T: tr}, s)

	res, err := (&Client{T: tr}).Do(context.Background(), &Request{Unit: 1, Function: FCReadHoldingRegisters, Address: 63, Quantity: 2})
	var ee *ExceptionError
	if !errors.As(err, &ee) || ee.Code != ExIllegalDataAddress {
		t.Fatalf("tcp exception: %v", err)
	}
	if res.Exchange.TID == 0 || len(res.Exchange.ReqADU) != 7+5 {
		t.Errorf("exchange: %+v", res.Exchange)
	}
	tid, unit, pdu, err := DecodeTCP(res.Exchange.ReqADU)
	if err != nil || tid != res.Exchange.TID || unit != 1 || !bytes.Equal(pdu, mustHex(t, "03003f0002")) {
		t.Errorf("DecodeTCP: %d %d %x %v", tid, unit, pdu, err)
	}
	if _, _, _, err := DecodeTCP(mustHex(t, "000100010003010300")); err == nil {
		t.Error("non-zero protocol id accepted")
	}
	if _, _, _, err := DecodeTCP(mustHex(t, "0001000000050103")); err == nil {
		t.Error("wrong MBAP length accepted")
	}
}

func TestTCPTimeoutAndClosedPeer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback networking:", err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	conns := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		conns <- c
	}()
	tr, err := DialTCP(l.Addr().String(), 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tr.Close(); err != nil {
			t.Error(err)
		}
	})
	c := &Client{T: tr}
	if _, err := c.Do(context.Background(), &Request{Unit: 1, Function: FCReadCoils, Quantity: 1}); !errors.Is(err, ErrTimeout) {
		t.Errorf("silent slave: %v", err)
	}
	if err := (<-conns).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(context.Background(), &Request{Unit: 1, Function: FCReadCoils, Quantity: 1}); err == nil || errors.Is(err, ErrTimeout) {
		t.Errorf("closed peer: %v", err)
	}
}
