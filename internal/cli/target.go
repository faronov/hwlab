package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/faronov/hwlab/internal/app"
)

type targetFlags struct {
	plugin, device     string
	config, args       []string
	pre, post          []string
	yes, attach        bool
	path, space        string
	address, length    string
	mode, verify, base string
	token, tokenConfig string
}

func kvMap(items []string, what string) (map[string]any, error) {
	m := map[string]any{}
	for _, a := range items {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("--%s %q: expected key=value", what, a)
		}
		m[k] = parseScalar(v)
	}
	return m, nil
}

func parseCalls(items []string) ([]app.TargetCall, error) {
	var out []app.TargetCall
	for _, s := range items {
		capName, method, ok := strings.Cut(s, ".")
		if !ok {
			return nil, fmt.Errorf("%q: expected CAPABILITY.METHOD", s)
		}
		out = append(out, app.TargetCall{Capability: capName, Method: method})
	}
	return out, nil
}

func targetCmd(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "target",
		Short: "Generic target/debug/flash operations on any debug-probe plugin",
		Long: `Run one operation on a debug probe's target through its generic capabilities
(DebugProbe, MemoryAccess, FlashProgramming): the same service the MCP tool
target_op uses. Each command opens the device with --config, runs --pre calls,
the operation, then --post calls (also after a failure), and closes the device.

A native CC-Debugger session needs, for example:
  --config access=reset-debug-session --config allow_target_reset=true \
  --config allow_cpu_control=true --config allow_memory_access=true --attach

Dangerous calls and dangerous configuration need --yes. Flash erase/program
additionally need the plugin's own confirmation token (--token).`,
	}
	f := &targetFlags{}
	add := func(use, short, op string, extra func(*cobra.Command)) *cobra.Command {
		c := &cobra.Command{
			Use: use, Short: short, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := kvMap(f.config, "config")
				if err != nil {
					return err
				}
				args, err := kvMap(f.args, "arg")
				if err != nil {
					return err
				}
				for k, v := range map[string]string{"path": f.path, "space": f.space, "address": f.address, "length": f.length,
					"mode": f.mode, "verify": f.verify, "base": f.base, "confirm": f.token, "confirm_config": f.tokenConfig} {
					if v != "" {
						args[k] = parseScalar(v)
					}
				}
				pre, err := parseCalls(f.pre)
				if err != nil {
					return err
				}
				post, err := parseCalls(f.post)
				if err != nil {
					return err
				}
				if f.attach {
					pre = append([]app.TargetCall{{Capability: "DebugProbe", Method: "attach"}}, pre...)
				}
				res, err := g.app.Target(cmd.Context(), app.TargetRequest{Plugin: f.plugin, Device: f.device, Config: cfg, Op: op,
					Args: args, Pre: pre, Post: post, Confirm: f.yes})
				if res != nil {
					if perr := g.printJSON(res); perr != nil {
						return perr
					}
				}
				return confirmHint(err)
			},
		}
		if extra != nil {
			extra(c)
		}
		return c
	}
	pf := root.PersistentFlags()
	pf.StringVar(&f.plugin, "plugin", "", "plugin id (default: the only device with the needed capability)")
	pf.StringVar(&f.device, "device", "", "device id/serial")
	pf.StringArrayVar(&f.config, "config", nil, "device session config key=value (repeatable)")
	pf.StringArrayVar(&f.args, "arg", nil, "method argument key=value (repeatable)")
	pf.StringArrayVar(&f.pre, "pre", nil, "CAPABILITY.METHOD to call before the operation (repeatable)")
	pf.StringArrayVar(&f.post, "post", nil, "CAPABILITY.METHOD to call afterwards, even after a failure, e.g. DebugProbe.resume (repeatable)")
	pf.BoolVar(&f.attach, "attach", false, "call DebugProbe.attach first (resets the target into debug halt on CC-Debugger)")
	pf.BoolVar(&f.yes, "yes", false, "confirm dangerous operations and configuration")
	pf.StringVar(&f.token, "token", "", "plugin-specific confirmation token for destructive operations (e.g. program-flash)")
	pf.StringVar(&f.tokenConfig, "token-config", "", "second confirmation token when device configuration is touched")

	mem := &cobra.Command{Use: "memory", Short: "Target memory access"}
	memArgs := func(c *cobra.Command) {
		c.Flags().StringVar(&f.space, "space", "", "memory space (plugin specific, e.g. xdata, code, flash)")
		c.Flags().StringVar(&f.address, "address", "", "start address")
		c.Flags().StringVar(&f.length, "length", "", "length in bytes")
		c.Flags().StringVar(&f.path, "path", "", "write the data to this new file")
	}
	mem.AddCommand(add("read", "Read target memory", "memory-read", memArgs), add("write", "Write target RAM (--arg data=HEX)", "memory-write", memArgs))
	fl := &cobra.Command{Use: "flash", Short: "Target flash"}
	flArgs := func(c *cobra.Command) {
		c.Flags().StringVar(&f.path, "path", "", "image or output file")
		c.Flags().StringVar(&f.address, "address", "", "start address (read)")
		c.Flags().StringVar(&f.length, "length", "", "length in bytes (read)")
		c.Flags().StringVar(&f.base, "base", "", "load address of a raw .bin image")
		c.Flags().StringVar(&f.mode, "mode", "", "program: preserve-device-config|preserve-mac|full-chip-image; verify: readback|crc|both")
		c.Flags().StringVar(&f.verify, "verify", "", "program verification: readback|crc|both|none")
	}
	fl.AddCommand(add("read", "Read flash to a new file", "flash-read", flArgs), add("program", "Program an image (destructive)", "flash-program", flArgs),
		add("verify", "Compare an image with flash without writing", "flash-verify", flArgs),
		add("erase", "Erase flash (destructive; plugins may require --mode region|app|chip and extra permissions)", "flash-erase", flArgs),
		add("info", "Flash identity, geometry, security state and partition table", "flash-info", nil),
		add("backup", "Read the whole flash twice and store a verified backup artifact (--arg dir=PATH)", "flash-backup", nil),
		add("restore", "Restore a verified backup (destructive; --path BACKUP.bin)", "flash-restore", flArgs))
	bp := &cobra.Command{Use: "breakpoint", Short: "Hardware breakpoints / triggers"}
	bpArgs := func(c *cobra.Command) {
		c.Flags().StringVar(&f.address, "address", "", "code address (clear: or --arg all=true)")
	}
	bp.AddCommand(add("set", "Install a hardware execution breakpoint (--arg run_wait_ms=N runs to it in one call)", "breakpoint-set", bpArgs),
		add("clear", "Remove a breakpoint and restore the trigger slot", "breakpoint-clear", bpArgs),
		add("list", "Enumerate trigger slots and installed breakpoints", "breakpoint-list", nil))
	wp := &cobra.Command{Use: "watchpoint", Short: "Data watchpoints (Cortex-M DWT)"}
	wpArgs := func(c *cobra.Command) {
		c.Flags().StringVar(&f.address, "address", "", "watched address (clear: or --arg all=true; --arg kind=read|write|access, --arg mask_bits=N)")
	}
	wp.AddCommand(add("set", "Install a DWT data watchpoint", "watchpoint-set", wpArgs),
		add("clear", "Remove a watchpoint and restore the comparator", "watchpoint-clear", wpArgs),
		add("list", "Enumerate DWT comparators and installed watchpoints", "watchpoint-list", nil))
	cc := &cobra.Command{Use: "cc23xx", Short: "TI CC23xx: SACI boot ROM interface and CCFG"}
	cc.AddCommand(
		add("info", "Device identity from documented registers, the CCFG decode and the SACI state", "cc23xx-info", nil),
		add("saci-status", "Boot state: whether SACI is active and the AHB-AP is open", "saci-status", nil),
		add("saci-enter", "Reset the device into SACI (--token enter-saci; --arg via=nreset|sysreset)", "saci-enter", nil),
		add("saci-exit", "Leave SACI (--arg mode=run|halt|reset-halt)", "saci-exit", nil),
		add("ccfg-info", "Decode the CCFG sector (no secrets are returned)", "ccfg-info", nil),
		add("ccfg-program", "Replace the CCFG (DANGEROUS; --path IMAGE --arg backup=BACKUP.bin; --arg dry_run=true shows the diff)", "ccfg-program", flArgs),
	)
	jt := &cobra.Command{Use: "jtag", Short: "JTAG chain (probe transport, no CPU debug)"}
	jt.AddCommand(
		add("scan", "Reset the TAPs and enumerate the JTAG chain (IDCODE/BYPASS)", "jtag-scan", nil),
		add("identify", "Decode the IDCODEs of the JTAG chain", "jtag-identify", nil),
	)
	root.AddCommand(
		add("info", "Probe and target information", "info", nil),
		add("identify", "Identify the target chip from its hardware registers", "identify", nil),
		add("attach", "Attach/reset into debug", "attach", nil),
		add("halt", "Halt the CPU", "halt", nil),
		add("resume", "Resume the CPU", "resume", nil),
		add("reset", "Reset the target", "reset", nil),
		add("registers", "Read the CPU registers", "registers", nil),
		add("step", "Single-step the halted CPU (--arg count=N)", "step", nil),
		add("harts", "Per-hart report (availability, state, registers, SBA)", "harts", nil),
		add("wait-halt", "Wait for the CPU to halt (--arg wait_ms=N) and report the cause", "wait-halt", nil),
		add("reconnect", "Drop the session and rediscover the same device by serial", "reconnect", nil),
		add("set-speed", "Change the probe clock (--arg khz=N)", "set-speed", nil),
		mem, fl, bp, wp, cc, jt,
	)
	return root
}
