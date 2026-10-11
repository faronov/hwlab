// Command hwlab-plugin-ccdebugger serves the TI CC-Debugger plugin over the
// hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/ccdebugger"
)

func main() { pluginsdk.Main(ccdebugger.New()) }
