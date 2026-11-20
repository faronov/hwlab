package xds110plugin

import (
	"encoding/json"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// cc26Spec is the CC13xx/CC26xx (interface=cjtag) side of a FlashProgramming
// method the CC23xx service also declares.
type cc26Spec struct {
	desc  string
	props string
}

const (
	backupProp26 = `"backup":{"type":"string","description":"a FlashProgramming.backup .bin of this chip (required to modify)"}`
	ccfgProps26  = `"confirm_config":{"type":"string","enum":["` + tokCCFG + `"],"description":"CC13xx/CC26xx: needed when the CCFG sector (last flash sector) changes, with allow_ccfg_write"},` +
		`"ccfg_override":{"type":"string","enum":["` + tokLockout + `"],"description":"DANGEROUS: accept a CCFG that disables the bootloader/backdoor, locks debug or is blank"},` +
		`"reset_after":{"type":"boolean","description":"CC13xx/CC26xx: reset into the new image afterwards (default true); false leaves the core halted"},` +
		`"dry_run":{"type":"boolean"},"data":{"type":"string","description":"hex image (max 4096 bytes) instead of path"},"mode":{"type":"string","enum":["region","bank"]},"length":{"type":"number"}`
)

var cc26Specs = map[string]cc26Spec{
	"info":      {desc: "identity, flash geometry, CCFG location and summary, the ROM flash API entry points and what modification needs; reads only"},
	"ccfg_info": {desc: "decode the CCFG: bootloader/backdoor, image valid, TAP/DAP enables, TI FA, erase enables, write protection, IEEE override; reads only"},
	"read":      {desc: "read flash through the MEM-AP; address defaults to 0 and length to the whole flash; max 4096 bytes inline, more to a new file with path", props: `"overwrite":{"type":"boolean"}`},
	"verify":    {desc: "compare flash with a file (path) or hex data at address (default 0) by reading it back; never writes", props: `"data":{"type":"string"}`},
	"backup":    {desc: "read the whole flash twice through the MEM-AP without halting the core, require identical SHA-256 and store it 0600 with a manifest bound to ICEPICK_DEVICE_ID, USER_ID, the factory IEEE and the flash size (default dir $HWLAB_HOME/artifacts/cc26xx-flash); never writes"},
	"erase":     {desc: "mode=region (sector-aligned address+length, confirm=\"" + tokErase + "\") or mode=bank (every sector, confirm=\"" + tokBankErase + "\"; the CCFG gets TI's default security block so debug and the bootloader stay open) through the ROM flash API; needs allow_erase, backup=, and for the CCFG sector allow_ccfg_write + confirm_config. Resets the target afterwards unless reset_after=false", props: ccfgProps26},
	"program":   {desc: "program an image (path .bin or hex data) at address (default 0) through the ROM flash API: only changed sectors are erased and rewritten, bytes outside the image are kept, every sector and the whole range are read back. CCFG sector last, only with allow_ccfg_write + confirm_config, and refused if the new CCFG locks the chip out unless ccfg_override. Needs allow_flash+allow_erase, confirm=\"" + tokProgram + "\" and backup=; dry_run=true plans only", props: ccfgProps26},
	"restore":   {desc: "rewrite every sector that differs from a backup of this chip (backup or path), CCFG sector last, then require the whole flash to equal it. Needs allow_flash+allow_erase and confirm=\"" + tokRestore + "\". Never restore onto a device that joined a Zigbee network since", props: ccfgProps26},
}

// mergeCC26Specs extends the CC23xx FlashProgramming specs with the
// CC13xx/CC26xx behaviour behind interface=cjtag: the descriptions are joined,
// the properties merged and only the arguments both paths require stay
// required.
func mergeCC26Specs(in []*pluginv1.MethodSpec) []*pluginv1.MethodSpec {
	for _, m := range in {
		cs, ok := cc26Specs[m.Name]
		if !ok || m.Capability != pluginv1.CapFlashProgramming {
			continue
		}
		m.Description = "SWD/CC23xx: " + m.Description + ". cJTAG/CC13x2-CC26x2 (interface=cjtag): " + cs.desc
		props := `"path":{"type":"string"},"address":{},"confirm":{"type":"string"},"dir":{"type":"string"},` + backupProp26
		if cs.props != "" {
			props += "," + cs.props
		}
		m.ArgsSchemaJson = mergeSchema(m.ArgsSchemaJson, props, cc26Required[m.Name])
	}
	return in
}

// cc26Required lists what the CC13xx/CC26xx side requires.
var cc26Required = map[string][]string{"erase": {"confirm", "backup"}, "program": {"confirm", "backup"}, "restore": {"confirm"}}

func mergeSchema(schema, props string, required []string) string {
	var s map[string]any
	if json.Unmarshal([]byte(schema), &s) != nil {
		return schema
	}
	var extra map[string]any
	if json.Unmarshal([]byte("{"+props+"}"), &extra) != nil {
		return schema
	}
	p, _ := s["properties"].(map[string]any)
	if p == nil {
		p = map[string]any{}
	}
	for k, v := range extra {
		if _, ok := p[k]; !ok {
			p[k] = v
		}
	}
	s["properties"] = p
	if req, ok := s["required"].([]any); ok {
		keep := []any{}
		for _, r := range req {
			for _, w := range required {
				if r == w {
					keep = append(keep, r)
				}
			}
		}
		if len(keep) > 0 {
			s["required"] = keep
		} else {
			delete(s, "required")
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return schema
	}
	return string(b)
}
