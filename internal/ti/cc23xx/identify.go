package cc23xx

import (
	"context"
	"fmt"

	"github.com/faronov/hwlab/internal/ti/cc23xx/ccfg"
)

// Memory-mapped identification registers (SWCU193 SYS0 and VIMS chapters).
const (
	addrSys0DeviceID = 0x40004000 + 0x3FC
	addrSys0PartID   = 0x40004000 + 0x7F8
	addrVimsDescEx   = 0x40024004

	FlashBase = 0x00000000
	SRAMBase  = 0x20000000
)

// Identity describes the attached device using documented registers only. The
// family manual has no part-number table, so no marketing name is derived.
type Identity struct {
	DeviceID      uint32 `json:"device_id"`
	DeviceIDField uint16 `json:"device_id_device"`
	Manufacturer  uint16 `json:"manufacturer"`
	PartID        uint32 `json:"part_id_raw"`
	CfgAPDeviceID uint32 `json:"cfg_ap_device_id"`
	FlashBytes    uint32 `json:"flash_bytes"`
	FlashBanks    int    `json:"flash_banks"`
	SRAMLevel     uint8  `json:"sram_level_raw,omitempty"`

	CCFG    *ccfg.Info `json:"ccfg,omitempty"`
	CCFGErr string     `json:"ccfg_error,omitempty"`

	Boot BootStatus `json:"boot"`
	SACI bool       `json:"in_saci"`
}

// Identify reads the identity. In SACI only the CFG-AP is reachable, so the
// memory-mapped fields are filled when the AHB-AP is open.
func (s *Session) Identify(ctx context.Context) (*Identity, error) {
	ok, err := s.IsCC23xx(ctx)
	if err != nil {
		return nil, &Error{Code: CodeSwdLost, Msg: "reading access port identification", Err: err}
	}
	if !ok {
		return nil, &Error{Code: CodeNotCC23xx, Msg: "CFG-AP/SEC-AP identification does not match a CC23xx"}
	}
	id := &Identity{}
	if id.Boot, err = s.ReadBootStatus(ctx); err != nil {
		return nil, &Error{Code: CodeSwdLost, Msg: "reading boot status", Err: err}
	}
	id.SACI = id.Boot.SACI
	if v, err := s.T.ReadAPOn(APCfg, cfgDeviceID); err == nil {
		id.CfgAPDeviceID = v
	}
	if !id.Boot.AHB {
		return id, nil
	}
	rd := func(a uint32) uint32 {
		v, e := s.T.ReadWord(a)
		if e != nil && err == nil {
			err = e
		}
		return v
	}
	id.DeviceID = rd(addrSys0DeviceID)
	id.DeviceIDField = uint16(id.DeviceID >> 12 & 0xFFFF)
	id.Manufacturer = uint16(id.DeviceID >> 1 & 0x7FF)
	id.PartID = rd(addrSys0PartID)
	desc := rd(addrVimsDescEx)
	if err != nil {
		return nil, &Error{Code: CodeSwdLost, Msg: "reading identification registers", Err: err}
	}
	id.FlashBytes = ((desc >> 15 & 0xFFF) + 1) * 1024
	id.FlashBanks = 1
	if desc>>27&1 == 1 {
		id.FlashBanks = 2
	}
	b, err := s.T.ReadMem(ccfg.BaseAddr, ccfg.Size)
	if err != nil {
		id.CCFGErr = err.Error()
		return id, nil
	}
	if id.CCFG, err = ccfg.Parse(b); err != nil {
		id.CCFGErr = err.Error()
	}
	return id, nil
}

func (id *Identity) String() string {
	return fmt.Sprintf("device 0x%04X manufacturer 0x%03X flash %d KiB", id.DeviceIDField, id.Manufacturer, id.FlashBytes/1024)
}
