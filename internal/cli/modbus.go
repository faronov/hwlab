package cli

import (
	"github.com/spf13/cobra"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/timeline"
)

func modbusCmd(g *globals) *cobra.Command {
	root := &cobra.Command{Use: "modbus", Short: "Modbus RTU/TCP analysis (the master itself is the modbus plugin)"}
	var source, mode, gap, from, to string
	var limit int
	c := &cobra.Command{
		Use:   "decode SESSION",
		Short: "Decode Modbus RTU transactions from the serial bytes of a session",
		Long: `Reads the raw serial packets the uart plugin recorded, cuts them into Modbus
RTU frames (CRC-checked, resynchronising after noise), pairs requests with
responses and lists the transactions. Works on replays with no hardware.

--mode says what the capturing host was on the bus: master (tx are requests,
rx are responses; the default), slave, or sniff (both directions carry both).
Latencies are differences of host timestamps, not wire times.`,
		Example: `  hwlab modbus decode latest
  hwlab modbus decode latest --source bus --mode sniff --limit 20`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			var out printer
			defer out.finish(&retErr)
			q := app.ModbusQuery{Session: args[0], Source: source, Mode: mode, Limit: limit}
			var err error
			if gap != "" {
				if q.GapNs, err = app.ParseTime(gap); err != nil {
					return err
				}
			}
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
			rep, err := g.app.DecodeModbus(q)
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(rep)
			}
			for _, s := range rep.Streams {
				st := s.Stats
				out.printf(g.out, "source %s (%s): %d chunks, %d frames, %d transactions: %d ok, %d exceptions, %d no response, %d orphan, %d crc errors, %d bad frames, %d junk bytes\n",
					s.Source, s.Mode, s.Chunks, st.Frames, st.Transactions, st.OK, st.Exceptions, st.NoResponse, st.Orphans, st.CRCErrors, st.BadFrames, st.JunkBytes)
				for _, t := range s.Transactions {
					lat := ""
					if t.LatencyNs > 0 {
						lat = "  +" + timeline.FormatNs(t.LatencyNs)
					}
					out.printf(g.out, "  %s  %-15s %s%s\n", timeline.FormatNs(t.StartNs), t.Status, t.Summary, lat)
				}
				if s.Truncated {
					out.printf(g.out, "  ... more transactions; raise --limit\n")
				}
			}
			out.println(g.out, rep.Note)
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&source, "source", "", "uart device alias (default: every serial source)")
	fl.StringVar(&mode, "mode", "master", "what the capturing host was: master, slave or sniff")
	fl.StringVar(&gap, "gap", "", "idle gap that ends a frame (default 50ms)")
	fl.StringVar(&from, "from", "", "start of the time range, e.g. 1.5s")
	fl.StringVar(&to, "to", "", "end of the time range")
	fl.IntVar(&limit, "limit", 100, "transactions listed per source (max 1000)")
	root.AddCommand(c)
	return root
}
