// Package ccfg parses the CC23xx customer configuration (CCFG) sector as
// documented in SWCU193 chapter 9.2: boot configuration, permissions, debug
// configuration, the four CRC32 values and the user record presence.
//
// Only documented, hardware-confirmed fields are decoded. Everything else in
// the central section (hwOpts, misc, flashProt, hwInitCopyList) is treated as
// opaque, and any change in it counts as security relevant. Password hashes,
// password ids and the user record are never copied into results.
package ccfg

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Layout of the 2 KiB CCFG sector.
const (
	Size = 0x800

	offBootCfg    = 0x00
	offBootCRC    = 0x0C
	offCentral    = 0x10
	offPerms      = 0x18
	offCentralCRC = 0x74C
	offUserRec    = 0x750
	offUserCRC    = 0x7CC
	offDebugCfg   = 0x7D0
	offDebugCRC   = 0x7FC

	// BaseAddr is where the CCFG sector is mapped in the address space.
	BaseAddr = 0x4E020000
)

// Permission is one 4-bit CCFG permission field.
type Permission uint8

// PermAllow is the CCFG_PERMISSION_ALLOW value.
const PermAllow Permission = 0xA

func (p Permission) String() string {
	if p == PermAllow {
		return "ALLOW"
	}
	return fmt.Sprintf("FORBID(0x%X)", uint8(p))
}

// Allowed reports whether the permission is ALLOW.
func (p Permission) Allowed() bool { return p == PermAllow }

// Debug authorization values.
const (
	AuthDebugOpen   = 0x5A
	AuthPasswordReq = 0xA5
)

// Permissions decodes CCFG.permissions (word at 0x18).
type Permissions struct {
	Raw                  uint32     `json:"raw"`
	AllowReturnToFactory Permission `json:"allow_return_to_factory"`
	AllowFakeStandby     Permission `json:"allow_fake_standby"`
	AllowToolsClientMode Permission `json:"allow_tools_client_mode"`
	AllowChipErase       Permission `json:"allow_chip_erase"`
	AllowFlashProgram    Permission `json:"allow_flash_program"`
	AllowFlashVerify     Permission `json:"allow_flash_verify"`
	AllowEnergyTrace     Permission `json:"allow_energytrace"`
	AllowDebugPort       Permission `json:"allow_debug_port"`
}

func decodePerms(v uint32) Permissions {
	n := func(shift uint) Permission { return Permission(v >> shift & 0xF) }
	return Permissions{Raw: v, AllowReturnToFactory: n(0), AllowFakeStandby: n(4), AllowToolsClientMode: n(8),
		AllowChipErase: n(12), AllowFlashProgram: n(16), AllowFlashVerify: n(20), AllowEnergyTrace: n(24), AllowDebugPort: n(28)}
}

// CRC is a stored/calculated CRC32 pair for one section.
type CRC struct {
	Stored     uint32 `json:"stored"`
	Calculated uint32 `json:"calculated"`
	OK         bool   `json:"ok"`
}

func crc(b []byte, from, to, at int) CRC {
	st := binary.LittleEndian.Uint32(b[at:])
	c := crc32.ChecksumIEEE(b[from:to])
	return CRC{Stored: st, Calculated: c, OK: st == c}
}

// BootCfg is CCFG.bootCfg.
type BootCfg struct {
	BldrVtor  uint32 `json:"p_bldr_vtor"`
	BldrParam uint32 `json:"bldr_param"`
	AppVtor   uint32 `json:"p_app_vtor"`
	CRC       CRC    `json:"crc32"`
}

// DebugCfg is CCFG.debugCfg without any password material.
type DebugCfg struct {
	Authorization     uint8  `json:"authorization"`
	AuthorizationName string `json:"authorization_name"`
	AllowBootloader   uint8  `json:"allow_bldr_raw"`
	PasswordIDSet     bool   `json:"password_id_programmed"`
	PasswordHashSet   bool   `json:"password_hash_programmed"`
	CRC               CRC    `json:"crc32"`
}

// Info is the decoded CCFG. It holds no secret material.
type Info struct {
	Blank         bool        `json:"blank"`
	BootCfg       BootCfg     `json:"boot_cfg"`
	Permissions   Permissions `json:"permissions"`
	CentralCRC    CRC         `json:"central_crc32"`
	UserRecordSet bool        `json:"user_record_programmed"`
	UserRecordCRC CRC         `json:"user_record_crc32"`
	Debug         DebugCfg    `json:"debug_cfg"`
	// Valid is true when the boot CRC, the central CRC and the debug CRC
	// all match.
	Valid bool `json:"valid"`
}

func authName(a uint8) string {
	switch a {
	case AuthDebugOpen:
		return "DEBUG_OPEN"
	case AuthPasswordReq:
		return "PASSWORD_REQUIRED"
	}
	return "DEBUG_FORBIDDEN"
}

func allFF(b []byte) bool {
	for _, x := range b {
		if x != 0xFF {
			return false
		}
	}
	return true
}

func notFF(b []byte) bool { return !allFF(b) }

// Parse decodes a full CCFG sector image.
func Parse(b []byte) (*Info, error) {
	if len(b) != Size {
		return nil, fmt.Errorf("a CCFG sector is %d bytes, got %d", Size, len(b))
	}
	i := &Info{Blank: allFF(b)}
	i.BootCfg = BootCfg{
		BldrVtor:  binary.LittleEndian.Uint32(b[offBootCfg:]),
		BldrParam: binary.LittleEndian.Uint32(b[offBootCfg+4:]),
		AppVtor:   binary.LittleEndian.Uint32(b[offBootCfg+8:]),
		CRC:       crc(b, offBootCfg, offBootCRC, offBootCRC),
	}
	i.Permissions = decodePerms(binary.LittleEndian.Uint32(b[offPerms:]))
	i.CentralCRC = crc(b, offCentral, offCentralCRC, offCentralCRC)
	i.UserRecordSet = notFF(b[offUserRec:offUserCRC])
	i.UserRecordCRC = crc(b, offUserRec, offUserCRC, offUserCRC)
	d := b[offDebugCfg:]
	i.Debug = DebugCfg{
		Authorization: d[0], AuthorizationName: authName(d[0]), AllowBootloader: d[1],
		PasswordIDSet:   notFF(d[4:12]),
		PasswordHashSet: notFF(d[12:44]),
		CRC:             crc(b, offDebugCfg, offDebugCRC, offDebugCRC),
	}
	i.Valid = !i.Blank && i.BootCfg.CRC.OK && i.CentralCRC.OK && i.Debug.CRC.OK
	return i, nil
}

// CRCs returns the four section CRC32 values of a sector image in the order
// VERIFY_CCFG_SECTOR takes them (bootCfg, central, user record, debugCfg).
func CRCs(b []byte) (boot, central, user, debug uint32, err error) {
	if len(b) != Size {
		return 0, 0, 0, 0, fmt.Errorf("a CCFG sector is %d bytes, got %d", Size, len(b))
	}
	return crc32.ChecksumIEEE(b[offBootCfg:offBootCRC]), crc32.ChecksumIEEE(b[offCentral:offCentralCRC]),
		crc32.ChecksumIEEE(b[offUserRec:offUserCRC]), crc32.ChecksumIEEE(b[offDebugCfg:offDebugCRC]), nil
}

// Change is one difference between two CCFG images.
type Change struct {
	Field    string `json:"field"`
	Old      string `json:"old"`
	New      string `json:"new"`
	Security bool   `json:"security_relevant"`
}

// Diff lists documented fields that differ between old and new, and flags
// the ones that change who can debug, erase or program the device. A change
// in the undocumented parts of the central section is reported and treated as
// security relevant.
func Diff(oldB, newB []byte) ([]Change, error) {
	o, err := Parse(oldB)
	if err != nil {
		return nil, err
	}
	n, err := Parse(newB)
	if err != nil {
		return nil, err
	}
	var out []Change
	add := func(f, a, b string, sec bool) {
		if a != b {
			out = append(out, Change{Field: f, Old: a, New: b, Security: sec})
		}
	}
	hx := func(v uint32) string { return fmt.Sprintf("0x%08X", v) }
	add("bootCfg.pBldrVtor", hx(o.BootCfg.BldrVtor), hx(n.BootCfg.BldrVtor), true)
	add("bootCfg.bldrParam", hx(o.BootCfg.BldrParam), hx(n.BootCfg.BldrParam), true)
	add("bootCfg.pAppVtor", hx(o.BootCfg.AppVtor), hx(n.BootCfg.AppVtor), true)
	op, np := o.Permissions, n.Permissions
	for _, p := range []struct {
		name string
		a, b Permission
	}{
		{"permissions.allowReturnToFactory", op.AllowReturnToFactory, np.AllowReturnToFactory},
		{"permissions.allowFakeStandby", op.AllowFakeStandby, np.AllowFakeStandby},
		{"permissions.allowToolsClientMode", op.AllowToolsClientMode, np.AllowToolsClientMode},
		{"permissions.allowChipErase", op.AllowChipErase, np.AllowChipErase},
		{"permissions.allowFlashProgram", op.AllowFlashProgram, np.AllowFlashProgram},
		{"permissions.allowFlashVerify", op.AllowFlashVerify, np.AllowFlashVerify},
		{"permissions.allowEnergyTrace", op.AllowEnergyTrace, np.AllowEnergyTrace},
		{"permissions.allowDebugPort", op.AllowDebugPort, np.AllowDebugPort},
	} {
		add(p.name, p.a.String(), p.b.String(), true)
	}
	add("debugCfg.authorization", o.Debug.AuthorizationName, n.Debug.AuthorizationName, true)
	add("debugCfg.allowBldr", fmt.Sprint(o.Debug.AllowBootloader), fmt.Sprint(n.Debug.AllowBootloader), true)
	add("debugCfg.pwdId", fmt.Sprint(o.Debug.PasswordIDSet), fmt.Sprint(n.Debug.PasswordIDSet), true)
	if string(oldB[offDebugCfg+4:offDebugCRC]) != string(newB[offDebugCfg+4:offDebugCRC]) {
		out = append(out, Change{Field: "debugCfg.pwdId/pwdHash", Old: "(not shown)", New: "(not shown)", Security: true})
	}
	add("userRecord.programmed", fmt.Sprint(o.UserRecordSet), fmt.Sprint(n.UserRecordSet), false)
	if string(oldB[offUserRec:offUserCRC]) != string(newB[offUserRec:offUserCRC]) {
		out = append(out, Change{Field: "userRecord.content", Old: "(not shown)", New: "(not shown)"})
	}
	// Undocumented central bytes: everything but permissions.
	ob, nb := oldB[offCentral:offCentralCRC], newB[offCentral:offCentralCRC]
	diff := 0
	for k := range ob {
		if off := offCentral + k; off >= offPerms && off < offPerms+4 {
			continue
		}
		if ob[k] != nb[k] {
			diff++
		}
	}
	if diff > 0 {
		out = append(out, Change{Field: "central(hwOpts/misc/flashProt/hwInitCopyList)", Old: "(opaque)", New: fmt.Sprintf("%d byte(s) differ", diff), Security: true})
	}
	return out, nil
}

// Security reports whether any change is security relevant.
func Security(ch []Change) bool {
	for _, c := range ch {
		if c.Security {
			return true
		}
	}
	return false
}

// Finalize recomputes the four CRC32 fields of a CCFG image in place. The
// user record CRC is only written when the record is programmed.
func Finalize(b []byte) error {
	if len(b) != Size {
		return fmt.Errorf("a CCFG sector is %d bytes, got %d", Size, len(b))
	}
	boot, central, user, debug, _ := CRCs(b)
	binary.LittleEndian.PutUint32(b[offBootCRC:], boot)
	binary.LittleEndian.PutUint32(b[offCentralCRC:], central)
	if notFF(b[offUserRec:offUserCRC]) {
		binary.LittleEndian.PutUint32(b[offUserCRC:], user)
	}
	binary.LittleEndian.PutUint32(b[offDebugCRC:], debug)
	return nil
}
