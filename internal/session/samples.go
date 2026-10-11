package session

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

// Chunk kinds stored in a .hwcap file.
const (
	KindPower   = "power"
	KindAnalog  = "analog"
	KindDigital = "digital"
)

const (
	hwcapMagic = "HWCAP001"

	chunkPower   byte = 1
	chunkAnalog  byte = 2
	chunkDigital byte = 3

	maxChunkBytes = 256 << 20

	// Power chunk flags. Digital words are stored in the narrowest of three
	// encodings: one word for a constant block, one byte per sample when all
	// words fit in 8 bits (the PPK2 has 8 inputs), else 4 bytes per sample.
	flagVoltage      byte = 1
	flagDigital      byte = 2
	flagDigitalU8    byte = 4
	flagDigitalConst byte = 8
)

// digitalEncoding picks the narrowest flags for a digital word slice.
func digitalEncoding(words []uint32) (flags byte, size int) {
	if len(words) == 0 {
		return 0, 0
	}
	same, narrow := true, true
	for _, w := range words {
		same = same && w == words[0]
		narrow = narrow && w <= 0xFF
	}
	switch {
	case same:
		return flagDigital | flagDigitalConst, 4
	case narrow:
		return flagDigital | flagDigitalU8, len(words)
	}
	return flagDigital, len(words) * 4
}

// PowerChunk is a block of evenly spaced power samples.
// Slices returned by readers are only valid inside the callback.
type PowerChunk struct {
	StartNs  int64
	PeriodNs int64
	// CurrentNA is current in nanoamps.
	CurrentNA []int32
	// VoltageUV is optional (len 0 or len(CurrentNA)).
	VoltageUV []uint32
	// Digital is optional logic bits per sample (len 0 or len(CurrentNA)).
	Digital []uint32
}

// N returns the sample count.
func (c *PowerChunk) N() int { return len(c.CurrentNA) }

// EndNs returns the timestamp one period after the last sample.
func (c *PowerChunk) EndNs() int64 { return c.StartNs + int64(len(c.CurrentNA))*c.PeriodNs }

// AnalogChunk is a block of interleaved analog samples.
type AnalogChunk struct {
	StartNs  int64
	PeriodNs int64
	Channels int
	Samples  []float32 // interleaved, len = n*Channels
}

func (c *AnalogChunk) N() int {
	if c.Channels == 0 {
		return 0
	}
	return len(c.Samples) / c.Channels
}
func (c *AnalogChunk) EndNs() int64 { return c.StartNs + int64(c.N())*c.PeriodNs }

// DigitalChunk is a block of sampled logic words.
type DigitalChunk struct {
	StartNs  int64
	PeriodNs int64
	Words    []uint32
}

func (c *DigitalChunk) N() int       { return len(c.Words) }
func (c *DigitalChunk) EndNs() int64 { return c.StartNs + int64(len(c.Words))*c.PeriodNs }

// StreamWriter appends chunks to a stream. Implementations must be safe for
// use from a single goroutine.
type StreamWriter interface {
	WritePower(*PowerChunk) error
	WriteAnalog(*AnalogChunk) error
	WriteDigital(*DigitalChunk) error
	Close() error
}

// StreamReader reads chunks by time range. Chunk slices passed to callbacks
// are reused: copy if you need to retain them.
type StreamReader interface {
	Kind() string
	// Bounds returns [start, end) of the stored data.
	Bounds() (start, end int64)
	Samples() int64
	ReadPower(ctx context.Context, from, to int64, fn func(*PowerChunk) error) error
	ReadAnalog(ctx context.Context, from, to int64, fn func(*AnalogChunk) error) error
	ReadDigital(ctx context.Context, from, to int64, fn func(*DigitalChunk) error) error
	Close() error
}

// SampleStore creates and opens sample streams. The directory implementation
// is the only backend today; others (Parquet, remote) can be added later.
type SampleStore interface {
	Create(relPath string) (StreamWriter, error)
	Open(relPath string) (StreamReader, error)
}

// DirSampleStore stores .hwcap files below a session directory.
type DirSampleStore struct{ Root string }

func (d DirSampleStore) Create(rel string) (StreamWriter, error) {
	p := filepath.Join(d.Root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := &fileWriter{f: f, bw: bufio.NewWriterSize(f, 256<<10)}
	if _, err := w.bw.WriteString(hwcapMagic); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

func (d DirSampleStore) Open(rel string) (StreamReader, error) {
	return openFileReader(filepath.Join(d.Root, rel))
}

type fileWriter struct {
	f       *os.File
	bw      *bufio.Writer
	scratch []byte
}

func (w *fileWriter) buf(n int) []byte {
	if cap(w.scratch) < n {
		w.scratch = make([]byte, n, n+n/4)
	}
	return w.scratch[:n]
}

func (w *fileWriter) writeChunk(kind byte, payload []byte) error {
	var hdr [5]byte
	binary.LittleEndian.PutUint32(hdr[:4], uint32(len(payload)))
	hdr[4] = kind
	if _, err := w.bw.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.bw.Write(payload); err != nil {
		return err
	}
	var crc [4]byte
	binary.LittleEndian.PutUint32(crc[:], crc32.ChecksumIEEE(payload))
	_, err := w.bw.Write(crc[:])
	return err
}

func (w *fileWriter) WritePower(c *PowerChunk) error {
	n := len(c.CurrentNA)
	var flags byte
	size := 8 + 8 + 4 + 1 + n*4
	if len(c.VoltageUV) == n && n > 0 {
		flags |= 1
		size += n * 4
	}
	if len(c.Digital) == n && n > 0 {
		df, ds := digitalEncoding(c.Digital)
		flags |= df
		size += ds
	}
	b := w.buf(size)
	le := binary.LittleEndian
	le.PutUint64(b[0:], uint64(c.StartNs))
	le.PutUint64(b[8:], uint64(c.PeriodNs))
	le.PutUint32(b[16:], uint32(n))
	b[20] = flags
	o := 21
	for _, v := range c.CurrentNA {
		le.PutUint32(b[o:], uint32(v))
		o += 4
	}
	if flags&1 != 0 {
		for _, v := range c.VoltageUV {
			le.PutUint32(b[o:], v)
			o += 4
		}
	}
	switch {
	case flags&flagDigitalConst != 0:
		le.PutUint32(b[o:], c.Digital[0])
	case flags&flagDigitalU8 != 0:
		for _, v := range c.Digital {
			b[o] = byte(v)
			o++
		}
	case flags&flagDigital != 0:
		for _, v := range c.Digital {
			le.PutUint32(b[o:], v)
			o += 4
		}
	}
	return w.writeChunk(chunkPower, b)
}

func (w *fileWriter) WriteAnalog(c *AnalogChunk) error {
	b := w.buf(8 + 8 + 4 + 2 + len(c.Samples)*4)
	le := binary.LittleEndian
	le.PutUint64(b[0:], uint64(c.StartNs))
	le.PutUint64(b[8:], uint64(c.PeriodNs))
	le.PutUint32(b[16:], uint32(c.N()))
	le.PutUint16(b[20:], uint16(c.Channels))
	o := 22
	for _, v := range c.Samples {
		le.PutUint32(b[o:], math.Float32bits(v))
		o += 4
	}
	return w.writeChunk(chunkAnalog, b)
}

func (w *fileWriter) WriteDigital(c *DigitalChunk) error {
	b := w.buf(8 + 8 + 4 + len(c.Words)*4)
	le := binary.LittleEndian
	le.PutUint64(b[0:], uint64(c.StartNs))
	le.PutUint64(b[8:], uint64(c.PeriodNs))
	le.PutUint32(b[16:], uint32(len(c.Words)))
	o := 20
	for _, v := range c.Words {
		le.PutUint32(b[o:], v)
		o += 4
	}
	return w.writeChunk(chunkDigital, b)
}

func (w *fileWriter) Close() error {
	err := w.bw.Flush()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ---------------------------------------------------------------- reader

type chunkRef struct {
	off     int64 // offset of payload
	size    uint32
	startNs int64
	endNs   int64
	n       uint32
}

type fileReader struct {
	f       *os.File
	kind    string
	chunks  []chunkRef
	samples int64
	mu      sync.Mutex
	pool    sync.Pool
}

func openFileReader(path string) (*fileReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &fileReader{f: f}
	r.pool.New = func() any { b := make([]byte, 0, 64<<10); return &b }
	if err := r.index(); err != nil {
		f.Close()
		return nil, fmt.Errorf("index %s: %w", path, err)
	}
	return r, nil
}

// index scans chunk headers. A truncated or corrupt tail (crash during
// capture) ends the index without error.
func (r *fileReader) index() error {
	br := bufio.NewReaderSize(r.f, 64<<10)
	magic := make([]byte, len(hwcapMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != hwcapMagic {
		return errors.New("bad magic")
	}
	off := int64(len(hwcapMagic))
	le := binary.LittleEndian
	for {
		var hdr [5]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return nil
		}
		size := le.Uint32(hdr[:4])
		kind := hdr[4]
		if size < 20 || size > maxChunkBytes {
			return nil
		}
		head := make([]byte, 24)
		hn := int(size)
		if hn > len(head) {
			hn = len(head)
		}
		if _, err := io.ReadFull(br, head[:hn]); err != nil {
			return nil
		}
		if _, err := br.Discard(int(size) - hn + 4); err != nil {
			return nil
		}
		ref := chunkRef{off: off + 5, size: size}
		ref.startNs = int64(le.Uint64(head[0:]))
		period := int64(le.Uint64(head[8:]))
		ref.n = le.Uint32(head[16:])
		ref.endNs = ref.startNs + int64(ref.n)*period
		var k string
		switch kind {
		case chunkPower:
			k = KindPower
		case chunkAnalog:
			k = KindAnalog
		case chunkDigital:
			k = KindDigital
		default:
			return nil
		}
		if r.kind == "" {
			r.kind = k
		} else if r.kind != k {
			return errors.New("mixed chunk kinds in one stream")
		}
		r.chunks = append(r.chunks, ref)
		r.samples += int64(ref.n)
		off += 5 + int64(size) + 4
	}
}

func (r *fileReader) Kind() string   { return r.kind }
func (r *fileReader) Samples() int64 { return r.samples }
func (r *fileReader) Close() error   { return r.f.Close() }

func (r *fileReader) Bounds() (int64, int64) {
	if len(r.chunks) == 0 {
		return 0, 0
	}
	s, e := r.chunks[0].startNs, r.chunks[0].endNs
	for _, c := range r.chunks {
		if c.startNs < s {
			s = c.startNs
		}
		if c.endNs > e {
			e = c.endNs
		}
	}
	return s, e
}

func (r *fileReader) each(ctx context.Context, from, to int64, fn func(payload []byte) error) error {
	bp := r.pool.Get().(*[]byte)
	defer r.pool.Put(bp)
	for _, c := range r.chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.endNs <= from || c.startNs >= to {
			continue
		}
		need := int(c.size) + 4
		if cap(*bp) < need {
			*bp = make([]byte, need)
		}
		buf := (*bp)[:need]
		if _, err := r.f.ReadAt(buf, c.off); err != nil {
			return fmt.Errorf("read chunk: %w", err)
		}
		payload := buf[:c.size]
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(buf[c.size:]) {
			return errors.New("chunk checksum mismatch")
		}
		if err := fn(payload); err != nil {
			return err
		}
	}
	return nil
}

func (r *fileReader) ReadPower(ctx context.Context, from, to int64, fn func(*PowerChunk) error) error {
	if r.kind != KindPower {
		return fmt.Errorf("stream kind is %q, not power", r.kind)
	}
	var pc PowerChunk
	le := binary.LittleEndian
	return r.each(ctx, from, to, func(b []byte) error {
		n := int(le.Uint32(b[16:]))
		flags := b[20]
		pc.StartNs = int64(le.Uint64(b[0:]))
		pc.PeriodNs = int64(le.Uint64(b[8:]))
		pc.CurrentNA = growI32(pc.CurrentNA, n)
		o := 21
		for i := 0; i < n; i++ {
			pc.CurrentNA[i] = int32(le.Uint32(b[o:]))
			o += 4
		}
		pc.VoltageUV, pc.Digital = pc.VoltageUV[:0], pc.Digital[:0]
		if flags&1 != 0 {
			pc.VoltageUV = growU32(pc.VoltageUV, n)
			for i := 0; i < n; i++ {
				pc.VoltageUV[i] = le.Uint32(b[o:])
				o += 4
			}
		}
		if flags&flagDigital != 0 {
			pc.Digital = growU32(pc.Digital, n)
			switch {
			case flags&flagDigitalConst != 0:
				w := le.Uint32(b[o:])
				for i := 0; i < n; i++ {
					pc.Digital[i] = w
				}
			case flags&flagDigitalU8 != 0:
				for i := 0; i < n; i++ {
					pc.Digital[i] = uint32(b[o+i])
				}
			default:
				for i := 0; i < n; i++ {
					pc.Digital[i] = le.Uint32(b[o:])
					o += 4
				}
			}
		}
		return fn(&pc)
	})
}

func (r *fileReader) ReadAnalog(ctx context.Context, from, to int64, fn func(*AnalogChunk) error) error {
	if r.kind != KindAnalog {
		return fmt.Errorf("stream kind is %q, not analog", r.kind)
	}
	var ac AnalogChunk
	le := binary.LittleEndian
	return r.each(ctx, from, to, func(b []byte) error {
		n := int(le.Uint32(b[16:]))
		ch := int(le.Uint16(b[20:]))
		ac.StartNs = int64(le.Uint64(b[0:]))
		ac.PeriodNs = int64(le.Uint64(b[8:]))
		ac.Channels = ch
		total := n * ch
		if cap(ac.Samples) < total {
			ac.Samples = make([]float32, total)
		}
		ac.Samples = ac.Samples[:total]
		o := 22
		for i := range ac.Samples {
			ac.Samples[i] = math.Float32frombits(le.Uint32(b[o:]))
			o += 4
		}
		return fn(&ac)
	})
}

func (r *fileReader) ReadDigital(ctx context.Context, from, to int64, fn func(*DigitalChunk) error) error {
	if r.kind != KindDigital {
		return fmt.Errorf("stream kind is %q, not digital", r.kind)
	}
	var dc DigitalChunk
	le := binary.LittleEndian
	return r.each(ctx, from, to, func(b []byte) error {
		n := int(le.Uint32(b[16:]))
		dc.StartNs = int64(le.Uint64(b[0:]))
		dc.PeriodNs = int64(le.Uint64(b[8:]))
		dc.Words = growU32(dc.Words, n)
		o := 20
		for i := 0; i < n; i++ {
			dc.Words[i] = le.Uint32(b[o:])
			o += 4
		}
		return fn(&dc)
	})
}

func growI32(s []int32, n int) []int32 {
	if cap(s) < n {
		return make([]int32, n)
	}
	return s[:n]
}

func growU32(s []uint32, n int) []uint32 {
	if cap(s) < n {
		return make([]uint32, n)
	}
	return s[:n]
}
