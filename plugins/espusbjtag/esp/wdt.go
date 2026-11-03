package esp

import (
	"context"
	"errors"

	"github.com/faronov/hwlab/internal/riscv"
)

// Watchdog registers. While a hart is halted its firmware cannot feed the
// watchdogs, and a long flash operation would otherwise reset the chip. The
// bases and the LP_WDT offsets are per chip (Chip.WDT); the timer-group
// offsets, the write-protect key and the enable bit are common to C6 and H2.
const (
	wdtKey = 0x50D83AA1

	timgWDTConfig0 = 0x48
	timgWDTFeed    = 0x60
	timgWDTProtect = 0x64

	wdtEnable = 1 << 31

	swdAutoFeed = 1 << 18 // LP_WDT SWD_CONF.SWD_AUTO_FEED_EN (esp32h2 PAC)
)

var errNoChip = errors.New("no chip profile: refusing to touch watchdog registers of an unidentified chip")

// Quiesced remembers the watchdog configuration to put back.
type Quiesced struct {
	TIMG0, TIMG1, LP uint32
	// SWD is the SWD_CONF value before Quiesce (only for profiles with
	// WDT.SWDConf set).
	SWD  uint32
	chip *Chip
}

// Quiesce disables the two timer-group watchdogs and the RTC watchdog and
// returns what they were. Restore must be called afterwards (it also feeds
// them, so the firmware gets a full period before the next expiry).
func Quiesce(ctx context.Context, dm Bus, c *Chip) (Quiesced, error) {
	if c == nil {
		return Quiesced{}, errNoChip
	}
	timg0, timg1, lpWDT := c.WDT.TIMG0, c.WDT.TIMG1, c.WDT.LP
	lpWDTConfig0, lpWDTProtect := c.WDT.LPConfig0, c.WDT.LPProtect
	r, err := dm.BusBatch(ctx, []riscv.BusOp{
		{Kind: riscv.BusRead, Addr: timg0 + timgWDTConfig0, Words: 1},
		{Kind: riscv.BusRead, Addr: timg1 + timgWDTConfig0, Words: 1},
		{Kind: riscv.BusRead, Addr: lpWDT + lpWDTConfig0, Words: 1},
	})
	if err != nil {
		return Quiesced{}, err
	}
	q := Quiesced{TIMG0: r[0][0], TIMG1: r[1][0], LP: r[2][0], chip: c}
	if c.WDT.SWDConf != 0 {
		v, err := ReadWord(ctx, dm, lpWDT+c.WDT.SWDConf)
		if err != nil {
			return Quiesced{}, err
		}
		q.SWD = v
	}
	_, err = dm.BusBatch(ctx, append([]riscv.BusOp{
		{Kind: riscv.BusWrite, Addr: timg0 + timgWDTProtect, Vals: []uint32{wdtKey}},
		{Kind: riscv.BusWrite, Addr: timg0 + timgWDTConfig0, Vals: []uint32{q.TIMG0 &^ wdtEnable}},
		{Kind: riscv.BusWrite, Addr: timg0 + timgWDTProtect, Vals: []uint32{0}},
		{Kind: riscv.BusWrite, Addr: timg1 + timgWDTProtect, Vals: []uint32{wdtKey}},
		{Kind: riscv.BusWrite, Addr: timg1 + timgWDTConfig0, Vals: []uint32{q.TIMG1 &^ wdtEnable}},
		{Kind: riscv.BusWrite, Addr: timg1 + timgWDTProtect, Vals: []uint32{0}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTProtect, Vals: []uint32{wdtKey}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTConfig0, Vals: []uint32{q.LP &^ wdtEnable}},
		{Kind: riscv.BusWrite, Addr: lpWDT + lpWDTProtect, Vals: []uint32{0}},
	}, swdOps(c, q.SWD|c.swdAutoFeed())...))
	return q, err
}

func (c *Chip) swdAutoFeed() uint32 {
	if c.WDT.SWDAutoFeed != 0 {
		return c.WDT.SWDAutoFeed
	}
	return swdAutoFeed
}

func (c *Chip) swdKey() uint32 {
	if c.WDT.SWDKey != 0 {
		return c.WDT.SWDKey
	}
	return wdtKey
}

// swdOps writes SWD_CONF under its write-protect key; nothing for profiles
// that leave the super watchdog alone.
func swdOps(c *Chip, conf uint32) []riscv.BusOp {
	if c.WDT.SWDConf == 0 {
		return nil
	}
	lp := c.WDT.LP
	return []riscv.BusOp{
		{Kind: riscv.BusWrite, Addr: lp + c.WDT.SWDProtect, Vals: []uint32{c.swdKey()}},
		{Kind: riscv.BusWrite, Addr: lp + c.WDT.SWDConf, Vals: []uint32{conf}},
		{Kind: riscv.BusWrite, Addr: lp + c.WDT.SWDProtect, Vals: []uint32{0}},
	}
}

// Restore feeds and re-enables whatever Quiesce disabled.
func (q Quiesced) Restore(ctx context.Context, dm Bus) error {
	c := q.chip
	if c == nil {
		return errNoChip
	}
	timg0, timg1, lpWDT := c.WDT.TIMG0, c.WDT.TIMG1, c.WDT.LP
	lpWDTConfig0, lpWDTFeed, lpWDTProtect := c.WDT.LPConfig0, c.WDT.LPFeed, c.WDT.LPProtect
	_, err := dm.BusBatch(ctx, append([]riscv.BusOp{
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
	}, swdOps(c, q.SWD)...))
	return err
}

// ArmSoCReset starts the RTC watchdog with stage 0 = "reset system" and a
// short timeout (about 0.3 s on the slow clock). The caller resumes the hart;
// the whole digital system, including the USB Serial/JTAG peripheral, then
// resets and the host sees the device disappear and re-enumerate.
func ArmSoCReset(ctx context.Context, dm Bus, c *Chip) error {
	if c == nil {
		return errNoChip
	}
	lpWDT := c.WDT.LP
	lpWDTConfig0, lpWDTConfig1, lpWDTFeed, lpWDTProtect := c.WDT.LPConfig0, c.WDT.LPConfig1, c.WDT.LPFeed, c.WDT.LPProtect
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
func SoCResetArmed(ctx context.Context, dm Bus, c *Chip) (bool, error) {
	if c == nil {
		return false, errNoChip
	}
	r, err := dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusRead, Addr: c.WDT.LP + c.WDT.LPConfig0, Words: 1}})
	if err != nil {
		return false, err
	}
	v := r[0][0]
	return v&wdtEnable != 0 && (v>>28)&7 == 3, nil
}
