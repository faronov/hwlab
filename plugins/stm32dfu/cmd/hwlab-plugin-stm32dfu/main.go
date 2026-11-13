// Command hwlab-plugin-stm32dfu serves STM32 ROM USB DFU bootloaders
// (DfuSe, 0483:df11) over the hwlab plugin protocol.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	stm32dfuplugin "github.com/faronov/hwlab/plugins/stm32dfu"
)

func main() { pluginsdk.Main(stm32dfuplugin.New()) }
