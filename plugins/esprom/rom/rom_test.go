package rom

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // protocol digest
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	espflasherstub "github.com/faronov/hwlab/third_party/esp-flasher-stub"
)

func fastTiming(t *testing.T) {
	t.Helper()
	a, b, c, d, e := ResetHold, StrapHold, StrapHoldLong, USJStep, EraseAllowance
	ResetHold, StrapHold, StrapHoldLong, USJStep, EraseAllowance = time.Millisecond, time.Millisecond, 30*time.Millisecond, time.Millisecond, 0
	t.Cleanup(func() { ResetHold, StrapHold, StrapHoldLong, USJStep, EraseAllowance = a, b, c, d, e })
}

// romClient puts a classic-wired sim into download mode and syncs.
func romClient(t *testing.T, ch *Chip, size int) (*Sim, *Client) {
	t.Helper()
	fastTiming(t)
	s := NewSim(ch, EntryClassic, size)
	s.PowerOn()
	if _, err := EnterBootloader(context.Background(), s.SetLines, EntryClassic, false); err != nil {
		t.Fatal(err)
	}
	c := New(s)
	c.Timeout = 300 * time.Millisecond
	if _, err := c.Sync(5, 50*time.Millisecond); err != nil {
		t.Fatalf("%v: %v", s, err)
	}
	return s, c
}

func stubClient(t *testing.T, ch *Chip, size int) (*Sim, *Client) {
	t.Helper()
	s, c := romClient(t, ch, size)
	st, err := StubFor(ch.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RunStub(st); err != nil {
		t.Fatal(err)
	}
	if err := c.SPIAttach(0); err != nil {
		t.Fatal(err)
	}
	if err := c.SPISetParams(uint32(size)); err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestSlipRoundTripJunkAndBadEscape(t *testing.T) {
	p := []byte{0x01, 0xC0, 0x02, 0xDB, 0x03, 0xDB, 0xDC}
	enc := SlipEncode(p)
	if !bytes.Equal(enc, []byte{0xC0, 0x01, 0xDB, 0xDC, 0x02, 0xDB, 0xDD, 0x03, 0xDB, 0xDD, 0xDC, 0xC0}) {
		t.Fatalf("% x", enc)
	}
	var d SlipDecoder
	in := append([]byte("waiting for download\r\n"), enc...)
	in = append(in, 0xC0, 0x01, 0xDB, 0x00, 0xC0) // invalid escape: dropped
	in = append(in, SlipEncode([]byte("OHAI"))...)
	var frames [][]byte
	for _, b := range in { // byte by byte, as a slow line delivers it
		frames = append(frames, d.Feed([]byte{b})...)
	}
	if len(frames) != 2 || !bytes.Equal(frames[0], p) || string(frames[1]) != "OHAI" || d.Dropped != 1 {
		t.Fatalf("%q dropped=%d", frames, d.Dropped)
	}
	if d.Junk != len("waiting for download\r\n") || string(d.JunkText()) != "waiting for download\r\n" {
		t.Fatalf("junk %d", d.Junk)
	}
}

func TestChecksumAndRequestLayout(t *testing.T) {
	if Checksum(nil) != 0xEF || Checksum([]byte{0xEF}) != 0 || Checksum([]byte{1, 2, 3}) != 0xEF^1^2^3 {
		t.Fatal("checksum")
	}
	r := request(CmdReadReg, le32(0x40001000), 0)
	if hex.EncodeToString(r) != "000a04000000000000100040" {
		t.Fatalf("%x", r) // the READ_REG example of the Serial Protocol document
	}
}

func TestStatusErrors(t *testing.T) {
	e := &StatusError{Cmd: CmdFlashData, Status: 0xC1, Stub: true}
	if !strings.Contains(e.Error(), "bad data checksum") || e.Unsupported() {
		t.Fatal(e)
	}
	e = &StatusError{Cmd: CmdGetSecurityInfo, Status: 1, Code: 0x05}
	if !e.Unsupported() || !strings.Contains(e.Error(), "ROM error 0x05") {
		t.Fatal(e)
	}
	if !(&StatusError{Status: 0xFF, Stub: true}).Unsupported() || !(&StatusError{Status: 1, Code: 0xC3, Stub: true}).Unsupported() {
		t.Fatal("stub unsupported")
	}
}

func TestSyncRetriesThroughBootMessage(t *testing.T) {
	fastTiming(t)
	s := NewSim(ESP32, EntryClassic, 1<<20)
	s.PowerOn()
	s.IgnoreSyncs = 2
	if _, err := EnterBootloader(context.Background(), s.SetLines, EntryClassic, false); err != nil {
		t.Fatal(err)
	}
	c := New(s)
	r, err := c.Sync(5, 30*time.Millisecond)
	if err != nil || r.Stub || r.Attempts != 3 || c.statusLen != 4 {
		t.Fatalf("%+v %v", r, err)
	}
	if n, txt := c.Junk(); n == 0 || !strings.Contains(string(txt), "DOWNLOAD_BOOT") {
		t.Fatalf("boot message not seen: %d %q", n, txt)
	}
	// Everything after the first answer was swallowed: the next command
	// gets its own answer, not a stale SYNC.
	v, err := c.ReadReg(ChipDetectAddr)
	if err != nil || v != 0x00F01D83 {
		t.Fatalf("%x %v", v, err)
	}
}

func TestSyncGivesUp(t *testing.T) {
	fastTiming(t)
	s := NewSim(ESP32, EntryClassic, 1<<20)
	s.PowerOn() // application running: nobody answers
	c := New(s)
	_, err := c.Sync(3, 10*time.Millisecond)
	if !errors.Is(err, ErrTimeout) || c.Stats().SyncAttempts != 3 {
		t.Fatalf("%v %+v", err, c.Stats())
	}
}

func TestDetectAndFactsESP32(t *testing.T) {
	_, c := romClient(t, ESP32, 1<<20)
	d, err := c.Detect()
	if err != nil || d.Chip != ESP32 || d.Security != nil {
		t.Fatalf("%+v %v", d, err)
	}
	f, err := c.ReadFacts(d)
	if err != nil {
		t.Fatal(err)
	}
	if f.MACString() != "24:dc:c3:12:34:56" || f.Revision() != "v3.0" || f.Package != "ESP32-D0WDR2-V3" || *f.MACCRC != 0x5A {
		t.Fatalf("%+v", f)
	}
	if !strings.Contains(strings.Join(f.Features, ","), "embedded PSRAM") || f.FlashEncrypted || f.SecureBoot {
		t.Fatalf("%+v", f)
	}
}

func TestDetectByChipIDAndEUI64(t *testing.T) {
	_, c := romClient(t, ESP32H2, 1<<20)
	d, err := c.Detect()
	if err != nil || d.Chip != ESP32H2 || !strings.Contains(d.Basis, "chip id 16") {
		t.Fatalf("%+v %v", d, err)
	}
	f, err := c.ReadFacts(d)
	if err != nil || f.MACString() != "74:4d:bd:64:1e:ce" || f.EUI64() != "74:4d:bd:ff:fe:64:1e:ce" {
		t.Fatalf("%+v %v", f, err)
	}
	s, c := romClient(t, ESP32C6, 1<<20)
	s.Chip = &Chip{Name: "X", SecChipID: 99}
	s.Regs[ChipDetectAddr] = 1
	if _, err := c.Detect(); !errors.Is(err, ErrUnknownChip) {
		t.Fatal(err)
	}
}

func TestStubDigestsAndUpload(t *testing.T) {
	want := map[string]string{
		"esp32.json":   "a6ff782c69ef16f7459cb12de5c4ea55c97457957c8cc7aee669f91627b062b8",
		"esp32s3.json": "9a8ad53a62a9db1164342d48fba2810a75bf0d1069d5973d8d521082a5756129",
		"esp32c6.json": "12f5f0d9aef5bfaafb67f1ddac4fa0263e8a9023e0c8bff181d2167976ce2cf6",
		"esp32h2.json": "8b1af9dbf666d6f53ebc3a3ef554b7904f892b02537978b36e6724627970fbd4",
	}
	for f, sum := range want {
		b, err := espflasherstub.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if h := sha256.Sum256(b); hex.EncodeToString(h[:]) != sum {
			t.Errorf("%s: SHA-256 %x differs from the provenance record", f, h)
		}
	}
	for _, ch := range []*Chip{ESP32, ESP32S3, ESP32C6, ESP32H2} {
		s, c := stubClient(t, ch, 1<<20)
		if !c.Stub() || s.Mode() != "stub" || c.statusLen != 2 {
			t.Fatalf("%s: %v", ch.Name, s)
		}
		// A SYNC to a running stub answers with value 0.
		r, err := c.Sync(2, 50*time.Millisecond)
		if err != nil || !r.Stub {
			t.Fatalf("%+v %v", r, err)
		}
	}
	if _, err := StubFor("esp32c3"); err == nil {
		t.Fatal("no C3 stub is vendored")
	}
}

func TestStubWithoutGreeting(t *testing.T) {
	s, c := romClient(t, ESP32, 1<<20)
	s.NoOHAI = true
	st, _ := StubFor("esp32")
	if err := c.RunStub(st); err == nil || !strings.Contains(err.Error(), "OHAI") || c.Stub() {
		t.Fatal(err)
	}
}

func TestFlashIDThroughSPIRegisters(t *testing.T) {
	s, c := romClient(t, ESP32, 16<<20)
	before := map[uint32]uint32{}
	for k, v := range s.Regs {
		before[k] = v
	}
	if id, err := c.FlashID(ESP32); err != nil || id != 0xFFFFFF {
		t.Fatalf("unattached flash must read all ones: %x %v", id, err)
	}
	if err := c.SPIAttach(0); err != nil {
		t.Fatal(err)
	}
	id, err := c.FlashID(ESP32)
	if err != nil || id != s.JEDEC || FlashSize(id) != 16<<20 || JEDECString(id) != "c84018" || Manufacturer(id) != "GigaDevice" {
		t.Fatalf("%x %v", id, err)
	}
	r := ESP32.SPI
	for _, o := range []uint32{r.User, r.User1, r.User2, r.MOSILen, r.MISOLen} {
		if s.Regs[r.Base+o] != before[r.Base+o] {
			t.Errorf("SPI register +0x%x not restored", o)
		}
	}
}

func TestFlashSizeTable(t *testing.T) {
	for jedec, want := range map[uint32]int{0x1640C8: 4 << 20, 0x1840EF: 16 << 20, 0x3960C2: 32 << 20, 0x2020C2: 64 << 20, 0x0040C8: 0} {
		if got := FlashSize(jedec); got != want {
			t.Errorf("%06x: %d want %d", jedec, got, want)
		}
	}
}

func TestMD5ROMHexAndStubRaw(t *testing.T) {
	s, c := romClient(t, ESP32, 1<<20)
	if err := c.SPIAttach(0); err != nil {
		t.Fatal(err)
	}
	want := md5.Sum(s.Flash[0x100:0x2100]) //nolint:gosec
	got, err := c.FlashMD5(0x100, 0x2000)
	if err != nil || got != want {
		t.Fatalf("ROM: %x %v", got, err)
	}
	_, c = stubClient(t, ESP32, 1<<20)
	got, err = c.FlashMD5(0x100, 0x2000)
	if err != nil || got != want {
		t.Fatalf("stub: %x %v", got, err)
	}
}

func TestReadFlashAcksAndDigest(t *testing.T) {
	s, c := stubClient(t, ESP32, 1<<20)
	var buf bytes.Buffer
	sum, err := c.ReadFlash(0x1003, 10000, &buf)
	if err != nil || !bytes.Equal(buf.Bytes(), s.Flash[0x1003:0x1003+10000]) || sum != md5.Sum(buf.Bytes()) { //nolint:gosec
		t.Fatalf("%v", err)
	}
	// A corrupted packet is caught by the stub's digest.
	s.CorruptReadPacket = s.pktCount + 2
	buf.Reset()
	if _, err := c.ReadFlash(0, 3*ReadPacket, &buf); !errors.Is(err, ErrDigest) {
		t.Fatal(err)
	}
	// ROM loaders have no READ_FLASH.
	_, rc := romClient(t, ESP32, 1<<20)
	if _, err := rc.ReadFlash(0, 16, &buf); !errors.Is(err, ErrNoStub) {
		t.Fatal(err)
	}
}

func TestReadFlashLostPacketTimesOutAndRecovers(t *testing.T) {
	s, c := stubClient(t, ESP32, 1<<20)
	s.DropReadPacket = s.pktCount + 2
	var buf bytes.Buffer
	_, err := c.ReadFlash(0, 4*ReadPacket, &buf)
	if !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
	if err := c.AbortRead(); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if _, err := c.ReadFlash(0, 4*ReadPacket, &buf); err != nil || !bytes.Equal(buf.Bytes(), s.Flash[:4*ReadPacket]) {
		t.Fatalf("retry after abort: %v", err)
	}
}

func TestReadBeyondSetParamsIsReported(t *testing.T) {
	s, c := romClient(t, ESP32, 8<<20)
	st, _ := StubFor("esp32")
	if err := c.RunStub(st); err != nil {
		t.Fatal(err)
	}
	_ = c.SPIAttach(0)
	var buf bytes.Buffer
	// Without SPI_SET_PARAMS only the first 4 MiB are reachable: the stub
	// accepts the command, streams nothing and reports the error next time.
	if _, err := c.ReadFlash(6<<20, ReadPacket, &buf); !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
	_, err := c.FlashMD5(0, 16)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 0xC4 {
		t.Fatalf("%v", err)
	}
	if err := c.SPISetParams(uint32(len(s.Flash))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadFlash(6<<20, ReadPacket, &buf); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFlashChunksAndDeferredError(t *testing.T) {
	s, c := stubClient(t, ESP32, 1<<20)
	data := bytes.Repeat([]byte{0xA5, 0x5A}, 0x6000/2) // 24 KiB: two FLASH_DATA blocks
	if n, err := c.WriteFlash(0x10000, data, 3); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	sum, err := c.FlashMD5(0x10000, len(data))
	if err != nil || sum != md5.Sum(data) || s.Commands[CmdFlashData] != 2 || len(s.Erased) != 6 { //nolint:gosec
		t.Fatalf("%v data=%d erased=%x", err, s.Commands[CmdFlashData], s.Erased)
	}
	if _, err := c.WriteFlash(0x10001, data, 3); err == nil {
		t.Fatal("unaligned write accepted")
	}
	// A failing write is reported in the answer to the following command.
	s.FailWriteAt = 0x20000 + 0x5000
	if n, err := c.WriteFlash(0x20000, data, 3); err != nil || n != 0 {
		t.Fatalf("the failure must surface only on the next command: %v", err)
	}
	_, err = c.FlashMD5(0x20000, len(data))
	var se *StatusError
	if !errors.As(err, &se) || !strings.Contains(se.Error(), "SPI flash operation failed") {
		t.Fatalf("%v", err)
	}
}

func TestChangeBaud(t *testing.T) {
	s, c := stubClient(t, ESP32, 1<<20)
	if err := c.ChangeBaud(921600, 115200); err != nil {
		t.Fatal(err)
	}
	if s.Baud() != 921600 {
		t.Fatal(s)
	}
	// The host still at the old rate gets nothing usable.
	if _, err := c.ReadReg(ChipDetectAddr); err == nil {
		t.Fatal("rate mismatch must fail")
	}
	_ = s.SetBaud(921600)
	_ = c.Flush()
	if _, err := c.ReadReg(ChipDetectAddr); err != nil {
		t.Fatal(err)
	}
	// The ROM wants 0 as the old rate.
	rs, rc := romClient(t, ESP32, 1<<20)
	if err := rc.ChangeBaud(460800, 115200); err != nil || rs.Baud() != 460800 {
		t.Fatalf("%v %v", err, rs)
	}
}

func TestEraseRegionAndChip(t *testing.T) {
	s, c := stubClient(t, ESP32, 1<<20)
	if err := c.EraseRegion(0x3000, 0x2000); err != nil || !bytes.Equal(s.Flash[0x3000:0x5000], bytes.Repeat([]byte{0xFF}, 0x2000)) {
		t.Fatal(err)
	}
	if s.Flash[0x2FFF] == 0xFF && s.Flash[0x5000] == 0xFF {
		t.Fatal("erased outside the region")
	}
	if err := c.EraseRegion(0x3001, 0x1000); err == nil {
		t.Fatal("unaligned erase accepted")
	}
	if err := c.EraseFlash(); err != nil || !bytes.Equal(s.Flash, bytes.Repeat([]byte{0xFF}, len(s.Flash))) {
		t.Fatal(err)
	}
}

func TestClassicSequences(t *testing.T) {
	fastTiming(t)
	s := NewSim(ESP32, EntryClassic, 1<<20)
	s.PowerOn()
	atomic, err := EnterBootloader(context.Background(), s.SetLines, EntryClassic, false)
	if err != nil || !atomic || s.Mode() != "rom" {
		t.Fatalf("%v %v", err, s)
	}
	// EN low only with (DTR=0,RTS=1); the strap only with (DTR=1,RTS=0).
	want := [][2]bool{{false, false}, {false, true}, {true, false}, {false, false}}
	if len(s.LineLog) != 4 {
		t.Fatalf("%v", s.LineLog)
	}
	for i := range want {
		if s.LineLog[i] != want[i] {
			t.Fatalf("%v", s.LineLog)
		}
	}
	if _, err := HardReset(context.Background(), s.SetLines, EntryClassic); err != nil || s.Mode() != "app" {
		t.Fatalf("%v %v", err, s)
	}
	// A board with a slow EN needs the long strap hold.
	s.StrapNeed = 15 * time.Millisecond
	_, _ = EnterBootloader(context.Background(), s.SetLines, EntryClassic, false)
	if s.Mode() != "app" {
		t.Fatalf("short hold must miss the strap: %v", s)
	}
	_, _ = EnterBootloader(context.Background(), s.SetLines, EntryClassic, true)
	if s.Mode() != "rom" {
		t.Fatalf("long hold: %v", s)
	}
	if _, err := HardReset(context.Background(), s.SetLines, EntryNone); err == nil {
		t.Fatal("no lines, no reset")
	}
}

func TestUSJSequences(t *testing.T) {
	fastTiming(t)
	s := NewSim(ESP32S3, EntryUSJ, 1<<20)
	s.PowerOn()
	if _, err := EnterBootloader(context.Background(), s.SetLines, EntryUSJ, false); err != nil || s.Mode() != "rom" {
		t.Fatalf("%v %v", err, s)
	}
	// TRM 33.4-3: never (0,0) between setting the flag and the reset.
	log := s.LineLog
	if log[1] != [2]bool{true, false} || log[2] != [2]bool{true, true} || log[3] != [2]bool{false, true} {
		t.Fatalf("%v", log)
	}
	n := len(s.LineLog)
	if _, err := HardReset(context.Background(), s.SetLines, EntryUSJ); err != nil || s.Mode() != "app" {
		t.Fatalf("%v %v", err, s)
	}
	if got := s.LineLog[n:]; len(got) != 3 || got[0] != [2]bool{false, false} || got[1] != [2]bool{false, true} || got[2] != [2]bool{false, false} {
		t.Fatalf("TRM 33.4-4 (clear flag, reset, release): %v", got)
	}
	// The classic sequence on a USJ port resets into the application: the
	// download flag is cleared at (0,0) right before the reset.
	_, _ = EnterBootloader(context.Background(), s.SetLines, EntryClassic, false)
	if s.Mode() != "app" {
		t.Fatalf("%v", s)
	}
}

func TestContextCancelsWait(t *testing.T) {
	s, c := romClient(t, ESP32, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.SetContext(ctx)
	s.IgnoreSyncs = 10
	if _, err := c.Sync(3, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// A lost FLASH_DATA (either direction) or a corrupted one restarts the
// download at that block with a new FLASH_BEGIN; nothing lands shifted.
func TestWriteFlashRestartsAtTheFailedBlock(t *testing.T) {
	data := make([]byte, 3*StubFlashBlock+SectorSize) // 4 blocks
	for i := range data {
		data[i] = byte(i*31 + i>>9)
	}
	for _, fault := range []string{"request lost", "answer lost", "corrupted"} {
		s, c := stubClient(t, ESP32, 1<<20)
		c.Timeout, c.Baud = 50*time.Millisecond, 1<<30
		n := s.Commands[CmdFlashData] + 2 // the second block of this download
		switch fault {
		case "request lost":
			s.DropFlashData = n
		case "answer lost":
			s.DropFlashDataAnswer = n
		case "corrupted":
			s.CorruptFlashData = n
		}
		before := s.Commands[CmdFlashBegin]
		retries, err := c.WriteFlash(0x40000, data, 3)
		if err != nil || retries != 1 || s.Commands[CmdFlashBegin]-before != 2 {
			t.Fatalf("%s: retries=%d begins=%d err=%v", fault, retries, s.Commands[CmdFlashBegin]-before, err)
		}
		if !bytes.Equal(s.Flash[0x40000:0x40000+len(data)], data) {
			t.Fatalf("%s: flash differs after the restart", fault)
		}
		if sum, err := c.FlashMD5(0x40000, len(data)); err != nil || sum != md5.Sum(data) { //nolint:gosec
			t.Fatalf("%s: %v", fault, err)
		}
	}
	// Out of attempts: the timeout is returned, not hidden.
	s, c := stubClient(t, ESP32, 1<<20)
	c.Timeout, c.Baud = 50*time.Millisecond, 1<<30
	s.DropFlashData = s.Commands[CmdFlashData] + 1
	if _, err := c.WriteFlash(0x40000, data, 0); !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
}

func TestBlockTimeoutCoversEraseAndWire(t *testing.T) {
	c := New(nil)
	c.Timeout, c.Baud = 3*time.Second, 460800
	// 16 KiB at 460800 is 0.36 s on the wire; the bound leaves 3 s for the
	// previous block's erase and program plus three wire times.
	if got := c.blockTimeout(); got < 7*time.Second || got > 8*time.Second {
		t.Fatal(got)
	}
	c.Baud = 0
	if got := c.blockTimeout(); got < 10*time.Second {
		t.Fatalf("115200: %v", got)
	}
}
