// Command hwlab-plugin-jlinknative serves SEGGER J-Link / SAM-ICE class debug
// probes natively (pure Go over Linux usbfs or macOS IOKit: no SEGGER
// software, no libusb, no CGO) over the hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	jlinknativeplugin "github.com/faronov/hwlab/plugins/jlinknative"
)

func main() { pluginsdk.Main(jlinknativeplugin.New()) }
