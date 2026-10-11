// Command hwlab-plugin-modbus serves Modbus RTU (over a serial port) and
// Modbus TCP masters over the hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	modbusplugin "github.com/faronov/hwlab/plugins/modbus"
)

func main() { pluginsdk.Main(modbusplugin.New()) }
