// Command hwlab-plugin-telink is the Telink SWire programmer plugin.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	telinkplugin "github.com/faronov/hwlab/plugins/telink"
)

func main() { pluginsdk.Main(telinkplugin.New()) }
