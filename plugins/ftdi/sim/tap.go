package sim

// tapState is the simulator's own encoding of the 1149.1 state machine.
type tapState int

const (
	tTLR tapState = iota
	tRTI
	tSelDR
	tCapDR
	tShDR
	tEx1DR
	tPauseDR
	tEx2DR
	tUpdDR
	tSelIR
	tCapIR
	tShIR
	tEx1IR
	tPauseIR
	tEx2IR
	tUpdIR
)

var tapNames = [...]string{"TLR", "RTI", "SelDR", "CapDR", "ShDR", "Ex1DR", "PauseDR", "Ex2DR", "UpdDR", "SelIR", "CapIR", "ShIR", "Ex1IR", "PauseIR", "Ex2IR", "UpdIR"}

func (s tapState) String() string { return tapNames[s] }

// tapNext[state] = {next on TMS=0, next on TMS=1}.
var tapNext = [16][2]tapState{
	tTLR:     {tRTI, tTLR},
	tRTI:     {tRTI, tSelDR},
	tSelDR:   {tCapDR, tSelIR},
	tCapDR:   {tShDR, tEx1DR},
	tShDR:    {tShDR, tEx1DR},
	tEx1DR:   {tPauseDR, tUpdDR},
	tPauseDR: {tPauseDR, tEx2DR},
	tEx2DR:   {tShDR, tUpdDR},
	tUpdDR:   {tRTI, tSelDR},
	tSelIR:   {tCapIR, tTLR},
	tCapIR:   {tShIR, tEx1IR},
	tShIR:    {tShIR, tEx1IR},
	tEx1IR:   {tPauseIR, tUpdIR},
	tPauseIR: {tPauseIR, tEx2IR},
	tEx2IR:   {tShIR, tUpdIR},
	tUpdIR:   {tRTI, tSelDR},
}

type tapDev struct {
	idcode uint32
	irLen  int
	ir     uint64 // current instruction
	irSh   uint64 // IR shift register
	drSh   uint64 // DR shift register
	drLen  int
}

func (t *tapDev) bypassIR() uint64 { return 1<<t.irLen - 1 }

// resetIR is the instruction loaded by Test-Logic-Reset: IDCODE (1) when the
// device has one, else BYPASS.
func (t *tapDev) resetIR() uint64 {
	if t.idcode != 0 {
		return 1
	}
	return t.bypassIR()
}

type tapModel struct {
	state tapState
	devs  []*tapDev
	float bool
}

func (m *tapModel) init(chain []TAP, floatHigh bool) {
	m.float = floatHigh
	for _, c := range chain {
		d := &tapDev{idcode: c.IDCode, irLen: c.IRLen}
		if d.irLen < 2 {
			d.irLen = 2
		}
		d.ir = d.resetIR()
		m.devs = append(m.devs, d)
	}
	m.state = tTLR
}

// tdo is the level on TDO after the falling edge: the LSB of device 0's
// shift register while shifting, otherwise the floating level.
func (m *tapModel) tdo() bool {
	if len(m.devs) == 0 {
		return m.float
	}
	switch m.state {
	case tShDR:
		return m.devs[0].drSh&1 == 1
	case tShIR:
		return m.devs[0].irSh&1 == 1
	}
	return m.float
}

// clock applies one rising TCK edge with the given TMS and TDI.
func (m *tapModel) clock(tms, tdi bool) {
	switch m.state {
	case tShDR:
		in := tdi
		for i := len(m.devs) - 1; i >= 0; i-- {
			d := m.devs[i]
			out := d.drSh&1 == 1
			d.drSh >>= 1
			if in {
				d.drSh |= 1 << (d.drLen - 1)
			}
			in = out
		}
	case tShIR:
		in := tdi
		for i := len(m.devs) - 1; i >= 0; i-- {
			d := m.devs[i]
			out := d.irSh&1 == 1
			d.irSh >>= 1
			if in {
				d.irSh |= 1 << (d.irLen - 1)
			}
			in = out
		}
	}
	idx := 0
	if tms {
		idx = 1
	}
	m.state = tapNext[m.state][idx]
	switch m.state {
	case tTLR:
		for _, d := range m.devs {
			d.ir = d.resetIR()
		}
	case tCapDR:
		for _, d := range m.devs {
			if d.ir == 1 && d.idcode != 0 {
				d.drLen, d.drSh = 32, uint64(d.idcode)
			} else {
				d.drLen, d.drSh = 1, 0
			}
		}
	case tCapIR:
		for _, d := range m.devs {
			d.irSh = 1 // the mandatory ...01 capture pattern
		}
	case tUpdIR:
		for _, d := range m.devs {
			d.ir = d.irSh
		}
	}
}
