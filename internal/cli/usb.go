package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/timeline"
)

func usbCmd(g *globals) *cobra.Command {
	root := &cobra.Command{Use: "usb", Short: "USB packet analysis (capture with the usbcap plugin)"}
	var source, device, from, to string
	var limit int
	var onlyErrors bool
	c := &cobra.Command{
		Use:   "analyze SESSION",
		Short: "Decode the USB traffic of a session: devices, classes, transfers, errors",
		Long: `Reads the URB events the usbcap plugin recorded, pairs submissions with
completions, learns each device's VID/PID and interface classes from the
enumeration traffic in the capture, and labels control requests and HID, CDC,
mass-storage and DFU transfers. Works on replays with no hardware.

This is URB-level (usbmon) analysis: there are no SOF, tokens or handshakes.
Latencies are differences of kernel URB timestamps, not bus times.`,
		Example: `  hwlab usb analyze latest
  hwlab usb analyze latest --device 1:5 --errors`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			var out printer
			defer out.finish(&retErr)
			q := app.USBQuery{Session: args[0], Source: source, Device: device, OnlyErrors: onlyErrors, Limit: limit}
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
			rep, err := g.app.AnalyzeUSB(q)
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(rep)
			}
			for _, s := range rep.Streams {
				st := s.Stats
				out.printf(g.out, "source %s: %d packets, %d transactions (%d errors, %d unpaired completions, %d still pending), %d bytes over %s\n",
					s.Source, st.Packets, st.Transactions, st.Errors, st.UnpairedComplete, st.StillPending, st.Bytes, timeline.FormatNs(st.DurationNs))
				for _, d := range s.Devices {
					id := ""
					if d.VID != "" {
						id = fmt.Sprintf(" %s:%s", d.VID, d.PID)
					}
					out.printf(g.out, "  device %s%s  %d URBs, %d errors  %v\n", d.Device, id, d.URBs, d.Errors, d.Interfaces)
				}
				for _, e := range s.Endpoints {
					out.printf(g.out, "  %-4s %-7s %-11s %-12s %5d URBs %7d bytes %3d errors  latency mean %.0f µs max %.0f µs\n",
						e.Device, e.Endpoint, e.Transfer, e.Class, e.URBs, e.Bytes, e.Errors, e.MeanLatUs, e.MaxLatUs)
				}
				for _, ec := range s.ErrorCounts {
					out.printf(g.out, "  error %s: %d\n", ec.Name, ec.Count)
				}
				for _, t := range s.Transactions {
					extra := t.Detail
					if extra != "" {
						extra = "  " + extra
					}
					out.printf(g.out, "  %s  %-4s %-7s %-34s%s\n", timeline.FormatNs(t.StartNs), t.Device, t.Endpoint, t.Name, extra)
				}
				if s.Truncated {
					out.printf(g.out, "  ... %d of %d transactions listed; raise --limit\n", len(s.Transactions), s.Matching)
				}
			}
			out.println(g.out, rep.Note)
			return nil
		},
	}
	fl := c.Flags()
	fl.StringVar(&source, "source", "", "usb source alias (default: every USB source)")
	fl.StringVar(&device, "device", "", "list only this bus:device, e.g. 1:5 (statistics stay global)")
	fl.BoolVar(&onlyErrors, "errors", false, "list only failed transactions")
	fl.StringVar(&from, "from", "", "start of the time range, e.g. 1.5s")
	fl.StringVar(&to, "to", "", "end of the time range")
	fl.IntVar(&limit, "limit", 100, "transactions listed per source (max 1000)")
	root.AddCommand(c)
	return root
}
