package bsl

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Entry is how the chip is put into its ROM bootloader.
type Entry string

const (
	// EntryManual: the user holds the BSL button (DIO15 on a ZBDongle-P)
	// while resetting; the lines are left alone.
	EntryManual Entry = "manual"
	// EntryDTRRTS: RTS drives RESET and DTR the backdoor pin through the
	// two-transistor circuit of Sonoff ZBDongle-P style boards (CP2102N):
	// (DTR=0,RTS=1) holds RESET low, (DTR=1,RTS=0) pulls the pin low, equal
	// levels release both.
	EntryDTRRTS Entry = "dtr-rts"
	// EntryRTSDTR is the same circuit with the roles swapped (DTR drives
	// RESET, RTS the backdoor pin).
	EntryRTSDTR Entry = "rts-dtr"
)

// Entries lists the supported entry methods.
var Entries = []Entry{EntryManual, EntryDTRRTS, EntryRTSDTR}

// ParseEntry checks an entry name.
func ParseEntry(s string) (Entry, error) {
	for _, e := range Entries {
		if string(e) == s {
			return e, nil
		}
	}
	names := make([]string, len(Entries))
	for i, e := range Entries {
		names[i] = string(e)
	}
	return "", fmt.Errorf("bsl_entry %q: want %s", s, strings.Join(names, ", "))
}

// Wired reports whether the entry drives the modem lines.
func (e Entry) Wired() bool { return e == EntryDTRRTS || e == EntryRTSDTR }

// Lines sets DTR and RTS; atomic reports one simultaneous change.
type Lines func(dtr, rts bool) (atomic bool, err error)

// Timing of the line sequence, measured on a CC2652P ZBDongle-P (CP2102N).
// Variables so tests can shorten them.
var (
	EntryStep = 100 * time.Millisecond
	EntryHold = 500 * time.Millisecond
)

// line levels (DTR, RTS) for each role.
func (e Entry) levels() (bslLow, resetLow, idle [2]bool) {
	bslLow, resetLow = [2]bool{true, false}, [2]bool{false, true}
	if e == EntryRTSDTR {
		bslLow, resetLow = resetLow, bslLow
	}
	return bslLow, resetLow, [2]bool{false, false}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Enter runs the reset sequence that starts the ROM bootloader: backdoor
// pin low, then RESET low with the pin released, then RESET released and
// the pin low in ONE change of both lines, held while the ROM samples it.
// Changing the lines one by one passes through "reset released, pin high"
// and boots the application instead. It returns whether the line changes
// were atomic.
func Enter(ctx context.Context, set Lines, e Entry) (atomic bool, err error) {
	if !e.Wired() {
		return false, nil
	}
	bslLow, resetLow, _ := e.levels()
	atomic = true
	for _, st := range []struct {
		lv [2]bool
		d  time.Duration
	}{{bslLow, EntryStep}, {resetLow, EntryStep}, {bslLow, EntryHold}} {
		a, err := set(st.lv[0], st.lv[1])
		if err != nil {
			return false, fmt.Errorf("set DTR/RTS: %w", err)
		}
		atomic = atomic && a
		if err := sleepCtx(ctx, st.d); err != nil {
			return atomic, err
		}
	}
	return atomic, nil
}

// Release returns both lines to the idle level (RESET and pin released).
func Release(set Lines, e Entry) error {
	if !e.Wired() {
		return nil
	}
	_, _, idle := e.levels()
	_, err := set(idle[0], idle[1])
	return err
}
