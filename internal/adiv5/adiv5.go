// Package adiv5 is the Arm Debug Interface v5 layer: SW-DP/MEM-AP register and
// memory access over any probe that can issue DP/AP transfers (CMSIS-DAP
// today). It knows nothing about USB or the probe protocol and nothing about
// the target vendor; Cortex-M control lives in internal/cortexm.
package adiv5

import (
	"errors"
	"fmt"
	"time"
)

// DP and MEM-AP registers (ADIv5).
const (
	dpDPIDR  = 0x0 // read
	dpCtrl   = 0x4 // CTRL/STAT, bank 0
	dpSelect = 0x8
	dpRdbuff = 0xC

	apCSW  = 0x00
	apTAR  = 0x04
	apDRW  = 0x0C
	apCFG  = 0xF4
	apBase = 0xF8
	apIDR  = 0xFC
)

// CTRL/STAT bits.
const (
	ctrlStickyOrun = 1 << 1
	ctrlStickyCmp  = 1 << 4
	ctrlStickyErr  = 1 << 5
	ctrlCDBGPwrReq = 1 << 28
	ctrlCDBGPwrAck = 1 << 29
	ctrlCSYSPwrReq = 1 << 30
	ctrlCSYSPwrAck = 1 << 31
)

// ABORT bits: clear every sticky flag.
const abortClearAll = 0x1E

// CSW fields.
const (
	cswSize8   = 0
	cswSize16  = 1
	cswSize32  = 2
	cswIncSing = 1 << 4
	cswTopMask = 0xFF000000
)

const (
	powerUpTimeout = time.Second
	// tarWrap is the span over which the AP's auto-increment is guaranteed.
	tarWrap     = 0x400
	waitRetries = 20
	waitPause   = 2 * time.Millisecond
)

// Link is a probe's DP/AP access. Transfer, BlockRead and BlockWrite return a
// *TransferError (wrapping ErrWait, ErrFault, ...) when the target does not
// acknowledge.
type Link interface {
	Transfer(ops []Op) ([]uint32, error)
	BlockRead(ap bool, reg byte, words int) ([]uint32, error)
	BlockWrite(ap bool, reg byte, data []uint32) error
	WriteABORT(v uint32) error
	// SWJSequence clocks out bits (LSB first) on SWDIO with SWCLK running.
	SWJSequence(bits int, data []byte) error
}

// Op is one DP or AP register access. Reg is the register address as the
// architecture names it (DP: 0x0, 0x4, 0x8, 0xC; AP: 0x00..0xFC), but the
// AP bank must already be selected: only bits 3:2 go on the wire.
type Op struct {
	AP    bool
	Read  bool
	Reg   byte
	Value uint32 // write data
}

// Errors a transfer can end with.
var (
	ErrWait     = errors.New("target answered WAIT")
	ErrFault    = errors.New("target answered FAULT (sticky error set)")
	ErrNoACK    = errors.New("no acknowledge from the target (SWD line down, wrong wiring or no power)")
	ErrProtocol = errors.New("SWD protocol error (parity or framing)")
	ErrMismatch = errors.New("value mismatch")
)

// TransferError ends a transfer early.
type TransferError struct {
	Done int  // transfers (or words) completed before the failure
	Ack  byte // the failing ACK
	Err  error
}

func (e *TransferError) Error() string {
	return fmt.Sprintf("after %d completed: %v", e.Done, e.Err)
}

// Unwrap returns the sentinel (ErrWait, ErrFault, ...).
func (e *TransferError) Unwrap() error { return e.Err }

// Target is a Cortex-M (or any ADIv5 MEM-AP) target reached over SWD. It is
// not safe for concurrent use; the caller serialises calls.
type Target struct {
	p     Link
	ap    uint8
	DPIDR uint32
	APIDR uint32
	// cswProt keeps the AP's reset protection/mode bits (31:24) so the
	// accesses use the privilege and security the AP defaults to.
	cswProt  uint32
	selected uint32
	selOK    bool
	csw      uint32
	cswOK    bool
}

// Connect brings the debug port up on link: line reset and the JTAG-to-SWD
// switch, DPIDR read, debug power-up and AP identification. It does not halt,
// reset or write the target.
func Connect(link Link, ap uint8) (*Target, error) {
	t := &Target{p: link, ap: ap}
	if err := t.Reinit(); err != nil {
		return nil, err
	}
	idr, err := t.ReadAP(apIDR)
	if err != nil {
		return nil, fmt.Errorf("read AP %d IDR: %w", t.ap, err)
	}
	if idr == 0 {
		return nil, fmt.Errorf("AP %d does not exist (IDR is 0)", t.ap)
	}
	t.APIDR = idr
	csw, err := t.ReadAP(apCSW)
	if err != nil {
		return nil, fmt.Errorf("read AP %d CSW: %w", t.ap, err)
	}
	t.cswProt = csw & cswTopMask
	return t, nil
}

// ConnectOptional is Connect for targets whose MEM-AP can be closed at attach
// time (a CC23xx waiting in its boot ROM). The Target is returned with
// APIDR == 0 until RefreshAP finds the AP.
func ConnectOptional(link Link, ap uint8) (*Target, error) {
	t := &Target{p: link, ap: ap}
	if err := t.Reinit(); err != nil {
		return nil, err
	}
	if err := t.RefreshAP(); err != nil {
		return nil, err
	}
	return t, nil
}

// RefreshAP reads the MEM-AP identification and CSW protection bits again.
// It leaves APIDR at 0 and returns no error when the AP does not answer.
func (t *Target) RefreshAP() error {
	idr, err := t.ReadAP(apIDR)
	if err != nil {
		return fmt.Errorf("read AP %d IDR: %w", t.ap, err)
	}
	t.APIDR = idr
	if idr == 0 {
		return nil
	}
	csw, err := t.ReadAP(apCSW)
	if err != nil {
		return fmt.Errorf("read AP %d CSW: %w", t.ap, err)
	}
	t.cswProt = csw & cswTopMask
	return nil
}

// Reinit repeats the SWD line reset, DPIDR read, error clearing and debug
// power-up. Attach calls it; a reset that takes the debug port down needs it
// again.
func (t *Target) Reinit() error {
	ones := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	for _, s := range []struct {
		bits int
		data []byte
	}{
		{56, ones},               // line reset
		{16, []byte{0x9E, 0xE7}}, // JTAG-to-SWD select, 0xE79E LSB first
		{56, ones},               // line reset
		{8, []byte{0x00}},        // idle
	} {
		if err := t.p.SWJSequence(s.bits, s.data); err != nil {
			return err
		}
	}
	t.invalidate()
	v, err := t.p.Transfer([]Op{{Read: true, Reg: dpDPIDR}})
	if err != nil {
		if errors.Is(err, ErrNoACK) {
			return fmt.Errorf("no target answered on SWD (check SWDIO/SWCLK/GND wiring, target power and that SWD is not disabled): %w", err)
		}
		return fmt.Errorf("read DPIDR: %w", err)
	}
	t.DPIDR = v[0]
	if t.DPIDR == 0 || t.DPIDR == 0xFFFFFFFF {
		return fmt.Errorf("DPIDR is 0x%08X: no debug port on the line", t.DPIDR)
	}
	if err := t.p.WriteABORT(abortClearAll); err != nil {
		return err
	}
	return t.powerUp()
}

func (t *Target) powerUp() error {
	if err := t.WriteDP(dpSelect, 0); err != nil {
		return err
	}
	t.selected, t.selOK = 0, true
	if err := t.WriteDP(dpCtrl, ctrlCDBGPwrReq|ctrlCSYSPwrReq); err != nil {
		return err
	}
	deadline := time.Now().Add(powerUpTimeout)
	for {
		v, err := t.ReadDP(dpCtrl)
		if err != nil {
			return err
		}
		if v&ctrlCDBGPwrAck != 0 && v&ctrlCSYSPwrAck != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("debug power-up not acknowledged (CTRL/STAT 0x%08X)", v)
		}
		time.Sleep(time.Millisecond)
	}
}

// AP returns the MEM-AP index in use.
func (t *Target) AP() uint8 { return t.ap }

// invalidate forgets cached register state after an error or reset.
func (t *Target) invalidate() { t.selOK, t.cswOK = false, false }

// recover clears sticky errors so the next access can succeed.
func (t *Target) recover() {
	_ = t.p.WriteABORT(abortClearAll)
	t.invalidate()
}

// run executes ops, retrying a target WAIT on the failing op and resuming
// after it. A FAULT clears the sticky flags and is returned.
func (t *Target) run(ops []Op) ([]uint32, error) {
	var out []uint32
	waits := 0
	for len(ops) > 0 {
		vals, err := t.p.Transfer(ops)
		out = append(out, vals...)
		if err == nil {
			return out, nil
		}
		var te *TransferError
		if !errors.As(err, &te) {
			return out, err
		}
		switch {
		case errors.Is(err, ErrWait) && waits < waitRetries:
			waits++
			time.Sleep(waitPause)
			ops = ops[te.Done:]
		case errors.Is(err, ErrFault):
			t.recover()
			return out, err
		default:
			t.invalidate()
			return out, err
		}
	}
	return out, nil
}

func (t *Target) apSelect(reg byte) uint32 { return uint32(t.ap)<<24 | uint32(reg&0xF0) }

func (t *Target) prefix(reg byte) []Op {
	want := t.apSelect(reg)
	if t.selOK && t.selected == want {
		return nil
	}
	t.selected, t.selOK = want, true
	return []Op{{Reg: dpSelect, Value: want}}
}

// ReadDP reads a DP register of bank 0.
func (t *Target) ReadDP(reg byte) (uint32, error) {
	v, err := t.run([]Op{{Read: true, Reg: reg}})
	if err != nil {
		return 0, err
	}
	return v[0], nil
}

// WriteDP writes a DP register of bank 0.
func (t *Target) WriteDP(reg byte, val uint32) error {
	_, err := t.run([]Op{{Reg: reg, Value: val}})
	return err
}

// ReadAP reads a MEM-AP register by its architectural address (0x00..0xFC).
func (t *Target) ReadAP(reg byte) (uint32, error) {
	ops := append(t.prefix(reg), Op{AP: true, Read: true, Reg: reg})
	v, err := t.run(ops)
	if err != nil {
		t.selOK = false
		return 0, err
	}
	return v[len(v)-1], nil
}

// WriteAP writes a MEM-AP register by its architectural address.
func (t *Target) WriteAP(reg byte, val uint32) error {
	ops := append(t.prefix(reg), Op{AP: true, Reg: reg, Value: val})
	if _, err := t.run(ops); err != nil {
		t.selOK = false
		return err
	}
	return nil
}

// checkSticky reads CTRL/STAT and reports any sticky error.
func (t *Target) checkSticky() error {
	v, err := t.ReadDP(dpCtrl)
	if err != nil {
		return err
	}
	if v&(ctrlStickyErr|ctrlStickyOrun|ctrlStickyCmp) != 0 {
		t.recover()
		return fmt.Errorf("sticky error after access (CTRL/STAT 0x%08X)", v)
	}
	return nil
}

func (t *Target) setupAccess(addr uint32, size int) error {
	csw := t.cswProt | cswIncSing | uint32(size)
	ops := t.prefix(apCSW)
	if !t.cswOK || t.csw != csw {
		ops = append(ops, Op{AP: true, Reg: apCSW, Value: csw})
	}
	ops = append(ops, Op{AP: true, Reg: apTAR, Value: addr})
	if _, err := t.run(ops); err != nil {
		t.cswOK = false
		return err
	}
	t.csw, t.cswOK = csw, true
	return nil
}

func sizeBytes(size int) int { return 1 << size }

// accessSmall reads or writes one 8-, 16- or 32-bit item through its byte
// lane, so no neighbouring location is touched.
func (t *Target) accessSmall(addr uint32, size int, write bool, val uint32) (uint32, error) {
	if addr&uint32(sizeBytes(size)-1) != 0 {
		return 0, fmt.Errorf("address 0x%08X is not aligned to %d bytes", addr, sizeBytes(size))
	}
	if err := t.setupAccess(addr, size); err != nil {
		return 0, err
	}
	shift := (addr & 3) * 8
	if write {
		_, err := t.run([]Op{{AP: true, Reg: apDRW, Value: val << shift}})
		return 0, err
	}
	v, err := t.run([]Op{{AP: true, Read: true, Reg: apDRW}})
	if err != nil {
		return 0, err
	}
	v32 := v[0] >> shift
	switch size {
	case cswSize8:
		v32 &= 0xFF
	case cswSize16:
		v32 &= 0xFFFF
	}
	return v32, nil
}

// ReadWord reads one aligned 32-bit word.
func (t *Target) ReadWord(addr uint32) (uint32, error) {
	return t.accessSmall(addr, cswSize32, false, 0)
}

// WriteWord writes one aligned 32-bit word.
func (t *Target) WriteWord(addr, val uint32) error {
	_, err := t.accessSmall(addr, cswSize32, true, val)
	return err
}

func (t *Target) blockRead(addr uint32, words int) ([]uint32, error) {
	var out []uint32
	for words > 0 {
		n := int(tarWrap-(addr&(tarWrap-1))) / 4
		if n > words {
			n = words
		}
		if err := t.setupAccess(addr, cswSize32); err != nil {
			return out, err
		}
		got, err := t.p.BlockRead(true, apDRW, n)
		out = append(out, got...)
		if err != nil {
			if errors.Is(err, ErrFault) {
				t.recover()
			} else {
				t.invalidate()
			}
			return out, err
		}
		addr += uint32(4 * n)
		words -= n
	}
	return out, nil
}

func (t *Target) blockWrite(addr uint32, data []uint32) error {
	for len(data) > 0 {
		n := int(tarWrap-(addr&(tarWrap-1))) / 4
		if n > len(data) {
			n = len(data)
		}
		if err := t.setupAccess(addr, cswSize32); err != nil {
			return err
		}
		if err := t.p.BlockWrite(true, apDRW, data[:n]); err != nil {
			if errors.Is(err, ErrFault) {
				t.recover()
			} else {
				t.invalidate()
			}
			return err
		}
		addr += uint32(4 * n)
		data = data[n:]
	}
	return nil
}

// ReadMem reads n bytes at addr. Unaligned edges use byte and halfword
// accesses, so no neighbouring location is read.
func (t *Target) ReadMem(addr uint32, n int) ([]byte, error) {
	if n < 0 || uint64(addr)+uint64(n) > 1<<32 {
		return nil, fmt.Errorf("read of %d bytes at 0x%08X leaves the 32-bit address space", n, addr)
	}
	out := make([]byte, 0, n)
	for n > 0 {
		switch {
		case addr&3 == 0 && n >= 4:
			words := n / 4
			ws, err := t.blockRead(addr, words)
			for _, w := range ws {
				out = append(out, byte(w), byte(w>>8), byte(w>>16), byte(w>>24))
			}
			if err != nil {
				return out, memErr(addr+uint32(4*len(ws)), err)
			}
			addr += uint32(4 * words)
			n -= 4 * words
		case addr&1 == 0 && n >= 2:
			v, err := t.accessSmall(addr, cswSize16, false, 0)
			if err != nil {
				return out, memErr(addr, err)
			}
			out = append(out, byte(v), byte(v>>8))
			addr += 2
			n -= 2
		default:
			v, err := t.accessSmall(addr, cswSize8, false, 0)
			if err != nil {
				return out, memErr(addr, err)
			}
			out = append(out, byte(v))
			addr++
			n--
		}
	}
	if err := t.checkSticky(); err != nil {
		return out, err
	}
	return out, nil
}

// WriteMem writes data at addr, using byte and halfword accesses at unaligned
// edges so no neighbouring location is modified.
func (t *Target) WriteMem(addr uint32, data []byte) error {
	if uint64(addr)+uint64(len(data)) > 1<<32 {
		return fmt.Errorf("write of %d bytes at 0x%08X leaves the 32-bit address space", len(data), addr)
	}
	for len(data) > 0 {
		switch {
		case addr&3 == 0 && len(data) >= 4:
			words := len(data) / 4
			ws := make([]uint32, words)
			for i := range ws {
				ws[i] = uint32(data[4*i]) | uint32(data[4*i+1])<<8 | uint32(data[4*i+2])<<16 | uint32(data[4*i+3])<<24
			}
			if err := t.blockWrite(addr, ws); err != nil {
				return memErr(addr, err)
			}
			addr += uint32(4 * words)
			data = data[4*words:]
		case addr&1 == 0 && len(data) >= 2:
			if _, err := t.accessSmall(addr, cswSize16, true, uint32(data[0])|uint32(data[1])<<8); err != nil {
				return memErr(addr, err)
			}
			addr += 2
			data = data[2:]
		default:
			if _, err := t.accessSmall(addr, cswSize8, true, uint32(data[0])); err != nil {
				return memErr(addr, err)
			}
			addr++
			data = data[1:]
		}
	}
	return t.checkSticky()
}

func memErr(addr uint32, err error) error {
	return fmt.Errorf("memory access at 0x%08X: %w", addr, err)
}
