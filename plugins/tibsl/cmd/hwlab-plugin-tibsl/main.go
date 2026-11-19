// Command hwlab-plugin-tibsl is the TI CC13xx/CC26xx/CC2538 ROM serial bootloader plugin.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	tibslplugin "github.com/faronov/hwlab/plugins/tibsl"
)

func main() { pluginsdk.Main(tibslplugin.New()) }
