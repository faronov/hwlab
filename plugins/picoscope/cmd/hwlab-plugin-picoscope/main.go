// Command hwlab-plugin-picoscope serves PicoScope oscilloscopes over the hwlab
// plugin protocol. This build has only the synthetic backend; see ADR 0024.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	picoscopeplugin "github.com/faronov/hwlab/plugins/picoscope"
)

func main() { pluginsdk.Main(picoscopeplugin.New()) }
