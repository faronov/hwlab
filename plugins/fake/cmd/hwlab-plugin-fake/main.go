// hwlab-plugin-fake is the reference/simulation plugin used for tests and demos.
package main

import (
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/fake"
)

func main() { pluginsdk.Main(fake.New()) }
