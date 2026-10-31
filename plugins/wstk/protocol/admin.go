package protocol

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Admin console: a CRLF text console with the prompt "WSTK> ", served on the
// kit's TCP port 4902.

// AdminPrompt ends every admin reply.
const AdminPrompt = "WSTK> "

// adminAllowed lists the read-only commands hwlab sends. Everything else is
// refused: some admin commands reset the board controller or the target
// (an observed "target status" made the kit re-enumerate), switch the debug
// mode or recalibrate the AEM.
var adminAllowed = map[string]bool{
	"sys ver":      true,
	"boardid list": true,
	"dbg info":     true,
	"aem current":  true,
	"aem voltage":  true,
	"aem info":     true,
	"pti info":     true,
	"dch info":     true,
	"time info":    true,
}

// AdminAllowed reports whether cmd (whitespace-normalised) may be sent.
func AdminAllowed(cmd string) bool {
	return adminAllowed[strings.Join(strings.Fields(strings.ToLower(cmd)), " ")]
}

// AdminCommands returns the allowed commands, for documentation and errors.
func AdminCommands() []string {
	return []string{"sys ver", "boardid list", "dbg info", "aem current", "aem voltage", "aem info", "pti info", "dch info", "time info"}
}

// Admin talks to the admin console over a connection.
type Admin struct {
	c net.Conn
	r *bufio.Reader
}

// NewAdmin wraps an open connection to the admin port.
func NewAdmin(c net.Conn) *Admin { return &Admin{c: c, r: bufio.NewReader(c)} }

// Sync provokes a prompt with an empty line and then drains further
// prompts (a greeting may carry its own) until the console is quiet.
func (a *Admin) Sync(ctx context.Context, timeout time.Duration) error {
	if _, err := a.Command(ctx, "", timeout); err != nil {
		return fmt.Errorf("admin console: %w", err)
	}
	for {
		if err := a.c.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
			return err
		}
		if _, err := readUntilPrompt(a.r); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return nil
			}
			return fmt.Errorf("admin console: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// Command sends an allowed command (or an empty line) and returns its
// output without echo and prompt.
func (a *Admin) Command(ctx context.Context, cmd string, timeout time.Duration) (string, error) {
	cmd = strings.Join(strings.Fields(cmd), " ")
	if cmd != "" && !AdminAllowed(cmd) {
		return "", fmt.Errorf("admin command %q is not on hwlab's read-only list %v", cmd, AdminCommands())
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := a.c.SetDeadline(deadline); err != nil {
		return "", err
	}
	stop := context.AfterFunc(ctx, func() { _ = a.c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if _, err := io.WriteString(a.c, cmd+"\r\n"); err != nil {
		return "", err
	}
	out, err := readUntilPrompt(a.r)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	return CleanAdminReply(out, cmd), nil
}

func readUntilPrompt(r *bufio.Reader) (string, error) {
	var buf bytes.Buffer
	for {
		b, err := r.ReadByte()
		if err != nil {
			return buf.String(), err
		}
		buf.WriteByte(b)
		if bytes.HasSuffix(buf.Bytes(), []byte(AdminPrompt)) {
			return strings.TrimSuffix(buf.String(), AdminPrompt), nil
		}
	}
}

// CleanAdminReply drops the echoed command and blank lines.
func CleanAdminReply(s, cmd string) string {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		l = strings.TrimRight(l, " ")
		if l == "" || (len(lines) == 0 && cmd != "" && strings.TrimSpace(l) == cmd) {
			continue
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// ParseAdminKV splits "key : value" lines.
func ParseAdminKV(s string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		if k = strings.TrimSpace(k); k != "" {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}

var quantityRE = regexp.MustCompile(`(-?[0-9]+(?:\.[0-9]+)?(?:[eE][-+]?[0-9]+)?)\s*(nA|uA|µA|mA|A|mV|V)\b`)

var unitScale = map[string]float64{"nA": 1e-9, "uA": 1e-6, "µA": 1e-6, "mA": 1e-3, "A": 1, "mV": 1e-3, "V": 1}

// ParseQuantity finds the first number with one of the given units (e.g.
// "mA", "uA", "A" for current) and returns it in base units (A or V).
func ParseQuantity(s string, units ...string) (float64, bool) {
	for _, m := range quantityRE.FindAllStringSubmatch(s, -1) {
		for _, u := range units {
			if m[2] != u {
				continue
			}
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				return 0, false
			}
			return v * unitScale[u], true
		}
	}
	return 0, false
}

// CurrentUnits and VoltageUnits are the units the console prints.
var (
	CurrentUnits = []string{"nA", "uA", "µA", "mA", "A"}
	VoltageUnits = []string{"mV", "V"}
)

// ParseAdminCurrent reads the AEM current from "aem current" or "dbg info"
// output, in A.
func ParseAdminCurrent(s string) (float64, bool) {
	if v, ok := ParseAdminKV(s)["AEM current"]; ok {
		return ParseQuantity(v, CurrentUnits...)
	}
	return ParseQuantity(s, CurrentUnits...)
}

// ParseAdminVoltage reads the target voltage from "aem voltage" or
// "dbg info" output, in V.
func ParseAdminVoltage(s string) (float64, bool) {
	if v, ok := ParseAdminKV(s)["Target Voltage"]; ok {
		return ParseQuantity(v, VoltageUnits...)
	}
	return ParseQuantity(s, VoltageUnits...)
}
