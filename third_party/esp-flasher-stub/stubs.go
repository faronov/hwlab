// Package espflasherstub embeds the prebuilt esp-flasher-stub images
// (Espressif, Apache-2.0 OR MIT; release and digests in README.md).
package espflasherstub

import "embed"

// FS holds <chip>.json for esp32, esp32s3, esp32c6 and esp32h2.
//
//go:embed esp32.json esp32s3.json esp32c6.json esp32h2.json
var FS embed.FS

// Release is the upstream release the files come from.
const Release = "esp-flasher-stub v2.0.0"
