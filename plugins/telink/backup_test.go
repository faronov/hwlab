package telinkplugin

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/telink/sws"
)

// haltAndBackup captures the simulated CPU and takes a full backup.
func haltAndBackup(t *testing.T, p *Plugin) string {
	t.Helper()
	must(t, p, D, "halt", nil)
	r := must(t, p, F, "backup", map[string]any{"dir": t.TempDir()})
	return r["path"].(string)
}

func blankFactory(sim *sws.Sim) {
	for i := 0x76000; i < 0x78000; i++ {
		sim.Flash()[i] = 0xff
	}
}

func TestBackupNeedsHaltAndIsBoundToBoard(t *testing.T) {
	p, sim := armed(t)
	dir := t.TempDir()
	if _, err := inv(t, p, F, "backup", map[string]any{"dir": dir}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("backup with running CPU: %v", err)
	}
	must(t, p, D, "halt", nil)
	r := must(t, p, F, "backup", map[string]any{"dir": dir})
	path := r["path"].(string)
	if r["identical_reads"] != true || r["size_bytes"].(float64) != 0x80000 || r["flash_uid"] != "53594e5448455449" || r["already_present"] != false {
		t.Fatalf("%v", r)
	}
	b, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(b, sim.Flash()) {
		t.Fatalf("backup content: %v", err)
	}
	for _, f := range []string{path, path + ".json"} {
		if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, st.Mode(), err)
		}
	}
	if !strings.Contains(path, "telink-flash-53594e5448455449-") {
		t.Fatal(path)
	}
	if r2 := must(t, p, F, "backup", map[string]any{"dir": dir}); r2["already_present"] != true || r2["path"] != path {
		t.Fatalf("%v", r2)
	}
	if len(sim.FlashOps) != 0 {
		t.Fatalf("backup modified flash: %+v", sim.FlashOps)
	}
}

func TestModifyRequiresMatchingBackup(t *testing.T) {
	p, sim := armed(t)
	data := map[string]any{"address": "0x1000", "data": "0102"}
	must(t, p, D, "halt", nil)
	if _, err := inv(t, p, F, "program", withTB04(data)); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("no backup: %v", err)
	}
	bk := must(t, p, F, "backup", map[string]any{"dir": t.TempDir()})["path"].(string)
	with := func(path string) map[string]any {
		return withTB04(map[string]any{"address": "0x1000", "data": "0102", "backup": path})
	}

	t.Run("running CPU", func(t *testing.T) {
		must(t, p, D, "resume", nil)
		defer must(t, p, D, "halt", nil)
		if _, err := inv(t, p, F, "program", with(bk)); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%v", err)
		}
	})
	t.Run("tampered bin", func(t *testing.T) {
		bad := copyBackup(t, bk, func(b []byte, m *backupManifest) { b[10] ^= 1 })
		if _, err := inv(t, p, F, "program", with(bad)); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("other board", func(t *testing.T) {
		bad := copyBackup(t, bk, func(b []byte, m *backupManifest) { m.FlashUID = "0102030405060708" })
		if _, err := inv(t, p, F, "program", with(bad)); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "another board") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("single read manifest", func(t *testing.T) {
		bad := copyBackup(t, bk, func(b []byte, m *backupManifest) { m.SecondRead = strings.Repeat("0", 64) })
		if _, err := inv(t, p, F, "program", with(bad)); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%v", err)
		}
	})
	t.Run("factory changed after backup", func(t *testing.T) {
		sim.Flash()[0x77010] ^= 0xff
		defer func() { sim.Flash()[0x77010] ^= 0xff }()
		if _, err := inv(t, p, F, "program", with(bk)); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "factory-config-adc") {
			t.Fatalf("%v", err)
		}
	})
	if len(sim.FlashOps) != 0 {
		t.Fatalf("a refused call touched flash: %+v", sim.FlashOps)
	}
	r := must(t, p, F, "program", with(bk))
	if r["backup"].(map[string]any)["board_matches"] != true {
		t.Fatalf("%v", r)
	}
}

func copyBackup(t *testing.T, path string, mut func([]byte, *backupManifest)) string {
	t.Helper()
	b, m, err := loadBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	b = append([]byte(nil), b...)
	mut(b, m)
	out := t.TempDir() + "/x.bin"
	mb, _ := json.Marshal(m)
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+".json", mb, 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRestoreRewritesOnlyDifferingSectors(t *testing.T) {
	p, sim := armed(t)
	bk := haltAndBackup(t, p)
	orig := append([]byte(nil), sim.Flash()...)
	must(t, p, F, "program", withTB04(map[string]any{"address": "0x3000", "data": "a1a2a3", "backup": bk}))
	sim.Flash()[0x71000] ^= 0x01 // a journal sector now differs as well

	r0 := len(sim.FlashOps)
	if _, err := inv(t, p, F, "restore", withTB04(map[string]any{"backup": bk})); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "aps-journal") {
		t.Fatalf("journal restore without opt-in: %v", err)
	}
	if len(sim.FlashOps) != r0 {
		t.Fatal("refused restore wrote flash")
	}
	r := must(t, p, F, "restore", withTB04(map[string]any{"backup": bk, "skip_journals": true}))
	if r["sectors_restored"].(float64) != 1 || len(r["journal_sectors_kept"].([]any)) != 1 || r["matches_backup"] != false ||
		sim.Flash()[0x71000] == orig[0x71000] || !bytes.Equal(orig[:0x70000], sim.Flash()[:0x70000]) {
		t.Fatalf("skip_journals: %v", r)
	}
	if _, err := inv(t, p, F, "restore", withTB04(map[string]any{"backup": bk, "skip_journals": true, "allow_journals": true})); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("both journal modes: %v", err)
	}
	r = must(t, p, F, "restore", withTB04(map[string]any{"backup": bk, "allow_journals": true}))
	if r["matches_backup"] != true || r["sectors_differing"].(float64) != 1 || r["sectors_restored"].(float64) != 1 || !bytes.Equal(orig, sim.Flash()) {
		t.Fatalf("%v", r)
	}
	r = must(t, p, F, "restore", withTB04(map[string]any{"path": bk}))
	if r["sectors_differing"].(float64) != 0 || r["sectors_restored"].(float64) != 0 {
		t.Fatalf("idempotent restore: %v", r)
	}
	sim.Flash()[0x7a000] = 0 // undescribed flash differs: refused
	if _, err := inv(t, p, F, "restore", withTB04(map[string]any{"backup": bk, "allow_journals": true})); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("undescribed sector: %v", err)
	}
}

func TestNoFactoryLayout(t *testing.T) {
	nf := func(a map[string]any) map[string]any {
		out := map[string]any{"layout": "tb04-512k-nofactory"}
		for k, v := range a {
			out[k] = v
		}
		return out
	}
	t.Run("refused when factory block is programmed", func(t *testing.T) {
		p, sim := armed(t)
		must(t, p, D, "halt", nil)
		_, err := inv(t, p, F, "program", nf(map[string]any{"address": "0x1000", "data": "01", "backup": "x"}))
		if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "holds data") || len(sim.FlashOps) != 0 {
			t.Fatalf("%v", err)
		}
	})
	t.Run("blank factory block with backup", func(t *testing.T) {
		p, sim := armed(t)
		blankFactory(sim)
		sim.Flash()[0x77ff0] = 0x00 // anywhere in the region, not only the first bytes
		must(t, p, D, "halt", nil)
		if _, err := inv(t, p, F, "program", nf(map[string]any{"address": "0x1000", "data": "01", "backup": "x"})); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("partially programmed factory block accepted: %v", err)
		}
		sim.Flash()[0x77ff0] = 0xff
		info := must(t, p, F, "info", nf(nil))
		if info["protection_verified"] != true || info["layout"] != "tb04-512k-nofactory" || len(info["regions"].([]any)) != 6 {
			t.Fatalf("%v", info)
		}
		bk := must(t, p, F, "backup", map[string]any{"dir": t.TempDir()})["path"].(string)
		if _, err := inv(t, p, F, "program", nf(map[string]any{"address": "0x76000", "data": "01", "backup": bk})); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("factory sector writable: %v", err)
		}
		must(t, p, F, "program", nf(map[string]any{"address": "0x1000", "data": "01", "backup": bk}))
		if sim.Flash()[0x1000] != 1 || !bytes.Equal(sim.Flash()[0x76000:0x78000], bytes.Repeat([]byte{0xff}, 0x2000)) {
			t.Fatal("program result")
		}
	})
}

func TestChipInfo(t *testing.T) {
	p, _ := armed(t)
	must(t, p, D, "halt", nil)
	r := must(t, p, D, "chip_info", nil)
	if r["target_chip_id"] != "0x5562" || r["target_family"] != "TLSR825x" || r["flash_kib"].(float64) != 512 || r["flash_uid"] != "53594e5448455449" {
		t.Fatalf("%v", r)
	}
	q, _ := newPlugin(t)
	if _, err := inv(t, q, D, "chip_info", nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unattached: %v", err)
	}
}
