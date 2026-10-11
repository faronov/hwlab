package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/policy"
)

func pluginsCmd(g *globals) *cobra.Command {
	var details bool
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "List discovered plugins with implementation and validation status",
		Long: `List discovered plugins, their capabilities and their support status.

IMPL says how much is written (complete, partial, skeleton, missing); VALIDATION
says how far it has been exercised (untested, synthetic, replay,
real-hardware-partial, real-hardware). Code and passing tests are not hardware
validation. Use --details for what is verified, unverified and known gaps.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ps := g.app.ListPlugins(cmd.Context())
			if g.json {
				return g.printJSON(ps)
			}
			if len(ps) == 0 {
				fmt.Fprintf(g.out, "no plugins found; searched: %s\n", strings.Join(g.app.Plugins.SearchPaths(), ", "))
				return nil
			}
			w := g.table()
			fmt.Fprintln(w, "ID\tVERSION\tIMPL\tVALIDATION\tDEPENDENCY\tMISSING BACKENDS\tCAPABILITIES\tPATH")
			for _, p := range ps {
				if p.Error != "" {
					fmt.Fprintf(w, "%s\t-\t-\t-\t-\t-\tERROR: %s\t%s\n", p.ID, p.Error, p.Path)
					continue
				}
				impl, val, dep, miss := "unclassified", "unknown", "-", "-"
				if p.Support != nil {
					impl, val, dep = string(p.Support.Implementation), string(p.Support.Validation), string(p.Support.Dependency)
					miss = orDash(strings.Join(p.Support.Missing, "; "))
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, orDash(p.Version), impl, val, dep, miss, join(p.Capabilities), orDash(p.Path))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if details {
				for _, p := range ps {
					if p.Support == nil {
						continue
					}
					fmt.Fprintf(g.out, "\n%s\n", p.ID)
					for _, b := range p.Support.Backends {
						fmt.Fprintf(g.out, "  backend: %s\n    implementation: %s\n    validation: %s\n    dependency: %s", b.Name, b.Implementation, b.Validation, b.Dependency)
						if b.Dependencies != "" {
							fmt.Fprintf(g.out, " (%s)", b.Dependencies)
						}
						fmt.Fprintf(g.out, "\n    cgo: %s\n", b.Cgo)
						for _, l := range []struct {
							label string
							items []string
						}{{"verified", b.Verified}, {"unverified", b.Unverified}, {"known gaps", b.Gaps}} {
							for i, it := range l.items {
								if i == 0 {
									fmt.Fprintf(g.out, "    %s:\n", l.label)
								}
								fmt.Fprintf(g.out, "      - %s\n", it)
							}
						}
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&details, "details", false, "show verified, unverified and known gaps per backend")
	return cmd
}

func doctorCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the environment and show the support matrix status of every plugin",
		Long: `Check the sessions directory, plugin discovery, external tools (tshark, SEGGER
J-Link, cc-tool), the Saleae Logic 2 automation endpoint and usbmon access, and
list every plugin with its implementation and validation status. A missing
optional tool is a warning; the command fails only on hard errors.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := g.app.Doctor(cmd.Context())
			if g.json {
				if err := g.printJSON(r); err != nil {
					return err
				}
			} else {
				w := g.table()
				fmt.Fprintln(w, "CHECK\tRESULT\tDETAIL")
				for _, c := range r.Checks {
					fmt.Fprintf(w, "%s\t%s\t%s\n", c.Name, c.Level, c.Detail)
				}
				if err := w.Flush(); err != nil {
					return err
				}
				fmt.Fprintln(g.out)
				w = g.table()
				fmt.Fprintln(w, "PLUGIN\tINSTALLED\tIMPL\tVALIDATION\tDEPENDENCY\tMISSING BACKENDS")
				for _, p := range r.Plugins {
					inst := "yes"
					if !p.Installed {
						inst = "no"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, inst, orDash(string(p.Implementation)), orDash(string(p.Validation)), orDash(string(p.Dependency)), orDash(strings.Join(p.Missing, "; ")))
				}
				if err := w.Flush(); err != nil {
					return err
				}
				fmt.Fprintln(g.out, "\nvalidation is the highest evidence level; see `hwlab plugins --details` and docs/support-matrix.md")
			}
			if r.Failed() {
				return fmt.Errorf("doctor found hard failures")
			}
			return nil
		},
	}
}

func devicesCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "devices",
		Short: "List devices reported by all plugins",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			devs, errs := g.app.ListDevices(cmd.Context())
			if g.json {
				return g.printJSON(map[string]any{"devices": devs, "errors": errs})
			}
			w := g.table()
			fmt.Fprintln(w, "DEVICE\tNAME\tTRANSPORT\tCAPABILITIES")
			for _, d := range devs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.ID, d.Name, orDash(d.Transport), join(d.Capabilities))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			for _, e := range errs {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: plugin %s: %s\n", e.Plugin, e.Error)
			}
			if len(devs) == 0 && len(errs) == 0 {
				fmt.Fprintln(g.out, "(no devices)")
			}
			return nil
		},
	}
}

func statusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Lab overview: plugins, devices, captures in progress and latest session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			plugins := g.app.ListPlugins(ctx)
			devs, derrs := g.app.ListDevices(ctx)
			sessions, _ := g.app.ListSessions()
			st := map[string]any{"version": versionString(), "sessions_dir": g.app.Cfg.SessionsDir,
				"plugins": len(plugins), "devices": len(devs), "device_errors": derrs,
				"running_captures": g.app.Status(), "sessions": len(sessions)}
			if len(sessions) > 0 {
				st["latest_session"] = sessions[0].ID
			}
			if g.json {
				return g.printJSON(st)
			}
			fmt.Fprintf(g.out, "hwlab %s\nsessions dir:   %s\nplugins:        %d\ndevices:        %d\nsessions:       %d\n",
				versionString(), g.app.Cfg.SessionsDir, len(plugins), len(devs), len(sessions))
			if len(sessions) > 0 {
				fmt.Fprintf(g.out, "latest session: %s (%s)\n", sessions[0].ID, sessions[0].Status)
			}
			for _, p := range plugins {
				if p.Error != "" {
					fmt.Fprintf(g.out, "  plugin %s: ERROR %s\n", p.ID, p.Error)
				}
			}
			for _, e := range derrs {
				fmt.Fprintf(g.out, "  plugin %s: cannot list devices: %s\n", e.Plugin, e.Error)
			}
			return nil
		},
	}
}

func invokeCmd(g *globals) *cobra.Command {
	var args []string
	var yes bool
	c := &cobra.Command{
		Use:   "invoke PLUGIN[/DEVICE] CAPABILITY.METHOD",
		Short: "Call a capability method on a device (dangerous methods need --yes)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, pos []string) error {
			plug, dev, _ := strings.Cut(pos[0], "/")
			capName, method, ok := strings.Cut(pos[1], ".")
			if !ok {
				return fmt.Errorf("expected CAPABILITY.METHOD, got %q", pos[1])
			}
			m := map[string]any{}
			for _, a := range args {
				k, v, ok := strings.Cut(a, "=")
				if !ok {
					return fmt.Errorf("--arg %q: expected key=value", a)
				}
				m[k] = parseScalar(v)
			}
			res, err := g.app.Invoke(cmd.Context(), appInvoke(plug, dev, capName, method, m, yes))
			if err != nil {
				return confirmHint(err)
			}
			return g.printJSON(res)
		},
	}
	c.Flags().StringArrayVarP(&args, "arg", "a", nil, "method argument key=value (repeatable)")
	c.Flags().BoolVar(&yes, "yes", false, "confirm a dangerous operation")
	return c
}

// parseScalar decodes a command-line value as a YAML scalar (number, bool, string).
func parseScalar(s string) any {
	var v any
	if err := yaml.Unmarshal([]byte(s), &v); err != nil || v == nil {
		return s
	}
	switch v.(type) {
	case map[string]any, map[any]any, []any:
		return s
	}
	return experiment.Normalize(v)
}

func confirmHint(err error) error {
	if policy.IsConfirmationRequired(err) {
		return fmt.Errorf("%w", err)
	}
	return err
}
