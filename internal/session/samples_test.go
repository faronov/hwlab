package session

import (
	"context"
	"math"
	"testing"
)

func TestHwcapPowerRoundTripAndRangeRead(t *testing.T) {
	store := DirSampleStore{Root: t.TempDir()}
	w, err := store.Create("samples/p.hwcap")
	if err != nil {
		t.Fatal(err)
	}
	const period = 10_000
	var written []int32
	// Three chunks of different sizes, with and without voltage/digital.
	for i, n := range []int{1000, 250, 4096} {
		c := &PowerChunk{StartNs: int64(len(written)) * period, PeriodNs: period, CurrentNA: make([]int32, n)}
		for j := range c.CurrentNA {
			c.CurrentNA[j] = int32(len(written)+j)*3 - 5000 // includes negatives
		}
		if i != 1 {
			c.VoltageUV = make([]uint32, n)
			c.Digital = make([]uint32, n)
			for j := range c.Digital {
				c.VoltageUV[j] = 3_300_000 + uint32(j)
				c.Digital[j] = uint32(j & 3)
			}
		}
		if err := w.WritePower(c); err != nil {
			t.Fatal(err)
		}
		written = append(written, c.CurrentNA...)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := store.Open("samples/p.hwcap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	if r.Kind() != "power" || r.Samples() != int64(len(written)) {
		t.Fatalf("kind=%s samples=%d", r.Kind(), r.Samples())
	}
	start, end := r.Bounds()
	if start != 0 || end != int64(len(written))*period {
		t.Errorf("bounds = %d..%d", start, end)
	}

	var got []int32
	if err := r.ReadPower(context.Background(), math.MinInt64, math.MaxInt64, func(c *PowerChunk) error {
		got = append(got, c.CurrentNA...) // copy: slices are reused
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(written) {
		t.Fatalf("read %d samples, wrote %d", len(got), len(written))
	}
	for i := range got {
		if got[i] != written[i] {
			t.Fatalf("sample %d: %d != %d", i, got[i], written[i])
		}
	}

	// A range inside the third chunk must not return the first two.
	var n int
	from := int64(1300) * period
	if err := r.ReadPower(context.Background(), from, from+period, func(c *PowerChunk) error {
		if c.StartNs > from || c.EndNs() <= from {
			t.Errorf("chunk %d..%d does not overlap the range", c.StartNs, c.EndNs())
		}
		n++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("range read touched %d chunks, want 1", n)
	}
}

func TestHwcapDigitalAndAnalog(t *testing.T) {
	store := DirSampleStore{Root: t.TempDir()}
	w, err := store.Create("d.hwcap")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteDigital(&DigitalChunk{StartNs: 100, PeriodNs: 10, Words: []uint32{0, 1, 3, 2}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := store.Open("d.hwcap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	var words []uint32
	if err := r.ReadDigital(context.Background(), math.MinInt64, math.MaxInt64, func(c *DigitalChunk) error {
		words = append(words, c.Words...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(words) != 4 || words[2] != 3 {
		t.Errorf("words = %v", words)
	}

	w, err = store.Create("a.hwcap")
	if err != nil {
		t.Fatal(err)
	}
	ac := &AnalogChunk{StartNs: 0, PeriodNs: 1000, Channels: 2, Samples: []float32{1, 2, 3, 4, 5, 6}}
	if err := w.WriteAnalog(ac); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := store.Open("a.hwcap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r2.Close(); err != nil {
			t.Error(err)
		}
	})
	var samples []float32
	if err := r2.ReadAnalog(context.Background(), math.MinInt64, math.MaxInt64, func(c *AnalogChunk) error {
		if c.Channels != 2 || c.N() != 3 {
			t.Errorf("channels=%d n=%d", c.Channels, c.N())
		}
		samples = append(samples, c.Samples...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(samples) != 6 || samples[5] != 6 {
		t.Errorf("samples = %v", samples)
	}
}

func TestOpenRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	store := DirSampleStore{Root: dir}
	w, err := store.Create("x.hwcap")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(dir+"/bad.hwcap", []byte("not an hwcap file at all")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open("bad.hwcap"); err == nil {
		t.Error("expected an error for a file without the hwcap magic")
	}
}

func TestPowerDigitalEncodings(t *testing.T) {
	cases := map[string]struct {
		words []uint32
		size  int // expected payload bytes for the digital part
	}{
		"constant": {[]uint32{0x1F, 0x1F, 0x1F, 0x1F}, 4},
		"u8":       {[]uint32{0, 1, 0xFF, 7}, 4},
		"u32":      {[]uint32{0, 0x100, 0xFFFF_FFFF, 7}, 16},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, size := digitalEncoding(tc.words); size != tc.size {
				t.Errorf("encoded digital size = %d, want %d", size, tc.size)
			}
			store := DirSampleStore{Root: t.TempDir()}
			w, err := store.Create("p.hwcap")
			if err != nil {
				t.Fatal(err)
			}
			c := &PowerChunk{PeriodNs: 10_000, CurrentNA: []int32{1, -2, 3, -4}, Digital: tc.words}
			if err := w.WritePower(c); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := store.Open("p.hwcap")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := r.Close(); err != nil {
					t.Error(err)
				}
			})
			var got []uint32
			if err := r.ReadPower(context.Background(), 0, 1<<40, func(pc *PowerChunk) error {
				got = append(got, pc.Digital...)
				if len(pc.CurrentNA) != 4 || pc.CurrentNA[1] != -2 {
					t.Errorf("current damaged: %v", pc.CurrentNA)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.words) {
				t.Fatalf("digital = %v", got)
			}
			for i := range got {
				if got[i] != tc.words[i] {
					t.Fatalf("digital = %v, want %v", got, tc.words)
				}
			}
		})
	}
}
