// Command hwlab-plugin-esprom is the Espressif serial bootloader plugin.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	espromplugin "github.com/faronov/hwlab/plugins/esprom"
)

func main() { pluginsdk.Main(espromplugin.New()) }
