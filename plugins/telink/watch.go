package telinkplugin

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/faronov/hwlab/plugins/telink/sws"
)

const (
	maxWatchBytes   = 256
	maxWatchMs      = 300000
	maxWatchChanges = 200
	maxWatchSegs    = 500
	maxPrimeWrites  = 4
	maxPrimeBytes   = 64
	maxWatchErrRun  = 50
)

type primeWrite struct {
	addr uint32
	data []byte
}

// checkPrimes needs the chip family, which only an awake target reports, so
// it runs at the first sample that answers.
func (d *device) checkPrimes(primes []primeWrite) error {
	start, end, err := d.sramWindow()
	if err != nil {
		return err
	}
	for i, w := range primes {
		if w.addr < start || uint64(w.addr)+uint64(len(w.data)) > uint64(end) {
			return argErr(fmt.Errorf("prime[%d]: 0x%06x+%d is outside SRAM (0x%06x-0x%06x)", i, w.addr, len(w.data), start, end-1))
		}
	}
	return nil
}

func (d *device) primeArgs(a map[string]any) ([]primeWrite, error) {
	v, ok := a["prime"]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("prime: want a list of {address, data}")
	}
	if len(list) > maxPrimeWrites {
		return nil, fmt.Errorf("prime: at most %d writes", maxPrimeWrites)
	}
	out := make([]primeWrite, 0, len(list))
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("prime[%d]: want {address, data}", i)
		}
		addr, err := addr24(m, "address")
		if err != nil {
			return nil, fmt.Errorf("prime[%d]: %w", i, err)
		}
		data, err := hexData(m, "data", maxPrimeBytes)
		if err != nil {
			return nil, fmt.Errorf("prime[%d]: %w", i, err)
		}
		out = append(out, primeWrite{addr, data})
	}
	return out, nil
}

// asleep reports whether err is the programmer's SWire read timeout, which is
// what a target in deep sleep (or powered off) answers.
func asleep(err error) bool {
	var se *sws.Error
	return errors.As(err, &se) && se.Code == sws.ErrRead
}

// primeOnWake makes the prime writes on a target that just answered. With
// haltOnWake it stalls the CPU first, because a sleepy target is awake for a
// few milliseconds only, and always lets it run again before returning. It
// returns nil when the target fell asleep before the writes could start.
func (d *device) primeOnWake(primes []primeWrite, haltOnWake bool, checked *bool, t0 time.Time) (res map[string]any, err error) {
	ms := func(t time.Duration) float64 { return float64(t.Microseconds()) / 1000 }
	var halted map[string]any
	if haltOnWake {
		switch herr := d.prog.Stall(); {
		case herr == nil:
			halted = map[string]any{"t_ms": ms(time.Since(t0))}
			if pc, perr := d.prog.PC(); perr == nil {
				halted["pc"] = fmt.Sprintf("0x%08x", pc)
			}
			defer func() {
				if _, rerr := d.run(); rerr != nil {
					res, err = nil, fmt.Errorf("resume after the prime writes: %w", rerr)
					return
				}
				if res != nil {
					res["resumed_ms"] = ms(time.Since(t0))
				}
			}()
		case asleep(herr):
			return nil, nil
		default:
			return nil, herr
		}
	}
	if !*checked {
		switch cerr := d.checkPrimes(primes); {
		case cerr == nil:
			*checked = true
		case asleep(cerr):
			return nil, nil
		default:
			return nil, cerr
		}
	}
	res = map[string]any{"t_ms": ms(time.Since(t0)), "ok": true}
	if halted != nil {
		res["halted"] = halted
	}
	for _, w := range primes {
		werr := d.prog.SWWrite(w.addr, w.data)
		var back []byte
		if werr == nil {
			back, werr = d.prog.SWRead(w.addr, len(w.data))
		}
		if werr == nil && !bytes.Equal(back, w.data) {
			werr = fmt.Errorf("verify at 0x%06x read back %s", w.addr, hex.EncodeToString(back))
		}
		if werr != nil {
			res["ok"] = false
			res["error"] = werr.Error()
			break
		}
	}
	return res, nil
}

// watch polls a small memory range for a while and condenses what it saw
// into reachable/unreachable segments and value changes, so sleep and wake
// cycles of a running target can be observed in one call. prime writes are
// made (and verified) at the first sample that answers.
func (d *device) watch(a map[string]any) (map[string]any, error) {
	addr, err := addr24(a, "address")
	if err != nil {
		return nil, argErr(err)
	}
	n, err := lengthArg(a, maxWatchBytes)
	if err != nil {
		return nil, argErr(err)
	}
	if uint64(addr)+uint64(n) > swireTop {
		return nil, argErr(fmt.Errorf("0x%x+%d runs past the 24-bit SWire address space", addr, n))
	}
	if addr < regSpaceBase {
		return nil, argErr(fmt.Errorf("0x%06x: watch reads SRAM or peripheral registers (from 0x%06x)", addr, regSpaceBase))
	}
	dur, err := msArg(a, "duration_ms", 10000, 1, maxWatchMs)
	if err != nil {
		return nil, argErr(err)
	}
	interval, err := msArg(a, "interval_ms", 5, 0, 10000)
	if err != nil {
		return nil, argErr(err)
	}
	primes, err := d.primeArgs(a)
	if err != nil {
		return nil, argErr(err)
	}
	primesChecked := false
	haltOnWake, err := optBool(a, "halt_on_wake")
	if err != nil {
		return nil, argErr(err)
	}
	if haltOnWake && len(primes) == 0 {
		return nil, argErr(errors.New("halt_on_wake needs prime: it only holds the target awake for the prime writes"))
	}

	type segment struct {
		state      string
		start, end time.Duration
		samples    int
		err        string
	}
	var (
		segs        []segment
		segsDropped int
		changes     []any
		chDropped   int
		last        []byte
		haveLast    bool
		samples     int
		awake       int
		errRun      int
		primeRes    map[string]any
		primeTries  int
		t0          = time.Now()
		deadline    = t0.Add(time.Duration(dur) * time.Millisecond)
	)
	ms := func(t time.Duration) float64 { return float64(t.Microseconds()) / 1000 }
	lastState, wakes := "", 0
	note := func(state string, at time.Duration, msg string) {
		if state == "awake" && lastState == "unreachable" {
			wakes++
		}
		changed := state != lastState
		lastState = state
		if segsDropped > 0 {
			// Past the cap: count the segments, keep the kept ones intact.
			if changed {
				segsDropped++
			}
			return
		}
		if k := len(segs); k > 0 && !changed {
			segs[k-1].end = at
			segs[k-1].samples++
			return
		}
		if len(segs) >= maxWatchSegs {
			segsDropped++
			return
		}
		segs = append(segs, segment{state: state, start: at, end: at, samples: 1, err: msg})
	}
	for {
		at := time.Since(t0)
		data, rerr := d.prog.SWRead(addr, n)
		samples++
		switch {
		case rerr == nil:
			errRun = 0
			awake++
			note("awake", at, "")
			if !haveLast || !bytes.Equal(data, last) {
				if len(changes) < maxWatchChanges {
					changes = append(changes, map[string]any{"t_ms": ms(at), "hex": hex.EncodeToString(data)})
				} else {
					chDropped++
				}
				last, haveLast = data, true
			}
			if len(primes) > 0 && (primeRes == nil || primeRes["ok"] != true) {
				r, perr := d.primeOnWake(primes, haltOnWake, &primesChecked, t0)
				if perr != nil {
					return nil, perr
				}
				if r != nil {
					primeTries++
					r["attempts"] = float64(primeTries)
					primeRes = r
				}
			}
		case asleep(rerr):
			errRun = 0
			note("unreachable", at, "")
		default:
			if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
				return nil, rerr
			}
			errRun++
			note("error", at, rerr.Error())
			if errRun >= maxWatchErrRun {
				return nil, fmt.Errorf("watch: %d link errors in a row, last: %w", errRun, rerr)
			}
		}
		if time.Now().After(deadline) {
			break
		}
		if interval > 0 {
			time.Sleep(time.Duration(interval) * time.Millisecond)
		}
	}
	elapsed := time.Since(t0)

	out := make([]any, 0, len(segs))
	for _, s := range segs {
		m := map[string]any{"state": s.state, "start_ms": ms(s.start), "end_ms": ms(s.end), "samples": float64(s.samples)}
		if s.err != "" {
			m["error"] = s.err
		}
		out = append(out, m)
	}
	res := map[string]any{
		"address": hex24(addr), "length": float64(n),
		"duration_ms": ms(elapsed), "samples": float64(samples), "awake_samples": float64(awake),
		"segments": out, "changes": changes, "wakes": float64(wakes),
		"note": "segments are bounded by samples: a sleep or wake is somewhere between the last sample of one segment and the first of the next",
	}
	if haveLast {
		res["last_hex"] = hex.EncodeToString(last)
	}
	if segsDropped > 0 {
		res["segments_dropped"] = float64(segsDropped)
	}
	if chDropped > 0 {
		res["changes_dropped"] = float64(chDropped)
	}
	if len(primes) > 0 {
		if primeRes == nil {
			primeRes = map[string]any{"ok": false, "attempts": float64(0), "error": "the target never answered"}
		}
		res["prime"] = primeRes
	}
	return res, nil
}
