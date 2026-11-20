// Package icepick drives the TI ICEPick-C JTAG route controller found in
// front of the debug and test TAPs of TI SoCs (CC13xx/CC26xx and others): it
// reads the ICEPick identification, writes and reads its router registers and
// links a secondary TAP (the CPU DAP or a test TAP) into the scan chain.
//
// It works on whole scans (jtag.Scanner) and never clocks Test-Logic-Reset,
// so it is usable over the IEEE 1149.7 two-pin formats, where a TAP reset
// drops the target out of the format. Router commands end in Pause-IR /
// Pause-DR, as the ICEPick documentation asks: Run-Test/Idle is only entered
// once a routing change is complete, which is when the ICEPick applies it.
//
// The protocol follows the public ICEPick type C description (TI wiki
// "ICEPick", SPRUH35) and the TAP numbering of the CC13xx/CC26xx technical
// reference manuals; the implementation is original.
package icepick

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/internal/jtag"
)

// IRLen is the ICEPick instruction register length.
const IRLen = 6

// ICEPick instructions.
const (
	IRRouter      = 0x02
	IRIDCode      = 0x04
	IRICEPickCode = 0x05
	IRConnect     = 0x07
	IRBypass      = 0x3F
)

// CONNECT register keys.
const (
	ConnectKey    = 0x89
	DisconnectKey = 0x86
)

// Router blocks.
const (
	BlockControl = 0
	BlockTest    = 1
	BlockDebug   = 2
)

// RegControl is the ICEPick control register of the control block.
const RegControl = 1

// ControlSetup is the control register value written before routing.
const ControlSetup = 0x001000

// Secondary TAP register values that power a TAP, put it in debug mode and
// select it, applied in this order.
const (
	TAPPowerClock = 0x110048
	TAPDebugMode  = 0x112048
	TAPSelect     = 0x112148
)

// Port numbers: 0..15 are debug TAPs, 16..31 test TAPs (16 + test TAP index).
const (
	PortCPUDAP = 0
	// PortChipEraseTAP is the CC13xx/CC26xx test TAP that carries the chip
	// erase register.
	PortChipEraseTAP = 21
)

// IRCapture is the value every IEEE 1149.1 IR captures in its two low bits.
const IRCapture = 0x01

// ErrNoICEPick means the chain did not answer like an ICEPick.
var ErrNoICEPick = errors.New("no ICEPick on the scan chain")

// Router is an ICEPick reached through a scanner. Until a secondary TAP is
// linked, the ICEPick is the only TAP on the chain.
type Router struct {
	S jtag.Scanner
}

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func word(b []byte) uint32 {
	var x [4]byte
	copy(x[:], b)
	return binary.LittleEndian.Uint32(x[:])
}

// Identify loads IDCODE and reads the 32-bit identification, and checks the
// IR capture pattern. It ends in Run-Test/Idle.
func (r *Router) Identify(ctx context.Context) (idcode uint32, irCapture byte, err error) {
	res, err := r.S.Scans(ctx, []jtag.ScanOp{
		{IR: true, Bits: IRLen, Out: []byte{IRIDCode}, End: jtag.RunTestIdle},
		{Bits: 32, End: jtag.RunTestIdle},
	})
	if err != nil {
		return 0, 0, err
	}
	irCapture, idcode = res[0][0], word(res[1])
	if irCapture&3 != IRCapture || idcode&1 != 1 || idcode == 0xFFFFFFFF {
		return idcode, irCapture, fmt.Errorf("%w: IR capture 0x%02x, IDCODE 0x%08x (%s)", ErrNoICEPick, irCapture, idcode, stuck(idcode))
	}
	return idcode, irCapture, nil
}

func stuck(id uint32) string {
	switch id {
	case 0:
		return "TDO/TMSC read all zeros: stuck low"
	case 0xFFFFFFFF:
		return "TDO/TMSC read all ones: stuck high or floating"
	}
	return "not an IEEE 1149.1 IDCODE"
}

// Code reads the ICEPICKCODE register (ICEPick type and capabilities).
func (r *Router) Code(ctx context.Context) (uint32, error) {
	res, err := r.S.Scans(ctx, []jtag.ScanOp{
		{IR: true, Bits: IRLen, Out: []byte{IRICEPickCode}, End: jtag.RunTestIdle},
		{Bits: 32, End: jtag.RunTestIdle},
	})
	if err != nil {
		return 0, err
	}
	return word(res[1]), nil
}

// RouterValue builds a ROUTER scan: write flag, block, register, 24-bit payload.
func RouterValue(write bool, block, reg int, payload uint32) uint32 {
	v := uint32(block&7)<<28 | uint32(reg&0xF)<<24 | payload&0xFFFFFF
	if write {
		v |= 1 << 31
	}
	return v
}

func connectOps() []jtag.ScanOp {
	return []jtag.ScanOp{
		{IR: true, Bits: IRLen, Out: []byte{IRConnect}, End: jtag.PauseIR},
		{Bits: 8, Out: []byte{ConnectKey}, End: jtag.PauseDR},
	}
}

func routerOps(write bool, block, reg int, payload uint32) []jtag.ScanOp {
	return []jtag.ScanOp{
		{IR: true, Bits: IRLen, Out: []byte{IRRouter}, End: jtag.PauseIR},
		{Bits: 32, Out: le32(RouterValue(write, block, reg, payload)), End: jtag.PauseDR},
	}
}

// Connect opens the router for writes (CONNECT with the connect key).
func (r *Router) Connect(ctx context.Context) error {
	_, err := r.S.Scans(ctx, connectOps())
	return err
}

// Read reads a router register: a read request, then a second ROUTER scan
// that captures the answer. The raw 32-bit capture is returned (block and
// register in bits 30:24, value in 23:0). It stays in Pause-DR.
func (r *Router) Read(ctx context.Context, block, reg int) (uint32, error) {
	ops := append(routerOps(false, block, reg, 0), routerOps(false, block, reg, 0)...)
	res, err := r.S.Scans(ctx, ops)
	if err != nil {
		return 0, err
	}
	return word(res[3]), nil
}

// TAPBlockReg maps a port number to its router block and register.
func TAPBlockReg(port int) (block, reg int, err error) {
	switch {
	case port >= 0 && port < 16:
		return BlockDebug, port, nil
	case port >= 16 && port < 32:
		return BlockTest, port - 16, nil
	}
	return 0, 0, fmt.Errorf("ICEPick port %d: want 0..15 (debug TAP) or 16..31 (test TAP)", port)
}

// LinkTAP connects to the router, configures it, powers and selects the TAP
// on port and enters Run-Test/Idle so that the ICEPick links it into the
// chain. The ICEPick is left in BYPASS; the new TAP sits between the ICEPick
// and TDO.
func (r *Router) LinkTAP(ctx context.Context, port int) error {
	block, reg, err := TAPBlockReg(port)
	if err != nil {
		return err
	}
	ops := connectOps()
	ops = append(ops, routerOps(true, BlockControl, RegControl, ControlSetup)...)
	for _, v := range []uint32{TAPPowerClock, TAPDebugMode, TAPSelect} {
		ops = append(ops, routerOps(true, block, reg, v)...)
	}
	ops = append(ops,
		jtag.ScanOp{IR: true, Bits: IRLen, Out: []byte{IRBypass}, End: jtag.RunTestIdle},
		jtag.ScanOp{Idle: 10, End: jtag.RunTestIdle},
	)
	_, err = r.S.Scans(ctx, ops)
	return err
}

// Linked is what ProbeLinked saw after LinkTAP.
type Linked struct {
	// IRCapture is the captured IR of the whole chain (linked TAP in the low bits).
	IRCapture uint32
	// IDCode is the linked TAP's IDCODE when an IDCODE instruction was given.
	IDCode uint32
}

// ProbeLinked checks that a TAP with irLen bits now sits in front of the
// ICEPick: the combined IR must capture 0b..01 for both TAPs. With idInstr
// non-zero it also loads that instruction into the linked TAP (ICEPick in
// BYPASS) and reads its 32-bit IDCODE, which must be a valid IDCODE.
func (r *Router) ProbeLinked(ctx context.Context, irLen int, idInstr uint32) (Linked, bool, error) {
	total := irLen + IRLen
	nb := (total + 7) / 8
	ops := []jtag.ScanOp{{IR: true, Bits: total, Out: le32(uint32(1)<<total - 1)[:nb], End: jtag.RunTestIdle}}
	if idInstr != 0 {
		ops = append(ops,
			jtag.ScanOp{IR: true, Bits: total, Out: le32(idInstr | IRBypass<<irLen)[:nb], End: jtag.RunTestIdle},
			jtag.ScanOp{Bits: 33, End: jtag.RunTestIdle},
		)
	}
	res, err := r.S.Scans(ctx, ops)
	if err != nil {
		return Linked{}, false, err
	}
	l := Linked{IRCapture: word(res[0])}
	want := uint32(IRCapture) | IRCapture<<irLen
	mask := uint32(3) | 3<<irLen
	ok := l.IRCapture&mask == want
	if idInstr != 0 {
		l.IDCode = word(res[2])
		ok = ok && l.IDCode&1 == 1 && l.IDCode != 0xFFFFFFFF
	}
	return l, ok, nil
}

// ChipEraseTAPIRLen, ChipEraseInstr and ChipEraseCommand describe the
// CC13xx/CC26xx chip erase register on test TAP PortChipEraseTAP: IR
// instruction 0x1 selects the 8-bit register, 0x22 requests the erase of the
// whole device (flash including the CCFG). Documented by the OpenOCD
// cc26xx_chip_erase procedure (tested by its author on a CC2650).
const (
	ChipEraseTAPIRLen = 4
	ChipEraseInstr    = 0x1
	ChipEraseCommand  = 0x22
)

// ChipErase links the chip erase test TAP and requests the erase of the whole
// device. The device resets during the erase, which drops a two-pin cJTAG
// connection: the caller has to reconnect.
func (r *Router) ChipErase(ctx context.Context) error {
	if err := r.LinkTAP(ctx, PortChipEraseTAP); err != nil {
		return err
	}
	total := ChipEraseTAPIRLen + IRLen
	ir := uint32(ChipEraseInstr) | IRBypass<<ChipEraseTAPIRLen
	_, err := r.S.Scans(ctx, []jtag.ScanOp{
		{IR: true, Bits: total, Out: le32(ir)[:(total+7)/8], End: jtag.PauseIR},
		// 8 register bits for the test TAP, then the ICEPick's BYPASS bit.
		{Bits: 9, Out: []byte{ChipEraseCommand, 0}, End: jtag.PauseDR},
		{Idle: 100, End: jtag.RunTestIdle},
	})
	return err
}
