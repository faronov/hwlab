// Command hwlab-plugin-usbcap serves the USB packet capture plugin over the
// hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	usbcapplugin "github.com/faronov/hwlab/plugins/usbcap"
)

func main() { pluginsdk.Main(usbcapplugin.New()) }
