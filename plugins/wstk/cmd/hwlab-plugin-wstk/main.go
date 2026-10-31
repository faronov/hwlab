// Command hwlab-plugin-wstk serves the Silicon Labs Wireless Starter Kit
// plugin over the hwlab plugin protocol. It is launched by hwlab; running it
// by hand just waits for a core.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/wstk"
)

func main() { pluginsdk.Main(wstk.New()) }
