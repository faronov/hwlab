package energytrace

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"
)

type fixtureSegment struct {
	Note     string   `json:"note"`
	URBs     []string `json:"urbs"`
	Expected []int64  `json:"expected_current_na"`
}

type fixtureCmd struct {
	Cmd    string `json:"cmd"`
	Args   string `json:"args"`
	Status int32  `json:"status"`
	Data   string `json:"data"`
}

type fixtureFile struct {
	Segments []fixtureSegment `json:"segments"`
	Start    []fixtureCmd     `json:"start_sequence"`
	Stop     []fixtureCmd     `json:"stop_sequence"`
}

func loadFixture(t testing.TB) fixtureFile {
	t.Helper()
	b, err := os.ReadFile("testdata/real-lpxds110et/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixtureFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// buildPacket makes a data packet from 24-bit sample codes.
func buildPacket(seq uint8, codes []uint32) []byte {
	b := make([]byte, 6+4*len(codes))
	b[0] = dataMarker
	b[1] = seq
	binary.LittleEndian.PutUint16(b[2:], uint16(4*len(codes)))
	for i, c := range codes {
		binary.BigEndian.PutUint32(b[6+4*i:], c<<8)
	}
	return b
}

func constCodes(v uint32) []uint32 {
	c := make([]uint32, SamplesPerPacket)
	for i := range c {
		c[i] = v
	}
	return c
}

func TestParserFixtureAgainstOracle(t *testing.T) {
	f := loadFixture(t)
	// The first calibration reply of the real run: sum 25143 over 5 readings.
	cal, err := ParseCalibration(unhex(t, f.Start[8].Data))
	if err != nil {
		t.Fatal(err)
	}
	off, err := cal.Offset()
	if err != nil {
		t.Fatal(err)
	}
	for si, seg := range f.Segments {
		var p Parser
		var got []int32
		var pkts []Packet
		for _, u := range seg.URBs {
			p.Feed(unhex(t, u), func(pk Packet) {
				pkts = append(pkts, pk)
			})
		}
		// The oracle CSV skips the first data packet; the expected rows are the
		// last len(expected)/256 packets of the segment.
		n := len(seg.Expected) / SamplesPerPacket
		if len(pkts) < n {
			t.Fatalf("segment %d: %d packets, need %d", si, len(pkts), n)
		}
		for _, pk := range pkts[len(pkts)-n:] {
			got = append(got, ToCurrentNA(pk.Codes, off)...)
		}
		if len(got) != len(seg.Expected) {
			t.Fatalf("segment %d: %d samples, want %d", si, len(got), len(seg.Expected))
		}
		maxErr := 0.0
		for i := range got {
			maxErr = math.Max(maxErr, math.Abs(float64(got[i])-float64(seg.Expected[i])))
		}
		if maxErr > 2 {
			t.Fatalf("segment %d: max error %.2f nA against the oracle CSV, want <= 2", si, maxErr)
		}
		if st := p.Stats(); st.LostPackets != 0 || st.SkippedBytes != 0 {
			t.Fatalf("segment %d: unexpected stats %+v", si, st)
		}
	}
}

func TestParserSequenceWrapAndPrelude(t *testing.T) {
	var p Parser
	var seqs []uint8
	feed := func(b []byte) {
		p.Feed(b, func(pk Packet) { seqs = append(seqs, pk.Seq) })
	}
	feed([]byte{0x35, 0x80, 0xbb})
	for _, s := range []uint8{0xfe, 0xff, 0x00, 0x01} {
		feed(buildPacket(s, constCodes(1)))
	}
	if len(seqs) != 4 || p.Stats().LostPackets != 0 {
		t.Fatalf("seqs %v stats %+v", seqs, p.Stats())
	}
	if p.Stats().Preludes != 1 {
		t.Fatalf("prelude not counted: %+v", p.Stats())
	}
}

func TestParserCountsLostPackets(t *testing.T) {
	var p Parser
	var lost []int
	for _, s := range []uint8{5, 6, 9, 10} {
		p.Feed(buildPacket(s, constCodes(1)), func(pk Packet) { lost = append(lost, pk.Lost) })
	}
	if p.Stats().LostPackets != 2 || lost[2] != 2 {
		t.Fatalf("lost %v stats %+v", lost, p.Stats())
	}
}

func TestParserSplitTransfers(t *testing.T) {
	pkt := buildPacket(7, constCodes(0x123456))
	var p Parser
	var n int
	for i := 0; i < len(pkt); i += 7 {
		end := min(i+7, len(pkt))
		p.Feed(pkt[i:end], func(Packet) { n++ })
	}
	if n != 1 {
		t.Fatalf("got %d packets from split input", n)
	}
}

func TestParserResyncsOnGarbage(t *testing.T) {
	var p Parser
	var seqs []uint8
	in := append([]byte{0x00, 0x11, 0x22}, buildPacket(1, constCodes(1))...)
	in = append(in, buildPacket(2, constCodes(1))...)
	p.Feed(in, func(pk Packet) { seqs = append(seqs, pk.Seq) })
	if len(seqs) != 2 || p.Stats().SkippedBytes != 3 {
		t.Fatalf("seqs %v stats %+v", seqs, p.Stats())
	}
}

func TestParserRejectsBadLength(t *testing.T) {
	bad := buildPacket(1, constCodes(1))
	binary.LittleEndian.PutUint16(bad[2:], 1021) // not a multiple of 4
	var p Parser
	var n int
	p.Feed(append(bad, buildPacket(2, constCodes(1))...), func(Packet) { n++ })
	if n != 1 || p.Stats().SkippedBytes == 0 {
		t.Fatalf("n=%d stats %+v", n, p.Stats())
	}
}

func TestParserRejectsOversizedLength(t *testing.T) {
	bad := []byte{dataMarker, 1, 0xff, 0xff, 0, 0}
	var p Parser
	var n int
	p.Feed(append(bad, buildPacket(2, constCodes(1))...), func(Packet) { n++ })
	if n != 1 {
		t.Fatalf("n=%d stats %+v", n, p.Stats())
	}
}

func TestParserBufferStaysBounded(t *testing.T) {
	var p Parser
	junk := make([]byte, 1<<20)
	for i := range junk {
		junk[i] = 0x68 // every byte looks like a marker with length 0x6868
	}
	p.Feed(junk, func(Packet) {})
	if len(p.buf) > maxPacket {
		t.Fatalf("buffer grew to %d", len(p.buf))
	}
}

func TestDecodeCodeIgnoresLowByte(t *testing.T) {
	if got := DecodeCode(0x007a1250); got != 0x007a12 {
		t.Fatalf("got %#x", got)
	}
}

func TestCalibration(t *testing.T) {
	if _, err := ParseCalibration([]byte{1, 2, 3}); err == nil {
		t.Fatal("short calibration must fail")
	}
	zero := make([]byte, 8)
	c, err := ParseCalibration(zero)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Offset(); err == nil {
		t.Fatal("count 0 must fail")
	}
	c, _ = ParseCalibration([]byte{0x37, 0x62, 0, 0, 5, 0, 0, 0})
	off, err := c.Offset()
	if err != nil || math.Abs(off-5028.6) > 1e-9 {
		t.Fatalf("offset %v err %v", off, err)
	}
}

func TestToCurrentNA(t *testing.T) {
	got := ToCurrentNA([]uint32{5000, 5000 + 1000, 5000 - 1000}, 5000)
	want := []int32{0, 15136, -15136}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func FuzzParser(f *testing.F) {
	f.Add(buildPacket(1, constCodes(7)))
	f.Add([]byte{0x35, 0x80, 0xbb, 0x68})
	f.Fuzz(func(t *testing.T, b []byte) {
		var p Parser
		p.Feed(b, func(pk Packet) {
			if len(pk.Codes) == 0 || len(pk.Codes) > maxPayload/4 {
				t.Fatalf("bad packet size %d", len(pk.Codes))
			}
		})
		if len(p.buf) > maxPacket {
			t.Fatalf("buffer %d", len(p.buf))
		}
	})
}
