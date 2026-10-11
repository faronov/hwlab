package esp

import (
	"context"

	"github.com/faronov/hwlab/internal/riscv"
)

// Watchdog registers. While a hart is halted its firmware cannot feed the
// watchdogs, and a long flash operation would otherwise reset the chip.
const (
	wdtKey = 0x50D83AA1

	timg0 = 0x60008000
	timg1 = 0x60009000

	timgWDTConfig0 = 0x48
	timgWDTFeed    = 0x60
	timgWDTProtect = 0x64

	lpWDT        = 0x600B1C00
	lpWDTConfig0 = 0x00
	lpWDTConfig1 = 0x04
	lpWDTFeed    = 0x14
	lpWDTProtect = 0x18

	wdtEnable = 1 << 31
)

// Quiesced remembers the watchdog configuration to put back.
type Quiesced struct {
	TIMG0, TIMG1, LP uint32
}

// Quiesce disables the two timer-group watchdogs and the RTC watchdog and
// returns what they were. Restore must be called afterwards (it also feeds
// them, so the firmware gets a full period before the next expiry).
func Quiesce(ctx context.Context, dm *riscv.DM) (Quiesced, error) {
	r, err := dm.BusBatch(ctx, []riscv.BusOp{
		{Kind: riscv.BusRead, Addr: timg0 + timgWDTConfig0, Words: 1},
		{Kind: riscv.BusRead, Addr: timg1 + timgWDTConfig0, Words: 1},
		{Kind: riscv.BusRead, Addr: lpWDT + lpWDTConfig0, Words: 1},
	})
	if err != nil {
		return Quiesced{}, err
	}
	q := Quiesced{TIMG0: r[0][0], TIMG1: r[1][0], LP: r[2][0]}
	_, err = dm.BusBatch(ctx, []riscv.BusOp{
		{Kind: riscv.BusWrite, Addr: timg0 + timgWDTProtect, Vals: []uint32{wdtKey}},
		{Kind: riscv.BusWrite, Addr: timg0 + timgWDTConfig0, Vals: []uint32{q.TIMG0 &^ wdtEnable}},
		{Kind: riscv.BusWrite, Addr: timg0 + timgWDTProtect, Vals: []uint32{0}},
		{Kind: riscv.BusWrite, Addr: timg1 + timgWDTProtect, Vals: []uint32{wdtKey}},
		{Kind: riscv.BusWrite, Addr: timg1 + timgWDTConfig0, Vals: []uint32{q.TIMG1 &^ wdtEnable}},
		{Kind: riscv.BusWrite, Addr: timg1 + timgWDTProtect, Vals: []uint32{0}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTProtect, Vals: []uint32{wdtKey}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTConfig0, Vals: []uint32{q.LP &^ wdtEnable}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTProtect, Vals: []uint32{0}},
	})
	return q, err
}

// Restore feeds and re-enables whatever Quiesce disabled.
func (q Quiesced) Restore(ctx context.Context, dm *riscv.DM) error {
	_, err := dm.BusBatch(ctx, []riscv.BusOp{
		{Kind: riscv.BusWrite, Addr: timg0 + timgWDTProtect, Vals: []uint32{wdtKey}},
		{Kind: riscv.BusWrite, Addr: timg0 + timgWDTFeed, Vals: []uint32{1}},
		{Kind: riscv.BusWrite, Addr: timg0 + timgWDTConfig0, Vals: []uint32{q.TIMG0}},
		{Kind: riscv.BusWrite, Addr: timg0 + timgWDTProtect, Vals: []uint32{0}},
		{Kind: riscv.BusWrite, Addr: timg1 + timgWDTProtect, Vals: []uint32{wdtKey}},
		{Kind: riscv.BusWrite, Addr: timg1 + timgWDTFeed, Vals: []uint32{1}},
		{Kind: riscv.BusWrite, Addr: timg1 + timgWDTConfig0, Vals: []uint32{q.TIMG1}},
		{Kind: riscv.BusWrite, Addr: timg1 + timgWDTProtect, Vals: []uint32{0}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTProtect, Vals: []uint32{wdtKey}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTFeed, Vals: []uint32{1 << 31}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTConfig0, Vals: []uint32{q.LP}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTProtect, Vals: []uint32{0}},
	})
	return err
}

// ArmSoCReset starts the RTC watchdog with stage 0 = "reset system" and a
// short timeout (about 0.3 s on the slow clock). The caller resumes the hart;
// the whole digital system, including the USB Serial/JTAG peripheral, then
// resets and the host sees the device disappear and re-enumerate.
func ArmSoCReset(ctx context.Context, dm *riscv.DM) error {
	const stage0ResetSystem = 3
	_, err := dm.BusBatch(ctx, []riscv.BusOp{
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTProtect, Vals: []uint32{wdtKey}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTConfig1, Vals: []uint32{40000}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTConfig0, Vals: []uint32{wdtEnable | stage0ResetSystem<<28 | 1<<13 | 1<<12}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTFeed, Vals: []uint32{1 << 31}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTProtect, Vals: []uint32{0}},
	})
	return err
}

// SoCResetArmed reports whether the RTC watchdog is still enabled with stage 0
// set to "reset system". Boot code that runs after a resume (ROM, bootloader,
// application start-up) disables it, so a reset requested while the hart is
// parked in early boot has to be re-armed until it sticks.
func SoCResetArmed(ctx context.Context, dm *riscv.DM) (bool, error) {
	r, err := dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusRead, Addr: lpWDT + lpWDTConfig0, Words: 1}})
	if err != nil {
		return false, err
	}
	v := r[0][0]
	return v&wdtEnable != 0 && (v>>28)&7 == 3, nil
}
