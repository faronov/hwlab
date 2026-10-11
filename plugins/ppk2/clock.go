package ppk2

import (
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/plugins/ppk2/protocol"
)

// startWindowNs limits how long arrival times contribute to the start bound.
// Beyond that the unknown device-clock drift (order 100 ppm) would loosen it.
const startWindowNs = 2_000_000_000

// startBound bounds when sample 0 was really taken.
//
// The device has no per-sample timestamps, so sample i is stamped
// t0 + i*period where t0 is the host time right after AVERAGE_START was
// written. Two facts bound the real start time S of sample 0:
//
//	S >= W            the device cannot sample before the command is written
//	S <= a_j - j*T    sample j cannot arrive before it was taken
//
// where W is the host time just before the write and a_j the host time at
// which sample j was read. The stamped-minus-true error of every sample is
// therefore within [t0 - min_j(a_j - j*T), t0 - W].
type startBound struct {
	writeNs int64 // W
	t0Ns    int64
	upperNs int64 // min_j(a_j - j*T) so far
	seen    bool
}

func newStartBound(writeNs, t0Ns int64) *startBound {
	return &startBound{writeNs: writeNs, t0Ns: t0Ns}
}

// observe records that after reading at host time arriveNs, total samples had
// been decoded (so the newest one has index total-1).
func (b *startBound) observe(arriveNs int64, total int64) {
	if b == nil || total <= 0 {
		return
	}
	j := total - 1
	if j*protocol.SamplePeriod > startWindowNs {
		return
	}
	u := arriveNs - j*protocol.SamplePeriod
	if !b.seen || u < b.upperNs {
		b.upperNs, b.seen = u, true
	}
}

func (b *startBound) clockInfo() *pluginv1.ClockInfo {
	if b == nil {
		return nil
	}
	ci := &pluginv1.ClockInfo{
		Source:       "sample count x 10 us from the host time of the AVERAGE_START write",
		ResolutionNs: protocol.SamplePeriod,
		DriftPpm:     0,
		Note: "the PPK2 has no per-sample timestamps; the start of sampling is bounded by " +
			"the write time (late side) and by data arrival (early side), assuming the " +
			"device clock does not drift over the first 2 s; later drift is not measured",
	}
	ci.ErrorMaxNs = b.t0Ns - b.writeNs
	ci.ErrorMaxBounded = true
	if b.seen {
		ci.ErrorMinNs = b.t0Ns - b.upperNs
		ci.ErrorMinBounded = true
		if ci.ErrorMinNs > 0 {
			// Data apparently arrived before it was taken: the assumptions
			// broke, so do not claim a bound.
			ci.ErrorMinBounded = false
			ci.ErrorMinNs = 0
			ci.Note += "; arrival bound was inconsistent and is not claimed"
		}
	}
	return ci
}
