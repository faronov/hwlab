// Package jtag implements an IEEE 1149.1 TAP controller driver, scan-chain
// enumeration and IDCODE decoding on top of a generic bit-level Port. It has
// no knowledge of any CPU or target: it only moves bits through instruction
// and data registers.
package jtag

import "fmt"

// State is a state of the IEEE 1149.1 TAP controller.
type State int

// The sixteen TAP states.
const (
	StateUnknown State = iota - 1
	TestLogicReset
	RunTestIdle
	SelectDRScan
	CaptureDR
	ShiftDR
	Exit1DR
	PauseDR
	Exit2DR
	UpdateDR
	SelectIRScan
	CaptureIR
	ShiftIR
	Exit1IR
	PauseIR
	Exit2IR
	UpdateIR
	numStates
)

var stateNames = [...]string{
	"Test-Logic-Reset", "Run-Test/Idle",
	"Select-DR-Scan", "Capture-DR", "Shift-DR", "Exit1-DR", "Pause-DR", "Exit2-DR", "Update-DR",
	"Select-IR-Scan", "Capture-IR", "Shift-IR", "Exit1-IR", "Pause-IR", "Exit2-IR", "Update-IR",
}

func (s State) String() string {
	if s < 0 || s >= numStates {
		return "Unknown"
	}
	return stateNames[s]
}

// next[state][tms] is the state after one TCK with the given TMS value,
// taken from the IEEE 1149.1 state diagram.
var next = [numStates][2]State{
	TestLogicReset: {RunTestIdle, TestLogicReset},
	RunTestIdle:    {RunTestIdle, SelectDRScan},
	SelectDRScan:   {CaptureDR, SelectIRScan},
	CaptureDR:      {ShiftDR, Exit1DR},
	ShiftDR:        {ShiftDR, Exit1DR},
	Exit1DR:        {PauseDR, UpdateDR},
	PauseDR:        {PauseDR, Exit2DR},
	Exit2DR:        {ShiftDR, UpdateDR},
	UpdateDR:       {RunTestIdle, SelectDRScan},
	SelectIRScan:   {CaptureIR, TestLogicReset},
	CaptureIR:      {ShiftIR, Exit1IR},
	ShiftIR:        {ShiftIR, Exit1IR},
	Exit1IR:        {PauseIR, UpdateIR},
	PauseIR:        {PauseIR, Exit2IR},
	Exit2IR:        {ShiftIR, UpdateIR},
	UpdateIR:       {RunTestIdle, SelectDRScan},
}

// Next returns the state reached from s after one clock with the given TMS.
func Next(s State, tms bool) State {
	if s < 0 || s >= numStates {
		return StateUnknown
	}
	if tms {
		return next[s][1]
	}
	return next[s][0]
}

// Path returns a shortest TMS sequence from one state to another (empty if
// they are equal). Ties are broken by preferring TMS=0, so the result is
// deterministic. It returns an error for StateUnknown.
func Path(from, to State) ([]bool, error) {
	if from < 0 || from >= numStates || to < 0 || to >= numStates {
		return nil, fmt.Errorf("jtag: no path between %v and %v", from, to)
	}
	if from == to {
		return nil, nil
	}
	type edge struct {
		prev State
		tms  bool
	}
	seen := [numStates]bool{}
	via := [numStates]edge{}
	seen[from] = true
	queue := []State{from}
	for len(queue) > 0 && !seen[to] {
		s := queue[0]
		queue = queue[1:]
		for _, tms := range []bool{false, true} {
			n := Next(s, tms)
			if !seen[n] {
				seen[n] = true
				via[n] = edge{s, tms}
				queue = append(queue, n)
			}
		}
	}
	var rev []bool
	for s := to; s != from; s = via[s].prev {
		rev = append(rev, via[s].tms)
	}
	out := make([]bool, len(rev))
	for i, t := range rev {
		out[len(rev)-1-i] = t
	}
	return out, nil
}

// Stable reports whether the state persists while TMS stays at the value
// that keeps it there (the states in which a TAP can idle).
func (s State) Stable() bool {
	switch s {
	case TestLogicReset, RunTestIdle, ShiftDR, ShiftIR, PauseDR, PauseIR:
		return true
	}
	return false
}
