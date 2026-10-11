// Command hwlab-plugin-xds110 serves TI XDS110 debug probes (SWD, Cortex-M)
// over the hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	xds110plugin "github.com/faronov/hwlab/plugins/xds110"
)

func main() { pluginsdk.Main(xds110plugin.New()) }
