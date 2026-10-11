package scpi

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseAddr(t *testing.T) {
	cases := []struct {
		in   string
		want Addr
		bad  bool
	}{
		{"192.168.1.5", Addr{KindTCP, "192.168.1.5:5025", 0}, false},
		{"tcp://scope.lan:5555", Addr{KindTCP, "scope.lan:5555", 0}, false},
		{"tcp://[::1]", Addr{KindTCP, "[::1]:5025", 0}, false},
		{"serial:/dev/ttyUSB0@115200", Addr{KindSerial, "/dev/ttyUSB0", 115200}, false},
		{"serial:/dev/ttyUSB0", Addr{KindSerial, "/dev/ttyUSB0", 9600}, false},
		{"usbtmc:/dev/usbtmc0", Addr{KindUSBTMC, "/dev/usbtmc0", 0}, false},
		{"", Addr{}, true},
		{"serial:", Addr{}, true},
		{"serial:/dev/x@zero", Addr{}, true},
		{"usbtmc:", Addr{}, true},
		{"/dev/ttyUSB0", Addr{}, true},
	}
	for _, c := range cases {
		got, err := ParseAddr(c.in)
		if (err != nil) != c.bad {
			t.Errorf("%q: err %v, bad=%v", c.in, err, c.bad)
			continue
		}
		if !c.bad && got != c.want {
			t.Errorf("%q: %+v, want %+v", c.in, got, c.want)
		}
		if !c.bad {
			if back, err := ParseAddr(got.String()); err != nil || back != got {
				t.Errorf("%q: String() %q does not round-trip: %+v %v", c.in, got.String(), back, err)
			}
		}
	}
}

func TestIsQuery(t *testing.T) {
	yes := []string{"*IDN?", ":MEAS:ITEM? VPP,CHAN1", "*IDN?;*OPC?", `:SYST:LANG? ; :SYST:ERR?`, `:DISP:TEXT? ";"`}
	no := []string{"", "*RST", ":RUN", "*IDN?;:RUN", ":CHAN1:SCAL 1", ":RUN;*IDN?", "*IDN?\n:RUN", "*IDN?\r", "*IDN?\x00", ":X \"a?"}
	for _, c := range yes {
		if !IsQuery(c) {
			t.Errorf("IsQuery(%q) = false", c)
		}
	}
	for _, c := range no {
		if IsQuery(c) {
			t.Errorf("IsQuery(%q) = true", c)
		}
	}
}

func TestParseIDN(t *testing.T) {
	i := ParseIDN("RIGOL TECHNOLOGIES,DS1054Z,DS1ZA1,00.04.04.SP4,extra")
	if i.Manufacturer != "RIGOL TECHNOLOGIES" || i.Model != "DS1054Z" || i.Serial != "DS1ZA1" || i.Firmware != "00.04.04.SP4,extra" {
		t.Fatalf("%+v", i)
	}
	if p := ParseIDN("garbage"); p.Manufacturer != "garbage" || p.Model != "" {
		t.Fatalf("%+v", p)
	}
	if Detect(i).ID != "rigol" || Detect(ParseIDN("Siglent Technologies,SDS1104X-E,1,2")).ID != "siglent" || Detect(ParseIDN("ACME,X,1,2")).ID != "generic" {
		t.Fatal("profile detection")
	}
}

// scripted is a Transport that answers with prepared chunks.
type scripted struct {
	chunks [][]byte
	wrote  []string
	delay  error // returned once chunks run out
}

func (s *scripted) Write(p []byte) error { s.wrote = append(s.wrote, string(p)); return nil }
func (s *scripted) Read(p []byte, _ time.Duration) (int, error) {
	if len(s.chunks) == 0 {
		if s.delay != nil {
			return 0, s.delay
		}
		return 0, ErrTimeout
	}
	n := copy(p, s.chunks[0])
	if n < len(s.chunks[0]) {
		s.chunks[0] = s.chunks[0][n:]
	} else {
		s.chunks = s.chunks[1:]
	}
	return n, nil
}
func (s *scripted) Close() error     { return nil }
func (s *scripted) Describe() string { return "scripted" }

func inst(chunks ...string) (*Instrument, *scripted) {
	s := &scripted{}
	for _, c := range chunks {
		s.chunks = append(s.chunks, []byte(c))
	}
	i := NewInstrument(s)
	i.Timeout = 200 * time.Millisecond
	return i, s
}

func TestQueryLineAcrossChunks(t *testing.T) {
	i, s := inst("RIGOL,DS", "1054Z,1,2\r", "\n")
	got, err := i.Query("*IDN?")
	if err != nil || got != "RIGOL,DS1054Z,1,2" {
		t.Fatalf("%q %v", got, err)
	}
	if s.wrote[0] != "*IDN?\n" {
		t.Fatalf("wrote %q", s.wrote)
	}
}

func TestQueryTimeout(t *testing.T) {
	i, _ := inst()
	if _, err := i.Query("*IDN?"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("%v", err)
	}
}

func TestRefusesControlCharacters(t *testing.T) {
	i, s := inst("x\n")
	for _, c := range []string{"*IDN?\n:RUN", "", "  ", "a\x00b"} {
		if err := i.Command(c); err == nil {
			t.Errorf("Command(%q) accepted", c)
		}
	}
	if len(s.wrote) != 0 {
		t.Fatalf("something was written: %q", s.wrote)
	}
}

func TestBlocks(t *testing.T) {
	payload := "\x00\x01\n\x02#5"
	good := "#1" + "6" + payload + "\n"
	cases := []struct {
		name   string
		chunks []string
		want   string
		prefix string
		bad    string
	}{
		{"definite", []string{good}, payload, "", ""},
		{"split everywhere", []string{"#", "1", "6", payload[:2], payload[2:], "\n"}, payload, "", ""},
		{"with prefix", []string{"DAT2," + good}, payload, "DAT2,", ""},
		{"no terminator", []string{"#1" + "6" + payload}, payload, "", ""},
		{"crlf terminator", []string{"#1" + "6" + payload + "\r\n"}, payload, "", ""},
		{"indefinite", []string{"#0abc\n"}, "abc", "", ""},
		{"truncated", []string{"#210" + "abc"}, "", "", "cut short"},
		{"not a block", []string{"+1.5E+00\n"}, "", "", "not a binary block"},
		{"bad digits", []string{"#x"}, "", "", "bad digit count"},
		{"bad length", []string{"#2zz"}, "", "", "bad length"},
		{"huge", []string{"#9999999999"}, "", "", "exceeds"},
	}
	for _, c := range cases {
		i, _ := inst(c.chunks...)
		got, prefix, err := i.QueryBinary(":WAV:DATA?")
		if c.bad != "" {
			if err == nil || !strings.Contains(err.Error(), c.bad) {
				t.Errorf("%s: err %v, want %q", c.name, err, c.bad)
			}
			continue
		}
		if err != nil || string(got) != c.want || prefix != c.prefix {
			t.Errorf("%s: %q %q %v", c.name, got, prefix, err)
		}
	}
}

func TestBlockTerminatorKeepsStreamInStep(t *testing.T) {
	i, _ := inst("#13abc\n", "42\n")
	if d, _, err := i.QueryBinary("A?"); err != nil || string(d) != "abc" {
		t.Fatalf("%q %v", d, err)
	}
	// Reply bytes left over from a previous reply never leak into the next query.
	i2, _ := inst("#13abc\nSTALE\n")
	if _, _, err := i2.QueryBinary("A?"); err != nil {
		t.Fatal(err)
	}
	if _, err := i2.Query("B?"); err == nil {
		t.Fatal("stale bytes were returned as the next reply")
	}
	if i2.StaleBytes() == 0 {
		t.Fatal("stale bytes not counted")
	}
}

func TestQueryRawIdle(t *testing.T) {
	i, _ := inst("BM12", "34567")
	b, err := i.QueryRaw("SCDP", 20*time.Millisecond, 1<<20)
	if err != nil || string(b) != "BM1234567" {
		t.Fatalf("%q %v", b, err)
	}
	i2, _ := inst("0123456789")
	if _, err := i2.QueryRaw("SCDP", 20*time.Millisecond, 5); err == nil {
		t.Fatal("limit not enforced")
	}
}

func TestErrorQueueParse(t *testing.T) {
	i, _ := inst("-113,\"Undefined header\"\n")
	c, m, err := i.ErrorQueue()
	if err != nil || c != -113 || m != "Undefined header" {
		t.Fatalf("%d %q %v", c, m, err)
	}
}

func TestWaveformMath(t *testing.T) {
	w := &Waveform{Channel: 1, T0: -1, DT: 0.5, Volts: []float32{-2, 0, 2, 0}}
	s := w.Stats()
	if s.N != 4 || s.Min != -2 || s.Max != 2 || s.Vpp != 4 || s.Mean != 0 || math.Abs(s.RMS-math.Sqrt(2)) > 1e-9 {
		t.Fatalf("%+v", s)
	}
	e := w.Envelope(2)
	if len(e.Min) != 2 || e.Min[0] != -2 || e.Max[0] != 0 || e.Min[1] != 0 || e.Max[1] != 2 || e.BucketDT != 1 {
		t.Fatalf("%+v", e)
	}
	if e := w.Envelope(100); len(e.Min) != 4 {
		t.Fatalf("envelope longer than the data: %+v", e)
	}
	if (&Waveform{}).Stats().N != 0 || len((&Waveform{}).Envelope(5).Min) != 0 {
		t.Fatal("empty waveform")
	}
	var b bytes.Buffer
	if err := w.WriteCSV(&b); err != nil || !strings.HasPrefix(b.String(), "time_s,ch1_volts\n-1,-2\n-0.5,0\n") {
		t.Fatalf("%q %v", b.String(), err)
	}
}

func fakeScope(t *testing.T, p *Profile) (*Scope, *Fake) {
	t.Helper()
	f := NewFake(p)
	t.Cleanup(f.Close)
	i := NewInstrument(f.Dial())
	i.Timeout = 2 * time.Second
	t.Cleanup(func() { _ = i.Close() })
	return NewScope(i, p, 4), f
}

func TestScopeAgainstFake(t *testing.T) {
	for _, p := range []*Profile{Rigol(), Siglent()} {
		t.Run(p.ID, func(t *testing.T) {
			s, f := fakeScope(t, p)
			idn, err := s.I.Identify()
			if err != nil || !strings.Contains(idn.Raw, "SYNTHETIC") || Detect(idn).ID != p.ID {
				t.Fatalf("%+v %v", idn, err)
			}

			st, err := s.ChannelState(2)
			if err != nil || st["scale_v_per_div"] != 1.0 || st["enabled"] != true || st["coupling"] != "DC" {
				t.Fatalf("%v %v", st, err)
			}
			scale, off, probe, off2 := 0.5, 0.1, 10.0, false
			cmds, err := s.ConfigureChannel(2, ChannelSettings{Scale: &scale, Offset: &off, Probe: &probe, Coupling: "ac", Enabled: &off2})
			if err != nil || len(cmds) != 5 {
				t.Fatalf("%v %v", cmds, err)
			}
			st, _ = s.ChannelState(2)
			if st["scale_v_per_div"] != 0.5 || st["offset_v"] != 0.1 || st["probe_ratio"] != 10.0 || st["coupling"] != "AC" {
				t.Fatalf("settings did not stick: %v", st)
			}
			if _, err := s.I.Query(":SYSTem:ERRor?"); err != nil {
				t.Fatal(err)
			}

			tb, err := s.Timebase()
			if err != nil || tb["scale_s_per_div"] != 0.0005 {
				t.Fatalf("%v %v", tb, err)
			}
			ts := 0.001
			if _, err := s.SetTimebase(&ts, nil); err != nil {
				t.Fatal(err)
			}
			if tb, _ = s.Timebase(); tb["scale_s_per_div"] != 0.001 {
				t.Fatalf("%v", tb)
			}

			lvl := 0.25
			if _, err := s.SetTrigger(TriggerSettings{Channel: 3, Slope: "falling", Level: &lvl, Sweep: "normal"}); err != nil {
				t.Fatal(err)
			}
			tr, err := s.Trigger()
			if err != nil || tr["source"] != p.Source(3) || tr["slope"] != "falling" || tr["level_v"] != 0.25 || tr["sweep"] != "normal" {
				t.Fatalf("%v %v", tr, err)
			}

			if _, err := s.Acquire("single"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			state, err := s.WaitDone(ctx, 3*time.Second, time.Millisecond)
			if err != nil || state != "triggered" {
				t.Fatalf("%s %v", state, err)
			}

			w, err := s.Waveform(2)
			if err != nil {
				t.Fatal(err)
			}
			if len(w.Volts) != fakePoints || w.DT <= 0 {
				t.Fatalf("%d points dt %v", len(w.Volts), w.DT)
			}
			// Channel 2 carries a 1.0 V sine; quantisation is a few mV.
			if sp := w.Stats(); math.Abs(sp.Max-1.0) > 0.05 || math.Abs(sp.Min+1.0) > 0.05 {
				t.Fatalf("amplitude %+v", sp)
			}
			m, err := s.Measure("vpp", 3)
			if err != nil || m["valid"] != true || math.Abs(m["value"].(float64)-3.0) > 1e-6 {
				t.Fatalf("%v %v", m, err)
			}
			if m, _ := s.Measure("duty", 1); m["valid"] != false {
				t.Fatalf("9.9E37 must be invalid: %v", m)
			}
			img, mime, err := s.Screenshot()
			if err != nil || len(img) < 8 || (mime != "image/png" && mime != "image/bmp") {
				t.Fatalf("%d bytes %s %v", len(img), mime, err)
			}
			if q, _, err := s.I.ErrorQueue(); err != nil || q != 0 {
				t.Fatalf("unexpected instrument errors %d %v (log %v)", q, err, f.Log())
			}
		})
	}
}

func TestScopeValidation(t *testing.T) {
	s, f := fakeScope(t, Rigol())
	bad := -1.0
	nan := math.NaN()
	for name, err := range map[string]error{
		"channel 0":      func() error { _, e := s.ConfigureChannel(0, ChannelSettings{Scale: &bad}); return e }(),
		"channel 9":      func() error { _, e := s.ConfigureChannel(9, ChannelSettings{Scale: &bad}); return e }(),
		"negative scale": func() error { _, e := s.ConfigureChannel(1, ChannelSettings{Scale: &bad}); return e }(),
		"nan offset":     func() error { _, e := s.ConfigureChannel(1, ChannelSettings{Offset: &nan}); return e }(),
		"bad coupling":   func() error { _, e := s.ConfigureChannel(1, ChannelSettings{Coupling: "dc;:RUN"}); return e }(),
		"empty":          func() error { _, e := s.ConfigureChannel(1, ChannelSettings{}); return e }(),
		"bad slope":      func() error { _, e := s.SetTrigger(TriggerSettings{Slope: "sideways"}); return e }(),
		"bad sweep":      func() error { _, e := s.SetTrigger(TriggerSettings{Sweep: "x"}); return e }(),
		"bad action":     func() error { _, e := s.Acquire("explode"); return e }(),
		"bad item":       func() error { _, e := s.Measure("bogus", 1); return e }(),
		"bad timebase":   func() error { _, e := s.SetTimebase(&bad, nil); return e }(),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if got := f.Log(); len(got) != 0 {
		t.Fatalf("a rejected request still reached the instrument: %v", got)
	}
}

func TestGenericProfileHasNoScope(t *testing.T) {
	s := NewScope(NewInstrument(&scripted{}), Generic(), 0)
	if _, err := s.Timebase(); err == nil || !strings.Contains(err.Error(), "no oscilloscope profile") {
		t.Fatalf("%v", err)
	}
	if s.Channels != 4 {
		t.Fatal("default channel count")
	}
}

func TestWaitDoneTimeoutAndCancel(t *testing.T) {
	s, _ := fakeScope(t, Rigol())
	ctx := context.Background()
	if st, err := s.WaitDone(ctx, 20*time.Millisecond, 5*time.Millisecond); err != nil || st != "stopped" {
		t.Fatalf("stopped scope: %s %v", st, err)
	}
	if _, err := s.Acquire("run"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WaitDone(ctx, 30*time.Millisecond, 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "no trigger within") {
		t.Fatalf("%v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.WaitDone(cctx, time.Second, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestRigolPreambleRejections(t *testing.T) {
	for name, pre := range map[string]string{
		"short":      "0,0,1200",
		"word fmt":   "1,0,10,1,1e-6,0,0,0.01,0,127",
		"zero xinc":  "0,0,10,1,0,0,0,0.01,0,127",
		"not number": "0,0,10,1,x,0,0,0.01,0,127",
	} {
		i, _ := inst("", "")
		i.t = &seqTransport{replies: []string{pre + "\n"}}
		if _, err := rigolWaveform(i, Rigol(), 1); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// seqTransport answers each Read with the next prepared reply.
type seqTransport struct{ replies []string }

func (s *seqTransport) Write([]byte) error { return nil }
func (s *seqTransport) Read(p []byte, _ time.Duration) (int, error) {
	if len(s.replies) == 0 {
		return 0, ErrTimeout
	}
	n := copy(p, s.replies[0])
	s.replies = s.replies[1:]
	return n, nil
}
func (s *seqTransport) Close() error     { return nil }
func (s *seqTransport) Describe() string { return "seq" }

func TestTCPTransport(t *testing.T) {
	f := NewFake(Rigol())
	defer f.Close()
	addr, err := f.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := DialTCP(addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	i := NewInstrument(tr)
	i.Timeout = time.Second
	defer i.Close()
	idn, err := i.Identify()
	if err != nil || idn.Model != "DS1054Z" {
		t.Fatalf("%+v %v", idn, err)
	}
	if _, err := DialTCP("127.0.0.1:1", 200*time.Millisecond); err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
}
