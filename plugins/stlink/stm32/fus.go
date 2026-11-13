package stm32

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
)

// STM32WB5x Firmware Upgrade Service (FUS, ST AN5185) driven from the
// debugger.
//
// CPU1 runs no code of ours. The debugger holds it at its reset vector with
// DEMCR.VC_CORERESET and plays CPU1's part of ST's transport layer (AN5289
// "mailbox"): it writes the reference table and the system/memory-manager
// tables into the non-secure SRAM2a, enables the IPCC clock, sets
// PWR_CR4.C2BOOT, then exchanges system commands (channel 2, CPU1->CPU2) and
// system events (channel 2, CPU2->CPU1, buffers returned on channel 4) by
// polling the IPCC status registers.
//
// The resets the wireless stack and the FUS raise on their own also reset the
// debug logic (observed on an STM32WB55 with FUS 1.1.1: the SWD link fails
// and DEMCR clears, so VC_CORERESET cannot hold CPU1). CPU1 then executes
// whatever its flash holds. An application would boot CPU2 itself and race
// the debugger, so every FUS operation requires CPU1's boot page to hold the
// hwlab park stub (ParkStub: a vector table whose handlers spin): after such
// a reset the debugger re-attaches (Reattach), halts CPU1 and sets
// VC_CORERESET again. Every reset clears C2BOOT, so CPU2 waits until the
// debugger has rebuilt the tables and booted it again: the debugger always
// owns the timing.
//
// Only these FUS commands are ever sent: FUS_GET_STATE, FUS_FW_UPGRADE,
// FUS_FW_DELETE and FUS_START_WS. Anti-rollback activation, key handling,
// FUS_LOCK_USER and option-byte writes are never issued.
const (
	wbSRAM2a     = 0x20030000
	wbSRAM2aEnd  = 0x20038000 // SRAM2a is 32 KiB
	wbRCCAHB3ENR = 0x58000050
	wbIPCCEN     = 1 << 20

	wbRCCCSR   = 0x58000094
	rccRMVF    = 1 << 23
	rccOBLRSTF = 1 << 25
	rccSFTRSTF = 1 << 28

	ipccBase     = 0x58000C00
	ipccC1CR     = ipccBase + 0x00
	ipccC1MR     = ipccBase + 0x04
	ipccC1SCR    = ipccBase + 0x08
	ipccC1TOC2SR = ipccBase + 0x0C
	ipccC2TOC1SR = ipccBase + 0x1C

	ipccSysCh  = 1 << 1 // channel 2: system command / system event
	ipccFreeCh = 1 << 3 // channel 4: memory-manager buffer release

	fusTableKey = 0xA94656B9 // FUS-layout device info table marker

	// FUSImageMagic is the last word of every ST coprocessor image.
	FUSImageMagic = 0xD3A12C5E

	opFUSGetState  = 0xFC52
	opFUSFWUpgrade = 0xFC54
	opFUSFWDelete  = 0xFC55
	opFUSStartWS   = 0xFC5A

	pktSysCmd   = 0x10
	evtCmdDone  = 0x0E
	evtVendor   = 0xFF
	subEvtReady = 0x9200

	// Ready event codes.
	ReadyWirelessStack = 0
	ReadyFUS           = 1

	// FUS states (shci.h SHCI_FUS_GetState_t).
	FUSStateIdle       = 0x00
	FUSStateNotRunning = 0xFE // the wireless stack runs
	FUSStateError      = 0xFF
	fusErrUnknown      = 0xFF
)

// SRAM2a layout written by the debugger (offsets from wbSRAM2a). The
// reference table must sit at the base (FLASH_IPCCBR.IPCCDBA == 0); the rest
// is hwlab's choice and stays well below any secure SRAM2a boundary.
const (
	tblRef       = 0x000 // 10 table pointers
	tblDevInfo   = 0x030 // 0x60
	tblBLE       = 0x090 // each table gets a 0x20 slot
	tblThread    = 0x0B0
	tblSys       = 0x0D0
	tblMM        = 0x0F0
	tblTraces    = 0x110
	tblMAC       = 0x130
	tblZigbee    = 0x150
	tblLLD       = 0x170
	tblBLELLD    = 0x190
	tblSysQueue  = 0x200 // list head {next, prev}
	tblFreeQueue = 0x208
	tblCmdBuf    = 0x210 // 0x110
	tblSpareSys  = 0x320 // 0x110
	tblSpareBLE  = 0x430 // 0x110
	tblPool      = 0x540 // 0x600
	tblPoolSize  = 0x600
	tblEnd       = tblPool + tblPoolSize
)

// Errors.
var (
	// ErrFUS is a FUS refusal or failure; the message carries the state and
	// error code.
	ErrFUS = errors.New("FUS operation failed")
	// ErrFUSUnexpected is a situation the procedure does not model; nothing
	// further was sent.
	ErrFUSUnexpected = errors.New("unexpected FUS behaviour")
	errReset         = errors.New("system reset observed")
	// ErrNotParked: CPU1's boot page does not hold ParkStub.
	ErrNotParked = errors.New("CPU1 is not parked")
)

// Version decodes a ST version word (major<<24 | minor<<16 | sub<<8 |
// branch<<4 | build).
type Version uint32

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", byte(v>>24), byte(v>>16), byte(v>>8))
}

// ParseVersion parses "major.minor.sub" into a Version (branch/build 0).
func ParseVersion(s string) (Version, error) {
	var a, b, c int
	if n, err := fmt.Sscanf(s, "%d.%d.%d", &a, &b, &c); err != nil || n != 3 || a > 255 || b > 255 || c > 255 || a < 0 || b < 0 || c < 0 {
		return 0, fmt.Errorf("version %q: want major.minor.sub", s)
	}
	return Version(uint32(a)<<24 | uint32(b)<<16 | uint32(c)<<8), nil
}

// Same compares major.minor.sub only.
func (v Version) Same(o Version) bool { return v>>8 == o>>8 }

// DeviceInfo is the decoded device information table and the related
// security registers.
type DeviceInfo struct {
	// Layout is "fus" when the FUS wrote the table (it carries the validity
	// key), "wireless_stack" when the stack did, "empty" for zeros.
	Layout       string  `json:"layout"`
	TablePtr     uint32  `json:"table_ptr"`
	FUSVersion   Version `json:"fus_version"`
	FUSMemSize   uint32  `json:"fus_memory_size"`
	StackVersion Version `json:"stack_version"`
	StackMemSize uint32  `json:"stack_memory_size"`
	// InfoStack is the stack type (wireless-stack layout only).
	InfoStack  uint32 `json:"info_stack"`
	StateFlags uint32 `json:"state_flags"`
	SafeBoot   uint32 `json:"safeboot_version"`
	UID64      uint64 `json:"uid64"`
	DevID      uint16 `json:"dev_id"`
	Raw        []byte `json:"raw"`
}

// Security is the boundary and boot state around the coprocessor.
type Security struct {
	SFR, SRRVR, IPCCBR, PWRCR4 uint32
	SecureBase                 uint32 // flash
	SRAM2aSecure               uint32 // first secure SRAM2a address
	C2Boot                     bool
}

// FUS drives the coprocessor's FUS. It is not safe for concurrent use.
type FUS struct {
	C       *cortexm.Core
	FlashKB int
	// Poll is the pause between IPCC status polls; BusyPoll between
	// FUS_GET_STATE while an operation runs. ReadyTimeout bounds the wait
	// for CPU2's ready event after it is booted, CmdTimeout a command.
	Poll, BusyPoll, ReadyTimeout, CmdTimeout time.Duration
	// OpTimeout bounds a whole upgrade/delete.
	OpTimeout time.Duration
	// Log receives progress lines.
	Log []string

	// Reattach reconnects the debug port after a reset that reset it (for
	// ST-LINK: ENTER_SWD again) and checks that the same target answers.
	Reattach func(context.Context) error

	t0         time.Time
	resets     int
	reattaches int
}

// ParkStubAddr is where ParkStub must be programmed: CPU1's boot address.
const ParkStubAddr = FlashBase

// ParkStub is CPU1's boot image during FUS operations: an initial stack
// pointer in SRAM1 and 15 exception vectors that all point at a branch to
// itself (offset 0x40). It touches no peripheral, never sets C2BOOT and
// never writes SRAM2a, so CPU2 and the mailbox stay under the debugger's
// control when a FUS reset lets CPU1 run.
var ParkStub = func() []byte {
	b := make([]byte, 0x48)
	binary.LittleEndian.PutUint32(b, 0x20001000)
	for i := 1; i < 16; i++ {
		binary.LittleEndian.PutUint32(b[4*i:], ParkStubAddr+0x40|1)
	}
	copy(b[0x40:], []byte{0xFE, 0xE7, 0xFE, 0xE7, 0x00, 0xBF, 0x00, 0xBF}) // b . ; b . ; nop ; nop
	return b
}()

// IsParked reports whether CPU1's boot page starts with ParkStub.
func (f *FUS) IsParked(ctx context.Context) (bool, error) {
	b, err := f.C.Read(ctx, ParkStubAddr, len(ParkStub))
	if err != nil {
		return false, err
	}
	return bytes.Equal(b, ParkStub), nil
}

func (f *FUS) requireParked(ctx context.Context) error {
	ok, err := f.IsParked(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: CPU1's boot page (0x%08x) does not hold the hwlab park stub. The resets the FUS raises reset the debug logic, "+
			"so CPU1 would run its application, which may boot CPU2 itself; park CPU1 first (fus_park_cpu1, with a verified backup)", ErrNotParked, uint32(ParkStubAddr))
	}
	return nil
}

// relink re-attaches the debug port after a persistent access failure.
func (f *FUS) relink(ctx context.Context, cause error) bool {
	if f.Reattach == nil || f.reattaches >= 20 {
		return false
	}
	f.reattaches++
	if err := f.Reattach(ctx); err != nil {
		f.logf("debug link lost (%v); re-attach failed: %v", cause, err)
		return false
	}
	f.logf("debug link lost (%v); re-attached", cause)
	return true
}

// NewFUS returns a driver with hardware-oriented timing.
func NewFUS(c *cortexm.Core, flashKB int) *FUS {
	return &FUS{C: c, FlashKB: flashKB, Poll: 10 * time.Millisecond, BusyPoll: time.Second,
		ReadyTimeout: 10 * time.Second, CmdTimeout: 5 * time.Second, OpTimeout: 10 * time.Minute, t0: time.Now()}
}

func (f *FUS) logf(format string, a ...any) {
	f.Log = append(f.Log, fmt.Sprintf("%7.3fs ", time.Since(f.t0).Seconds())+fmt.Sprintf(format, a...))
}

func (f *FUS) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// rd reads a word, retrying for about a second: the debug link may hiccup
// across a system reset the FUS raises on its own.
func (f *FUS) rd(ctx context.Context, addr uint32) (uint32, error) {
	var last error
	relinked := false
	for i := 0; i < 50; i++ {
		v, err := f.C.Bus.Read32(ctx, addr)
		if err == nil {
			return v, nil
		}
		last = err
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if i >= 4 && !relinked {
			relinked = f.relink(ctx, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, fmt.Errorf("reading 0x%08x: %w", addr, last)
}

func (f *FUS) wr(ctx context.Context, addr, v uint32) error { return f.C.Bus.Write32(ctx, addr, v) }

// ReadSecurity reads the coprocessor boundary registers. Read only.
func (f *FUS) ReadSecurity(ctx context.Context) (Security, error) {
	var s Security
	var err error
	for _, r := range []struct {
		a uint32
		d *uint32
	}{{wbSFR, &s.SFR}, {wbSRRVR, &s.SRRVR}, {wbIPCCBR, &s.IPCCBR}, {wbPWRCR4, &s.PWRCR4}} {
		if *r.d, err = f.rd(ctx, r.a); err != nil {
			return s, err
		}
	}
	s.SecureBase = FlashBase + (s.SFR&0xFF)*wbPage
	if end := FlashBase + uint32(f.FlashKB)<<10; s.SecureBase > end {
		s.SecureBase = end
	}
	s.SRAM2aSecure = wbSRAM2a + (s.SRRVR>>18&0x1F)<<10
	if s.SRRVR>>23&1 != 0 { // BRSD: SRAM2a secure area disabled
		s.SRAM2aSecure = wbSRAM2aEnd
	}
	s.C2Boot = s.PWRCR4&wbC2BOOT != 0
	return s, nil
}

// ReadDeviceInfo decodes the device information table the reference table
// points at. Read only. When CPU2 has not run since the last reset the
// table is whatever the previous owner left there.
func (f *FUS) ReadDeviceInfo(ctx context.Context) (DeviceInfo, error) {
	var d DeviceInfo
	ipccbr, err := f.rd(ctx, wbIPCCBR)
	if err != nil {
		return d, err
	}
	if ipccbr&0x3FFF != 0 {
		return d, fmt.Errorf("%w: FLASH_IPCCBR 0x%08x moves the mailbox away from 0x%08x; not supported", ErrFUSUnexpected, ipccbr, uint32(wbSRAM2a))
	}
	ptr, err := f.rd(ctx, wbSRAM2a+tblRef)
	if err != nil {
		return d, err
	}
	if !DeviceInfoPtrValid(ptr) {
		return DeviceInfo{TablePtr: ptr, Layout: "none"}, nil
	}
	raw, err := f.C.Read(ctx, ptr, DeviceInfoLen)
	if err != nil {
		return d, err
	}
	return DecodeDeviceInfo(ptr, raw), nil
}

// Coprocessor mailbox locations shared with other transports (the ROM USB
// DFU bootloader reads the same SRAM2a words).
const (
	SRAM2aBase = wbSRAM2a
	SRAM2aEnd  = wbSRAM2aEnd
	// RefTable holds the pointer to the device information table.
	RefTable = wbSRAM2a + tblRef
	// DeviceInfoLen is how many bytes of the device information table are
	// decoded.
	DeviceInfoLen = 0x60
)

// DeviceInfoPtrValid reports whether a reference-table pointer can address
// a device information table inside SRAM2a.
func DeviceInfoPtrValid(ptr uint32) bool {
	return ptr >= wbSRAM2a && ptr+DeviceInfoLen <= wbSRAM2aEnd && ptr&3 == 0
}

// DecodeDeviceInfo decodes DeviceInfoLen bytes of a device information
// table read from ptr. It is pure, so every transport decodes identically.
func DecodeDeviceInfo(ptr uint32, raw []byte) DeviceInfo {
	d := DeviceInfo{TablePtr: ptr}
	if !DeviceInfoPtrValid(ptr) || len(raw) < DeviceInfoLen {
		d.Layout = "none"
		return d
	}
	raw = raw[:DeviceInfoLen]
	d.Raw = raw
	w := func(o int) uint32 { return binary.LittleEndian.Uint32(raw[o:]) }
	switch {
	case w(0) == fusTableKey:
		d.Layout = "fus"
		d.StateFlags, d.SafeBoot, d.FUSVersion, d.FUSMemSize = w(4), w(8), Version(w(12)), w(16)
		d.StackVersion, d.StackMemSize = Version(w(20)), w(24)
		d.UID64 = binary.LittleEndian.Uint64(raw[40:])
		d.DevID = binary.LittleEndian.Uint16(raw[48:])
	case bytes.Equal(raw[:32], make([]byte, 32)):
		d.Layout = "empty"
	default:
		d.Layout = "wireless_stack"
		d.SafeBoot, d.FUSVersion, d.FUSMemSize = w(0), Version(w(4)), w(8)
		d.StackVersion, d.StackMemSize, d.InfoStack = Version(w(16)), w(20), w(24)
	}
	return d
}

// Hold sets DEMCR.VC_CORERESET so that every system reset (ours or the
// FUS's) leaves CPU1 halted at its reset vector; the returned function
// restores the original DEMCR.
func (f *FUS) Hold(ctx context.Context) (func(), error) {
	demcr, err := f.rd(ctx, cortexm.AddrDEMCR)
	if err != nil {
		return nil, err
	}
	if err := f.wr(ctx, cortexm.AddrDEMCR, demcr|cortexm.DEMCRVCCoreReset); err != nil {
		return nil, err
	}
	return func() { _ = f.wr(context.WithoutCancel(ctx), cortexm.AddrDEMCR, demcr) }, nil
}

// SystemReset resets the whole chip with CPU1 held at its reset vector.
// That clears C2BOOT: CPU2 stays stopped until Boot.
func (f *FUS) SystemReset(ctx context.Context) error {
	if _, err := f.C.Reset(ctx, cortexm.ResetSystem, true); err != nil {
		return fmt.Errorf("system reset: %w", err)
	}
	// cortexm.Reset restores DEMCR without VC_CORERESET; set it again for
	// the resets the FUS will raise.
	demcr, err := f.rd(ctx, cortexm.AddrDEMCR)
	if err != nil {
		return err
	}
	if err := f.wr(ctx, cortexm.AddrDEMCR, demcr|cortexm.DEMCRVCCoreReset); err != nil {
		return err
	}
	sec, err := f.ReadSecurity(ctx)
	if err != nil {
		return err
	}
	if sec.C2Boot {
		return fmt.Errorf("%w: PWR_CR4.C2BOOT still reads set after a system reset", ErrFUSUnexpected)
	}
	f.logf("system reset done, CPU1 halted, CPU2 stopped (SFSA 0x%02x; %s)", sec.SFR&0xFF, f.resetCause(ctx))
	return nil
}

// afterReset is called when C2BOOT was found clear while CPU2 should run:
// the FUS or the wireless stack reset the chip. That reset may have reset
// the debug logic too, letting CPU1 run (the park stub): re-attach, halt
// CPU1 and arm VC_CORERESET again.
func (f *FUS) afterReset(ctx context.Context) error {
	f.resets++
	if f.resets > 40 {
		return fmt.Errorf("%w: more than 40 system resets in one operation", ErrFUSUnexpected)
	}
	var st cortexm.State
	var err error
	relinked := false
	for i := 0; i < 50; i++ {
		if st, err = f.C.Status(ctx); err == nil {
			break
		}
		if i >= 4 && !relinked {
			relinked = f.relink(ctx, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("debug link lost after a system reset: %w", err)
	}
	how := "CPU1 held at its reset vector"
	if !st.DebugEnabled || !st.Halted {
		// Only the park stub may have run; anything else is a stop.
		if _, err := f.C.Halt(ctx); err != nil {
			return fmt.Errorf("halting CPU1 after a system reset: %w", err)
		}
		parked, perr := f.IsParked(ctx)
		if perr != nil || !parked {
			return fmt.Errorf("%w: after a system reset CPU1 ran (DHCSR 0x%08x) and its boot page is not the park stub (%v); halted it now and stopped. "+
				"Check the state with fus_info before retrying", ErrFUSUnexpected, st.DHCSR, perr)
		}
		how = "the reset also reset the debug logic; CPU1 ran the park stub, halted it"
	}
	demcr, err := f.rd(ctx, cortexm.AddrDEMCR)
	if err != nil {
		return err
	}
	if demcr&cortexm.DEMCRVCCoreReset == 0 {
		if err := f.wr(ctx, cortexm.AddrDEMCR, demcr|cortexm.DEMCRVCCoreReset); err != nil {
			return err
		}
	}
	f.logf("system reset #%d observed (%s; %s)", f.resets, how, f.resetCause(ctx))
	return nil
}

// resetCause reads and clears the RCC_CSR reset flags (diagnostics only).
func (f *FUS) resetCause(ctx context.Context) string {
	v, err := f.C.Bus.Read32(ctx, wbRCCCSR)
	if err != nil {
		return "RCC_CSR unreadable"
	}
	_ = f.wr(ctx, wbRCCCSR, v|rccRMVF)
	var names []string
	for bit, n := range map[uint]string{25: "OBL", 26: "PIN", 27: "BOR", 28: "SFT", 29: "IWDG", 30: "WWDG", 31: "LPWR"} {
		if v&(1<<bit) != 0 {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return fmt.Sprintf("RCC_CSR 0x%08x reset flags %v", v, names)
}

// tables builds the SRAM2a image.
func tables() []byte {
	b := make([]byte, tblEnd)
	put := func(off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
	a := func(off int) uint32 { return wbSRAM2a + uint32(off) }
	for i, t := range []int{tblDevInfo, tblBLE, tblThread, tblSys, tblMM, tblTraces, tblMAC, tblZigbee, tblLLD, tblBLELLD} {
		put(tblRef+4*i, a(t))
	}
	put(tblSys, a(tblCmdBuf))
	put(tblSys+4, a(tblSysQueue))
	// MB_MemManagerTable_t: spare_ble, spare_sys, blepool, blepoolsize,
	// pevt_free_buffer_queue, traces_evt_pool, tracespoolsize.
	put(tblMM, a(tblSpareBLE))
	put(tblMM+4, a(tblSpareSys))
	put(tblMM+8, a(tblPool))
	put(tblMM+12, tblPoolSize)
	put(tblMM+16, a(tblFreeQueue))
	for _, q := range []int{tblSysQueue, tblFreeQueue} {
		put(q, a(q))
		put(q+4, a(q))
	}
	return b
}

// Boot rebuilds the mailbox, boots CPU2 and returns its ready code
// (ReadyFUS or ReadyWirelessStack). CPU1 must be halted and C2BOOT clear.
func (f *FUS) Boot(ctx context.Context) (byte, error) {
	st, err := f.C.Status(ctx)
	if err != nil {
		return 0, err
	}
	if !st.Halted {
		return 0, fmt.Errorf("%w: CPU1 must be halted", ErrFUSUnexpected)
	}
	sec, err := f.ReadSecurity(ctx)
	if err != nil {
		return 0, err
	}
	if sec.C2Boot {
		return 0, fmt.Errorf("%w: CPU2 is already booted (PWR_CR4 0x%08x): only a fresh system reset gives a defined mailbox", ErrFUSUnexpected, sec.PWRCR4)
	}
	if sec.IPCCBR&0x3FFF != 0 {
		return 0, fmt.Errorf("%w: FLASH_IPCCBR 0x%08x: the mailbox is not at 0x%08x", ErrFUSUnexpected, sec.IPCCBR, uint32(wbSRAM2a))
	}
	if wbSRAM2a+tblEnd > sec.SRAM2aSecure {
		return 0, fmt.Errorf("%w: the mailbox (0x%08x..0x%08x) would overlap the secure SRAM2a from 0x%08x (FLASH_SRRVR 0x%08x)",
			ErrFUSUnexpected, uint32(wbSRAM2a), uint32(wbSRAM2a+tblEnd), sec.SRAM2aSecure, sec.SRRVR)
	}
	en, err := f.rd(ctx, wbRCCAHB3ENR)
	if err != nil {
		return 0, err
	}
	if err := f.wr(ctx, wbRCCAHB3ENR, en|wbIPCCEN); err != nil {
		return 0, err
	}
	if err := f.C.WriteVerified(ctx, wbSRAM2a, tables()); err != nil {
		return 0, fmt.Errorf("writing the mailbox tables: %w", err)
	}
	// Interrupts are not used; clear any stale CPU2->CPU1 flags.
	if err := f.wr(ctx, ipccC1CR, 0); err != nil {
		return 0, err
	}
	if err := f.wr(ctx, ipccC1SCR, 0x3F); err != nil {
		return 0, err
	}
	cr4, err := f.rd(ctx, wbPWRCR4)
	if err != nil {
		return 0, err
	}
	if err := f.wr(ctx, wbPWRCR4, cr4|wbC2BOOT); err != nil {
		return 0, err
	}
	f.logf("mailbox written, CPU2 booted")
	deadline := time.Now().Add(f.ReadyTimeout)
	for {
		ev, err := f.event(ctx, time.Until(deadline))
		if err != nil {
			return 0, err
		}
		if ev.sub == subEvtReady && len(ev.payload) >= 1 {
			switch ev.payload[0] {
			case ReadyFUS:
				f.logf("ready event: FUS running")
			case ReadyWirelessStack:
				f.logf("ready event: wireless stack running")
			default:
				return 0, fmt.Errorf("%w: ready event with code 0x%02x", ErrFUSUnexpected, ev.payload[0])
			}
			return ev.payload[0], nil
		}
		f.logf("ignoring system event 0x%02x/0x%04x before the ready event", ev.code, ev.sub)
	}
}

type sysEvent struct {
	code    byte
	sub     uint16
	payload []byte
}

// checkC2 returns errReset when C2BOOT reads clear (a system reset).
func (f *FUS) checkC2(ctx context.Context) error {
	cr4, err := f.rd(ctx, wbPWRCR4)
	if err != nil {
		return err
	}
	if cr4&wbC2BOOT == 0 {
		return errReset
	}
	return nil
}

// event waits for one system event, removes it from the queue and returns
// its buffer to CPU2.
func (f *FUS) event(ctx context.Context, timeout time.Duration) (sysEvent, error) {
	deadline := time.Now().Add(timeout)
	for {
		if err := f.checkC2(ctx); err != nil {
			return sysEvent{}, err
		}
		sr, err := f.rd(ctx, ipccC2TOC1SR)
		if err != nil {
			return sysEvent{}, err
		}
		if sr&ipccSysCh != 0 {
			break
		}
		if time.Now().After(deadline) {
			return sysEvent{}, fmt.Errorf("%w: no system event from CPU2 within %s", ErrFUSUnexpected, timeout)
		}
		if err := f.sleep(ctx, f.Poll); err != nil {
			return sysEvent{}, err
		}
	}
	head := uint32(wbSRAM2a + tblSysQueue)
	node, err := f.rd(ctx, head)
	if err != nil {
		return sysEvent{}, err
	}
	if node == head {
		// Flag without a queued event: acknowledge and wait again.
		if err := f.wr(ctx, ipccC1SCR, ipccSysCh); err != nil {
			return sysEvent{}, err
		}
		return f.event(ctx, time.Until(deadline))
	}
	if node < wbSRAM2a || node+0x10 > wbSRAM2aEnd {
		return sysEvent{}, fmt.Errorf("%w: system event queue points at 0x%08x", ErrFUSUnexpected, node)
	}
	hdr, err := f.C.Read(ctx, node, 12)
	if err != nil {
		return sysEvent{}, err
	}
	next := binary.LittleEndian.Uint32(hdr[0:])
	ev := sysEvent{code: hdr[9]}
	plen := int(hdr[10])
	if plen > 0 {
		p, err := f.C.Read(ctx, node+11, plen)
		if err != nil {
			return sysEvent{}, err
		}
		if ev.code == evtVendor && plen >= 2 {
			ev.sub = binary.LittleEndian.Uint16(p)
			ev.payload = p[2:]
		} else {
			ev.payload = p
		}
	}
	// LST_remove_head: head.next = node.next; node.next.prev = head.
	if err := f.wr(ctx, head, next); err != nil {
		return ev, err
	}
	if err := f.wr(ctx, next+4, head); err != nil {
		return ev, err
	}
	if err := f.release(ctx, node); err != nil {
		return ev, err
	}
	if n, err := f.rd(ctx, head); err == nil && n == head {
		if err := f.wr(ctx, ipccC1SCR, ipccSysCh); err != nil { // CH2C: event consumed
			return ev, err
		}
	}
	return ev, nil
}

// release returns an event buffer to CPU2 (TL_MM_EvtDone + SendFreeBuf).
func (f *FUS) release(ctx context.Context, node uint32) error {
	deadline := time.Now().Add(time.Second)
	for {
		sr, err := f.rd(ctx, ipccC1TOC2SR)
		if err != nil {
			return err
		}
		if sr&ipccFreeCh == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: CPU2 did not take back released buffers within 1 s", ErrFUSUnexpected)
		}
		if err := f.sleep(ctx, f.Poll); err != nil {
			return err
		}
	}
	q := uint32(wbSRAM2a + tblFreeQueue)
	tail, err := f.rd(ctx, q+4)
	if err != nil {
		return err
	}
	// LST_insert_tail.
	for _, w := range [][2]uint32{{node, q}, {node + 4, tail}, {q + 4, node}, {tail, node}} {
		if err := f.wr(ctx, w[0], w[1]); err != nil {
			return err
		}
	}
	return f.wr(ctx, ipccC1SCR, ipccFreeCh<<16) // CH4S
}

// command sends one system command and returns the command-complete
// payload after the opcode (status, ...). errReset when CPU2 reset meanwhile.
func (f *FUS) command(ctx context.Context, op uint16, payload []byte) ([]byte, error) {
	if err := f.checkC2(ctx); err != nil {
		return nil, err
	}
	sr, err := f.rd(ctx, ipccC1TOC2SR)
	if err != nil {
		return nil, err
	}
	if sr&ipccSysCh != 0 {
		return nil, fmt.Errorf("%w: the system command channel is still occupied", ErrFUSUnexpected)
	}
	// The response overwrites the buffer from offset 0 (it has no packet
	// header); zero that part so a stale response cannot be mistaken.
	pkt := make([]byte, (12+len(payload)+3)&^3)
	pkt[8] = pktSysCmd
	binary.LittleEndian.PutUint16(pkt[9:], op)
	pkt[11] = byte(len(payload))
	copy(pkt[12:], payload)
	if err := f.C.Write(ctx, wbSRAM2a+tblCmdBuf, pkt); err != nil {
		return nil, err
	}
	if err := f.wr(ctx, ipccC1SCR, ipccSysCh<<16); err != nil { // CH2S
		return nil, err
	}
	deadline := time.Now().Add(f.CmdTimeout)
	for {
		if err := f.checkC2(ctx); err != nil {
			return nil, err
		}
		sr, err := f.rd(ctx, ipccC1TOC2SR)
		if err != nil {
			return nil, err
		}
		if sr&ipccSysCh == 0 {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: no response to command 0x%04x within %s", ErrFUSUnexpected, op, f.CmdTimeout)
		}
		if err := f.sleep(ctx, f.Poll); err != nil {
			return nil, err
		}
	}
	rsp, err := f.C.Read(ctx, wbSRAM2a+tblCmdBuf, 16)
	if err != nil {
		return nil, err
	}
	if rsp[1] != evtCmdDone || binary.LittleEndian.Uint16(rsp[4:]) != op || rsp[2] < 3 {
		return nil, fmt.Errorf("%w: response to 0x%04x is not a matching command complete (% x)", ErrFUSUnexpected, op, rsp[:8])
	}
	n := int(rsp[2]) - 3
	if n > 10 {
		n = 10
	}
	return rsp[6 : 6+n], nil
}

// GetState sends FUS_GET_STATE and returns (state, error code).
func (f *FUS) GetState(ctx context.Context) (byte, byte, error) {
	p, err := f.command(ctx, opFUSGetState, nil)
	if err != nil {
		return 0, 0, err
	}
	if len(p) == 0 {
		return 0, 0, fmt.Errorf("%w: FUS_GET_STATE response without a state", ErrFUSUnexpected)
	}
	ec := byte(0)
	if len(p) > 1 {
		ec = p[1]
	}
	return p[0], ec, nil
}

// FUSErrorName names a FUS error code (shci.h).
func FUSErrorName(c byte) string {
	switch c {
	case 0x00:
		return "NO_ERROR"
	case 0x01:
		return "IMG_NOT_FOUND"
	case 0x02:
		return "IMG_CORRUPT"
	case 0x03:
		return "IMG_NOT_AUTHENTIC"
	case 0x04:
		return "NOT_ENOUGH_SPACE"
	case 0x05:
		return "IMAGE_USRABORT"
	case 0x06:
		return "IMAGE_ERSERROR"
	case 0x07:
		return "IMAGE_WRTERROR"
	case 0x08:
		return "AUTH_TAG_ST_NOTFOUND"
	case 0x09:
		return "AUTH_TAG_CUST_NOTFOUND"
	case 0x0A:
		return "AUTH_KEY_LOCKED"
	case 0x11:
		return "FW_ROLLBACK_ERROR"
	case 0xFE:
		return "STATE_NOT_RUNNING"
	case 0xFF:
		return "ERR_UNKNOWN"
	}
	return fmt.Sprintf("0x%02x", c)
}

// EnterFUS resets the chip and brings CPU2 into the FUS: when the wireless
// stack starts instead, it sends FUS_GET_STATE twice, which makes the stack
// reboot into the FUS (AN5185). Returns the FUS state (idle unless an
// operation is pending) and the device info.
func (f *FUS) EnterFUS(ctx context.Context) (byte, DeviceInfo, error) {
	if err := f.SystemReset(ctx); err != nil {
		return 0, DeviceInfo{}, err
	}
	if err := f.requireParked(ctx); err != nil {
		return 0, DeviceInfo{}, err
	}
	unknown := 0
	for attempt := 1; attempt <= 6; attempt++ {
		code, err := f.Boot(ctx)
		if errors.Is(err, errReset) {
			if err := f.afterReset(ctx); err != nil {
				return 0, DeviceInfo{}, err
			}
			continue
		}
		if err != nil {
			return 0, DeviceInfo{}, err
		}
		if code == ReadyWirelessStack {
			if di, err := f.ReadDeviceInfo(ctx); err == nil {
				f.logf("wireless stack %s running (FUS %s); requesting FUS with two FUS_GET_STATE", di.StackVersion, di.FUSVersion)
			}
			reset := false
			for i := 0; i < 2 && !reset; i++ {
				st, ec, err := f.GetState(ctx)
				switch {
				case errors.Is(err, errReset):
					reset = true
				case err != nil:
					return 0, DeviceInfo{}, err
				default:
					f.logf("FUS_GET_STATE from the stack: state 0x%02x error %s", st, FUSErrorName(ec))
				}
			}
			for t := time.Now().Add(5 * time.Second); !reset && time.Now().Before(t); {
				if err := f.checkC2(ctx); errors.Is(err, errReset) {
					reset = true
				} else if err != nil {
					return 0, DeviceInfo{}, err
				} else if err := f.sleep(ctx, 50*time.Millisecond); err != nil {
					return 0, DeviceInfo{}, err
				}
			}
			if reset {
				if err := f.afterReset(ctx); err != nil {
					return 0, DeviceInfo{}, err
				}
			} else {
				f.logf("the stack did not reboot into the FUS within 5 s; resetting and retrying")
				if err := f.SystemReset(ctx); err != nil {
					return 0, DeviceInfo{}, err
				}
			}
			continue
		}
		st, ec, err := f.GetState(ctx)
		if errors.Is(err, errReset) {
			if err := f.afterReset(ctx); err != nil {
				return 0, DeviceInfo{}, err
			}
			continue
		}
		if err != nil {
			return 0, DeviceInfo{}, err
		}
		f.logf("FUS state 0x%02x, error %s", st, FUSErrorName(ec))
		if st == FUSStateError && ec == fusErrUnknown && unknown < 2 {
			// First FUS start in the product's life (AN5185): a reset
			// initialises it.
			unknown++
			f.logf("FUS reports ERR_UNKNOWN (first start); resetting once more")
			if err := f.SystemReset(ctx); err != nil {
				return 0, DeviceInfo{}, err
			}
			continue
		}
		di, err := f.ReadDeviceInfo(ctx)
		if err != nil {
			return st, di, err
		}
		if di.Layout != "fus" {
			return st, di, fmt.Errorf("%w: the FUS is running but the device info table has the %s layout", ErrFUSUnexpected, di.Layout)
		}
		f.logf("FUS %s running; installed stack %s", di.FUSVersion, di.StackVersion)
		return st, di, nil
	}
	return 0, DeviceInfo{}, fmt.Errorf("%w: CPU2 did not settle in the FUS after 6 boots", ErrFUSUnexpected)
}

// WaitIdle polls FUS_GET_STATE through the FUS's own resets until it is idle
// (FUSStateIdle) or the wireless stack runs (FUSStateNotRunning). An error
// state fails.
//
// With needActivity, an idle state is only accepted after the FUS showed it
// started (a busy state or a reset) or after a 15 s grace period: the FUS
// acknowledges a request before acting on it.
func (f *FUS) WaitIdle(ctx context.Context, needActivity bool) (byte, error) {
	deadline := time.Now().Add(f.OpTimeout)
	grace := time.Now().Add(15 * time.Second)
	active := !needActivity
	last := byte(0xAA)
	for time.Now().Before(deadline) {
		st, ec, err := f.GetState(ctx)
		if errors.Is(err, errReset) {
			active = true
			if err := f.afterReset(ctx); err != nil {
				return 0, err
			}
			code, err := f.Boot(ctx)
			if errors.Is(err, errReset) {
				continue // the next GetState sees C2BOOT clear again
			}
			if err != nil {
				return 0, err
			}
			if code == ReadyWirelessStack {
				// Do not send FUS_GET_STATE now: twice would re-enter the FUS.
				return FUSStateNotRunning, nil
			}
			continue
		}
		if err != nil {
			return 0, err
		}
		if st != last {
			f.logf("FUS state 0x%02x error %s", st, FUSErrorName(ec))
			last = st
		}
		switch {
		case st == FUSStateIdle && !active && time.Now().Before(grace):
			if err := f.sleep(ctx, f.BusyPoll); err != nil {
				return 0, err
			}
		case st == FUSStateIdle, st == FUSStateNotRunning:
			return st, nil
		case st >= 0x10 && st <= 0x3F:
			active = true
			if err := f.sleep(ctx, f.BusyPoll); err != nil {
				return 0, err
			}
		case st == FUSStateError:
			return st, fmt.Errorf("%w: FUS error state, code %s", ErrFUS, FUSErrorName(ec))
		default:
			return st, fmt.Errorf("%w: FUS state 0x%02x", ErrFUSUnexpected, st)
		}
	}
	return 0, fmt.Errorf("%w: the FUS operation did not finish within %s", ErrFUSUnexpected, f.OpTimeout)
}

// CheckImage validates an ST coprocessor image: non-empty, a multiple of 4
// bytes, ending with the footer magic.
func CheckImage(img []byte) error {
	if len(img) < 64 || len(img)%4 != 0 {
		return fmt.Errorf("image of %d bytes: want a non-trivial multiple of 4", len(img))
	}
	if m := binary.LittleEndian.Uint32(img[len(img)-4:]); m != FUSImageMagic {
		return fmt.Errorf("image does not end with the ST coprocessor footer magic 0x%08x (found 0x%08x)", uint32(FUSImageMagic), m)
	}
	return nil
}

// StagingAddress is the tightest placement of an image below the secure
// start: the highest page-aligned address at which it still ends below it,
// (secure start - size) rounded down to a page. It reproduces ST's release
// notes for wireless-stack images on a part with only the FUS installed
// (stm32wb5x_Mac_802_15_4_fw.bin, 74100 bytes, below 0x080F4000 ->
// 0x080E1000), and with no stack installed the FUS installs the stack
// where it was downloaded. FUS images in the release notes sit lower
// (FUS 1.2.0: 0x080EC000, 0x8000 below 0x080F4000), so callers pass FUS
// image addresses explicitly.
func StagingAddress(secureBase uint32, size int) uint32 {
	return (secureBase - uint32(size)) &^ (wbPage - 1)
}

// Stage erases [addr, secure start) and programs the image (padded with
// 0xFF to a double-word) at addr, then reads it back. CPU2 must not be
// booted (the flash driver checks it).
func (f *FUS) Stage(ctx context.Context, fl *Flash, addr uint32, img []byte) error {
	if addr%wbPage != 0 || addr < FlashBase || uint64(addr)+uint64(len(img)) > uint64(fl.P.SecureBase) {
		return fmt.Errorf("%w: staging address 0x%08x for %d bytes is not page aligned below the secure start 0x%08x", ErrRange, addr, len(img), fl.P.SecureBase)
	}
	for a := addr; a < fl.P.SecureBase; a += wbPage {
		s, ok := fl.P.SectorAt(a)
		if !ok {
			return fmt.Errorf("%w: no page at 0x%08x", ErrRange, a)
		}
		if err := fl.EraseSector(ctx, s); err != nil {
			return fmt.Errorf("erasing 0x%08x: %w", a, err)
		}
	}
	data := append([]byte(nil), img...)
	for len(data)%8 != 0 {
		data = append(data, 0xFF)
	}
	for off := 0; off < len(data); off += 4096 {
		end := min(off+4096, len(data))
		if err := fl.Program(ctx, addr+uint32(off), data[off:end]); err != nil {
			return fmt.Errorf("programming 0x%08x: %w", addr+uint32(off), err)
		}
	}
	got, err := f.C.Read(ctx, addr, len(img))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, img) {
		return fmt.Errorf("%w: the staged image does not read back", ErrVerify)
	}
	f.logf("staged %d bytes at 0x%08x, erased up to 0x%08x", len(img), addr, fl.P.SecureBase)
	return nil
}

// Request sends one FUS command (FW_UPGRADE, FW_DELETE, START_WS) with the
// retry embassy uses after a delete (the FUS may refuse briefly).
func (f *FUS) Request(ctx context.Context, op uint16, tries int) error {
	for i := 1; ; i++ {
		p, err := f.command(ctx, op, nil)
		if err != nil {
			return err
		}
		if len(p) > 0 && p[0] == 0 {
			f.logf("command 0x%04x accepted", op)
			return nil
		}
		st := byte(0xFF)
		if len(p) > 0 {
			st = p[0]
		}
		if i >= tries {
			return fmt.Errorf("%w: command 0x%04x refused with status 0x%02x", ErrFUS, op, st)
		}
		f.logf("command 0x%04x refused with status 0x%02x; retrying", op, st)
		if err := f.sleep(ctx, f.BusyPoll); err != nil {
			return err
		}
	}
}

// Opcodes for Request.
const (
	OpFWUpgrade = opFUSFWUpgrade
	OpFWDelete  = opFUSFWDelete
	OpStartWS   = opFUSStartWS
)

// StartStack sends FUS_START_WS from an idle FUS and waits for the reset
// and the wireless stack's ready event. It never sends FUS_GET_STATE
// afterwards (two of them would re-enter the FUS).
func (f *FUS) StartStack(ctx context.Context) error {
	err := f.Request(ctx, OpStartWS, 1)
	if err != nil && !errors.Is(err, errReset) {
		return err
	}
	deadline := time.Now().Add(f.ReadyTimeout)
	for {
		if err := f.checkC2(ctx); errors.Is(err, errReset) {
			break
		} else if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: no system reset within %s after FUS_START_WS", ErrFUSUnexpected, f.ReadyTimeout)
		}
		if err := f.sleep(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
	if err := f.afterReset(ctx); err != nil {
		return err
	}
	for i := 0; i < 3; i++ {
		code, err := f.Boot(ctx)
		if errors.Is(err, errReset) {
			if err := f.afterReset(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if code != ReadyWirelessStack {
			return fmt.Errorf("%w: CPU2 came back with the FUS instead of the wireless stack", ErrFUS)
		}
		return nil
	}
	return fmt.Errorf("%w: CPU2 kept resetting after FUS_START_WS", ErrFUSUnexpected)
}

// IsResetErr reports the internal reset marker (for callers that run their
// own command sequences).
func IsResetErr(err error) bool { return errors.Is(err, errReset) }

// OpResult describes a FUS operation.
type OpResult struct {
	Before, After       DeviceInfo
	SecBefore, SecAfter Security
	StagedAt            uint32
	StagedBytes         int
	FinalState          byte
}

// Check lets the caller verify the state before anything is changed.
type Check func(DeviceInfo, Security) error

func (f *FUS) enterIdle(ctx context.Context) (DeviceInfo, Security, error) {
	st, di, err := f.EnterFUS(ctx)
	if err != nil {
		return di, Security{}, err
	}
	if st != FUSStateIdle {
		return di, Security{}, fmt.Errorf("%w: the FUS is not idle (state 0x%02x); nothing was changed", ErrFUSUnexpected, st)
	}
	sec, err := f.ReadSecurity(ctx)
	return di, sec, err
}

// Upgrade installs a FUS or wireless-stack image: enter the FUS and run
// pre (nothing changed yet), reset, stage the image below the secure start
// (addr 0 = StagingAddress), re-enter the FUS, check nothing else moved,
// send FUS_FW_UPGRADE, follow the FUS through its resets until idle, then
// reset and re-enter the FUS to read the result. The caller verifies the
// result.
func (f *FUS) Upgrade(ctx context.Context, img []byte, addr uint32, pre Check) (OpResult, error) {
	var r OpResult
	if err := CheckImage(img); err != nil {
		return r, err
	}
	var err error
	if r.Before, r.SecBefore, err = f.enterIdle(ctx); err != nil {
		return r, err
	}
	if pre != nil {
		if err := pre(r.Before, r.SecBefore); err != nil {
			return r, err
		}
	}
	if err := f.SystemReset(ctx); err != nil {
		return r, err
	}
	p, err := Detect(ctx, f.C)
	if err != nil {
		return r, err
	}
	if p.SecureBase != r.SecBefore.SecureBase {
		return r, fmt.Errorf("%w: the secure start moved from 0x%08x to 0x%08x across a reset", ErrFUSUnexpected, r.SecBefore.SecureBase, p.SecureBase)
	}
	if addr == 0 {
		addr = StagingAddress(p.SecureBase, len(img))
	}
	r.StagedAt, r.StagedBytes = addr, len(img)
	if err := f.Stage(ctx, NewFlash(f.C, p, 0), addr, img); err != nil {
		return r, err
	}
	mid, sec, err := f.enterIdle(ctx)
	if err != nil {
		return r, err
	}
	if mid.FUSVersion != r.Before.FUSVersion || mid.StackVersion != r.Before.StackVersion || sec.SFR != r.SecBefore.SFR {
		return r, fmt.Errorf("%w: versions or SFSA changed while staging (FUS %s->%s, stack %s->%s, SFR 0x%x->0x%x)", ErrFUSUnexpected,
			r.Before.FUSVersion, mid.FUSVersion, r.Before.StackVersion, mid.StackVersion, r.SecBefore.SFR, sec.SFR)
	}
	if err := f.Request(ctx, OpFWUpgrade, 5); err != nil && !errors.Is(err, errReset) {
		return r, err
	}
	if r.FinalState, err = f.WaitIdle(ctx, true); err != nil {
		return r, err
	}
	if r.After, r.SecAfter, err = f.enterIdle(ctx); err != nil {
		return r, err
	}
	return r, nil
}

// DeleteStack sends FUS_FW_DELETE, waits until the device info table
// reports no stack (resetting before that would abort the delete), waits
// for the FUS to be idle, then resets and re-enters the FUS to read the
// result.
func (f *FUS) DeleteStack(ctx context.Context, pre Check) (OpResult, error) {
	var r OpResult
	var err error
	if r.Before, r.SecBefore, err = f.enterIdle(ctx); err != nil {
		return r, err
	}
	if pre != nil {
		if err := pre(r.Before, r.SecBefore); err != nil {
			return r, err
		}
	}
	if r.Before.StackVersion == 0 {
		return r, fmt.Errorf("%w: no wireless stack is installed", ErrFUS)
	}
	if err := f.Request(ctx, OpFWDelete, 1); err != nil && !errors.Is(err, errReset) {
		return r, err
	}
	deadline := time.Now().Add(f.OpTimeout)
	for {
		if err := f.checkC2(ctx); errors.Is(err, errReset) {
			if err := f.afterReset(ctx); err != nil {
				return r, err
			}
			if _, err := f.Boot(ctx); err != nil && !errors.Is(err, errReset) {
				return r, err
			}
			continue
		} else if err != nil {
			return r, err
		}
		di, err := f.ReadDeviceInfo(ctx)
		if err != nil {
			return r, err
		}
		if di.Layout == "fus" && di.StackVersion == 0 {
			f.logf("device info table reports no wireless stack")
			break
		}
		if time.Now().After(deadline) {
			return r, fmt.Errorf("%w: the stack was not deleted within %s", ErrFUSUnexpected, f.OpTimeout)
		}
		if err := f.sleep(ctx, 100*time.Millisecond); err != nil {
			return r, err
		}
	}
	if r.FinalState, err = f.WaitIdle(ctx, false); err != nil {
		return r, err
	}
	if r.After, r.SecAfter, err = f.enterIdle(ctx); err != nil {
		return r, err
	}
	return r, nil
}
