// Command hwlab-plugin-cmsisdap serves CMSIS-DAP debug probes (SWD, Cortex-M)
// over the hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	cmsisdapplugin "github.com/faronov/hwlab/plugins/cmsisdap"
)

func main() { pluginsdk.Main(cmsisdapplugin.New()) }
