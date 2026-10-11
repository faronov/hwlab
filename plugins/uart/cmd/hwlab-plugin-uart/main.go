// Command hwlab-plugin-uart serves the generic serial (UART) transport plugin
// over the hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/uart"
)

func main() { pluginsdk.Main(uart.New()) }
