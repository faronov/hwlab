// Command hwlab-plugin-espusbjtag serves Espressif USB Serial/JTAG devices
// over the hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	espusbjtag "github.com/faronov/hwlab/plugins/espusbjtag"
)

func main() { pluginsdk.Main(espusbjtag.New()) }
