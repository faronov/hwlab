package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/faronov/hwlab/internal/analysis"
	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/plot"
	"github.com/faronov/hwlab/internal/timeline"
)

func sortStrings(s []string) { sort.Strings(s) }

func plotSpec(format string, from, to int64) plot.Spec {
	return plot.Spec{Format: format, StartNs: from, EndNs: to}
}

func printInfo(g *globals, info *app.SessionInfo) (retErr error) {
	var out printer
	defer out.finish(&retErr)
	if g.json {
		return g.printJSON(info)
	}
	m := info.Meta
	out.printf(g.out, "session:   %s\nname:      %s\nstatus:    %s\ncreated:   %s\nduration:  %s\ndir:       %s\n",
		m.ID, m.Name, m.Status, m.CreatedAt.Format("2006-01-02 15:04:05Z07:00"), fmtDur(m.DurationNs), info.Dir)
	if m.Error != "" {
		out.printf(g.out, "error:     %s\n", m.Error)
	}
	out.println(g.out, "devices:")
	for _, d := range m.Devices {
		out.printf(g.out, "  %-10s %s/%s  [%s]\n", d.Alias, d.PluginID, d.DeviceID, strings.Join(d.Capabilities, ","))
	}
	if len(info.Streams) > 0 {
		out.println(g.out, "sample streams:")
		for _, s := range info.Streams {
			out.printf(g.out, "  %-10s %-8s %-10s %d samples  %s .. %s\n", s.Alias, s.Kind, s.ID, s.Samples, fmtDur(s.StartNs), fmtDur(s.EndNs))
		}
	}
	var evs []string
	for t, n := range info.Events {
		evs = append(evs, fmt.Sprintf("%s=%d", t, n))
	}
	sort.Strings(evs)
	out.printf(g.out, "timeline:  %s\n", orDash(strings.Join(evs, " ")))
	for _, c := range m.Captures {
		if c.Clock == nil {
			continue
		}
		k := c.Clock
		out.printf(g.out, "clock:     %-10s stamped-true in [%s, %s], resolution %s", c.Alias,
			bound(k.ErrorMinNs, k.ErrorMinBounded, "-inf"), bound(k.ErrorMaxNs, k.ErrorMaxBounded, "+inf"), fmtDur(k.ResolutionNs))
		if k.OffsetAppliedNs != 0 {
			out.printf(g.out, ", offset %s applied", fmtDur(k.OffsetAppliedNs))
		}
		out.println(g.out)
	}
	for _, c := range m.ClockCalibrations {
		out.printf(g.out, "calibrated: %-9s -> %s: %d edges over %s, drift %.2f ppm, residual p95 %s, uncertainty +/- %s (%s)\n",
			c.Alias, c.Reference, c.Pairs, fmtDur(c.SpanNs), c.DriftPPM, fmtDur(c.ResidualP95Ns), fmtDur(c.UncertaintyNs), c.Method)
	}
	for _, a := range info.Artifacts {
		out.printf(g.out, "artifact:  %s\n", a)
	}
	for _, p := range info.Plots {
		out.printf(g.out, "plot:      %s\n", p)
	}
	return nil
}

func sessionCmd(g *globals) *cobra.Command {
	root := &cobra.Command{Use: "session", Short: "Inspect stored sessions"}
	root.AddCommand(&cobra.Command{
		Use: "list", Short: "List sessions, newest first", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) (retErr error) {
			var out printer
			defer out.finish(&retErr)
			ms, err := g.app.ListSessions()
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(ms)
			}
			w := g.table()
			out.println(w, "SESSION\tSTATUS\tDURATION\tDEVICES")
			for _, m := range ms {
				var names []string
				for _, d := range m.Devices {
					names = append(names, d.Alias)
				}
				out.printf(w, "%s\t%s\t%s\t%s\n", m.ID, m.Status, fmtDur(m.DurationNs), join(names))
			}
			return w.Flush()
		},
	})
	root.AddCommand(&cobra.Command{
		Use: "info SESSION", Short: "Describe a session", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			info, err := g.app.SessionInfo(args[0])
			if err != nil {
				return err
			}
			return printInfo(g, info)
		},
	})
	return root
}

func timelineCmd(g *globals) *cobra.Command {
	var from, to, types, sources, contains string
	var limit int
	c := &cobra.Command{
		Use:   "timeline SESSION",
		Short: "Print the unified timeline of a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) (retErr error) {
			var out printer
			defer out.finish(&retErr)
			f, err := app.ParseTime(from)
			if err != nil {
				return err
			}
			t, err := app.ParseTime(to)
			if err != nil {
				return err
			}
			evs, err := g.app.QueryTimeline(app.TimelineQuery{Session: args[0], FromNs: f, ToNs: t, Types: splitList(types),
				Sources: splitList(sources), Contains: contains, Limit: limit})
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(evs)
			}
			for _, e := range evs {
				out.println(g.out, e.String())
			}
			if len(evs) == limit || (limit == 0 && len(evs) == 200) {
				out.println(g.out, "... (limit reached; narrow with --from/--to/--type or raise --limit)")
			}
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&from, "from", "", "start time (e.g. 10ms)")
	fl.StringVar(&to, "to", "", "end time")
	fl.StringVar(&types, "type", "", "comma-separated event types (packet,marker,analysis.power_rise,...)")
	fl.StringVar(&sources, "source", "", "comma-separated device aliases")
	fl.StringVar(&contains, "contains", "", "summary substring")
	fl.IntVar(&limit, "limit", 200, "maximum events")
	return c
}

func correlateCmd(g *globals) *cobra.Command {
	var anchor, related, aSrc, bSrc, window string
	var limit int
	c := &cobra.Command{
		Use:     "correlate SESSION",
		Short:   "Find events related to anchor events (e.g. packets around power rises)",
		Example: `  hwlab correlate latest --anchor analysis.power_rise --related packet --window 5ms`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) (retErr error) {
			var out printer
			defer out.finish(&retErr)
			w, err := app.ParseTime(window)
			if err != nil {
				return err
			}
			res, err := g.app.Correlate(app.CorrelateQuery{Session: args[0], WindowNs: w, Limit: limit,
				A: timeline.Filter{Types: splitList(anchor), Sources: splitList(aSrc)},
				B: timeline.Filter{Types: splitList(related), Sources: splitList(bSrc)}})
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(res)
			}
			out.printf(g.out, "%d anchors, %d with related events", res.Anchors, res.Matched)
			if s := res.FollowDelay; s.Count > 0 {
				out.printf(g.out, "; first following event after %s..%s (mean %s, n=%d)",
					fmtDur(s.MinNs), fmtDur(s.MaxNs), fmtDur(s.MeanNs), s.Count)
			}
			out.println(g.out)
			for _, c := range res.Correlations {
				out.printf(g.out, "\n%s\n", c.Anchor.String())
				for _, h := range c.Related {
					out.printf(g.out, "   %+10.3f ms  %s\n", float64(h.DeltaNs)/1e6, h.Event.String())
				}
			}
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&anchor, "anchor", "analysis.power_rise", "anchor event types")
	fl.StringVar(&aSrc, "anchor-source", "", "anchor device aliases")
	fl.StringVar(&related, "related", "packet", "related event types")
	fl.StringVar(&bSrc, "related-source", "", "related device aliases")
	fl.StringVar(&window, "window", "5ms", "search window around each anchor")
	fl.IntVar(&limit, "limit", 20, "anchors to print")
	return c
}

func calibrateCmd(g *globals) *cobra.Command {
	var aType, bType, aSrc, bSrc, window string
	c := &cobra.Command{
		Use:   "calibrate SESSION",
		Short: "Estimate the clock offset between two devices from a shared trigger",
		Long: `Pairs events that both devices saw at the same instant (for example one
trigger line wired to the logic analyser and flashing a marker on the radio)
and reports the median offset. The result is a suggestion for the
clock_offset key of the experiment; it is NOT valid for events that merely
follow each other (a packet after a current spike has a real delay).`,
		Example: `  hwlab calibrate latest --a-source power --a-type digital.edge --b-source radio --b-type marker`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) (retErr error) {
			var out printer
			defer out.finish(&retErr)
			w, err := app.ParseTime(window)
			if err != nil {
				return err
			}
			res, err := g.app.Calibrate(app.CalibrateQuery{Session: args[0], WindowNs: w,
				A: timeline.Filter{Types: splitList(aType), Sources: splitList(aSrc)},
				B: timeline.Filter{Types: splitList(bType), Sources: splitList(bSrc)}})
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(res)
			}
			out.printf(g.out, "%d pairs (%d ambiguous, %d unmatched); B - A: median %s, range %s .. %s, MAD %s\n", res.Pairs,
				res.Ambiguous, res.Unmatched, fmtDur(res.MedianNs), fmtDur(res.MinNs), fmtDur(res.MaxNs), fmtDur(res.MADNs))
			for _, w := range res.Warnings {
				out.printf(g.out, "warning: %s\n", w)
			}
			if res.Pairs > 0 {
				out.printf(g.out, "suggested for the B device:  clock_offset: %dns\n", res.SuggestedOffsetNs)
			}
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&aType, "a-type", "", "reference event types")
	fl.StringVar(&aSrc, "a-source", "", "reference device alias")
	fl.StringVar(&bType, "b-type", "", "event types of the device to align")
	fl.StringVar(&bSrc, "b-source", "", "alias of the device to align")
	fl.StringVar(&window, "window", "20ms", "pairing window around each reference event")
	return c
}

func calibrateClocksCmd(g *globals) *cobra.Command {
	var ref, dev, refCh, devCh string
	var tol int64
	var save bool
	c := &cobra.Command{
		Use:   "calibrate-clocks SESSION",
		Short: "Measure offset, drift and uncertainty between two devices from a shared digital signal",
		Long: `Pairs the edges of one physical signal seen by two capture devices and fits
session = a*device + b by least squares. Reports the initial offset, residual
scatter, the measured clock-rate difference (ppm) and a timing uncertainty.
The signal must contain a non-periodic marker (a stretched half-period) so the
edges can be paired; a perfectly periodic signal is rejected as ambiguous.
The timeline is not modified; --save stores the mapping in the session metadata.`,
		Example: `  hwlab calibrate-clocks latest --ref logic --dev ppk2 --ref-channel SHARED --dev-channel D0 --save`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) (retErr error) {
			var out printer
			defer out.finish(&retErr)
			res, err := g.app.CalibrateClocks(app.ClockCalibrationQuery{Session: args[0], Ref: ref, Dev: dev,
				RefChan: refCh, DevChan: devCh, TolNs: tol, Save: save})
			if err != nil {
				if res != nil && !g.json {
					out.printf(g.out, "edges: %d (ref) / %d (dev); best alignment %d matching intervals, runner-up %d\n",
						res.RefEdges, res.DevEdges, res.MatchScore, res.MatchRunner)
				}
				return err
			}
			if g.json {
				return g.printJSON(res)
			}
			c, f := res.Calibration, res.Fit
			out.printf(g.out, "reference %s, device %s\n", c.Reference, c.Alias)
			out.printf(g.out, "matched edges:           %d (of %d / %d; %d rejected as outliers), span %s\n", c.Pairs, res.RefEdges, res.DevEdges, c.Rejected, fmtDur(c.SpanNs))
			out.printf(g.out, "initial offset:          %s (reference - device at the first edge)\n", fmtDur(c.InitialOffsetNs))
			out.printf(g.out, "residual median/p95/max: %s / %s / %s\n", fmtDur(c.ResidualMedianNs), fmtDur(c.ResidualP95Ns), fmtDur(c.ResidualMaxNs))
			out.printf(g.out, "drift:                   %.2f ppm (+/- %.2f, 1 sigma)\n", c.DriftPPM, c.DriftStdErrPPM)
			out.printf(g.out, "fixed offset only:       p95 residual %s, max %s\n", fmtDur(f.OffsetOnlyP95Ns), fmtDur(f.OffsetOnlyMaxNs))
			out.printf(g.out, "uncertainty:             +/- %s\n", fmtDur(c.UncertaintyNs))
			for _, w := range res.Warnings {
				out.printf(g.out, "warning: %s\n", w)
			}
			if res.Saved {
				out.println(g.out, "saved to the session metadata")
			}
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&ref, "ref", "", "reference device alias")
	fl.StringVar(&dev, "dev", "", "alias of the device to map onto the reference")
	fl.StringVar(&refCh, "ref-channel", "", "channel name on the reference device")
	fl.StringVar(&devCh, "dev-channel", "", "channel name on the device")
	fl.Int64Var(&tol, "tolerance-ns", 0, "edge interval tolerance (default: from the clock resolutions)")
	fl.BoolVar(&save, "save", false, "store the result in the session metadata")
	return c
}

func analyzeCmd(g *globals) *cobra.Command {
	var source, from, to string
	var voltage float64
	var spikes bool
	c := &cobra.Command{
		Use:   "analyze SESSION",
		Short: "Power statistics and current spikes of a recorded session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			var out printer
			defer out.finish(&retErr)
			f, err := app.ParseTime(from)
			if err != nil {
				return err
			}
			t, err := app.ParseTime(to)
			if err != nil {
				return err
			}
			res, err := g.app.Analyze(cmd.Context(), app.AnalyzeOptions{Session: args[0], Source: source, FromNs: f, ToNs: t,
				NominalVoltage: voltage, Spikes: spikes})
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(res)
			}
			for _, pa := range res {
				st := pa.Stats
				out.printf(g.out, "%s/%s  %s .. %s  samples=%d\n", pa.Source, pa.Stream, fmtDur(st.StartNs), fmtDur(st.EndNs), st.Samples)
				out.printf(g.out, "  mean %s  min %s  max %s (at %s)\n", analysis.FormatCurrent(st.MeanNA),
					analysis.FormatCurrent(st.MinNA), analysis.FormatCurrent(st.MaxNA), fmtDur(st.PeakAtNs))
				out.printf(g.out, "  charge %.3f µC", st.ChargeUC)
				if st.HasEnergy {
					out.printf(g.out, "  energy %.3f µJ", st.EnergyUJ)
				}
				out.println(g.out)
				for i, sp := range pa.Spikes {
					out.printf(g.out, "  spike %2d  %s  dur %.1f µs  peak %s  baseline %s  %.3f µC\n", i+1, fmtDur(sp.StartNs),
						float64(sp.DurationNs())/1e3, analysis.FormatCurrent(sp.PeakNA), analysis.FormatCurrent(sp.BaselineNA), sp.ChargeUC)
				}
			}
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&source, "source", "", "device alias (default: all power streams)")
	fl.StringVar(&from, "from", "", "range start")
	fl.StringVar(&to, "to", "", "range end")
	fl.Float64Var(&voltage, "nominal-voltage", 0, "supply voltage (V) for energy when no voltage samples exist")
	fl.BoolVar(&spikes, "spikes", true, "detect current spikes")
	return c
}

func plotCmd(g *globals) *cobra.Command {
	var out, format, from, to, title string
	var logScale bool
	var width int
	c := &cobra.Command{
		Use:   "plot SESSION",
		Short: "Render a timeline plot (SVG or PNG) of a session range",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			var output printer
			defer output.finish(&retErr)
			f, err := app.ParseTime(from)
			if err != nil {
				return err
			}
			t, err := app.ParseTime(to)
			if err != nil {
				return err
			}
			if format == "" && strings.HasSuffix(strings.ToLower(out), ".png") {
				format = "png"
			}
			spec := plot.Spec{Title: title, StartNs: f, EndNs: t, Format: format, LogPower: logScale, Width: width}
			res, err := g.app.RenderPlot(cmd.Context(), args[0], "plot", out, spec)
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(res)
			}
			output.printf(g.out, "%s  (%dx%d, %d tracks, %s .. %s)\n", res.Path, res.Width, res.Height, res.Tracks, fmtDur(res.StartNs), fmtDur(res.EndNs))
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVarP(&out, "out", "o", "", "output file (default <session>/plots/plot.<format>)")
	fl.StringVar(&format, "format", "", "svg (default) or png")
	fl.StringVar(&from, "from", "", "range start")
	fl.StringVar(&to, "to", "", "range end")
	fl.StringVar(&title, "title", "", "plot title")
	fl.BoolVar(&logScale, "log", false, "logarithmic current axis")
	fl.IntVar(&width, "width", 0, "image width in px (default 1200)")
	return c
}
