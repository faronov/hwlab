// Command hwlab-plugin-bflb is the Bouffalo BL702 boot-ROM ISP plugin.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	bflbplugin "github.com/faronov/hwlab/plugins/bflb"
)

func main() { pluginsdk.Main(bflbplugin.New()) }
