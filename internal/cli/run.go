package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/replay"
)

func printRun(g *globals, res *app.RunResult) (retErr error) {
	var out printer
	defer out.finish(&retErr)
	if g.json {
		return g.printJSON(res)
	}
	out.printf(g.out, "session:   %s\nstatus:    %s\ndir:       %s\nduration:  %s\ndevices:   %s\nsamples:   %d\n",
		res.SessionID, res.Status, res.Dir, fmtDur(res.DurationNs), join(res.Devices), res.Samples)
	if len(res.Events) > 0 {
		var parts []string
		for t, n := range res.Events {
			parts = append(parts, fmt.Sprintf("%s=%d", t, n))
		}
		sortStrings(parts)
		out.printf(g.out, "timeline:  %s\n", strings.Join(parts, " "))
	}
	if res.Spikes > 0 {
		out.printf(g.out, "power spikes annotated: %d\n", res.Spikes)
	}
	for _, a := range res.Artifacts {
		out.printf(g.out, "artifact:  %s\n", a)
	}
	for _, p := range res.Plots {
		out.printf(g.out, "plot:      %s\n", p)
	}
	for _, w := range res.Warnings {
		out.printf(g.out, "warning:   %s\n", w)
	}
	if res.Error != "" {
		out.printf(g.out, "error:     %s\n", res.Error)
	}
	return nil
}

func runCmd(g *globals) *cobra.Command {
	var yes bool
	var replaySession string
	c := &cobra.Command{
		Use:   "run EXPERIMENT.yaml",
		Short: "Run an experiment: capture, analyse, plot and store a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := experiment.Load(args[0])
			if err != nil {
				return err
			}
			if replaySession != "" {
				if err := g.app.UseReplay(replaySession); err != nil {
					return err
				}
			}
			res, err := g.app.RunExperiment(cmd.Context(), e, yes || e.AllowDangerous)
			if res != nil {
				if perr := printRun(g, res); perr != nil && err == nil {
					err = perr
				}
			}
			return confirmHint(err)
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "confirm dangerous configuration (e.g. supply voltage)")
	c.Flags().StringVar(&replaySession, "replay", "", "make `plugin: replay` replay this recorded session")
	return c
}

func replayCmd(g *globals) *cobra.Command {
	var rerun bool
	var plotOut string
	c := &cobra.Command{
		Use:   "replay SESSION",
		Short: "Open a recorded session without hardware; optionally re-run it through the pipeline",
		Long: `Opens SESSION (path, id, unique prefix or "latest") read-only and prints its
summary. With --rerun the recorded samples and events are replayed through the
normal capture pipeline into a new session, exercising ingest, analysis and
plotting exactly as a live run (no hardware needed).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			info, err := g.app.SessionInfo(args[0])
			if err != nil {
				return err
			}
			if !rerun {
				if plotOut != "" {
					if _, err := g.app.RenderPlot(ctx, args[0], "replay", plotOut, plotSpec("", 0, 0)); err != nil {
						return err
					}
				}
				return printInfo(g, info)
			}
			if err := g.app.UseReplay(args[0]); err != nil {
				return err
			}
			var devs []experiment.DeviceSpec
			for _, d := range info.Meta.Devices {
				devs = append(devs, experiment.DeviceSpec{Alias: d.Alias, Plugin: replay.PluginID, Device: d.Alias, Config: map[string]any{}})
			}
			res, err := g.app.RunExperiment(ctx, &experiment.Experiment{
				Name: "replay-of-" + info.Meta.Name, Duration: experiment.Duration(time.Duration(info.Meta.DurationNs)), Devices: devs}, false)
			if res != nil {
				if perr := printRun(g, res); perr != nil && err == nil {
					err = perr
				}
			}
			return err
		},
	}
	c.Flags().BoolVar(&rerun, "rerun", false, "replay through the capture pipeline into a new session")
	c.Flags().StringVar(&plotOut, "plot", "", "also render an overview plot to this file (.svg or .png)")
	return c
}

func labCmd(g *globals) *cobra.Command {
	lab := &cobra.Command{Use: "lab", Short: "Multi-device lab operations"}
	var devs, sets []string
	var dur time.Duration
	var yes bool
	var nominal float64
	capc := &cobra.Command{
		Use:   "capture",
		Short: "Ad-hoc synchronized capture on several devices",
		Example: `  hwlab lab capture --device pwr=fake/power0 --device rf=fake/radio0 \
      --set pwr.realtime=false --duration 1s`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(devs) == 0 {
				return fmt.Errorf("at least one --device ALIAS=PLUGIN[/DEVICE] is required")
			}
			var specs []experiment.DeviceSpec
			for _, d := range devs {
				alias, rest, ok := strings.Cut(d, "=")
				if !ok {
					return fmt.Errorf("--device %q: expected ALIAS=PLUGIN[/DEVICE]", d)
				}
				plug, dev, _ := strings.Cut(rest, "/")
				specs = append(specs, experiment.DeviceSpec{Alias: alias, Plugin: plug, Device: dev, Config: map[string]any{}})
			}
			for _, s := range sets {
				key, val, ok := strings.Cut(s, "=")
				alias, k, ok2 := strings.Cut(key, ".")
				if !ok || !ok2 {
					return fmt.Errorf("--set %q: expected ALIAS.KEY=VALUE", s)
				}
				found := false
				for i := range specs {
					if specs[i].Alias == alias {
						specs[i].Config[k] = parseScalar(val)
						found = true
					}
				}
				if !found {
					return fmt.Errorf("--set %q: unknown alias %q", s, alias)
				}
			}
			return runAdhoc(cmd.Context(), g, app.RunSpec{Name: "lab-capture", Duration: dur, Devices: specs, Confirm: yes,
				Analysis: experiment.Analysis{NominalVoltage: nominal}})
		},
	}
	capc.Flags().StringArrayVar(&devs, "device", nil, "ALIAS=PLUGIN[/DEVICE] (repeatable)")
	capc.Flags().StringArrayVar(&sets, "set", nil, "ALIAS.KEY=VALUE device configuration (repeatable)")
	capc.Flags().DurationVar(&dur, "duration", 10*time.Second, "capture duration")
	capc.Flags().Float64Var(&nominal, "nominal-voltage", 0, "supply voltage (V) for energy when the meter has no voltage samples")
	capc.Flags().BoolVar(&yes, "yes", false, "confirm dangerous configuration")
	lab.AddCommand(capc)
	return lab
}

func runAdhoc(ctx context.Context, g *globals, spec app.RunSpec) (retErr error) {
	var out printer
	defer out.finish(&retErr)
	r, err := g.app.StartCapture(ctx, spec)
	if err != nil {
		return confirmHint(err)
	}
	if !g.json {
		out.printf(g.out, "capturing %s into session %s (Ctrl-C to stop early)\n", spec.Duration, r.Session.ID())
	}
	select {
	case <-r.Finished():
	case <-ctx.Done():
	}
	res, err := r.Finalize(context.WithoutCancel(ctx))
	if res != nil {
		if perr := printRun(g, res); perr != nil && err == nil {
			err = perr
		}
	}
	return err
}

func ppk2Cmd(g *globals) *cobra.Command {
	root := &cobra.Command{Use: "ppk2", Short: "Nordic Power Profiler Kit II"}
	var dur time.Duration
	var voltage int
	var mode, devSel string
	var yes bool
	c := &cobra.Command{
		Use:   "capture",
		Short: "Capture current (and logic bits) from a PPK2",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := map[string]any{"mode": mode}
			nominal := 0.0
			if mode == "source" {
				cfg["supply_mv"] = float64(voltage)
				nominal = float64(voltage) / 1000
			}
			spec := app.RunSpec{Name: "ppk2-capture", Duration: dur, Confirm: yes, Analysis: experiment.Analysis{NominalVoltage: nominal},
				Devices: []experiment.DeviceSpec{{Alias: "ppk2", Plugin: "ppk2", Device: devSel, Config: cfg}}}
			return runAdhoc(cmd.Context(), g, spec)
		},
	}
	c.Flags().DurationVar(&dur, "duration", 10*time.Second, "capture duration")
	c.Flags().IntVar(&voltage, "voltage", 3300, "source-meter supply voltage in mV (800-5000); sets the DUT supply")
	c.Flags().StringVar(&mode, "mode", "source", "source (PPK2 powers the DUT) or ampere (external supply)")
	c.Flags().StringVar(&devSel, "device", "", "device serial or id when several PPK2s are attached")
	c.Flags().BoolVar(&yes, "yes", false, "confirm applying a supply voltage to the DUT")
	root.AddCommand(c)
	return root
}

func snifferCmd(g *globals) *cobra.Command {
	root := &cobra.Command{Use: "sniffer", Short: "IEEE 802.15.4 / Zigbee / Thread sniffer (Nordic nRF52840)"}
	var dur time.Duration
	var channel int
	var devSel string
	var noDecode bool
	c := &cobra.Command{
		Use:   "capture",
		Short: "Capture 802.15.4 packets into the session timeline and a pcapng artifact",
		Long: `Captures raw IEEE 802.15.4 frames from a Nordic sniffer on one channel. The
session gets packet events (timestamp, channel, RSSI, LQI) and a pcapng artifact
(802.15.4 TAP link type) that opens directly in Wireshark. If tshark is installed,
frames are also decoded (Zigbee, 6LoWPAN, ...) into timeline events; raw capture
never needs it.`,
		Example: "  hwlab sniffer capture --channel 15 --duration 10s",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := map[string]any{"channel": float64(channel)}
			if noDecode {
				cfg["decode"] = false
			}
			spec := app.RunSpec{Name: "sniffer-capture", Duration: dur,
				Devices: []experiment.DeviceSpec{{Alias: "sniffer", Plugin: "nrf802154", Device: devSel, Config: cfg}}}
			return runAdhoc(cmd.Context(), g, spec)
		},
	}
	c.Flags().DurationVar(&dur, "duration", 30*time.Second, "capture duration")
	c.Flags().IntVar(&channel, "channel", 15, "802.15.4 channel (11-26)")
	c.Flags().StringVar(&devSel, "device", "", "device id/serial when several sniffers are attached")
	c.Flags().BoolVar(&noDecode, "no-decode", false, "skip the optional tshark decoding pass")
	root.AddCommand(c)

	var channels string
	var dwell time.Duration
	var minPackets int
	sc := &cobra.Command{
		Use:   "scan",
		Short: "Listen on each channel for a short time and report activity",
		Long: `Dwells on every selected channel and reports packet counts, RSSI and LQI
statistics. "likely active" only means at least --min-packets frames were heard;
it does not identify networks.`,
		Example: "  hwlab sniffer scan --dwell 2s --channels 11-26",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			args := map[string]any{"channels": channels, "dwell_ms": float64(dwell.Milliseconds()), "min_packets": float64(minPackets)}
			res, err := g.app.Invoke(cmd.Context(), appInvoke("nrf802154", devSel, "PacketCapture", "scan", args, false))
			if err != nil {
				return err
			}
			if g.json {
				return g.printJSON(res)
			}
			return printScan(g, res)
		},
	}
	sc.Flags().StringVar(&channels, "channels", "11-26", "channels to scan, e.g. 11-26 or 15,20,25")
	sc.Flags().DurationVar(&dwell, "dwell", time.Second, "time spent on each channel")
	sc.Flags().IntVar(&minPackets, "min-packets", 3, "packets needed to call a channel likely active")
	sc.Flags().StringVar(&devSel, "device", "", "device id/serial when several sniffers are attached")
	root.AddCommand(sc)
	return root
}

func printScan(g *globals, res map[string]any) (retErr error) {
	var out printer
	defer out.finish(&retErr)
	tw := g.table()
	out.println(tw, "CHANNEL\tPACKETS\tPKT/S\tRSSI MIN\tRSSI MEAN\tRSSI MAX\tLQI\tACTIVE")
	rows, _ := res["channels"].([]any)
	num := func(m map[string]any, k string) string {
		v, ok := m[k].(float64)
		if !ok {
			return "-"
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	for _, r := range rows {
		m, _ := r.(map[string]any)
		active := ""
		if b, _ := m["likely_active"].(bool); b {
			active = "*"
		}
		out.printf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", num(m, "channel"), num(m, "packets"), num(m, "packets_per_s"),
			num(m, "rssi_min_dbm"), num(m, "rssi_mean_dbm"), num(m, "rssi_max_dbm"), num(m, "lqi_mean"), active)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	var act []string
	for _, a := range res["likely_active"].([]any) {
		act = append(act, strconv.FormatFloat(a.(float64), 'f', -1, 64))
	}
	out.printf(g.out, "likely active channels: %s\n", join(act))
	return nil
}
