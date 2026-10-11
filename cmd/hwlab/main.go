// Command hwlab is the hardware-lab automation CLI and MCP server.
package main

import (
	"os"

	"github.com/faronov/hwlab/internal/cli"
)

func main() { os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr)) }
