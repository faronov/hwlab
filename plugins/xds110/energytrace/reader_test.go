package energytrace

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/usb"
)

type scriptedPort struct {
	reads  [][]byte
	errs   []error
	cancel context.CancelFunc
	i      int
}

func (s *scriptedPort) BulkIn(ctx context.Context, ep uint8, n int, d time.Duration) ([]byte, error) {
	if ep != DataIn {
		panic("wrong endpoint")
	}
	if s.i >= len(s.reads) {
		if s.cancel != nil {
			s.cancel()
		}
		return nil, usb.ErrTimeout
	}
	k := s.i
	s.i++
	if s.errs != nil && s.errs[k] != nil {
		return nil, s.errs[k]
	}
	return s.reads[k], nil
}

func testCal() Calibration { return Calibration{Sum: 5000 * 5, Count: 5} }

func TestReaderDropsFirstPacketAndCountsGaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &scriptedPort{cancel: cancel, reads: [][]byte{
		{0x35, 0x80, 0xbb},
		buildPacket(0, constCodes(5000)), // dropped
		buildPacket(1, constCodes(5100)),
		buildPacket(4, constCodes(5200)), // 2 packets lost
	}}
	r, err := NewReader(p, testCal())
	if err != nil {
		t.Fatal(err)
	}
	var got []Batch
	if err := r.Run(ctx, time.Minute, func(b Batch) {
		b.CurrentNA = append([]int32(nil), b.CurrentNA...)
		got = append(got, b)
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d batches", len(got))
	}
	if got[0].StartSample != 0 || got[0].CurrentNA[0] != 1514 {
		t.Fatalf("first batch %+v", got[0])
	}
	if got[1].StartSample != 256+2*256 || got[1].Lost != 2 {
		t.Fatalf("second batch start %d lost %d", got[1].StartSample, got[1].Lost)
	}
	if st, dropped := r.Stats(); dropped != 1 || st.LostPackets != 2 {
		t.Fatalf("stats %+v dropped %d", st, dropped)
	}
}

func TestReaderPropagatesDeviceGone(t *testing.T) {
	p := &scriptedPort{reads: [][]byte{nil}, errs: []error{usb.ErrDisconnected}}
	r, _ := NewReader(p, testCal())
	err := r.Run(context.Background(), time.Minute, func(Batch) {})
	if !errors.Is(err, usb.ErrDisconnected) {
		t.Fatalf("err %v", err)
	}
}

func TestReaderStalls(t *testing.T) {
	p := &scriptedPort{} // always times out
	r, _ := NewReader(p, testCal())
	err := r.Run(context.Background(), time.Millisecond, func(Batch) {})
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("err %v", err)
	}
}

func TestReaderRejectsEmptyCalibration(t *testing.T) {
	if _, err := NewReader(&scriptedPort{}, Calibration{}); err == nil {
		t.Fatal("count 0 must fail")
	}
}
