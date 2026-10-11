// Command hwlab-plugin-stlink serves ST-Link probes over the hwlab plugin
// protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	stlinkplugin "github.com/faronov/hwlab/plugins/stlink"
)

func main() { pluginsdk.Main(stlinkplugin.New()) }
