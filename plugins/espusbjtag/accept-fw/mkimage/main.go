// Command mkimage wraps the flat acceptance binary in an ESP32-C6 image.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: mkimage <flat.bin> <load-addr> <entry> <out.bin>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	load, err1 := strconv.ParseUint(os.Args[2], 0, 32)
	entry, err2 := strconv.ParseUint(os.Args[3], 0, 32)
	if err1 != nil || err2 != nil {
		fmt.Fprintln(os.Stderr, "bad address")
		os.Exit(2)
	}
	img := esp.BuildImage(esp.ChipIDESP32C6, uint32(entry), map[uint32][]byte{uint32(load): raw}, []uint32{uint32(load)})
	img[8] = 0xEE // wp_pin "unused", as the standard bootloader header has it
	if err := os.WriteFile(os.Args[4], img, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("image %d bytes, %d bytes of code and data at 0x%08x\n", len(img), len(raw), load)
}
