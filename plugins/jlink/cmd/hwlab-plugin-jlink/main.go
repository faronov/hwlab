// Command hwlab-plugin-jlink serves the SEGGER J-Link debug probe plugin over
// the hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/jlink"
)

func main() { pluginsdk.Main(jlink.New()) }
