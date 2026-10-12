// Package cli implements the hwlab command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/timeline"
)

type globals struct {
	home     string
	sessions string
	plugins  []string
	json     bool
	verbose  bool

	app *app.App
	out io.Writer
}

// Execute runs the CLI and returns the process exit code.
func Execute(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := NewRoot(stdout, stderr)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		if _, writeErr := fmt.Fprintln(stderr, "hwlab:", err); writeErr != nil {
			return 1 // The command already failed; stderr cannot report another error.
		}
		return 1
	}
	return 0
}

// NewRoot builds the command tree.
func NewRoot(stdout, stderr io.Writer) *cobra.Command {
	g := &globals{out: stdout}
	root := &cobra.Command{
		Use:           "hwlab",
		Short:         "Hardware-lab automation with a unified experiment timeline",
		Version:       config.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	pf := root.PersistentFlags()
	pf.StringVar(&g.home, "home", "", "hwlab home directory (default $HWLAB_HOME or ~/.hwlab)")
	pf.StringVar(&g.sessions, "sessions", "", "sessions directory (default <home>/sessions)")
	pf.StringArrayVar(&g.plugins, "plugin-path", nil, "extra plugin directory (repeatable)")
	pf.BoolVar(&g.json, "json", false, "machine-readable JSON output")
	pf.BoolVarP(&g.verbose, "verbose", "v", false, "debug logging on stderr")

	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Name() == "help" || cmd.Name() == "completion" {
			return nil
		}
		if g.home != "" {
			if err := os.Setenv("HWLAB_HOME", g.home); err != nil {
				return fmt.Errorf("set HWLAB_HOME: %w", err)
			}
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if g.sessions != "" {
			cfg.SessionsDir = g.sessions
		}
		cfg.PluginPaths = append(cfg.PluginPaths, g.plugins...)
		level := slog.LevelWarn
		if g.verbose {
			level = slog.LevelDebug
		}
		log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
		g.app = app.New(cfg, log)
		return nil
	}
	root.PersistentPostRun = func(*cobra.Command, []string) { g.close() }

	root.AddCommand(
		pluginsCmd(g), doctorCmd(g), devicesCmd(g), statusCmd(g), runCmd(g), replayCmd(g), analyzeCmd(g),
		sessionCmd(g), timelineCmd(g), correlateCmd(g), explainCmd(g), calibrateCmd(g), calibrateClocksCmd(g), plotCmd(g), invokeCmd(g),
		labCmd(g), ppk2Cmd(g), snifferCmd(g), modbusCmd(g), waveformCmd(g), usbCmd(g), targetCmd(g), mcpCmd(g),
	)
	return root
}

func (g *globals) close() {
	if g.app != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		g.app.Close(ctx)
		g.app = nil
	}
}

func (g *globals) printJSON(v any) error {
	enc := json.NewEncoder(g.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (g *globals) table() *tabwriter.Writer { return tabwriter.NewWriter(g.out, 0, 4, 2, ' ', 0) }

// printer retains the first output failure and attaches it to the command's
// result. Subsequent writes stop after a broken pipe or other output error.
type printer struct{ err error }

func (p *printer) printf(w io.Writer, format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(w, format, args...)
	}
}

func (p *printer) println(w io.Writer, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(w, args...)
	}
}

func (p *printer) print(w io.Writer, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprint(w, args...)
	}
}

func (p *printer) finish(err *error) {
	if p.err != nil {
		*err = errors.Join(*err, p.err)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func join(s []string) string { return orDash(strings.Join(s, ",")) }

func fmtDur(ns int64) string { return timeline.FormatNs(ns) }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func versionString() string { return config.Version }

func appInvoke(plug, dev, capName, method string, args map[string]any, yes bool) app.InvokeRequest {
	return app.InvokeRequest{Plugin: plug, Device: dev, Capability: capName, Method: method, Args: args, Confirm: yes}
}
