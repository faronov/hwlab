package jlink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/faronov/hwlab/internal/ownedtemp"
)

// ExeEnv overrides the J-Link Commander executable.
const ExeEnv = "HWLAB_JLINK_EXE"

// DefaultRTTPort is the RTT telnet port of the J-Link software.
const DefaultRTTPort = 19021

// Target selects the debug target behind a probe.
type Target struct {
	Device    string // J-Link device name, e.g. "NRF52840_XXAA"; empty = probe only
	Interface string // "SWD" or "JTAG"
	SpeedKHz  int
}

// Commander runs J-Link Commander scripts. It is the only place that knows
// about the SEGGER tools; tests replace it.
type Commander interface {
	// Available reports the executable used, or an error that explains how to
	// install it.
	Available() (string, error)
	// Run executes script (one command per line) against the probe with the
	// given serial number and returns the combined output.
	Run(ctx context.Context, serial string, t Target, script []string) (string, error)
	// Start launches a long-running Commander session that keeps the target
	// connected and serves RTT on the telnet port. rttAddr, when non-zero, is
	// the RTT control block address; otherwise the J-Link software has to find
	// the block itself, which it does not always do. stop ends it.
	Start(ctx context.Context, serial string, t Target, rttPort int, rttAddr uint64) (stop func(), err error)
	// DeviceDatabase returns SEGGER's device list as exported by
	// ExpDevListXML. It needs no probe.
	DeviceDatabase(ctx context.Context) ([]byte, error)
}

// dialogGuard runs first in every script that talks to a probe: it asks the
// J-Link DLL not to open the device selection dialog, which would block a
// headless run until someone closes it (-NoGui alone does not prevent the
// device picker; names are also resolved against SEGGER's list before any
// launch). Firmware updates are only suppressed, never run. SetBatchMode is
// deliberately not used: it answers dialogs with their default, and the
// default of an "unsecure device?" prompt is a mass erase.
var dialogGuard = []string{"exec HideDeviceSelection = 1", "exec SuppressInfoUpdateFW"}

type timeoutKey struct{}

// WithTimeout raises the per-Run timeout of ExecCommander for long flash
// operations.
func WithTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, timeoutKey{}, d)
}

// ExecCommander drives the real JLinkExe.
type ExecCommander struct {
	Exe     string        // overrides discovery
	Timeout time.Duration // per Run; default 60 s
}

var windowsExe = `C:\Program Files\SEGGER\JLink\JLink.exe`

func (c *ExecCommander) find() (string, error) {
	cands := []string{c.Exe, os.Getenv(ExeEnv)}
	for _, n := range []string{"JLinkExe", "JLink.exe", "JLink"} {
		if p, err := exec.LookPath(n); err == nil {
			cands = append(cands, p)
		}
	}
	cands = append(cands, "/opt/SEGGER/JLink/JLinkExe", "/Applications/SEGGER/JLink/JLinkExe", windowsExe)
	for _, p := range cands {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("SEGGER J-Link Commander (JLinkExe) not found: install the J-Link Software and Documentation Pack "+
		"from https://www.segger.com/downloads/jlink/ or set %s", ExeEnv)
}

// Available implements Commander.
func (c *ExecCommander) Available() (string, error) { return c.find() }

func (t Target) args(serial string) []string {
	iface := t.Interface
	if iface == "" {
		iface = "SWD"
	}
	speed := t.SpeedKHz
	if speed <= 0 {
		speed = 4000
	}
	a := []string{"-NoGui", "1", "-ExitOnError", "1"}
	if serial != "" {
		a = append(a, "-USB", serial)
	}
	a = append(a, "-If", iface, "-Speed", strconv.Itoa(speed))
	if t.Device != "" {
		a = append(a, "-Device", t.Device, "-AutoConnect", "1")
	}
	return a
}

// Run implements Commander.
func (c *ExecCommander) Run(ctx context.Context, serial string, t Target, script []string) (result string, err error) {
	exe, err := c.find()
	if err != nil {
		return "", err
	}
	dir, err := ownedtemp.New("hwlab-jlink-")
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	f, err := os.OpenFile(dir.File("commands.jlink"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	lines := []string{}
	if serial != "" {
		lines = append(lines, dialogGuard...)
	}
	lines = append(append(lines, script...), "exit")
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		return "", errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close Commander script: %w", err)
	}

	return c.exec(ctx, exe, append(t.args(serial), "-CommandFile", f.Name()))
}

// exec runs the commander with a timeout. Only the child it started is
// killed when the timeout expires.
func (c *ExecCommander) exec(ctx context.Context, exe string, args []string) (string, error) {
	to := c.Timeout
	if to <= 0 {
		to = 60 * time.Second
	}
	if d, ok := ctx.Value(timeoutKey{}).(time.Duration); ok && d > to {
		to = d
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return out.String(), fmt.Errorf("%s %w after %s (only the child process it started was stopped)", filepath.Base(exe), ErrTimeout, to)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return out.String(), err
		}
		// A non-zero exit still carries the useful diagnostics in the output.
	}
	return out.String(), nil
}

// ErrTimeout marks a commander run that did not finish in time.
var ErrTimeout = errors.New("timed out")

// DeviceDatabase implements Commander.
func (c *ExecCommander) DeviceDatabase(ctx context.Context) (data []byte, err error) {
	exe, err := c.find()
	if err != nil {
		return nil, err
	}
	dir, err := ownedtemp.New("hwlab-jlink-devs-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	xml := dir.File("devices.xml")
	script := dir.File("commands.jlink")
	if err := os.WriteFile(script, []byte(fmt.Sprintf("ExpDevListXML %s\nexit\n", xml)), 0o600); err != nil {
		return nil, err
	}
	out, err := c.exec(ctx, exe, []string{"-NoGui", "1", "-ExitOnError", "1", "-CommandFile", script})
	if err != nil {
		return nil, err
	}
	data, err = os.ReadFile(xml)
	if err != nil {
		return nil, fmt.Errorf("%s did not export its device list (ExpDevListXML): %w; output: %v", filepath.Base(exe), err, tail(out, 3))
	}
	return data, nil
}

// Start implements Commander.
func (c *ExecCommander) Start(_ context.Context, serial string, t Target, rttPort int, rttAddr uint64) (func(), error) {
	exe, err := c.find()
	if err != nil {
		return nil, err
	}
	args := append(t.args(serial), "-RTTTelnetPort", strconv.Itoa(rttPort))
	cmd := exec.Command(exe, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", filepath.Base(exe), err)
	}
	for _, l := range dialogGuard {
		if _, err := fmt.Fprintln(stdin, l); err != nil {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("send %s: %w", l, err)
		}
	}
	if rttAddr != 0 {
		if _, err := fmt.Fprintf(stdin, "exec SetRTTAddr 0x%X\n", rttAddr); err != nil {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("send SetRTTAddr: %w", err)
		}
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stop := func() {
		if _, err := io.WriteString(stdin, "exit\n"); err != nil {
			slog.Warn("request Commander exit", "err", err)
		}
		if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			slog.Warn("close Commander stdin", "err", err)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	return stop, nil
}
