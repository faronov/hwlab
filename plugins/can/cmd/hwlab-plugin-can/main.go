// Command hwlab-plugin-can serves the CAN / CAN-FD bus plugin over the hwlab
// plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	canplugin "github.com/faronov/hwlab/plugins/can"
)

func main() { pluginsdk.Main(canplugin.New()) }
