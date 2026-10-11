package native

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/faronov/hwlab/plugins/ccdebugger/native/target"
)

// State is the session lifecycle.
type State int

// Session states. A failed operation leaves the session Faulted: there is no
// automatic reconnect, retry or reset.
const (
	StateOpen State = iota
	StateFaulted
	StateClosed
)

func (s State) String() string {
	return [...]string{"open", "faulted", "closed"}[s]
}

// Debugger is one CC-Debugger session with the CC2530 behind it. It is safe
// for sequential use; concurrent or reentrant calls are refused, not queued.
type Debugger struct {
	tr  Transport
	cfg Config

	mu          sync.Mutex
	state       State
	ready       bool // a debug session with the target exists
	dmaEligible bool // fresh reset by this session, no CPU execution since

	geom       *target.Geometry // identified by ReadChipInfo, never assumed
	needsReset bool             // flash or RAM staging changed; reset before executing code
}

// New wraps an opened transport. The transport is closed by [Debugger.Close].
func New(tr Transport, cfg Config) (*Debugger, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Debugger{tr: tr, cfg: cfg, ready: cfg.Access == ExistingDebugSession}, nil
}

// State reports the lifecycle state.
func (d *Debugger) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// Config returns the session's policy.
func (d *Debugger) Config() Config { return d.cfg }

// Attached reports whether the session may touch the target.
func (d *Debugger) Attached() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ready
}

// Close releases the transport. It is idempotent.
func (d *Debugger) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == StateClosed {
		return nil
	}
	d.state = StateClosed
	return d.tr.Close()
}

type deadline struct {
	end time.Time
	now func() time.Time
}

// remaining never returns zero: USB stacks read a zero timeout as infinite.
func (dl *deadline) remaining() (time.Duration, error) {
	r := dl.end.Sub(dl.now())
	if r <= 0 {
		return 0, transportf("USB operation deadline exceeded")
	}
	return r.Round(time.Millisecond) + time.Millisecond/2, nil
}

func (d *Debugger) do(pre func() error, fn func(dl *deadline) error) error {
	if !d.mu.TryLock() {
		return policyf("session is busy; concurrent or reentrant access is not permitted")
	}
	defer d.mu.Unlock()
	if d.state != StateOpen {
		return policyf("session is %s, expected open; no automatic reconnect or target reset", d.state)
	}
	if pre != nil {
		if err := pre(); err != nil {
			return err
		}
	}
	d.state = StateFaulted
	dl := &deadline{end: d.cfg.Now().Add(d.cfg.Timeout), now: d.cfg.Now}
	if err := fn(dl); err != nil {
		if errors.Is(err, errPreflight) {
			d.state = StateOpen // refused before any state-changing exchange
		}
		err = classify(err)
		if !errors.Is(err, ErrPolicy) && !errors.Is(err, ErrTransport) && !errors.Is(err, ErrInvalid) {
			err = fmt.Errorf("%w: %w", ErrTransport, err)
		}
		return err
	}
	if _, err := dl.remaining(); err != nil {
		return err
	}
	d.state = StateOpen
	return nil
}

// need lists the permissions an operation uses.
type need struct{ cpu, reset, mem, write, brk, dma, fErase, fProg, chip, cfg bool }

func (d *Debugger) target(n need, fn func(dl *deadline) error) error {
	check := func() error {
		p := d.cfg.Permissions
		switch {
		case !d.ready:
			return policyf("target access requires an explicitly confirmed existing debug session or a completed reset-attach")
		case n.cpu && !p.CPUControl:
			return policyf("CPU control requires separate explicit permission")
		case n.reset && !p.TargetReset:
			return policyf("target reset requires separate explicit permission")
		case n.mem && !p.MemoryAccess:
			return policyf("memory access requires separate explicit permission")
		case n.write && !p.MemoryWrite:
			return policyf("memory write requires separate explicit permission")
		case n.brk && !p.Breakpoints:
			return policyf("breakpoints require separate explicit permission")
		case n.dma && !p.DMAEnable:
			return policyf("DMA enable requires separate explicit permission")
		case n.dma && !d.dmaEligible:
			return policyf("DMA enable requires a fresh reset by this session, before CPU execution")
		case n.fErase && !p.FlashErase:
			return policyf("flash erase requires separate explicit permission")
		case n.fProg && !p.FlashProgram:
			return policyf("flash program requires separate explicit permission")
		case n.chip && !p.ChipErase:
			return policyf("chip erase requires separate explicit permission")
		case n.cfg && !p.ConfigWrite:
			return policyf("device-configuration write (lock bits, secondary IEEE) requires separate explicit permission")
		}
		return nil
	}
	return d.do(check, func(dl *deadline) error {
		st, err := d.adapterState(dl)
		if err != nil {
			return err
		}
		if st.TargetID != targetCC2530 {
			return transportf("adapter does not report a CC2530 target (0x%04X); no target command sent", st.TargetID)
		}
		return fn(dl)
	})
}

func (d *Debugger) stopped(n need, fn func(dl *deadline) error) error {
	return d.target(n, func(dl *deadline) error {
		st, err := d.exchangeByte(cmdReadStatus, dl)
		if err != nil {
			return err
		}
		if err := checkStatus(Status(st), true, true); err != nil {
			return err
		}
		if err := fn(dl); err != nil {
			return err
		}
		st, err = d.exchangeByte(cmdReadStatus, dl)
		if err != nil {
			return err
		}
		return checkStatus(Status(st), true, true)
	})
}

func exact(b []byte, n int) ([]byte, error) {
	if len(b) != n {
		return nil, transportf("USB reply must contain exactly %d bytes, got %d", n, len(b))
	}
	return b, nil
}

func (d *Debugger) adapterState(dl *deadline) (AdapterState, error) {
	t, err := dl.remaining()
	if err != nil {
		return AdapterState{}, err
	}
	b, err := d.tr.ControlRead(0xC0, 0xC0, 0, 0, 8, t)
	if err != nil {
		return AdapterState{}, err
	}
	if b, err = exact(b, 8); err != nil {
		return AdapterState{}, err
	}
	if _, err := dl.remaining(); err != nil {
		return AdapterState{}, err
	}
	le := func(o int) uint16 { return uint16(b[o]) | uint16(b[o+1])<<8 }
	st := AdapterState{TargetID: le(0), FirmwareVersion: le(2), FirmwareRev: le(4)}
	if st.TargetID != targetCC2530 {
		d.dmaEligible = false
	}
	return st, nil
}

// ReadAdapterState asks the adapter for its target id and firmware version
// without touching the target.
func (d *Debugger) ReadAdapterState() (st AdapterState, err error) {
	err = d.do(nil, func(dl *deadline) (e error) {
		st, e = d.adapterState(dl)
		return e
	})
	return
}

func (d *Debugger) writePacket(packet []byte, dl *deadline) error {
	t, err := dl.remaining()
	if err != nil {
		return err
	}
	n, err := d.tr.BulkWrite(EndpointOut, packet, t)
	if err != nil {
		return err
	}
	if n != len(packet) {
		return transportf("incomplete USB command write; exchange must not be retried")
	}
	_, err = dl.remaining()
	return err
}

func (d *Debugger) exchange(packet []byte, n int, dl *deadline) ([]byte, error) {
	if err := d.writePacket(packet, dl); err != nil {
		return nil, err
	}
	t, err := dl.remaining()
	if err != nil {
		return nil, err
	}
	b, err := d.tr.BulkRead(EndpointIn, n, t)
	if err != nil {
		return nil, err
	}
	if b, err = exact(b, n); err != nil {
		return nil, err
	}
	_, err = dl.remaining()
	return b, err
}

func (d *Debugger) exchangeByte(cmd byte, dl *deadline) (uint8, error) {
	b, err := d.exchange([]byte{0x1F, cmd}, 1, dl)
	if err != nil {
		return 0, err
	}
	if cmd == cmdReadStatus && b[0] != resetStatus || cmd == cmdReadConfig && b[0] != resetConfig {
		d.dmaEligible = false
	}
	return b[0], nil
}

// instruction executes 1..3 supplied 8051 bytes on the halted CPU and returns
// the accumulator. These are individually observed ACC-returning adapter forms,
// not a general grammar.
func (d *Debugger) instruction(ins []byte, dl *deadline) (uint8, error) {
	if len(ins) < 1 || len(ins) > 3 {
		return 0, invalidf("an instruction must contain 1..3 bytes")
	}
	prefix := [...]byte{0x4F, 0x7F, 0xAF}[len(ins)-1]
	packet := append([]byte{prefix, cmdDebugInstr | byte(len(ins))}, ins...)
	b, err := d.exchange(packet, 1, dl)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (d *Debugger) pc(dl *deadline) (uint16, error) {
	b, err := d.exchange([]byte{0x3F, cmdGetPC}, 2, dl)
	if err != nil {
		return 0, err
	}
	pc := uint16(b[0])<<8 | uint16(b[1])
	if pc != 0 {
		d.dmaEligible = false
	}
	return pc, nil
}

func checkStatus(s Status, active, halted bool) error {
	if s&(DebugLocked|ChipEraseBusy) != 0 {
		return transportf("target is locked or erasing (status=0x%02x)", uint8(s))
	}
	if active && (s&OscillatorStable == 0 || s&(PMActive|PconIdle) != 0) {
		return transportf("target needs a stable oscillator and normal power state (status=0x%02x)", uint8(s))
	}
	if halted && s&CPUHalted == 0 {
		return transportf("target must be halted (status=0x%02x)", uint8(s))
	}
	return nil
}

// ReadDebugStatus reads the raw debug status byte (no state checks).
func (d *Debugger) ReadDebugStatus() (st Status, err error) {
	err = d.target(need{}, func(dl *deadline) error {
		v, e := d.exchangeByte(cmdReadStatus, dl)
		st = Status(v)
		return e
	})
	return
}

// ReadDebugConfig reads the debug configuration byte.
func (d *Debugger) ReadDebugConfig() (cfg uint8, err error) {
	err = d.target(need{}, func(dl *deadline) error {
		v, e := d.exchangeByte(cmdReadStatus, dl)
		if e != nil {
			return e
		}
		if e := checkStatus(Status(v), false, false); e != nil {
			return e
		}
		if cfg, e = d.exchangeByte(cmdReadConfig, dl); e != nil {
			return e
		}
		if v, e = d.exchangeByte(cmdReadStatus, dl); e != nil {
			return e
		}
		return checkStatus(Status(v), false, false)
	})
	return
}

// EnableDMAAfterReset clears only DMA_PAUSE in the debug configuration. It is
// allowed once, right after this session's own reset and before any CPU
// execution, so a resumed firmware sees normal DMA.
func (d *Debugger) EnableDMAAfterReset() (cfg uint8, err error) {
	err = d.target(need{dma: true}, func(dl *deadline) error {
		d.dmaEligible = false
		if v, e := d.exchangeByte(cmdReadStatus, dl); e != nil {
			return e
		} else if v != resetStatus {
			return transportf("DMA enable requires the normal halted reset status")
		}
		if pc, e := d.pc(dl); e != nil {
			return e
		} else if pc != 0 {
			return transportf("DMA enable requires PC at the reset vector")
		}
		bank, e := d.exchangeByte(cmdGetBank, dl)
		if e != nil {
			return e
		}
		bank &= 7
		if v, e := d.exchangeByte(cmdReadConfig, dl); e != nil {
			return e
		} else if v != resetConfig {
			return transportf("DMA enable requires the unchanged reset debug configuration")
		}
		// An individually observed adapter form with no USB reply, unlike WR_CONFIG on the target.
		if e := d.writePacket([]byte{0x4C, cmdWriteConfig, dmaEnabledConfig}, dl); e != nil {
			return e
		}
		if cfg, e = d.exchangeByte(cmdReadConfig, dl); e != nil {
			return e
		}
		if cfg != dmaEnabledConfig {
			return transportf("DMA configuration readback mismatch; no automatic restoration")
		}
		if pc, e := d.pc(dl); e != nil {
			return e
		} else if pc != 0 {
			return transportf("PC changed during DMA enable; do not resume")
		}
		if b, e := d.exchangeByte(cmdGetBank, dl); e != nil {
			return e
		} else if b&7 != bank {
			return transportf("FMAP changed during DMA enable; do not resume")
		}
		if v, e := d.exchangeByte(cmdReadStatus, dl); e != nil {
			return e
		} else if v != resetStatus {
			return transportf("target state changed during DMA enable; do not resume")
		}
		return nil
	})
	return
}

// ReadBank reads the flash bank (FMAP) selection seen by the debug interface.
func (d *Debugger) ReadBank() (bank uint8, err error) {
	err = d.target(need{}, func(dl *deadline) error {
		v, e := d.exchangeByte(cmdReadStatus, dl)
		if e != nil {
			return e
		}
		if e := checkStatus(Status(v), true, true); e != nil {
			return e
		}
		b, e := d.exchangeByte(cmdGetBank, dl)
		if e != nil {
			return e
		}
		bank = b & 7
		v, e = d.exchangeByte(cmdReadStatus, dl)
		if e != nil {
			return e
		}
		return checkStatus(Status(v), true, true)
	})
	return
}

// ReadPC reads the program counter of the halted CPU.
func (d *Debugger) ReadPC() (pc uint16, err error) {
	err = d.stopped(need{}, func(dl *deadline) (e error) {
		pc, e = d.pc(dl)
		return e
	})
	return
}

func (d *Debugger) registers(dl *deadline) (Registers, error) {
	var r Registers
	var err error
	ins := func(b ...byte) uint8 {
		if err != nil {
			return 0
		}
		var v uint8
		v, err = d.instruction(b, dl)
		return v
	}
	if r.PC, err = d.pc(dl); err != nil {
		return r, err
	}
	bank, err := d.exchangeByte(cmdGetBank, dl)
	if err != nil {
		return r, err
	}
	r.Bank = bank & 7
	r.A = ins(0x00)
	r.PSW = ins(0xE5, 0xD0)
	r.B = ins(0xE5, 0xF0)
	r.SP = ins(0xE5, 0x81)
	dpl0, dph0 := ins(0xE5, 0x82), ins(0xE5, 0x83)
	dpl1, dph1 := ins(0xE5, 0x84), ins(0xE5, 0x85)
	r.DPS, r.MPAGE = ins(0xE5, 0x92), ins(0xE5, 0x93)
	for i := range r.R {
		r.R[i] = ins(0xE8 + byte(i))
	}
	ins(0x74, r.A)
	ins(0x75, 0xD0, r.PSW)
	if err != nil {
		return r, err
	}
	r.DPTR0, r.DPTR1 = uint16(dpl0)|uint16(dph0)<<8, uint16(dpl1)|uint16(dph1)<<8
	return r, nil
}

// ReadRegisters snapshots the CPU registers twice and requires the snapshots
// to agree, proving the inspection did not disturb the CPU.
func (d *Debugger) ReadRegisters() (r Registers, err error) {
	err = d.stopped(need{mem: true}, func(dl *deadline) error {
		first, e := d.registers(dl)
		if e != nil {
			return e
		}
		second, e := d.registers(dl)
		if e != nil {
			return e
		}
		if first != second {
			return transportf("register inspection did not preserve CPU state; do not resume")
		}
		r = first
		return nil
	})
	return
}

// preserve runs fn, then restores DPTR0, DPS, A and PSW and verifies the CPU
// snapshot is unchanged. After a failed fn nothing is restored: the reply
// stream is untrusted.
func (d *Debugger) preserve(dl *deadline, fn func() error) error {
	before, err := d.registers(dl)
	if err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	for _, w := range [][2]byte{{0x82, byte(before.DPTR0)}, {0x83, byte(before.DPTR0 >> 8)},
		{0x92, before.DPS}, {0xE0, before.A}, {0xD0, before.PSW}} {
		if _, err := d.instruction([]byte{0x75, w[0], w[1]}, dl); err != nil {
			return err
		}
	}
	after, err := d.registers(dl)
	if err != nil {
		return err
	}
	if after != before {
		return transportf("memory access did not preserve CPU state; do not resume")
	}
	return nil
}

// ReadSFR reads one reviewed core SFR (not peripheral registers).
func (d *Debugger) ReadSFR(addr uint8) (v uint8, err error) {
	if !readableCoreSFRs[addr] {
		return 0, invalidf("only reviewed core SFRs may be read; peripheral reads can have side effects")
	}
	err = d.stopped(need{mem: true}, func(dl *deadline) error {
		return d.preserve(dl, func() (e error) {
			v, e = d.instruction([]byte{0xE5, addr}, dl)
			return e
		})
	})
	return
}

func memoryRange(addr, length, limit int) error {
	if addr < 0 || addr >= limit {
		return invalidf("memory address 0x%X outside 0..0x%X", addr, limit-1)
	}
	if length < 1 || length > 256 {
		return invalidf("memory length must be 1..256")
	}
	if addr+length > limit {
		return invalidf("memory range crosses the supported address-space boundary 0x%X", limit)
	}
	return nil
}

func (d *Debugger) readMemoryBytes(addr, length int, code bool, dl *deadline) ([]byte, error) {
	if _, err := d.instruction([]byte{0x75, 0x92, 0x00}, dl); err != nil {
		return nil, err
	}
	if _, err := d.instruction([]byte{0x90, byte(addr >> 8), byte(addr)}, dl); err != nil {
		return nil, err
	}
	out := make([]byte, 0, length)
	for i := 0; i < length; i++ {
		op := byte(0xE0)
		if code {
			if _, err := d.instruction([]byte{0xE4}, dl); err != nil {
				return nil, err
			}
			op = 0x93
		}
		v, err := d.instruction([]byte{op}, dl)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if i+1 < length {
			if _, err := d.instruction([]byte{0xA3}, dl); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (d *Debugger) readMemory(addr, length int, code bool) (out []byte, err error) {
	limit := 0x2000
	if code {
		limit = 0x8000
	}
	if err := memoryRange(addr, length, limit); err != nil {
		return nil, err
	}
	err = d.stopped(need{mem: true}, func(dl *deadline) error {
		return d.preserve(dl, func() (e error) {
			out, e = d.readMemoryBytes(addr, length, code, dl)
			return e
		})
	})
	return
}

// ReadXDATA reads 1..256 bytes of XDATA below 0x2000.
func (d *Debugger) ReadXDATA(addr, length int) ([]byte, error) {
	return d.readMemory(addr, length, false)
}

// ReadCode reads 1..256 bytes of the CPU's CODE space below 0x8000 (bank 0
// plus the common area).
func (d *Debugger) ReadCode(addr, length int) ([]byte, error) {
	return d.readMemory(addr, length, true)
}

// FlashSize is the physical CODE range ReadFlashCode covers (CC2530F256,
// excluding the NV and lock pages).
const FlashSize = 0x3E800

// ReadFlashCode reads physical CODE of a CC2530F256 through the FMAP bank
// window, restoring FMAP afterwards. A read must not cross a 32 KiB bank.
func (d *Debugger) ReadFlashCode(addr, length int) ([]byte, error) {
	if err := memoryRange(addr, length, d.flashLimit()); err != nil {
		return nil, err
	}
	if addr/0x8000 != (addr+length-1)/0x8000 {
		return nil, invalidf("physical CODE read crosses a flash bank boundary")
	}
	if addr < 0x8000 {
		return d.ReadCode(addr, length)
	}
	var out []byte
	err := d.stopped(need{mem: true}, func(dl *deadline) error {
		return d.preserve(dl, func() error {
			ins := func(b ...byte) (uint8, error) { return d.instruction(b, dl) }
			memctr, err := ins(0xE5, 0xC7)
			if err != nil {
				return err
			}
			if memctr&0x08 != 0 {
				return transportf("XMAP overlays banked CODE; refusing physical flash read")
			}
			fmap, err := ins(0xE5, 0x9F)
			if err != nil {
				return err
			}
			selected := fmap&0xF8 | byte(addr/0x8000)
			if _, err := ins(0x75, 0x9F, selected); err != nil {
				return err
			}
			if v, err := ins(0xE5, 0x9F); err != nil {
				return err
			} else if v != selected {
				return transportf("physical CODE bank selection failed; do not resume")
			}
			if out, err = d.readMemoryBytes(0x8000|addr&0x7FFF, length, true, dl); err != nil {
				return err
			}
			if v, err := ins(0xE5, 0x9F); err != nil {
				return err
			} else if v != selected {
				return transportf("physical CODE bank changed during read; do not resume")
			}
			if v, err := ins(0xE5, 0xC7); err != nil {
				return err
			} else if v != memctr {
				return transportf("MEMCTR changed during physical CODE read; do not resume")
			}
			if _, err := ins(0x75, 0x9F, fmap); err != nil {
				return err
			}
			if v, err := ins(0xE5, 0x9F); err != nil {
				return err
			} else if v != fmap {
				return transportf("physical CODE bank restoration failed; do not resume")
			}
			return nil
		})
	})
	return out, err
}

// WriteLimitXDATA is the exclusive upper bound of writable XDATA: the area
// from 0x1E00 up is firmware bootstrap and peripheral space.
const WriteLimitXDATA = 0x1E00

// WriteXDATA writes RAM below 0x1E00 with per-byte readback verification. It
// never writes flash or memory-mapped peripherals.
func (d *Debugger) WriteXDATA(addr int, data []byte) error {
	if err := memoryRange(addr, len(data), WriteLimitXDATA); err != nil {
		return err
	}
	return d.stopped(need{mem: true, write: true}, func(dl *deadline) error {
		return d.preserve(dl, func() error {
			if _, err := d.instruction([]byte{0x75, 0x92, 0x00}, dl); err != nil {
				return err
			}
			if _, err := d.instruction([]byte{0x90, byte(addr >> 8), byte(addr)}, dl); err != nil {
				return err
			}
			for i, v := range data {
				if _, err := d.instruction([]byte{0x74, v}, dl); err != nil {
					return err
				}
				if _, err := d.instruction([]byte{0xF0}, dl); err != nil {
					return err
				}
				got, err := d.instruction([]byte{0xE0}, dl)
				if err != nil {
					return err
				}
				if got != v {
					return transportf("XDATA write readback mismatch at 0x%X; do not resume", addr+i)
				}
				if i+1 < len(data) {
					if _, err := d.instruction([]byte{0xA3}, dl); err != nil {
						return err
					}
				}
			}
			return nil
		})
	})
}

// SetBreakpoint configures one of the four unbanked CODE hardware breakpoints.
func (d *Debugger) SetBreakpoint(slot int, addr uint16, enabled bool) (res BreakpointResult, err error) {
	if slot < 0 || slot > 3 {
		return res, invalidf("breakpoint slot must be 0..3")
	}
	if addr > 0x7FFF {
		return res, invalidf("breakpoints take an unbanked CODE address below 0x8000")
	}
	flags := byte(slot) << 4
	if enabled {
		flags |= 0x08
	}
	params := []byte{flags, byte(addr >> 8), byte(addr)}
	err = d.stopped(need{brk: true}, func(dl *deadline) error {
		before, e := d.pc(dl)
		if e != nil {
			return e
		}
		b, e := d.exchange(append([]byte{0xAF, cmdSetHWBreak}, params...), 1, dl)
		if e != nil {
			return e
		}
		after := Status(b[0])
		if e := checkStatus(after, true, true); e != nil {
			return e
		}
		if pc, e := d.pc(dl); e != nil {
			return e
		} else if pc != before {
			return transportf("PC changed while configuring a breakpoint; do not resume")
		}
		res = BreakpointResult{Slot: slot, Address: addr, Enabled: enabled, StatusAfter: after}
		return nil
	})
	return
}

// Halt stops the CPU. It sends HALT only if the CPU is running.
func (d *Debugger) Halt() (res CPUControlResult, err error) {
	err = d.target(need{cpu: true}, func(dl *deadline) error {
		v, e := d.exchangeByte(cmdReadStatus, dl)
		if e != nil {
			return e
		}
		before := Status(v)
		if e := checkStatus(before, true, false); e != nil {
			return e
		}
		res = CPUControlResult{StatusBefore: before, StatusAfter: before, CommandSent: before&CPUHalted == 0}
		if !res.CommandSent {
			return nil
		}
		d.dmaEligible = false
		// A breakpoint can win the race: the HALT reply itself is undefined.
		if _, e := d.exchangeByte(cmdHalt, dl); e != nil {
			return e
		}
		v, e = d.exchangeByte(cmdReadStatus, dl)
		if e != nil {
			return e
		}
		res.StatusAfter = Status(v)
		return checkStatus(res.StatusAfter, true, true)
	})
	return
}

// Resume lets the halted CPU run from its current PC.
func (d *Debugger) Resume() (res CPUControlResult, err error) {
	err = d.target(need{cpu: true}, func(dl *deadline) error {
		if d.needsReset {
			return errPreflightf("flash or RAM was modified by this session; reset the target (reset_halt) before resuming")
		}
		d.dmaEligible = false
		v, e := d.exchangeByte(cmdReadStatus, dl)
		if e != nil {
			return e
		}
		before := Status(v)
		if e := checkStatus(before, true, true); e != nil {
			return e
		}
		if v, e = d.exchangeByte(cmdResume, dl); e != nil {
			return e
		}
		if e := checkStatus(Status(v), true, false); e != nil {
			return e
		}
		if v, e = d.exchangeByte(cmdReadStatus, dl); e != nil {
			return e
		}
		after := Status(v)
		if e := checkStatus(after, true, false); e != nil {
			return e
		}
		if after&CPUHalted != 0 && after&HaltStatus == 0 {
			return transportf("CPU remained halted without a hardware-breakpoint cause after resume")
		}
		res = CPUControlResult{StatusBefore: before, StatusAfter: after, CommandSent: true}
		return nil
	})
	return
}

// Step executes one instruction of the halted CPU.
func (d *Debugger) Step() (res StepResult, err error) {
	err = d.target(need{cpu: true}, func(dl *deadline) error {
		if d.needsReset {
			return errPreflightf("flash or RAM was modified by this session; reset the target (reset_halt) before stepping")
		}
		d.dmaEligible = false
		v, e := d.exchangeByte(cmdReadStatus, dl)
		if e != nil {
			return e
		}
		res.StatusBefore = Status(v)
		if e := checkStatus(res.StatusBefore, true, true); e != nil {
			return e
		}
		if res.Accumulator, e = d.exchangeByte(cmdStepInstr, dl); e != nil {
			return e
		}
		if v, e = d.exchangeByte(cmdReadStatus, dl); e != nil {
			return e
		}
		res.StatusAfter = Status(v)
		return checkStatus(res.StatusAfter, true, true)
	})
	return
}

// WaitHalted polls the debug status until the CPU halts (a breakpoint) or max
// elapses. It needs no permission beyond an attached session.
func (d *Debugger) WaitHalted(max time.Duration) (st Status, pc uint16, err error) {
	if max < time.Millisecond || max > 60*time.Second {
		return 0, 0, invalidf("wait must be between 1ms and 60s")
	}
	if !d.mu.TryLock() {
		return 0, 0, policyf("session is busy; concurrent or reentrant access is not permitted")
	}
	d.mu.Unlock()
	end := d.cfg.Now().Add(max)
	for {
		polled := false
		err = d.target(need{}, func(dl *deadline) error {
			v, e := d.exchangeByte(cmdReadStatus, dl)
			if e != nil {
				return e
			}
			st = Status(v)
			if e := checkStatus(st, true, false); e != nil {
				return e
			}
			if st&CPUHalted != 0 {
				polled = true
				pc, e = d.pc(dl)
			}
			return e
		})
		if err != nil || polled {
			return st, pc, err
		}
		if !d.cfg.Now().Before(end) {
			return st, 0, transportf("CPU did not halt within %s", max)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (d *Debugger) controlWrite(request uint8, value, index uint16, data []byte, dl *deadline) error {
	t, err := dl.remaining()
	if err != nil {
		return err
	}
	n, err := d.tr.ControlWrite(0x40, request, value, index, data, t)
	if err != nil {
		return err
	}
	if n != len(data) {
		return transportf("invalid USB control completion for 0x%02x; command must not be retried", request)
	}
	_, err = dl.remaining()
	return err
}

func (d *Debugger) resetIntoHalt(dl *deadline) (Status, error) {
	d.dmaEligible = false
	d.needsReset = false
	if err := d.controlWrite(0xC9, 0, 1, nil, dl); err != nil {
		return 0, err
	}
	st, err := d.adapterState(dl)
	if err != nil {
		return 0, err
	}
	if st.TargetID != targetCC2530 {
		return 0, transportf("adapter target changed after reset; no further target command sent")
	}
	v, err := d.exchangeByte(cmdReadStatus, dl)
	if err != nil {
		return 0, err
	}
	if err := checkStatus(Status(v), true, true); err != nil {
		return 0, err
	}
	d.dmaEligible = v == resetStatus
	return Status(v), nil
}

// ResetHalt resets the target into debug halt (PC 0). The firmware is
// interrupted and restarts only after Resume.
func (d *Debugger) ResetHalt() (res CPUControlResult, err error) {
	err = d.target(need{reset: true}, func(dl *deadline) error {
		v, e := d.exchangeByte(cmdReadStatus, dl)
		if e != nil {
			return e
		}
		before := Status(v)
		if e := checkStatus(before, true, false); e != nil {
			return e
		}
		after, e := d.resetIntoHalt(dl)
		if e != nil {
			return e
		}
		res = CPUControlResult{StatusBefore: before, StatusAfter: after, CommandSent: true}
		return nil
	})
	return
}

// AttachReset establishes the debug session by resetting the target into
// debug halt. It needs the reset-debug-session policy and target-reset
// permission, and may be done once per session.
func (d *Debugger) AttachReset() (res AttachResult, err error) {
	check := func() error {
		if d.cfg.Access != ResetDebugSession || d.ready {
			return policyf("initial attach requires a fresh explicit reset-debug-session policy")
		}
		if !d.cfg.Permissions.TargetReset {
			return policyf("reset-attach requires separate target-reset permission")
		}
		return nil
	}
	err = d.do(check, func(dl *deadline) error {
		st, e := d.adapterState(dl)
		if e != nil {
			return e
		}
		if st.TargetID != targetCC2530 {
			return transportf("adapter does not report a CC2530 target (0x%04X); no attach command sent", st.TargetID)
		}
		rev, e := d.tr.Revision()
		if e != nil {
			return e
		}
		if _, e := dl.remaining(); e != nil {
			return e
		}
		// Adapter chip-information fields, not a target identity or an 8051 program.
		info := bytes.Repeat([]byte{' '}, 48)
		copy(info[0:], "CC2530")
		copy(info[16:], "DID:")
		copy(info[21:], fmt.Sprintf("%04X", rev))
		if e := d.controlWrite(0xC5, 0, 0, nil, dl); e != nil {
			return e
		}
		if e := d.controlWrite(0xC8, 1, 0, info, dl); e != nil {
			return e
		}
		after, e := d.resetIntoHalt(dl)
		if e != nil {
			return e
		}
		d.ready = true
		res = AttachResult{StatusAfter: after, ResetSent: true}
		return nil
	})
	return
}
