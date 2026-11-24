package rom

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Entry is how the host resets the chip and selects its boot mode.
type Entry string

const (
	// EntryClassic is the two-transistor auto-reset circuit of USB-UART
	// bridges (Espressif DevKit schematics): RTS drives EN (CHIP_PU), DTR
	// the boot strap (GPIO0 on the ESP32, GPIO9/8 on newer chips).
	// EN is low only for (DTR=0, RTS=1), the strap only for (DTR=1, RTS=0);
	// equal levels release both, so opening a port never resets the chip.
	// The S3 tbr-bridge/tbr-hub maps CDC 0's DTR/RTS to the H2's EN/BOOT the
	// same way.
	EntryClassic Entry = "classic"
	// EntryUSJ is the chip's own USB-Serial/JTAG CDC-ACM port (ESP32-S3 TRM
	// v1.8 table 33.3-2, the same on the C3/C6/H2): (RTS=0, DTR=0) clears the
	// download-mode flag, (0, 1) sets it, (1, 0) resets the chip, (1, 1)
	// does nothing; the chip boots into download mode when the flag is set at
	// the reset.
	EntryUSJ Entry = "usj"
	// EntryNone leaves the lines alone: the chip is already in download mode
	// (strap held at reset by hand) or a stub is still running.
	EntryNone Entry = "none"
)

// Entries lists the supported entries.
var Entries = []Entry{EntryClassic, EntryUSJ, EntryNone}

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
	return "", fmt.Errorf("entry %q: want %s", s, strings.Join(names, ", "))
}

// Wired reports whether the entry drives the modem lines.
func (e Entry) Wired() bool { return e == EntryClassic || e == EntryUSJ }

// Lines sets DTR and RTS (true = asserted); atomic reports that both changed
// in one operation.
type Lines func(dtr, rts bool) (atomic bool, err error)

// Timing of the line sequences. Variables so tests can shorten them.
var (
	// ResetHold keeps the chip in reset (EN low / USJ reset state).
	ResetHold = 100 * time.Millisecond
	// StrapHold keeps the boot strap low after EN is released, so the ROM
	// samples it even behind a slow EN RC; a second attempt uses
	// StrapHoldLong (boards with a large EN capacitor).
	StrapHold     = 100 * time.Millisecond
	StrapHoldLong = 550 * time.Millisecond
	// USJStep separates the USJ line states (TRM 33.4-3).
	USJStep = 100 * time.Millisecond
)

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

// Step is one line state and how long it is held. Reset marks the state
// that resets the chip.
type Step struct {
	DTR, RTS bool
	Hold     time.Duration
	Reset    bool
}

// LineError is a failed line change. AfterReset reports that the resetting
// state had already been applied: a USB-Serial/JTAG port can vanish right
// after it, which is not a failed reset.
type LineError struct {
	Err        error
	DTR, RTS   bool
	AfterReset bool
}

func (e *LineError) Error() string {
	return fmt.Sprintf("set DTR=%v RTS=%v: %v", e.DTR, e.RTS, e.Err)
}

func (e *LineError) Unwrap() error { return e.Err }

func run(ctx context.Context, set Lines, steps []Step) (bool, error) {
	atomic, reset := true, false
	for _, s := range steps {
		a, err := set(s.DTR, s.RTS)
		if err != nil {
			return false, &LineError{Err: err, DTR: s.DTR, RTS: s.RTS, AfterReset: reset}
		}
		reset = reset || s.Reset
		atomic = atomic && a
		if s.Hold > 0 {
			if err := sleepCtx(ctx, s.Hold); err != nil {
				return atomic, err
			}
		}
	}
	return atomic, nil
}

// EnterSteps is the line sequence that resets the chip into the ROM
// download mode; long selects StrapHoldLong.
func EnterSteps(e Entry, long bool) []Step {
	switch e {
	case EntryClassic:
		hold := StrapHold
		if long {
			hold = StrapHoldLong
		}
		// Idle, EN low (strap released), then EN released and the strap low
		// in ONE change: a two-step change would pass through (0,0) or (1,1),
		// which releases the strap and boots the application.
		return []Step{{false, false, 0, false}, {false, true, ResetHold, true}, {true, false, hold, false}, {false, false, 0, false}}
	case EntryUSJ:
		// TRM 33.4-3: set the download flag, pass through (1,1) (no action)
		// so the flag is never cleared, reset, then idle (which clears the
		// flag for the next reset; the chip already booted into download).
		return []Step{{false, false, USJStep, false}, {true, false, USJStep, false}, {true, true, 0, false}, {false, true, ResetHold, true}, {false, false, 0, false}}
	}
	return nil
}

// RunSteps is the line sequence that resets the chip into its application.
func RunSteps(e Entry) []Step {
	switch e {
	case EntryClassic:
		return []Step{{false, false, 0, false}, {false, true, ResetHold, true}, {false, false, 0, false}}
	case EntryUSJ:
		// TRM 33.4-4: clear the download flag, reset, release.
		return []Step{{false, false, USJStep, false}, {false, true, ResetHold, true}, {false, false, 0, false}}
	}
	return nil
}

// EnterBootloader resets the chip into the ROM download mode.
func EnterBootloader(ctx context.Context, set Lines, e Entry, long bool) (bool, error) {
	if !e.Wired() {
		return false, nil
	}
	return run(ctx, set, EnterSteps(e, long))
}

// HardReset resets the chip into its application (the flash image).
func HardReset(ctx context.Context, set Lines, e Entry) (bool, error) {
	if !e.Wired() {
		return false, fmt.Errorf("entry %q drives no reset line: reset the board by hand", e)
	}
	return run(ctx, set, RunSteps(e))
}
