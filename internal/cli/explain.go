package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/faronov/hwlab/internal/analysis"
	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/plot"
)

func explainCmd(g *globals) *cobra.Command {
	var spike, packet int
	var at, window, source, plotOut string
	var voltage float64
	c := &cobra.Command{
		Use:   "explain SESSION",
		Short: "Explain what happened around a current spike, a packet or a time",
		Long: `Relates a current spike (or a packet, or a point in time) to the radio
packets around it, the energy of the burst, the current before and after,
and the timing uncertainty of the two clocks. Delays are signed:
positive means the packet came after the anchor. The uncertainty is part
of the answer; cross-device timing is not nanosecond accurate.`,
		Example: `  hwlab explain latest --spike 2 --nominal-voltage 3.3
  hwlab explain latest --packet 17
  hwlab explain latest --at 4.38s`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			var out printer
			defer out.finish(&retErr)
			w, err := app.ParseTime(window)
			if err != nil {
				return err
			}
			q := app.ExplainQuery{Session: args[0], Source: source, Spike: spike, Packet: packet, WindowNs: w, NominalVoltage: voltage}
			if at != "" {
				if q.AtNs, err = app.ParseTime(at); err != nil {
					return err
				}
				q.HasAt = true
			}
			res, err := g.app.Explain(cmd.Context(), q)
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(res)
			}
			if err := printExplanation(g.out, res); err != nil {
				return err
			}
			if plotOut != "" {
				from, to := res.AnchorNs-w, res.AnchorNs+w
				if res.Anchor == "spike" && res.Spike != nil {
					to = res.Spike.EndNs + w
				}
				format := ""
				if strings.HasSuffix(strings.ToLower(plotOut), ".png") {
					format = "png"
				}
				pr, err := g.app.RenderPlot(cmd.Context(), args[0], "explain", plotOut,
					plot.Spec{Title: "explain: " + res.Anchor, StartNs: from, EndNs: to, Format: format})
				if err != nil {
					return err
				}
				out.printf(g.out, "plot: %s\n", pr.Path)
			}
			return nil
		},
	}
	fl := c.Flags()
	fl.IntVarP(&spike, "spike", "n", 0, "anchor: spike number (1 = first in time)")
	fl.IntVar(&packet, "packet", 0, "anchor: packet number (1 = first in time)")
	fl.StringVar(&at, "at", "", "anchor: session time, e.g. 4.38s")
	fl.StringVar(&window, "window", "5ms", "context window before/after the anchor")
	fl.StringVar(&source, "source", "", "power device alias (default: first power stream)")
	fl.Float64Var(&voltage, "nominal-voltage", 0, "supply voltage (V) for energy when no voltage samples exist")
	fl.StringVar(&plotOut, "plot", "", "also render a zoomed plot around the anchor to this file (.svg or .png)")
	return c
}

func printExplanation(w io.Writer, x *app.Explanation) (retErr error) {
	var out printer
	defer out.finish(&retErr)
	out.printf(w, "session %s, anchor %s at %s\n", x.Session, x.Anchor, fmtDur(x.AnchorNs))
	if sp := x.Spike; sp != nil {
		label := "spike"
		if x.Anchor != "spike" {
			label = "nearest spike"
		}
		out.printf(w, "%s #%d of %d: %s .. %s (%.1f µs), peak %s at %s, mean %s over a %s baseline, %.3f µC",
			label, sp.Index, sp.Total, fmtDur(sp.StartNs), fmtDur(sp.EndNs), float64(sp.DurationNs)/1e3,
			analysis.FormatCurrent(sp.PeakNA), fmtDur(sp.PeakAtNs), analysis.FormatCurrent(sp.MeanNA),
			analysis.FormatCurrent(sp.BaselineNA), sp.ChargeUC)
		if sp.HasEnergy {
			out.printf(w, ", %.3f µJ", sp.EnergyUJ)
		}
		out.println(w)
	}
	if len(x.InBurst) > 0 {
		kinds := make([]string, 0, len(x.InBurst))
		for k := range x.InBurst {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		out.print(w, "packets during the burst:")
		for _, k := range kinds {
			out.printf(w, " %s=%d", k, x.InBurst[k])
		}
		out.println(w)
	} else if x.Anchor == "spike" {
		out.println(w, "packets during the burst: none")
	}
	printRefs(&out, w, "before", x.Before)
	printRefs(&out, w, "after", x.After)
	if x.Nearest != nil {
		out.printf(w, "nearest packet: #%d %s %+.3f ms (%s, %d B, %.0f dBm)\n", x.Nearest.Index, x.Nearest.Kind,
			float64(x.Nearest.DeltaNs)/1e6, x.Nearest.Source, x.Nearest.Bytes, x.Nearest.RSSIdBm)
	}
	if x.FirstTx != nil {
		out.printf(w, "first DATA after the anchor: #%d at %+.3f ms", x.FirstTx.Index, float64(x.FirstTx.DeltaNs)/1e6)
		if x.AckAfter != nil && x.DataAck != nil {
			out.printf(w, ", ACK #%d at %+.3f ms (%.3f ms after DATA)", x.AckAfter.Index,
				float64(x.AckAfter.DeltaNs)/1e6, float64(*x.DataAck)/1e6)
		} else {
			out.print(w, ", no ACK seen after it")
		}
		out.println(w)
	}
	if c := x.Current; c.HasData {
		out.printf(w, "current %s before: mean %s peak %s; %s after: mean %s peak %s\n", fmtDur(c.WindowNs),
			analysis.FormatCurrent(c.BeforeMean), analysis.FormatCurrent(c.BeforePeak), fmtDur(c.WindowNs),
			analysis.FormatCurrent(c.AfterMean), analysis.FormatCurrent(c.AfterPeak))
	}
	e := x.Energy
	out.printf(w, "charge %.3f µC in %s .. %s", e.WindowUC, fmtDur(e.WindowFrom), fmtDur(e.WindowTo))
	if e.HasEnergy {
		out.printf(w, ", energy %.3f µJ at %.2f V", e.WindowUJ, e.NominalV)
	}
	out.println(w)
	if e.GapNs > 0 {
		out.printf(w, "since the previous spike (%s ago): mean %s, %.3f µC", fmtDur(e.GapNs), analysis.FormatCurrent(e.GapMeanNA), e.GapUC)
		if e.HasEnergy {
			out.printf(w, ", %.3f µJ", e.GapUJ)
		}
		out.println(w)
	}
	for _, c := range x.Clocks {
		if c.Clock == nil {
			out.printf(w, "clock %s: not reported\n", c.Capture)
			continue
		}
		out.printf(w, "clock %s: error in [%s, %s] (%s)\n", c.Capture,
			bound(c.Clock.ErrorMinNs, c.Clock.ErrorMinBounded, "-inf"), bound(c.Clock.ErrorMaxNs, c.Clock.ErrorMaxBounded, "+inf"), c.Clock.Source)
	}
	if t := x.Timing; t != nil {
		out.printf(w, "timing of the %s: stamped %+.3f ms; %s\n", t.Reference, float64(t.DeltaNs)/1e6, t.Statement)
	}
	for _, n := range x.Notes {
		out.printf(w, "note: %s\n", n)
	}
	return nil
}

func printRefs(out *printer, w io.Writer, label string, refs []app.PacketRef) {
	if len(refs) == 0 {
		out.printf(w, "packets %s: none in the window\n", label)
		return
	}
	out.printf(w, "packets %s:\n", label)
	for _, r := range refs {
		out.printf(w, "  %+10.3f ms  #%-4d %-7s %3d B  %4.0f dBm  lqi %d  (%s)\n", float64(r.DeltaNs)/1e6, r.Index, r.Kind, r.Bytes, r.RSSIdBm, r.LQI, r.Source)
	}
}

func bound(ns int64, known bool, inf string) string {
	if !known {
		return inf
	}
	return fmt.Sprintf("%+.3f ms", float64(ns)/1e6)
}
