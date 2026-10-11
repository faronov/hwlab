package modbusplugin

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/modbus"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

func m(danger pluginv1.Danger, name, desc, schema string) *pluginv1.MethodSpec {
	return &pluginv1.MethodSpec{Capability: pluginv1.CapModbus, Name: name, Description: desc, Danger: danger, ArgsSchemaJson: schema}
}

const (
	unitProp  = `"unit":{"type":"number","minimum":0,"maximum":255,"description":"slave/unit id; defaults to the device's unit (1)"}`
	addrProp  = `"address":{"type":"number","minimum":0,"maximum":65535,"description":"0-based protocol address, not the 4xxxx/3xxxx convention"}`
	qtyProp   = `"quantity":{"type":"number","minimum":1,"default":1}`
	asProp    = `"as":{"enum":["u16","i16","u32","i32","f32"],"description":"also interpret the registers as numbers"},"word_order":{"enum":["big","little"]}`
	readProps = unitProp + `,` + addrProp + `,` + qtyProp
)

// Info implements pluginsdk.Plugin.
func (p *Plugin) Info() *pluginv1.PluginInfo {
	safe, confirm := pluginv1.Danger_DANGER_SAFE, pluginv1.Danger_DANGER_CONFIRM
	return &pluginv1.PluginInfo{
		Id: "modbus", Name: "Modbus master (RTU and TCP)", Vendor: "hwlab", Version: "0.1.0",
		Capabilities: caps(),
		Methods: []*pluginv1.MethodSpec{
			m(safe, "status", "how the device is reached, its default unit and timeouts; sends nothing on the bus", `{"type":"object"}`),
			m(safe, "read_coils", "function 01: read coils", `{"type":"object","required":["address"],"properties":{`+readProps+`}}`),
			m(safe, "read_discrete_inputs", "function 02: read discrete inputs", `{"type":"object","required":["address"],"properties":{`+readProps+`}}`),
			m(safe, "read_holding_registers", "function 03: read holding registers (max 125)", `{"type":"object","required":["address"],"properties":{`+readProps+`,`+asProp+`}}`),
			m(safe, "read_input_registers", "function 04: read input registers (max 125)", `{"type":"object","required":["address"],"properties":{`+readProps+`,`+asProp+`}}`),
			m(confirm, "write_coil", "function 05: write one coil; changes the slave", `{"type":"object","required":["address","value"],"properties":{`+unitProp+`,`+addrProp+`,"value":{"type":"boolean"}}}`),
			m(confirm, "write_register", "function 06: write one holding register; changes the slave", `{"type":"object","required":["address","value"],"properties":{`+unitProp+`,`+addrProp+`,"value":{"type":"number"}}}`),
			m(confirm, "write_coils", "function 15: write several coils; changes the slave", `{"type":"object","required":["address","values"],"properties":{`+unitProp+`,`+addrProp+`,"values":{"type":"array","items":{"type":"boolean"}}}}`),
			m(confirm, "write_registers", "function 16: write several holding registers (max 123); changes the slave", `{"type":"object","required":["address","values"],"properties":{`+unitProp+`,`+addrProp+`,"values":{"type":"array","items":{"type":"number"}}}}`),
			m(confirm, "mask_write_register", "function 22: result = (current AND and_mask) OR (or_mask AND NOT and_mask); changes the slave",
				`{"type":"object","required":["address","and_mask","or_mask"],"properties":{`+unitProp+`,`+addrProp+`,"and_mask":{"type":"number"},"or_mask":{"type":"number"}}}`),
			m(confirm, "read_write_registers", "function 23: write registers then read registers in one transaction; changes the slave",
				`{"type":"object","required":["read_address","read_quantity","address","values"],"properties":{`+unitProp+`,"read_address":{"type":"number"},"read_quantity":{"type":"number"},`+addrProp+`,"values":{"type":"array","items":{"type":"number"}}}}`),
			m(confirm, "raw", "send an arbitrary PDU (hex, starting with the function code); the effect on the slave is unknown, so it is never retried",
				`{"type":"object","required":["pdu"],"properties":{`+unitProp+`,"pdu":{"type":"string","description":"hex, e.g. 0300000002"}}}`),
		},
	}
}

func argErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

func optNumber(a map[string]any, key string) (float64, bool, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false, fmt.Errorf("%s: want a number, got %v", key, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, fmt.Errorf("%s must be finite", key)
	}
	return f, true, nil
}

func intArg(a map[string]any, key string, min, max int, def *int) (int, error) {
	f, ok, err := optNumber(a, key)
	if err != nil {
		return 0, err
	}
	if !ok {
		if def == nil {
			return 0, fmt.Errorf("%s is required", key)
		}
		return *def, nil
	}
	if f != math.Trunc(f) || f < float64(min) || f > float64(max) {
		return 0, fmt.Errorf("%s %v: want an integer %d..%d", key, f, min, max)
	}
	return int(f), nil
}

func strArg(a map[string]any, key string) (string, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", fmt.Errorf("%s: want a string, got %v", key, v)
	}
	return s, nil
}

func listArg(a map[string]any, key string) ([]any, error) {
	v, ok := a[key]
	if !ok {
		return nil, fmt.Errorf("%s is required", key)
	}
	l, isList := v.([]any)
	if !isList || len(l) == 0 {
		return nil, fmt.Errorf("%s: want a non-empty array", key)
	}
	return l, nil
}

func regList(a map[string]any, key string) ([]uint16, error) {
	l, err := listArg(a, key)
	if err != nil {
		return nil, err
	}
	out := make([]uint16, len(l))
	for i, e := range l {
		f, ok := e.(float64)
		if !ok || f != math.Trunc(f) || f < -32768 || f > 65535 {
			return nil, fmt.Errorf("%s[%d] = %v: want an integer -32768..65535", key, i, e)
		}
		out[i] = uint16(int32(f))
	}
	return out, nil
}

func unitArg(a map[string]any, d *device) (byte, error) {
	def := int(d.opts.unit)
	u, err := intArg(a, "unit", 0, 255, &def)
	return byte(u), err
}

// buildRequest turns a method call into a request.
func buildRequest(method string, a map[string]any, d *device) (*modbus.Request, error) {
	unit, err := unitArg(a, d)
	if err != nil {
		return nil, err
	}
	one := 1
	req := &modbus.Request{Unit: unit}
	addr := func() (uint16, error) {
		v, err := intArg(a, "address", 0, 65535, nil)
		return uint16(v), err
	}
	switch method {
	case "read_coils", "read_discrete_inputs", "read_holding_registers", "read_input_registers":
		req.Function, _ = modbus.FunctionByName(method)
		if req.Address, err = addr(); err != nil {
			return nil, err
		}
		q, err := intArg(a, "quantity", 1, modbus.MaxReadBits, &one)
		if err != nil {
			return nil, err
		}
		req.Quantity = uint16(q)
	case "write_coil":
		req.Function = modbus.FCWriteSingleCoil
		if req.Address, err = addr(); err != nil {
			return nil, err
		}
		b, ok := a["value"].(bool)
		if !ok {
			return nil, errors.New("value: want true or false")
		}
		req.Coils = []bool{b}
	case "write_register":
		req.Function = modbus.FCWriteSingleRegister
		if req.Address, err = addr(); err != nil {
			return nil, err
		}
		vs, err := regList(map[string]any{"v": []any{a["value"]}}, "v")
		if err != nil {
			return nil, errors.New("value: want an integer -32768..65535")
		}
		req.Registers = vs
	case "write_coils":
		req.Function = modbus.FCWriteMultipleCoils
		if req.Address, err = addr(); err != nil {
			return nil, err
		}
		l, err := listArg(a, "values")
		if err != nil {
			return nil, err
		}
		for i, e := range l {
			b, ok := e.(bool)
			if !ok {
				return nil, fmt.Errorf("values[%d] = %v: want true or false", i, e)
			}
			req.Coils = append(req.Coils, b)
		}
	case "write_registers":
		req.Function = modbus.FCWriteMultipleRegisters
		if req.Address, err = addr(); err != nil {
			return nil, err
		}
		if req.Registers, err = regList(a, "values"); err != nil {
			return nil, err
		}
	case "mask_write_register":
		req.Function = modbus.FCMaskWriteRegister
		if req.Address, err = addr(); err != nil {
			return nil, err
		}
		am, err := intArg(a, "and_mask", 0, 65535, nil)
		if err != nil {
			return nil, err
		}
		om, err := intArg(a, "or_mask", 0, 65535, nil)
		if err != nil {
			return nil, err
		}
		req.AndMask, req.OrMask = uint16(am), uint16(om)
	case "read_write_registers":
		req.Function = modbus.FCReadWriteRegisters
		ra, err := intArg(a, "read_address", 0, 65535, nil)
		if err != nil {
			return nil, err
		}
		rq, err := intArg(a, "read_quantity", 1, modbus.MaxRWReadRegs, nil)
		if err != nil {
			return nil, err
		}
		req.ReadAddress, req.ReadQuantity = uint16(ra), uint16(rq)
		if req.Address, err = addr(); err != nil {
			return nil, err
		}
		if req.Registers, err = regList(a, "values"); err != nil {
			return nil, err
		}
	case "raw":
		s, err := strArg(a, "pdu")
		if err != nil {
			return nil, err
		}
		b, err := hex.DecodeString(strings.ReplaceAll(strings.TrimPrefix(s, "0x"), " ", ""))
		if err != nil || len(b) == 0 || len(b) > modbus.MaxPDU {
			return nil, fmt.Errorf("pdu %q: want 1..%d bytes of hex", s, modbus.MaxPDU)
		}
		req, err = modbus.ParseRequest(unit, b)
		if err != nil {
			return nil, err
		}
		return req, req.Validate()
	default:
		return nil, status.Errorf(codes.Unimplemented, "method Modbus.%s", method)
	}
	return req, req.Validate()
}

// Invoke implements pluginsdk.Plugin.
func (p *Plugin) Invoke(ctx context.Context, req *pluginv1.InvokeRequest) (*pluginv1.InvokeResponse, error) {
	if c := req.GetCapability(); c != pluginv1.CapModbus {
		return nil, status.Errorf(codes.Unimplemented, "capability %q", c)
	}
	args := req.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	d, release, err := p.lease(req.GetDeviceId(), nil)
	if err != nil {
		return nil, err
	}
	defer release()

	var res map[string]any
	if req.GetMethod() == "status" {
		res = d.status()
	} else {
		var mr *modbus.Request
		if mr, err = buildRequest(req.GetMethod(), args, d); err != nil {
			if _, isStatus := status.FromError(err); isStatus && status.Code(err) != codes.Unknown {
				return nil, err
			}
			return nil, argErr(err)
		}
		as, _ := strArg(args, "as")
		order, _ := strArg(args, "word_order")
		res, err = d.do(ctx, mr, as, order)
		if err != nil {
			return nil, err
		}
	}
	s, err := structpb.NewStruct(res)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InvokeResponse{Result: s}, nil
}

func (d *device) status() map[string]any {
	res := map[string]any{
		"device": d.id, "transport": d.cl.T.Kind(), "endpoint": d.cl.T.Describe(), "unit": int(d.opts.unit),
		"timeout_ms": d.opts.timeout.Milliseconds(), "retries": d.opts.retries,
		"validated": false, "note": "no real Modbus device has been tested with hwlab; this master is validated against a simulated slave only",
	}
	if d.entry.Fake != nil {
		res["synthetic"] = true
	}
	return res
}

// do runs one request and describes the outcome. Exceptions, timeouts and CRC
// errors become gRPC errors so a failed step is visible as failed.
func (d *device) do(ctx context.Context, req *modbus.Request, as, order string) (map[string]any, error) {
	res, err := d.cl.Do(ctx, req)
	if err != nil {
		return nil, mapErr(req, err)
	}
	out := map[string]any{
		"unit": int(req.Unit), "function": int(req.Function), "name": modbus.FunctionName(req.Function),
		"attempts": res.Attempts, "request": req.Fields(),
		"request_hex": modbus.Hex(res.Exchange.ReqADU),
		"rtt_ms":      float64(res.Exchange.Received.Sub(res.Exchange.Sent).Microseconds()) / 1000,
	}
	if res.Response == nil { // broadcast
		out["broadcast"] = true
		return out, nil
	}
	out["response_hex"] = modbus.Hex(res.Exchange.RespADU)
	out["response"] = res.Response.Fields()
	if as != "" || order != "" {
		if len(res.Response.Registers) == 0 {
			return nil, argErr(errors.New("as/word_order apply to register reads only"))
		}
		vals, err := modbus.Interpret(res.Response.Registers, as, order)
		if err != nil {
			return nil, argErr(err)
		}
		out["values"] = vals
		if as == "" {
			as = "u16"
		}
		out["as"] = as
	}
	return out, nil
}

func mapErr(req *modbus.Request, err error) error {
	var ex *modbus.ExceptionError
	switch {
	case errors.As(err, &ex):
		return status.Errorf(codes.FailedPrecondition, "unit %d %s: slave answered exception %d (%s)", req.Unit, modbus.FunctionName(req.Function), ex.Code, modbus.ExceptionName(ex.Code))
	case errors.Is(err, modbus.ErrTimeout):
		return status.Errorf(codes.Unavailable, "unit %d %s: no response (check the unit id, wiring, baud rate/parity and the timeout)", req.Unit, modbus.FunctionName(req.Function))
	case errors.Is(err, modbus.ErrCRC), errors.Is(err, modbus.ErrShort), errors.Is(err, modbus.ErrProtocol):
		return status.Errorf(codes.DataLoss, "unit %d %s: %v", req.Unit, modbus.FunctionName(req.Function), err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	}
	return status.Errorf(codes.Unavailable, "unit %d %s: %v", req.Unit, modbus.FunctionName(req.Function), err)
}
