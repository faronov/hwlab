// Package ccsim models a CC23xx device behind an SWD debug port for tests and
// probe replay: the three access ports (AHB-AP, CFG-AP, SEC-AP), the SACI
// boot state machine and a Cortex-M0+ core with flash, SRAM, CCFG and the
// identification registers. It is a behavioural model written from the
// documented register facts, not a copy of any firmware or tool.
//
// The probe simulators (plugins/xds110/xds, plugins/cmsisdap/dap) delegate
// access-port transfers to a Device. A Device is not safe for concurrent use;
// the owning simulator serialises calls.
package ccsim

import (
	"context"
	"encoding/binary"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx/ccfg"
	"github.com/faronov/hwlab/internal/ti/cc23xx/saci"
)

// Identification values of the modelled device.
const (
	DeviceID   = 0x1BB8402F
	PartID     = 0x80012DDA
	DescEx     = 0x08FFB000 // 512 KiB, two banks
	FlashBytes = 512 * 1024
	SRAMBytes  = 0x9000

	RomAddr = 0x0F002DC8 // where the modelled boot ROM waits after EXIT_SACI_HALT

	idrAHB = 0x84770001
	idrCfg = 0x002E0001
	idrSec = 0x002E0000
)

// Boot states the model reports in BOOTSTA.
const (
	BootSACI    = 0x20
	BootWaiting = 0x81
	BootAppRun  = 0xC2
)

// Device is one simulated CC23xx.
type Device struct {
	Core *cortexm.Sim
	SACI *saci.Sim

	// NeverExit makes the boot ROM ignore SACI exit commands (device stuck).
	NeverExit bool

	inSACI     bool
	bootsta    uint8
	seenResets int
	seenSACI   int
	fault      bool
	AppStarts  int

	jump bool // the boot ROM branches to the application on the next instruction
}

// New returns a device with a valid, debug-open CCFG and a deterministic
// application image whose reset handler is at 0x8934, in SACI state.
func New() *Device {
	d := &Device{Core: cortexm.NewSimM0Plus(), SACI: saci.NewSim(FlashBytes)}
	fl := d.SACI.Flash
	binary.LittleEndian.PutUint32(fl[0:], 0x20009000)
	binary.LittleEndian.PutUint32(fl[4:], 0x8935)
	for i := 2; i < 48; i++ {
		binary.LittleEndian.PutUint32(fl[4*i:], 0x8AFD)
	}
	for off := 0x100; off+4 <= 0x10000; off += 2 {
		hw := uint16(0xBF00)
		switch (off / 2) % 5 {
		case 1:
			hw = 0xF000
		case 2:
			hw = 0xF800
		}
		binary.LittleEndian.PutUint16(fl[off:], hw)
	}
	c := d.SACI.CCFG
	binary.LittleEndian.PutUint32(c[0x00:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(c[0x04:], 0)
	binary.LittleEndian.PutUint32(c[0x08:], 0)
	binary.LittleEndian.PutUint32(c[0x18:], 0xAAAAAAAA)
	c[0x7D0] = ccfg.AuthDebugOpen
	_ = ccfg.Finalize(c)
	d.SACI.CCFGValid = true

	d.Core.Regions = []*cortexm.Region{
		{Base: 0, Data: fl, ROM: true},
		{Base: 0x20000000, Data: make([]byte, SRAMBytes)},
		{Base: ccfg.BaseAddr, Data: c, ROM: true},
	}
	d.Core.Hooks = append(d.Core.Hooks, func(addr uint32, write bool, v uint32) (uint32, bool, error) {
		if write {
			return 0, false, nil
		}
		switch addr {
		case 0x40004000 + 0x3FC:
			return DeviceID, true, nil
		case 0x40004000 + 0x7F8:
			return PartID, true, nil
		case 0x40024004:
			return DescEx, true, nil
		}
		return 0, false, nil
	})
	d.seenResets = d.Core.Resets
	d.Core.Exec = func(m cortexm.Machine) bool {
		if !d.jump {
			return false
		}
		d.jump = false
		m.SetReg(15, binary.LittleEndian.Uint32(d.SACI.Flash[4:])&^1)
		return true
	}
	d.enterSACI()
	return d
}

func (d *Device) enterSACI() {
	d.inSACI, d.bootsta = true, BootSACI
	d.SACI.Exited = ""
	d.SACI.SessionTainted = false
	d.seenSACI = d.SACI.Resets
	d.seenResets = d.Core.Resets
}

// PinReset models a falling edge on nRESET.
func (d *Device) PinReset() {
	_ = d.Core.Write32(context.Background(), cortexm.AddrAIRCR, 0x05FA0004)
	d.sync()
}

// InSACI reports whether the boot ROM is waiting in SACI.
func (d *Device) InSACI() bool { d.sync(); return d.inSACI }

// BootSta returns the modelled BOOTSTA.
func (d *Device) BootSta() uint8 { d.sync(); return d.bootsta }

// TakeFault reports and clears a pending access fault (AHB-AP access while
// the AHB-AP is closed).
func (d *Device) TakeFault() bool { f := d.fault; d.fault = false; return f }

func (d *Device) halted() bool {
	v, _ := d.Core.Read32(context.Background(), cortexm.AddrDHCSR)
	return v&cortexm.DHCSRSHalt != 0
}

// sync advances the boot state machine to match what the core and the SACI
// mailbox have done since the last access.
func (d *Device) sync() {
	if d.Core.Resets != d.seenResets || d.SACI.Resets != d.seenSACI {
		d.enterSACI()
		return
	}
	if d.inSACI && d.SACI.Exited != "" && !d.NeverExit {
		ctx := context.Background()
		mode := d.SACI.Exited
		d.inSACI = false
		d.SACI.Exited = ""
		if mode == saci.CmdExitSACIHalt.String() {
			d.bootsta = BootWaiting
			_ = d.Core.Write32(ctx, cortexm.AddrDHCSR, 0xA05F0003)
			d.Core.SetReg(3, 1)
			d.Core.SetReg(15, RomAddr)
			return
		}
		d.bootsta = BootAppRun
		d.AppStarts++
		return
	}
	if !d.inSACI && d.bootsta == BootWaiting && !d.halted() && d.Core.Reg(3) == 0 {
		d.bootsta = BootAppRun
		d.AppStarts++
		d.jump = true
	}
}

// Read handles a read of AP apsel register reg (bank included). handled is
// false when the access belongs to the AHB-AP and the caller's memory model.
func (d *Device) Read(apsel uint8, reg uint32) (v uint32, handled bool) {
	d.sync()
	switch apsel {
	case 0:
		if !d.inSACI {
			if reg == 0xFC {
				return idrAHB, true
			}
			return 0, false
		}
		if reg == 0x0C {
			d.fault = true
		}
		return 0, true
	case 1:
		switch reg {
		case 0x00:
			return DeviceID, true
		case 0x04:
			return PartID, true
		case 0x08:
			return 0x10000050, true
		case 0x0C:
			base := uint32(0x030D0005)
			if d.inSACI {
				base = 0x020D0005
			}
			return base | uint32(d.bootsta)<<8, true
		case 0xFC:
			return idrCfg, true
		}
		return 0, true
	case 2:
		switch reg {
		case 0xFC:
			return idrSec, true
		case 0x04, 0x08, 0x0C:
			v, _ := d.SACI.ReadReg(byte(reg))
			d.sync()
			return v, true
		}
		return 0, true
	}
	return 0, true
}

// Write handles a write to AP apsel; see Read.
func (d *Device) Write(apsel uint8, reg, v uint32) bool {
	d.sync()
	switch apsel {
	case 0:
		if !d.inSACI {
			return false
		}
		if reg == 0x0C {
			d.fault = true
		}
		return true
	case 2:
		if reg <= 0x0C {
			_ = d.SACI.WriteReg(byte(reg), v)
			d.sync()
		}
		return true
	}
	return true
}
