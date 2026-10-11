// Command hwlab-plugin-saleae serves the Saleae Logic 2 plugin over the hwlab
// plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/saleae"
)

func main() { pluginsdk.Main(saleae.New()) }
