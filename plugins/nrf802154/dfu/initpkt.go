package dfu

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

// Init packet: the nRF5 SDK dfu-cc.proto message, unsigned (the Open
// bootloader accepts unsigned packets), byte-identical to what
// "nrfutil nrf5sdk-tools pkg generate --hw-version 52 --sd-req 0x00
// --application-version N --application X.hex" writes:
//
//	Packet { command = 1: Command { op_code = 1: INIT(1),
//	  init = 2: InitCommand {
//	    fw_version = 1, hw_version = 2, sd_req = 3 (packed) [0x00],
//	    type = 4 APPLICATION(0), sd_size = 5, bl_size = 6, app_size = 7,
//	    hash = 8: Hash { hash_type = 1 SHA256(3), hash = 2 (digest, byte-reversed) },
//	    is_debug = 9 false,
//	    boot_validation = 10: BootValidation { type = 1 VALIDATE_GENERATED_CRC(1), bytes = 2 "" } } } }

func pbTag(b []byte, field, wire int) []byte { return binary.AppendUvarint(b, uint64(field<<3|wire)) }

func pbUint(b []byte, field int, v uint64) []byte {
	return binary.AppendUvarint(pbTag(b, field, 0), v)
}

func pbBytes(b []byte, field int, v []byte) []byte {
	return append(binary.AppendUvarint(pbTag(b, field, 2), uint64(len(v))), v...)
}

// digest is the SHA-256 of bin in the byte order nrfutil stores (reversed).
func digest(bin []byte) []byte {
	h := sha256.Sum256(bin)
	d := h[:]
	slices.Reverse(d)
	return d
}

// InitPacket returns the unsigned init packet for an application image.
func InitPacket(bin []byte, appVersion uint32) []byte {
	var hash, bv, init, cmd []byte
	hash = pbUint(hash, 1, 3) // SHA256
	hash = pbBytes(hash, 2, digest(bin))
	bv = pbUint(bv, 1, 1) // VALIDATE_GENERATED_CRC
	bv = pbBytes(bv, 2, nil)

	init = pbUint(init, 1, uint64(appVersion))
	init = pbUint(init, 2, HWVersion)
	init = pbBytes(init, 3, []byte{0x00}) // sd_req [0x00]: no SoftDevice
	init = pbUint(init, 4, 0)             // APPLICATION
	init = pbUint(init, 5, 0)
	init = pbUint(init, 6, 0)
	init = pbUint(init, 7, uint64(len(bin)))
	init = pbBytes(init, 8, hash)
	init = pbUint(init, 9, 0)
	init = pbBytes(init, 10, bv)

	cmd = pbUint(cmd, 1, 1) // INIT
	cmd = pbBytes(cmd, 2, init)
	return pbBytes(nil, 1, cmd)
}

// CheckInit checks that a package's init packet is an application init
// command (unsigned or signed) whose size and SHA-256 match bin.
func CheckInit(dat, bin []byte) error {
	init, err := initCommand(dat)
	if err != nil {
		return err
	}
	if v, ok := init[4]; ok {
		if t, _ := binary.Uvarint(v); t != 0 {
			return fmt.Errorf("init packet is for firmware type %d, not an application", t)
		}
	}
	if n, _ := binary.Uvarint(init[7]); n != uint64(len(bin)) {
		return fmt.Errorf("init packet app_size %d does not match the %d-byte image", n, len(bin))
	}
	hf, err := pbParse(init[8])
	if err != nil {
		return fmt.Errorf("init packet hash: %w", err)
	}
	if !bytes.Equal(hf[2], digest(bin)) {
		return errors.New("init packet SHA-256 does not match the image")
	}
	return nil
}

// pbParse decodes one protobuf message level into field -> raw value (the
// varint bytes for wire type 0, the payload for wire type 2). Later
// occurrences of a field win. Only wire types 0 and 2 are accepted.
func pbParse(b []byte) (map[int][]byte, error) {
	out := map[int][]byte{}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("bad tag")
		}
		b = b[n:]
		field, wire := int(tag>>3), int(tag&7)
		switch wire {
		case 0:
			_, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("bad varint")
			}
			out[field] = b[:n]
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errors.New("bad length")
			}
			out[field] = b[n : n+int(l)]
			b = b[n+int(l):]
		default:
			return nil, fmt.Errorf("unsupported wire type %d", wire)
		}
	}
	return out, nil
}

// InitAppSize returns app_size from an unsigned or signed init packet.
func InitAppSize(dat []byte) (int, error) {
	init, err := initCommand(dat)
	if err != nil {
		return 0, err
	}
	n, k := binary.Uvarint(init[7])
	if k <= 0 {
		return 0, errors.New("init packet: no app_size")
	}
	return int(n), nil
}

// initCommand returns the fields of the InitCommand inside an init packet.
func initCommand(dat []byte) (map[int][]byte, error) {
	fields, err := pbParse(dat)
	if err != nil {
		return nil, fmt.Errorf("init packet: %w", err)
	}
	cmd := fields[1]
	if signed, ok := fields[2]; ok {
		// SignedCommand { command = 1, signature_type = 2, signature = 3 }
		sf, err := pbParse(signed)
		if err != nil {
			return nil, fmt.Errorf("init packet: %w", err)
		}
		cmd = sf[1]
	}
	cf, err := pbParse(cmd)
	if err != nil {
		return nil, fmt.Errorf("init packet: %w", err)
	}
	init, err := pbParse(cf[2])
	if err != nil {
		return nil, fmt.Errorf("init packet: %w", err)
	}
	return init, nil
}
