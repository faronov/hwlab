package pcap

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// Recorded timing/radio metadata comes from a real 10 s capture. MAC frames
// are synthetic so the fixture contains no original network identities.
func TestReadRealCaptureFixture(t *testing.T) {
	f, err := os.Open("../../testdata/real/nrf802154-ch15-10s.pcapng")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ng, err := ReadNG(f)
	if err != nil {
		t.Fatal(err)
	}
	if ng.LinkType != LinkIEEE802154TAP {
		t.Errorf("link type = %d", ng.LinkType)
	}
	if len(ng.Packets) != 335 {
		t.Fatalf("packets = %d, want 335", len(ng.Packets))
	}
	for i := 1; i < len(ng.Packets); i++ {
		if ng.Packets[i].TsNs < ng.Packets[i-1].TsNs {
			t.Fatalf("timestamps not monotone at %d", i)
		}
	}

	if !strings.Contains(ng.Comment, "clock.") {
		t.Errorf("ISB comment lacks clock metadata: %q", ng.Comment)
	}
}

func TestRadioFixturesContainOnlySyntheticMACFrames(t *testing.T) {
	for _, path := range []string{
		"../../testdata/real/nrf802154-ch15-10s.pcapng",
		"../../testdata/real-session/2026-10-06T183751Z-zigbee-power/artifacts/radio/nrf802154-ch15.pcapng",
	} {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(raw, []byte("NRFFIXTURE000001")) {
				t.Fatal("radio interface must use the documented fictional identifier")
			}
			capture, err := ReadNG(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			for i, packet := range capture.Packets {
				frame := packet.Data
				if capture.LinkType == LinkIEEE802154TAP {
					if len(frame) < 4 {
						t.Fatalf("packet %d: short TAP header", i)
					}
					header := int(binary.LittleEndian.Uint16(frame[2:4]))
					if header < 4 || header > len(frame) {
						t.Fatalf("packet %d: invalid TAP size %d", i, header)
					}
					frame = frame[header:]
				}
				expected := []byte{0x02, 0x00, 0x01}
				if len(frame) >= 9 {
					expected = []byte{0x41, 0x88, 0x01, 0xfe, 0xca, 0x01, 0x00, 0x02, 0x00}
				}
				if len(frame) < len(expected) || !bytes.Equal(frame[:len(expected)], expected) ||
					!bytes.Equal(frame[len(expected):], make([]byte, len(frame)-len(expected))) {
					t.Fatalf("packet %d: non-synthetic MAC bytes in privacy-safe fixture", i)
				}
			}
		})
	}
}
