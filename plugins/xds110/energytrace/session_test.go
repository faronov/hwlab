package energytrace

import (
	"bytes"
	"errors"
	"testing"

	"github.com/faronov/hwlab/plugins/xds110/xds"
)

type fakeCmd struct {
	cmd    byte
	args   []byte
	status xds.Status
	data   []byte
}

// fakeProbe replays a scripted command sequence and fails on any deviation.
type fakeProbe struct {
	t      *testing.T
	script []fakeCmd
	pos    int
	seen   []byte
}

func (f *fakeProbe) Do(cmd byte, args []byte) ([]byte, error) {
	f.seen = append(f.seen, cmd)
	if f.pos >= len(f.script) {
		f.t.Fatalf("unexpected command %#02x after the script ended", cmd)
	}
	s := f.script[f.pos]
	f.pos++
	if s.cmd != cmd || !bytes.Equal(s.args, args) {
		f.t.Fatalf("step %d: sent %#02x %x, script has %#02x %x", f.pos-1, cmd, args, s.cmd, s.args)
	}
	if s.status != 0 {
		return s.data, &xds.Error{Cmd: cmd, Status: s.status}
	}
	return s.data, nil
}

func scriptOf(t testing.TB, cmds []fixtureCmd) []fakeCmd {
	var out []fakeCmd
	for _, c := range cmds {
		out = append(out, fakeCmd{cmd: unhex(t, c.Cmd)[0], args: unhex(t, c.Args), status: xds.Status(c.Status), data: unhex(t, c.Data)})
	}
	return out
}

func TestStartReplaysObservedSequence(t *testing.T) {
	f := loadFixture(t)
	script := append([]fakeCmd{{cmd: 0x02}}, scriptOf(t, f.Start)...)
	p := &fakeProbe{t: t, script: script}
	cal, err := Start(p)
	if err != nil {
		t.Fatal(err)
	}
	if p.pos != len(script) {
		t.Fatalf("sent %d of %d commands", p.pos, len(script))
	}
	off, err := cal.Range0.Offset()
	if err != nil || off < 5028 || off > 5029 {
		t.Fatalf("range 0 offset %v err %v", off, err)
	}
	if cal.Range4.Count != 5 || cal.Range1.Count != 5 {
		t.Fatalf("calibration %+v", cal)
	}
}

func TestStartStopsOnFirstFailure(t *testing.T) {
	f := loadFixture(t)
	script := append([]fakeCmd{{cmd: 0x02}}, scriptOf(t, f.Start)...)
	script[6].status = xds.StatusFail // 0x30
	p := &fakeProbe{t: t, script: script[:7]}
	if _, err := Start(p); err == nil {
		t.Fatal("expected an error")
	}
	for _, c := range p.seen {
		if c == CmdStreamStart {
			t.Fatal("stream was started after a failure")
		}
	}
}

func TestStartRejectsShortCalibration(t *testing.T) {
	f := loadFixture(t)
	script := append([]fakeCmd{{cmd: 0x02}}, scriptOf(t, f.Start)...)
	script[9].data = []byte{1, 2}
	p := &fakeProbe{t: t, script: script[:10]}
	if _, err := Start(p); !errors.Is(err, ErrCalibration) {
		t.Fatalf("err %v", err)
	}
}

func TestStartRejectsFirmwareSurprise(t *testing.T) {
	f := loadFixture(t)
	script := append([]fakeCmd{{cmd: 0x02}}, scriptOf(t, f.Start)...)
	script[2].data = []byte{0x32, 0, 0, 3, 0x99, 0x00} // other hardware id
	p := &fakeProbe{t: t, script: script[:3]}
	if _, err := Start(p); err == nil {
		t.Fatal("an unknown hardware id must be refused")
	}
}

func TestStopSequenceToleratesAlreadyStopped(t *testing.T) {
	f := loadFixture(t)
	p := &fakeProbe{t: t, script: scriptOf(t, f.Stop)}
	for i := 0; i < 2; i++ {
		// The real tool sends the pair twice; the second pair answers -644.
		if err := StopStream(p); err != nil {
			t.Fatal(err)
		}
		if err := ReleaseStream(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := Disconnect(p); err != nil {
		t.Fatal(err)
	}
	if p.pos != len(p.script) {
		t.Fatalf("sent %d of %d", p.pos, len(p.script))
	}
}

func TestStopReportsRealFailure(t *testing.T) {
	p := &fakeProbe{t: t, script: []fakeCmd{{cmd: 0x20, status: xds.StatusFail}}}
	err := StopStream(p)
	if err == nil {
		t.Fatal("a -261 stop must be reported")
	}
}
