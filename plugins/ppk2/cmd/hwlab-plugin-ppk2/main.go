// Command hwlab-plugin-ppk2 serves the PPK2 plugin over the hwlab plugin
// protocol. It is launched by hwlab; running it by hand just waits for a core.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/ppk2"
)

func main() { pluginsdk.Main(ppk2.New()) }
