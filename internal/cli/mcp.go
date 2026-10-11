package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/faronov/hwlab/internal/mcp"
)

func mcpCmd(g *globals) *cobra.Command {
	var (
		stdio  bool
		replay string
	)
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server",
		Long: `Runs the Model Context Protocol server. Only the stdio transport is supported:
configure your MCP client with command "hwlab" and args ["mcp", "--stdio"].
Logs go to stderr; stdout carries protocol messages only.

With --replay SESSION the server also offers the recorded session as plugin
"replay" (experiment_run with plugin: replay re-emits its timeline, including
recorded invoke/target results), so a whole workflow runs without hardware.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !stdio {
				return fmt.Errorf("choose a transport: --stdio")
			}
			if replay != "" {
				if err := g.app.UseReplay(replay); err != nil {
					return err
				}
			}
			return mcp.ServeStdio(cmd.Context(), g.app)
		},
	}
	c.Flags().BoolVar(&stdio, "stdio", false, "serve MCP over stdin/stdout")
	c.Flags().StringVar(&replay, "replay", "", "make `plugin: replay` replay this recorded session (no hardware needed)")
	return c
}
