// Command hwlab-plugin-ftdi serves FTDI MPSSE JTAG adapters over the hwlab
// plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	ftdiplugin "github.com/faronov/hwlab/plugins/ftdi"
)

func main() { pluginsdk.Main(ftdiplugin.New()) }
