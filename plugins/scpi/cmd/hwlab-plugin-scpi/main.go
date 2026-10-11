// Command hwlab-plugin-scpi serves SCPI instruments (oscilloscopes with Rigol
// and Siglent profiles) over the hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	scpiplugin "github.com/faronov/hwlab/plugins/scpi"
)

func main() { pluginsdk.Main(scpiplugin.New()) }
