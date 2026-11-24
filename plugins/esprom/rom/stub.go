package rom

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	espflasherstub "github.com/faronov/hwlab/third_party/esp-flasher-stub"
)

// Stub is one esp-flasher-stub image: two segments loaded with MEM_* and an
// entry point. Optional plugins in the JSON are ignored (never uploaded).
type Stub struct {
	Chip      string
	Entry     uint32
	Text      []byte
	TextStart uint32
	Data      []byte
	DataStart uint32
	BSSStart  uint32
}

type stubJSON struct {
	Entry     uint32 `json:"entry"`
	Text      string `json:"text"`
	TextStart uint32 `json:"text_start"`
	Data      string `json:"data"`
	DataStart uint32 `json:"data_start"`
	BSSStart  uint32 `json:"bss_start"`
}

// ParseStub decodes a stub JSON file.
func ParseStub(chip string, b []byte) (*Stub, error) {
	var j stubJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("stub %s: %w", chip, err)
	}
	s := &Stub{Chip: chip, Entry: j.Entry, TextStart: j.TextStart, DataStart: j.DataStart, BSSStart: j.BSSStart}
	var err error
	if s.Text, err = base64.StdEncoding.DecodeString(j.Text); err != nil {
		return nil, fmt.Errorf("stub %s: text segment: %w", chip, err)
	}
	if len(s.Text) == 0 {
		return nil, fmt.Errorf("stub %s: empty text segment", chip)
	}
	if j.Data != "" {
		if s.Data, err = base64.StdEncoding.DecodeString(j.Data); err != nil {
			return nil, fmt.Errorf("stub %s: data segment: %w", chip, err)
		}
	}
	if s.Entry < s.TextStart || s.Entry >= s.TextStart+uint32(len(s.Text)) {
		return nil, fmt.Errorf("stub %s: entry 0x%08X outside the text segment", chip, s.Entry)
	}
	return s, nil
}

// StubFor returns the vendored stub of a chip key (esp32, esp32s3, esp32c6,
// esp32h2).
func StubFor(key string) (*Stub, error) {
	b, err := espflasherstub.FS.ReadFile(key + ".json")
	if err != nil {
		return nil, fmt.Errorf("no flasher stub is vendored for %s", key)
	}
	return ParseStub(key, b)
}

// StubRelease names the vendored stub release.
const StubRelease = espflasherstub.Release

// ohai is the stub's greeting, the only unsolicited frame it sends.
var ohai = []byte("OHAI")

// RunStub uploads s into RAM through the ROM, starts it and waits for its
// greeting. From then on the client speaks to the stub (2 status bytes).
func (c *Client) RunStub(s *Stub) error {
	if c.stub {
		return nil
	}
	for _, seg := range []struct {
		addr uint32
		b    []byte
	}{{s.TextStart, s.Text}, {s.DataStart, s.Data}} {
		if len(seg.b) == 0 {
			continue
		}
		blocks := (len(seg.b) + RAMBlock - 1) / RAMBlock
		if err := c.MemBegin(uint32(len(seg.b)), uint32(blocks), RAMBlock, seg.addr); err != nil {
			return fmt.Errorf("stub upload: %w", err)
		}
		for i := 0; i < blocks; i++ {
			if err := c.MemData(uint32(i), seg.b[i*RAMBlock:min(len(seg.b), (i+1)*RAMBlock)]); err != nil {
				return fmt.Errorf("stub upload at 0x%08X: %w", seg.addr+uint32(i*RAMBlock), err)
			}
		}
	}
	if err := c.MemEnd(true, s.Entry); err != nil {
		return fmt.Errorf("stub start: %w", err)
	}
	deadline := time.Now().Add(c.Timeout)
	for {
		f, err := c.readFrame(deadline)
		if err != nil {
			return fmt.Errorf("stub start: no OHAI greeting: %w", err)
		}
		if string(f) == string(ohai) {
			c.stub, c.statusLen = true, 2
			return nil
		}
		c.stats.Stale++
	}
}
