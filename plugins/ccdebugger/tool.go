package ccdebugger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Tool runs the cc-tool host programmer.
type Tool interface {
	// Available returns the tool path or an error with an install hint.
	Available() (string, error)
	// Run executes cc-tool with args and returns its combined output. A
	// non-zero exit is reported as an error that still carries the output.
	Run(ctx context.Context, args []string) (string, error)
}

// ExecTool runs the real cc-tool binary.
type ExecTool struct {
	// Path overrides the lookup (tests); otherwise HWLAB_CC_TOOL, PATH and
	// /usr/local/bin are tried.
	Path string
	// Timeout bounds one invocation; zero means ten minutes (a slow-speed
	// 256 KB flash read takes minutes).
	Timeout time.Duration
}

const installHint = "cc-tool not found: install it (Debian/Ubuntu: apt install cc-tool; source: https://github.com/dashesy/cc-tool) or set HWLAB_CC_TOOL"

// Available implements Tool.
func (t *ExecTool) Available() (string, error) {
	for _, c := range []string{t.Path, os.Getenv("HWLAB_CC_TOOL")} {
		if c == "" {
			continue
		}
		if fileExists(c) {
			return c, nil
		}
		return "", fmt.Errorf("cc-tool path %q is not usable; %s", c, installHint)
	}
	if p, err := exec.LookPath("cc-tool"); err == nil {
		return p, nil
	}
	if p := filepath.Join("/usr/local/bin", "cc-tool"); fileExists(p) {
		return p, nil
	}
	return "", errors.New(installHint)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// Run implements Tool.
func (t *ExecTool) Run(ctx context.Context, args []string) (string, error) {
	path, err := t.Available()
	if err != nil {
		return "", err
	}
	d := t.Timeout
	if d == 0 {
		d = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err = cmd.Run()
	out := buf.String()
	if ctx.Err() != nil {
		return out, fmt.Errorf("cc-tool %s: %w", strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		return out, fmt.Errorf("cc-tool %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
