// Package decode wraps optional external protocol dissectors. Today that is
// tshark (Wireshark): hwlab writes standard pcap artifacts and can ask tshark
// for per-packet summaries, but never requires it. When tshark is missing the
// caller gets ErrUnavailable and everything else keeps working.
package decode

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ErrUnavailable is returned when the dissector binary cannot be found.
var ErrUnavailable = errors.New("tshark not found in PATH (optional; install Wireshark to decode packets)")

// Summary is one dissected packet.
type Summary struct {
	Frame     int
	EpochSec  float64
	Protocols string // e.g. "eth:ethertype:ip:udp" or "wpan:zbee_nwk:zbee_aps"
	Info      string
}

// Runner executes a command and returns its stdout. Tests replace it.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Tshark decodes pcap files with the tshark binary.
type Tshark struct {
	// Path overrides the binary location; empty means look up "tshark".
	Path string
	// Run overrides command execution (tests).
	Run Runner
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

// Available reports the resolved tshark path or ErrUnavailable.
func (t *Tshark) Available() (string, error) {
	if t.Path != "" {
		return t.Path, nil
	}
	p, err := exec.LookPath("tshark")
	if err != nil {
		return "", ErrUnavailable
	}
	return p, nil
}

// Summaries returns one Summary per packet of the pcap file.
func (t *Tshark) Summaries(ctx context.Context, pcapPath string) ([]Summary, error) {
	bin, err := t.Available()
	if err != nil {
		return nil, err
	}
	run := t.Run
	if run == nil {
		run = defaultRun
	}
	out, err := run(ctx, bin, "-r", pcapPath, "-n", "-T", "fields", "-E", "separator=\t",
		"-e", "frame.number", "-e", "frame.time_epoch", "-e", "frame.protocols", "-e", "_ws.col.Info")
	if err != nil {
		return nil, fmt.Errorf("tshark: %w", err)
	}
	return ParseFields(string(out))
}

// ParseFields parses the tab-separated tshark field output used by Summaries.
func ParseFields(out string) ([]Summary, error) {
	var res []Summary
	for i, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 3 {
			return nil, fmt.Errorf("tshark output line %d: want 4 tab-separated fields, got %d", i+1, len(f))
		}
		n, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("tshark output line %d: frame number: %w", i+1, err)
		}
		ts, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			return nil, fmt.Errorf("tshark output line %d: time: %w", i+1, err)
		}
		s := Summary{Frame: n, EpochSec: ts, Protocols: f[2]}
		if len(f) == 4 {
			s.Info = f[3]
		}
		res = append(res, s)
	}
	return res, nil
}
