// Command hwlab-plugin-nrf802154 serves the nRF 802.15.4 sniffer plugin
// over the hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/nrf802154"
)

func main() { pluginsdk.Main(nrf802154.New()) }
