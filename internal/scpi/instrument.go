package scpi

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTimeout bounds one instrument reply.
	DefaultTimeout = 5 * time.Second
	// MaxBlock bounds one binary block; larger replies are refused.
	MaxBlock = 64 << 20
	maxLine  = 1 << 20
	maxScan  = 32
)

// Instrument is one SCPI endpoint. Calls are serialised, so concurrent users
// cannot interleave a command with another call's reply.
type Instrument struct {
	// Timeout bounds every reply; set before use.
	Timeout time.Duration

	mu    sync.Mutex
	t     Transport
	buf   []byte
	stale int
}

// NewInstrument wraps a transport. The instrument owns it from now on.
func NewInstrument(t Transport) *Instrument {
	return &Instrument{t: t, Timeout: DefaultTimeout}
}

// Describe names the endpoint.
func (i *Instrument) Describe() string { return i.t.Describe() }

// Close closes the transport.
func (i *Instrument) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.t.Close()
}

// StaleBytes counts reply bytes that arrived without a pending query and were
// dropped; a non-zero value means an earlier reply was not fully consumed.
func (i *Instrument) StaleBytes() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.stale
}

// checkText refuses text that could smuggle a second message.
func checkText(cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return errors.New("empty SCPI command")
	}
	for _, r := range cmd {
		if r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("SCPI command contains control character %#x: send one message per call", r)
		}
	}
	return nil
}

// IsQuery reports whether every ";"-separated message of cmd is a query: its
// header (the first word, before any parameters) ends with "?". A compound
// message that mixes a setting with a query is a command, so it cannot pass as
// a safe read.
func IsQuery(cmd string) bool {
	if checkText(cmd) != nil {
		return false
	}
	var seg strings.Builder
	var quote rune
	ok := func() bool {
		s := strings.TrimSpace(seg.String())
		seg.Reset()
		if k := strings.IndexAny(s, " \t"); k >= 0 {
			s = s[:k]
		}
		return strings.HasSuffix(s, "?")
	}
	for _, r := range cmd {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			seg.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			seg.WriteRune(r)
		case r == ';':
			if !ok() {
				return false
			}
		default:
			seg.WriteRune(r)
		}
	}
	return quote == 0 && ok()
}

func (i *Instrument) send(cmd string) error {
	if err := checkText(cmd); err != nil {
		return err
	}
	if len(i.buf) > 0 {
		i.stale += len(i.buf)
		i.buf = i.buf[:0]
	}
	return i.t.Write([]byte(strings.TrimRight(cmd, " ") + "\n"))
}

// Command sends a message that expects no reply.
func (i *Instrument) Command(cmd string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.send(cmd)
}

// Query sends a message and returns its single-line reply.
func (i *Instrument) Query(cmd string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.send(cmd); err != nil {
		return "", err
	}
	line, err := i.readLine(time.Now().Add(i.Timeout))
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(cmd), err)
	}
	return strings.TrimSpace(line), nil
}

// QueryBinary sends a message whose reply is an IEEE 488.2 block and returns
// the block payload. A short prefix before the "#" (some instruments echo a
// header such as "DAT2,") is returned separately.
func (i *Instrument) QueryBinary(cmd string) (data []byte, prefix string, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.send(cmd); err != nil {
		return nil, "", err
	}
	data, prefix, err = i.readBlock(time.Now().Add(i.Timeout))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", strings.TrimSpace(cmd), err)
	}
	return data, prefix, nil
}

// QueryRaw sends a message and collects everything the instrument returns
// until it has been silent for idle. It serves replies without a length header
// (some screenshot commands) and is bounded by max bytes.
func (i *Instrument) QueryRaw(cmd string, idle time.Duration, max int) ([]byte, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.send(cmd); err != nil {
		return nil, err
	}
	var out []byte
	first := true
	for len(out) < max {
		wait := idle
		if first {
			wait = i.Timeout
		}
		b, err := i.fill(time.Now().Add(wait))
		if err != nil {
			if errors.Is(err, ErrTimeout) && !first {
				return out, nil
			}
			return nil, fmt.Errorf("%s: %w", strings.TrimSpace(cmd), err)
		}
		first = false
		out = append(out, b...)
	}
	return nil, fmt.Errorf("%s: reply exceeds %d bytes", strings.TrimSpace(cmd), max)
}

// fill returns the next chunk, from the leftover buffer first.
func (i *Instrument) fill(deadline time.Time) ([]byte, error) {
	if len(i.buf) > 0 {
		b := i.buf
		i.buf = nil
		return b, nil
	}
	return i.pull(deadline)
}

// pull reads one chunk from the transport.
func (i *Instrument) pull(deadline time.Time) ([]byte, error) {
	left := time.Until(deadline)
	if left <= 0 {
		return nil, ErrTimeout
	}
	tmp := make([]byte, 32<<10)
	n, err := i.t.Read(tmp, left)
	if n > 0 {
		return tmp[:n], nil
	}
	return nil, err
}

// more appends the next transport chunk to the buffer.
func (i *Instrument) more(deadline time.Time) error {
	b, err := i.pull(deadline)
	if err != nil {
		return err
	}
	i.buf = append(i.buf, b...)
	return nil
}

func (i *Instrument) readLine(deadline time.Time) (string, error) {
	for {
		if k := bytes.IndexByte(i.buf, '\n'); k >= 0 {
			line := string(i.buf[:k])
			i.buf = i.buf[k+1:]
			return strings.TrimRight(line, "\r"), nil
		}
		if len(i.buf) > maxLine {
			return "", errors.New("reply line exceeds 1 MiB without a terminator")
		}
		if err := i.more(deadline); err != nil {
			return "", err
		}
	}
}

func (i *Instrument) need(n int, deadline time.Time) error {
	for len(i.buf) < n {
		if err := i.more(deadline); err != nil {
			return err
		}
	}
	return nil
}

func (i *Instrument) readBlock(deadline time.Time) ([]byte, string, error) {
	for bytes.IndexByte(i.buf, '#') < 0 {
		if len(i.buf) > maxScan {
			return nil, "", fmt.Errorf("reply is not a binary block (no \"#\" in the first %d bytes: %q)", maxScan, printable(i.buf))
		}
		if k := bytes.IndexByte(i.buf, '\n'); k >= 0 {
			return nil, "", fmt.Errorf("reply is not a binary block: %q", printable(i.buf[:k]))
		}
		if err := i.more(deadline); err != nil {
			return nil, "", err
		}
	}
	k := bytes.IndexByte(i.buf, '#')
	prefix := string(i.buf[:k])
	i.buf = i.buf[k+1:]
	if err := i.need(1, deadline); err != nil {
		return nil, "", err
	}
	nd := i.buf[0]
	i.buf = i.buf[1:]
	if nd == '0' { // indefinite length: runs to the terminator
		line, err := i.readLine(deadline)
		return []byte(line), prefix, err
	}
	if nd < '1' || nd > '9' {
		return nil, "", fmt.Errorf("binary block header: bad digit count %q", nd)
	}
	digits := int(nd - '0')
	if err := i.need(digits, deadline); err != nil {
		return nil, "", err
	}
	size, err := strconv.Atoi(string(i.buf[:digits]))
	if err != nil || size < 0 {
		return nil, "", fmt.Errorf("binary block header: bad length %q", printable(i.buf[:digits]))
	}
	if size > MaxBlock {
		return nil, "", fmt.Errorf("binary block of %d bytes exceeds the %d byte limit", size, MaxBlock)
	}
	i.buf = i.buf[digits:]
	if err := i.need(size, deadline); err != nil {
		return nil, "", fmt.Errorf("binary block cut short (want %d bytes): %w", size, err)
	}
	data := append([]byte(nil), i.buf[:size]...)
	i.buf = i.buf[size:]
	// Consume the message terminator so the stream stays in step.
	if len(i.buf) == 0 {
		short := time.Now().Add(150 * time.Millisecond)
		if short.After(deadline) {
			short = deadline
		}
		_ = i.more(short)
	}
	if len(i.buf) > 0 && i.buf[0] == '\r' {
		i.buf = i.buf[1:]
	}
	if len(i.buf) > 0 && i.buf[0] == '\n' {
		i.buf = i.buf[1:]
	}
	return data, prefix, nil
}

func printable(b []byte) string {
	if len(b) > 40 {
		b = b[:40]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '.'
		}
		return r
	}, string(b))
}

// IDN is a parsed *IDN? reply.
type IDN struct {
	Manufacturer string
	Model        string
	Serial       string
	Firmware     string
	Raw          string
}

// ParseIDN splits the four comma-separated fields of an *IDN? reply; missing
// fields stay empty, extra fields stay in Firmware.
func ParseIDN(s string) IDN {
	s = strings.TrimSpace(s)
	f := strings.SplitN(s, ",", 4)
	for k := range f {
		f[k] = strings.TrimSpace(f[k])
	}
	idn := IDN{Raw: s}
	if len(f) > 0 {
		idn.Manufacturer = f[0]
	}
	if len(f) > 1 {
		idn.Model = f[1]
	}
	if len(f) > 2 {
		idn.Serial = f[2]
	}
	if len(f) > 3 {
		idn.Firmware = f[3]
	}
	return idn
}

// Identify queries *IDN?.
func (i *Instrument) Identify() (IDN, error) {
	s, err := i.Query("*IDN?")
	if err != nil {
		return IDN{}, err
	}
	if s == "" {
		return IDN{}, errors.New("*IDN? returned an empty reply")
	}
	return ParseIDN(s), nil
}

// ErrorQueue reads one entry of the standard error queue (code 0 = empty).
func (i *Instrument) ErrorQueue() (code int, msg string, err error) {
	s, err := i.Query(":SYSTem:ERRor?")
	if err != nil {
		return 0, "", err
	}
	c, m, _ := strings.Cut(s, ",")
	code, perr := strconv.Atoi(strings.TrimSpace(c))
	if perr != nil {
		return 0, s, fmt.Errorf("unparseable error queue reply %q", s)
	}
	return code, strings.Trim(strings.TrimSpace(m), `"`), nil
}
