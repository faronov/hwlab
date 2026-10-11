package uart

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Flow control modes.
const (
	FlowNone    = "none"
	FlowRTSCTS  = "rtscts"
	FlowXonXoff = "xonxoff"
)

// Mode is the line configuration of a serial port.
type Mode struct {
	Baud     int
	DataBits int
	Parity   string // none, even, odd, mark, space
	StopBits string // "1", "1.5", "2"
	Flow     string
	// DTR and RTS are the initial modem output levels; nil leaves the OS
	// default (both asserted on Linux and macOS).
	DTR, RTS *bool
}

// DefaultMode is 115200 8N1 without flow control.
func DefaultMode() Mode {
	return Mode{Baud: 115200, DataBits: 8, Parity: "none", StopBits: "1", Flow: FlowNone}
}

// Framing renders the classic "8N1" notation.
func (m Mode) Framing() string {
	p := map[string]string{"none": "N", "even": "E", "odd": "O", "mark": "M", "space": "S"}[m.Parity]
	return fmt.Sprintf("%d%s%s", m.DataBits, p, m.StopBits)
}

func (m Mode) String() string {
	return fmt.Sprintf("%d %s flow=%s", m.Baud, m.Framing(), m.Flow)
}

// sameLine reports whether two modes need no SetMode call (modem lines are
// handled separately).
func (m Mode) sameLine(o Mode) bool {
	return m.Baud == o.Baud && m.DataBits == o.DataBits && m.Parity == o.Parity &&
		m.StopBits == o.StopBits && m.Flow == o.Flow
}

// parseMode overlays the line settings found in cfg on base.
func parseMode(base Mode, cfg map[string]any) (Mode, error) {
	m := base
	if m.Baud == 0 {
		m = DefaultMode()
	}
	if v, ok := cfg["baud"]; ok {
		n, err := toInt(v)
		if err != nil || n < 1 || n > 12_000_000 {
			return m, fmt.Errorf("baud %v: want 1..12000000", v)
		}
		m.Baud = n
	}
	if v, ok := cfg["data_bits"]; ok {
		n, err := toInt(v)
		if err != nil || n < 5 || n > 8 {
			return m, fmt.Errorf("data_bits %v: want 5..8", v)
		}
		m.DataBits = n
	}
	if v, ok := cfg["parity"]; ok {
		s := strings.ToLower(fmt.Sprint(v))
		switch s {
		case "none", "n":
			m.Parity = "none"
		case "even", "e":
			m.Parity = "even"
		case "odd", "o":
			m.Parity = "odd"
		case "mark", "m":
			m.Parity = "mark"
		case "space", "s":
			m.Parity = "space"
		default:
			return m, fmt.Errorf("parity %v: want none, even, odd, mark or space", v)
		}
	}
	if v, ok := cfg["stop_bits"]; ok {
		f, err := toFloat(v)
		switch {
		case err != nil:
			return m, fmt.Errorf("stop_bits %v: want 1, 1.5 or 2", v)
		case f == 1:
			m.StopBits = "1"
		case f == 1.5:
			m.StopBits = "1.5"
		case f == 2:
			m.StopBits = "2"
		default:
			return m, fmt.Errorf("stop_bits %v: want 1, 1.5 or 2", v)
		}
	}
	if v, ok := cfg["flow_control"]; ok {
		s := strings.ToLower(fmt.Sprint(v))
		switch s {
		case "none", "off", "":
			m.Flow = FlowNone
		case "rtscts", "rts/cts", "hardware":
			m.Flow = FlowRTSCTS
		case "xonxoff", "software":
			m.Flow = FlowXonXoff
		default:
			return m, fmt.Errorf("flow_control %v: want none, rtscts or xonxoff", v)
		}
	}
	for _, k := range []string{"dtr", "rts"} {
		v, ok := cfg[k]
		if !ok {
			continue
		}
		b, isBool := v.(bool)
		if !isBool {
			return m, fmt.Errorf("%s must be true or false", k)
		}
		if k == "dtr" {
			m.DTR = &b
		} else {
			m.RTS = &b
		}
	}
	return m, nil
}

// captureConfig tunes how received bytes are grouped into timeline events.
type captureConfig struct {
	// idleGap closes a chunk when no byte arrived for this long.
	idleGap time.Duration
	// maxChunkBytes and maxChunkAge bound a chunk of continuous traffic.
	maxChunkBytes int
	maxChunkAge   time.Duration
	// lines additionally emits one Log event per received text line.
	lines bool
}

func parseCaptureConfig(cfg map[string]any) (captureConfig, error) {
	c := captureConfig{idleGap: 10 * time.Millisecond, maxChunkBytes: 1024, maxChunkAge: 100 * time.Millisecond}
	if v, ok := cfg["idle_gap_ms"]; ok {
		n, err := toInt(v)
		if err != nil || n < 1 || n > 1000 {
			return c, fmt.Errorf("idle_gap_ms %v: want 1..1000", v)
		}
		c.idleGap = time.Duration(n) * time.Millisecond
	}
	if v, ok := cfg["max_chunk_bytes"]; ok {
		n, err := toInt(v)
		if err != nil || n < 1 || n > 65536 {
			return c, fmt.Errorf("max_chunk_bytes %v: want 1..65536", v)
		}
		c.maxChunkBytes = n
	}
	if v, ok := cfg["max_chunk_ms"]; ok {
		n, err := toInt(v)
		if err != nil || n < 1 || n > 10000 {
			return c, fmt.Errorf("max_chunk_ms %v: want 1..10000", v)
		}
		c.maxChunkAge = time.Duration(n) * time.Millisecond
	}
	if v, ok := cfg["lines"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return c, fmt.Errorf("lines must be true or false")
		}
		c.lines = b
	}
	return c, nil
}

func toInt(v any) (int, error) {
	f, err := toFloat(v)
	if err != nil {
		return 0, err
	}
	if f != float64(int(f)) {
		return 0, fmt.Errorf("not an integer: %v", v)
	}
	return int(f), nil
}

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(x), 64)
	}
	return 0, fmt.Errorf("not a number: %v", v)
}
