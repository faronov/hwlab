package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/timeline"
)

func waveformCmd(g *globals) *cobra.Command {
	root := &cobra.Command{Use: "waveform", Short: "Analog waveform analysis (oscilloscope and analog captures)"}
	var source, channel, from, to string
	var points int
	c := &cobra.Command{
		Use:   "measure SESSION",
		Short: "Measure a recorded analog channel: min/max/mean/rms, frequency, duty, rise/fall time",
		Long: `Reads the analog samples a session recorded (picoscope, scpi, saleae analog) and
measures one channel per contiguous segment: min, max, mean, rms, peak-to-peak,
base and top level, frequency, period, duty, pulse widths and 10-90% rise and
fall times. A block-mode instrument stores one segment per acquisition.

Works on replays with no hardware. Nothing here asks the instrument anything;
it is computed from the stored samples. Raw samples are never printed: the
JSON output carries a min/max envelope of at most --points buckets per segment.`,
		Example: `  hwlab waveform measure latest
  hwlab waveform measure latest --source scope --channel A --from 1ms --to 4ms --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := app.WaveformQuery{Session: args[0], Source: source, Channel: channel, Points: points}
			var err error
			if from != "" {
				if q.FromNs, err = app.ParseTime(from); err != nil {
					return err
				}
			}
			if to != "" {
				if q.ToNs, err = app.ParseTime(to); err != nil {
					return err
				}
			}
			rep, err := g.app.MeasureWaveform(cmd.Context(), q)
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(rep)
			}
			for _, s := range rep.Streams {
				fmt.Fprintf(g.out, "source %s channel %s (%s), %d segment(s)\n", s.Source, s.Channel, s.Unit, len(s.Segments))
				for _, sg := range s.Segments {
					fmt.Fprintf(g.out, "  %s .. %s  %d samples, %d ns/sample\n", timeline.FormatNs(sg.StartNs), timeline.FormatNs(sg.EndNs), sg.Samples, sg.PeriodNs)
					if sg.Error != "" {
						fmt.Fprintf(g.out, "    %s\n", sg.Error)
						continue
					}
					m := sg.Measurements
					fmt.Fprintf(g.out, "    min %.4g  max %.4g  mean %.4g  rms %.4g  vpp %.4g\n", m.Min, m.Max, m.Mean, m.RMS, m.Vpp)
					if m.FrequencyHz != nil && m.PeriodS != nil {
						fmt.Fprintf(g.out, "    %.6g Hz  period %.4g s", *m.FrequencyHz, *m.PeriodS)
						if m.Duty != nil {
							fmt.Fprintf(g.out, "  duty %.1f%%", *m.Duty*100)
						}
						fmt.Fprintln(g.out)
					}
					if m.RiseTimeS != nil || m.FallTimeS != nil {
						fmt.Fprint(g.out, "   ")
						if m.RiseTimeS != nil {
							fmt.Fprintf(g.out, " rise %.4g s", *m.RiseTimeS)
						}
						if m.FallTimeS != nil {
							fmt.Fprintf(g.out, " fall %.4g s", *m.FallTimeS)
						}
						fmt.Fprintln(g.out)
					}
					for _, n := range m.Notes {
						fmt.Fprintf(g.out, "    note: %s\n", n)
					}
				}
				if s.Truncated {
					fmt.Fprintln(g.out, "  ... more segments; narrow with --from/--to")
				}
			}
			fmt.Fprintln(g.out, rep.Note)
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&source, "source", "", "device alias (default: every analog stream)")
	fl.StringVar(&channel, "channel", "", "channel name (default: the first channel of each stream)")
	fl.StringVar(&from, "from", "", "start of the window, e.g. 1ms")
	fl.StringVar(&to, "to", "", "end of the window")
	fl.IntVar(&points, "points", 200, "envelope buckets per segment (max 1000)")
	root.AddCommand(c)
	return root
}
