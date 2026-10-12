package scpi

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
)

// Fake is a SYNTHETIC SCPI oscilloscope for tests, demos and fixtures. It
// answers the commands of one vendor Profile with a 1 kHz sine whose amplitude
// is 0.5 V times the channel number. It proves the code paths, not any real
// instrument's behaviour.
type Fake struct {
	p     *Profile
	idn   string
	mu    sync.Mutex
	vals  map[string]string // upper-case node -> value
	log   []string
	errs  []string
	src   string
	state string
	polls int
	never bool // a single trigger never fires
	conns []net.Conn
	lis   net.Listener
}

// FakeFreq is the frequency of the fake's test signal.
const FakeFreq = 1000.0

// NewFake builds a fake of the given profile (rigol or siglent).
func NewFake(p *Profile) *Fake {
	f := &Fake{p: p, vals: map[string]string{}, state: "STOP", src: p.Source(1)}
	switch p.ID {
	case "siglent":
		f.idn = "Siglent Technologies,SDS1104X-E,SDSSYNTHETIC0001,1.0.0.0 (SYNTHETIC)"
	default:
		f.idn = "RIGOL TECHNOLOGIES,DS1054Z,DS1ZASYNTHETIC0001,00.00.00 (SYNTHETIC)"
	}
	for ch := 1; ch <= 4; ch++ {
		b := p.Chan(ch)
		f.set(b+p.ChanScale, "1.0")
		f.set(b+p.ChanOffset, "0")
		f.set(b+p.ChanCoupling, "DC")
		f.set(b+p.ChanEnable, "1")
		f.set(b+p.ChanProbe, "1")
	}
	f.set(p.TimebaseScale, "0.0005")
	f.set(p.TimebaseOffset, "0")
	f.set(p.TrigSource, p.Source(1))
	f.set(p.TrigSlope, shortForm(p.Slopes["rising"]))
	f.set(p.TrigLevel, "0.5")
	f.set(p.TrigSweep, shortForm(p.Sweeps["auto"]))
	return f
}

// NeverTrigger makes every later single acquisition wait forever.
func (f *Fake) NeverTrigger() {
	f.mu.Lock()
	f.never = true
	f.mu.Unlock()
}

func (f *Fake) set(node, v string) { f.vals[strings.ToUpper(node)] = v }

// Log returns the messages received so far.
func (f *Fake) Log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// Dial returns an in-memory transport connected to the fake.
func (f *Fake) Dial() Transport {
	a, b := net.Pipe()
	f.track(b)
	go f.serve(b)
	return &tcpTransport{c: a, desc: "fake://" + f.p.ID}
}

func (f *Fake) track(c net.Conn) {
	f.mu.Lock()
	f.conns = append(f.conns, c)
	f.mu.Unlock()
}

// Listen serves the fake on a TCP address ("127.0.0.1:0" picks a port) and
// returns the bound address.
func (f *Fake) Listen(addr string) (string, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.lis = l
	f.mu.Unlock()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			f.track(c)
			go f.serve(c)
		}
	}()
	return l.Addr().String(), nil
}

// Close stops the listener and drops every connection.
func (f *Fake) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lis != nil {
		_ = f.lis.Close()
	}
	for _, c := range f.conns {
		_ = c.Close()
	}
}

func (f *Fake) serve(c net.Conn) {
	defer func() {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Warn("close fake SCPI connection", "err", err)
		}
	}()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		for _, msg := range strings.Split(strings.TrimSpace(line), ";") {
			if out := f.handle(strings.TrimSpace(msg)); out != nil {
				if _, err := c.Write(out); err != nil {
					return
				}
			}
		}
	}
}

func str(s string) []byte { return []byte(s + "\n") }

func block(data []byte) []byte {
	n := strconv.Itoa(len(data))
	out := append([]byte(fmt.Sprintf("#%d%s", len(n), n)), data...)
	return append(out, '\n')
}

func (f *Fake) amp(src string) float64 {
	n, _ := strconv.Atoi(strings.TrimLeft(strings.ToUpper(src), "CHAN"))
	if n < 1 {
		n = 1
	}
	return 0.5 * float64(n)
}

func (f *Fake) channelOf(src string) int {
	n, _ := strconv.Atoi(strings.TrimLeft(strings.ToUpper(src), "CHAN"))
	return n
}

// handle processes one message and returns the reply (nil for none).
func (f *Fake) handle(msg string) []byte {
	if msg == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, msg)
	head, arg, _ := strings.Cut(msg, " ")
	up := strings.ToUpper(head)
	arg = strings.TrimSpace(arg)
	isRigol := f.p.ID != "siglent"

	switch up {
	case "*IDN?":
		return str(f.idn)
	case ":SYSTEM:ERROR?", ":SYST:ERR?":
		if len(f.errs) == 0 {
			return str(`0,"No error"`)
		}
		e := f.errs[0]
		f.errs = f.errs[1:]
		return str(e)
	case strings.ToUpper(f.p.Run), strings.ToUpper(f.p.Stop):
		f.state = "RUN"
		if up == strings.ToUpper(f.p.Stop) {
			f.state = "STOP"
		}
		return nil
	case strings.ToUpper(f.p.Force):
		f.state = "STOP"
		return nil
	case ":SINGLE":
		f.state, f.polls = "WAIT", 0
		return nil
	case ":TRIGGER:MODE":
		if strings.EqualFold(arg, "SINGLE") {
			f.state, f.polls = "WAIT", 0
			return nil
		}
	case strings.ToUpper(f.p.TrigStatus):
		return str(f.status(isRigol))
	case ":WAVEFORM:SOURCE":
		f.src = arg
		return nil
	case ":WAVEFORM:MODE", ":WAVEFORM:FORMAT", ":WAVEFORM:WIDTH":
		return nil
	case ":WAVEFORM:PREAMBLE?":
		scale := f.num(f.p.Chan(f.channelOf(f.src)) + f.p.ChanScale)
		if scale == 0 {
			scale = 1
		}
		return str(fmt.Sprintf("0,0,%d,1,%g,%g,%d,%g,0,127", fakePoints, 1e-6, -0.0006, 0, scale/25))
	case ":ACQUIRE:SRATE?":
		return str("1.000000E+06")
	case ":WAVEFORM:DATA?":
		return block(f.waveform(isRigol))
	case ":DISPLAY:DATA?":
		return block(fakePNG())
	case "SCDP":
		return append([]byte("BM"), bytes.Repeat([]byte{0x2a}, 62)...)
	}
	if strings.HasPrefix(up, ":MEASURE:") && strings.HasSuffix(up, "?") {
		return str(f.measure(arg))
	}
	if strings.HasSuffix(up, "?") {
		if v, ok := f.vals[strings.TrimSuffix(up, "?")]; ok {
			return str(v)
		}
	} else if _, ok := f.vals[up]; ok {
		f.vals[up] = arg
		return nil
	}
	f.errs = append(f.errs, `-113,"Undefined header; `+head+`"`)
	if strings.HasSuffix(up, "?") {
		return str("")
	}
	return nil
}

const fakePoints = 1200

func (f *Fake) num(node string) float64 {
	v, _ := strconv.ParseFloat(f.vals[strings.ToUpper(node)], 64)
	return v
}

// status walks WAIT -> TD -> STOP after a single trigger.
func (f *Fake) status(rigol bool) string {
	switch f.state {
	case "WAIT":
		f.polls++
		if f.polls >= 2 && !f.never {
			f.state = "TD"
		}
	case "TD":
		f.polls++
		if f.polls >= 4 {
			f.state = "STOP"
		}
		out := "TD"
		if !rigol {
			out = "Trig'd"
		}
		return out
	}
	if rigol {
		return f.state
	}
	return map[string]string{"WAIT": "Ready", "RUN": "Auto", "STOP": "Stop"}[f.state]
}

func (f *Fake) waveform(rigol bool) []byte {
	ch := f.channelOf(f.src)
	amp := f.amp(f.src)
	scale := f.num(f.p.Chan(ch) + f.p.ChanScale)
	ofst := f.num(f.p.Chan(ch) + f.p.ChanOffset)
	if scale == 0 {
		scale = 1
	}
	step := scale / 25
	out := make([]byte, fakePoints)
	for i := range out {
		v := amp * math.Sin(2*math.Pi*FakeFreq*(float64(i)*1e-6))
		code := math.Round(v / step)
		if rigol {
			out[i] = byte(math.Max(0, math.Min(255, code+127)))
		} else {
			out[i] = byte(int8(math.Max(-127, math.Min(127, code+ofst/step))))
		}
	}
	return out
}

func (f *Fake) measure(arg string) string {
	item, src, _ := strings.Cut(arg, ",")
	amp := f.amp(src)
	switch strings.ToUpper(item) {
	case "VPP", "PKPK":
		return strconv.FormatFloat(2*amp, 'E', 6, 64)
	case "VMAX", "MAX":
		return strconv.FormatFloat(amp, 'E', 6, 64)
	case "VMIN", "MIN":
		return strconv.FormatFloat(-amp, 'E', 6, 64)
	case "VAVG", "MEAN":
		return "0.000000E+00"
	case "VRMS", "RMS":
		return strconv.FormatFloat(amp/math.Sqrt2, 'E', 6, 64)
	case "FREQUENCY", "FREQ":
		return strconv.FormatFloat(FakeFreq, 'E', 6, 64)
	case "PERIOD", "PER":
		return strconv.FormatFloat(1/FakeFreq, 'E', 6, 64)
	}
	return "9.9E37"
}

func fakePNG() []byte {
	img := image.NewGray(image.Rect(0, 0, 16, 8))
	for x := 0; x < 16; x++ {
		img.SetGray(x, x%8, color.Gray{Y: 255})
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}
