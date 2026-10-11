package scpi

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Profile maps the semantic oscilloscope operations onto one vendor's command
// set. Every profile is data plus two acquisition functions; none of them has
// been validated against a real instrument unless Validated says so.
type Profile struct {
	ID   string
	Name string
	// Validated is true only after the profile ran against real hardware.
	Validated bool
	// Notes lists known gaps and assumptions, surfaced to the user.
	Notes []string

	Match func(IDN) bool

	// Source names a channel as a source argument (CHAN1, C1); Chan names its
	// command subsystem (:CHANnel1).
	Source func(ch int) string
	Chan   func(ch int) string

	ChanScale, ChanOffset, ChanCoupling, ChanEnable, ChanProbe string
	TimebaseScale, TimebaseOffset                              string
	TrigSource, TrigSlope, TrigLevel, TrigSweep                string
	Slopes, Sweeps                                             map[string]string
	Run, Stop, Single, Force                                   string
	TrigStatus                                                 string
	TrigStates                                                 map[string]string // vendor reply (upper-case) -> triggered|waiting|running|auto|stopped

	// Measures maps the normalised item name to the vendor item.
	Measures     map[string]string
	MeasureQuery func(item, source string) string

	Waveform   func(*Instrument, *Profile, int) (*Waveform, error)
	Screenshot func(*Instrument) (data []byte, mime string, err error)
}

// Profiles lists the built-in profiles in matching order; Generic matches
// everything and comes last.
func Profiles() []*Profile { return []*Profile{Rigol(), Siglent(), Generic()} }

// ProfileByID returns a built-in profile by id.
func ProfileByID(id string) (*Profile, bool) {
	for _, p := range Profiles() {
		if p.ID == strings.ToLower(id) {
			return p, true
		}
	}
	return nil, false
}

// Detect picks the profile for an *IDN? reply.
func Detect(idn IDN) *Profile {
	for _, p := range Profiles() {
		if p.Match(idn) {
			return p
		}
	}
	return Generic()
}

// Scopeless reports whether the profile has no oscilloscope semantics.
func (p *Profile) Scopeless() bool { return p.Source == nil }

// Generic is the profile for any IEEE 488.2 instrument: raw SCPI only.
func Generic() *Profile {
	return &Profile{
		ID: "generic", Name: "generic IEEE 488.2 / SCPI instrument",
		Notes: []string{"no oscilloscope semantics: use raw identify/command/query"},
		Match: func(IDN) bool { return true },
	}
}

func num(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func finite(name string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a finite number", name)
	}
	return nil
}

func parseFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	return f, nil
}

// Rigol is the DS1000Z-style profile (DS1054Z, DS1104Z and relatives). The
// command names come from memory of the public programming guide and have not
// been checked against an instrument.
func Rigol() *Profile {
	return &Profile{
		ID: "rigol", Name: "Rigol DS1000Z family",
		Notes: []string{
			"UNVALIDATED: command names are from the public programming guide, not checked on a real instrument",
			"waveform transfer uses the screen-memory (NORMal) mode only, at most 1200 points; deep-memory RAW readout is not implemented",
			"time axis comes from the instrument's :WAVeform:PREamble? (x origin/reference/increment)",
		},
		Match:  func(i IDN) bool { return strings.Contains(strings.ToUpper(i.Manufacturer), "RIGOL") },
		Source: func(ch int) string { return fmt.Sprintf("CHAN%d", ch) },
		Chan:   func(ch int) string { return fmt.Sprintf(":CHANnel%d", ch) },

		ChanScale: ":SCALe", ChanOffset: ":OFFSet", ChanCoupling: ":COUPling", ChanEnable: ":DISPlay", ChanProbe: ":PROBe",
		TimebaseScale: ":TIMebase:MAIN:SCALe", TimebaseOffset: ":TIMebase:MAIN:OFFSet",
		TrigSource: ":TRIGger:EDGE:SOURce", TrigSlope: ":TRIGger:EDGE:SLOPe", TrigLevel: ":TRIGger:EDGE:LEVel", TrigSweep: ":TRIGger:SWEep",
		Slopes: map[string]string{"rising": "POSitive", "falling": "NEGative", "either": "RFALl"},
		Sweeps: map[string]string{"auto": "AUTO", "normal": "NORMal", "single": "SINGle"},
		Run:    ":RUN", Stop: ":STOP", Single: ":SINGle", Force: ":TFORce",
		TrigStatus: ":TRIGger:STATus?",
		TrigStates: map[string]string{"TD": "triggered", "WAIT": "waiting", "RUN": "running", "AUTO": "auto", "STOP": "stopped"},

		Measures: map[string]string{
			"vpp": "VPP", "vmax": "VMAX", "vmin": "VMIN", "vavg": "VAVG", "vrms": "VRMS",
			"frequency": "FREQuency", "period": "PERiod", "rise_time": "RTIMe", "fall_time": "FTIMe",
			"pulse_width": "PWIDth", "neg_pulse_width": "NWIDth", "duty": "PDUTy",
		},
		MeasureQuery: func(item, src string) string { return ":MEASure:ITEM? " + item + "," + src },

		Waveform: rigolWaveform,
		Screenshot: func(i *Instrument) ([]byte, string, error) {
			b, _, err := i.QueryBinary(":DISPlay:DATA? ON,OFF,PNG")
			return b, sniffImage(b), err
		},
	}
}

func rigolWaveform(i *Instrument, p *Profile, ch int) (*Waveform, error) {
	src := p.Source(ch)
	for _, c := range []string{":WAVeform:SOURce " + src, ":WAVeform:MODE NORMal", ":WAVeform:FORMat BYTE"} {
		if err := i.Command(c); err != nil {
			return nil, err
		}
	}
	pre, err := i.Query(":WAVeform:PREamble?")
	if err != nil {
		return nil, err
	}
	f := strings.Split(pre, ",")
	if len(f) < 10 {
		return nil, fmt.Errorf("preamble has %d fields, want 10: %q", len(f), pre)
	}
	v := make([]float64, 10)
	for k := range v {
		if v[k], err = parseFloat(f[k]); err != nil {
			return nil, fmt.Errorf("preamble field %d: %w", k, err)
		}
	}
	if v[0] != 0 {
		return nil, fmt.Errorf("instrument reports waveform format %v after BYTE was requested", v[0])
	}
	xinc, xorg, xref, yinc, yorg, yref := v[4], v[5], v[6], v[7], v[8], v[9]
	if !(xinc > 0) || math.IsInf(xinc, 0) || yinc == 0 {
		return nil, fmt.Errorf("preamble is not usable (x increment %v, y increment %v): %q", xinc, yinc, pre)
	}
	data, _, err := i.QueryBinary(":WAVeform:DATA?")
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s returned no samples (channel off or no acquisition yet)", src)
	}
	w := &Waveform{Channel: ch, Source: src, T0: xorg - xref*xinc, DT: xinc, Volts: make([]float32, len(data))}
	for k, b := range data {
		w.Volts[k] = float32((float64(b) - yorg - yref) * yinc)
	}
	return w, nil
}

// Siglent is the SDS1000X-E-style profile. UNVALIDATED, and the least certain
// of the two: channel naming differs between firmware generations.
func Siglent() *Profile {
	return &Profile{
		ID: "siglent", Name: "Siglent SDS1000X-E family",
		Notes: []string{
			"UNVALIDATED: command names are from memory of the public programming guide and public search results disagreed on channel naming (C1 vs CHAN1); not checked on a real instrument",
			"waveform samples are scaled with vdiv/25 and offset; the time axis is centred on the screen (trigger delay is not applied)",
			"screenshot uses SCDP and assumes a headerless BMP reply",
		},
		Match:  func(i IDN) bool { return strings.Contains(strings.ToUpper(i.Manufacturer), "SIGLENT") },
		Source: func(ch int) string { return fmt.Sprintf("C%d", ch) },
		Chan:   func(ch int) string { return fmt.Sprintf(":CHANnel%d", ch) },

		ChanScale: ":SCALe", ChanOffset: ":OFFSet", ChanCoupling: ":COUPling", ChanEnable: ":SWITch", ChanProbe: ":PROBe",
		TimebaseScale: ":TIMebase:SCALe", TimebaseOffset: ":TIMebase:DELay",
		TrigSource: ":TRIGger:EDGE:SOURce", TrigSlope: ":TRIGger:EDGE:SLOPe", TrigLevel: ":TRIGger:EDGE:LEVel", TrigSweep: ":TRIGger:MODE",
		Slopes: map[string]string{"rising": "RISing", "falling": "FALLing", "either": "ALTernate"},
		Sweeps: map[string]string{"auto": "AUTO", "normal": "NORMal", "single": "SINGle"},
		Run:    ":TRIGger:RUN", Stop: ":TRIGger:STOP", Single: ":TRIGger:MODE SINGle", Force: ":TRIGger:FORCe",
		TrigStatus: ":TRIGger:STATus?",
		TrigStates: map[string]string{"TRIG'D": "triggered", "READY": "waiting", "ARM": "waiting", "RUN": "running", "AUTO": "auto", "STOP": "stopped", "ROLL": "running"},

		Measures: map[string]string{
			"vpp": "PKPK", "vmax": "MAX", "vmin": "MIN", "vavg": "MEAN", "vrms": "RMS",
			"frequency": "FREQ", "period": "PER", "rise_time": "RISE", "fall_time": "FALL",
			"pulse_width": "PWID", "neg_pulse_width": "NWID", "duty": "DUTY",
		},
		MeasureQuery: func(item, src string) string { return ":MEASure:SIMPle:VALue? " + item + "," + src },

		Waveform: siglentWaveform,
		Screenshot: func(i *Instrument) ([]byte, string, error) {
			b, err := i.QueryRaw("SCDP", 1500*time.Millisecond, 16<<20)
			return b, sniffImage(b), err
		},
	}
}

func siglentWaveform(i *Instrument, p *Profile, ch int) (*Waveform, error) {
	src := p.Source(ch)
	for _, c := range []string{":WAVeform:SOURce " + src, ":WAVeform:WIDTh BYTE"} {
		if err := i.Command(c); err != nil {
			return nil, err
		}
	}
	ask := func(q string) (float64, error) {
		s, err := i.Query(q)
		if err != nil {
			return 0, err
		}
		return parseFloat(s)
	}
	vdiv, err := ask(p.Chan(ch) + p.ChanScale + "?")
	if err != nil {
		return nil, err
	}
	ofst, err := ask(p.Chan(ch) + p.ChanOffset + "?")
	if err != nil {
		return nil, err
	}
	sara, err := ask(":ACQuire:SRATe?")
	if err != nil {
		return nil, err
	}
	if !(sara > 0) || math.IsInf(sara, 0) {
		return nil, fmt.Errorf("sample rate %v is not usable", sara)
	}
	data, _, err := i.QueryBinary(":WAVeform:DATA?")
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s returned no samples", src)
	}
	dt := 1 / sara
	w := &Waveform{Channel: ch, Source: src, T0: -dt * float64(len(data)) / 2, DT: dt, Volts: make([]float32, len(data)),
		Note: "time axis centred on the screen; trigger delay not applied"}
	for k, b := range data {
		w.Volts[k] = float32(float64(int8(b))*(vdiv/25) - ofst)
	}
	return w, nil
}

// sniffImage names the image type from its magic bytes.
func sniffImage(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[1:4]) == "PNG":
		return "image/png"
	case len(b) >= 2 && b[0] == 'B' && b[1] == 'M':
		return "image/bmp"
	case len(b) >= 2 && b[0] == 0xff && b[1] == 0xd8:
		return "image/jpeg"
	}
	return "application/octet-stream"
}
