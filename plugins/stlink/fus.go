package stlinkplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/plugins/stlink/stlink"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

// Confirmation tokens of the STM32WB FUS methods.
const (
	tokFUSUpgrade   = "upgrade-fus"
	tokStackDelete  = "delete-wireless-stack"
	tokStackInstall = "install-wireless-stack"
	tokStackStart   = "start-wireless-stack"
	tokParkCPU1     = "park-cpu1"
)

// fusMinV1 is the FUS every FUS v1.x part must pass through (release notes
// of STM32CubeWB, "FUSv1.x.y => Must be updated first to FUSv1.2.0").
const fusMinV1 = stm32.Version(0x01020000)

// FUS 0.5.3 (found on early P-NUCLEO-WB55 boards) cannot take the plain FUS
// images: STM32CubeWB 8f0328f0, Release_Notes.html V1.24.0, "STEP 5" says to
// install stm32wb5x_FUS_fw_for_fus_0_5_3.bin first, which yields FUS 1.2.0.
// That image is for FUS 0.5.3 only. Identified by SHA-256.
const (
	fus053        = stm32.Version(0x00050300)
	fusFor053SHA  = "3f73a9be93eec3de7ff663d4b8a7669064c86f841db401c8ccee4f6ccd4e4b30" // stm32wb5x_FUS_fw_for_fus_0_5_3.bin -> 1.2.0
	fusFor053Next = stm32.Version(0x01020000)
)

// plainFUSImages are the regular FUS images that must not go onto FUS 0.5.3.
var plainFUSImages = map[string]string{
	"2712d4d23417c00af9a0fb6337c6f93118d8ffce02c61db7f4e611b74baddbfd": "stm32wb5x_FUS_fw.bin (FUS 1.2.0)",
	"bbab9b42d92c31f8a04e2f04ef9361c3f6f01714c14408cd34fb1583a6edf30d": "stm32wb5x_FUS_fw.bin (FUS 2.2.0)",
}

// checkFUSImage applies the FUS 0.5.3 rule of the release notes to the
// running (or expected) FUS, the image's SHA-256 and the expected result.
func checkFUSImage(cur, next stm32.Version, imgSHA string) error {
	imgSHA = strings.ToLower(imgSHA)
	if cur.Same(fus053) {
		if what, ok := plainFUSImages[imgSHA]; ok {
			return denied("FUS 0.5.3 must not receive %s: install stm32wb5x_FUS_fw_for_fus_0_5_3.bin (SHA-256 %s) first (STM32CubeWB release notes V1.24.0, STEP 5)", what, fusFor053SHA)
		}
		if imgSHA != fusFor053SHA {
			return denied("FUS 0.5.3 only accepts stm32wb5x_FUS_fw_for_fus_0_5_3.bin (SHA-256 %s), not an image with SHA-256 %q (STM32CubeWB release notes V1.24.0, STEP 5)", fusFor053SHA, imgSHA)
		}
		if !next.Same(fusFor053Next) {
			return argErr(fmt.Errorf("stm32wb5x_FUS_fw_for_fus_0_5_3.bin yields FUS %s: pass expect_new_fus_version=%s, not %s", fusFor053Next, fusFor053Next, next))
		}
		return nil
	}
	if imgSHA == fusFor053SHA {
		return denied("stm32wb5x_FUS_fw_for_fus_0_5_3.bin is only for a running FUS 0.5.3, this one is %s", cur)
	}
	return nil
}

// wbTarget is flashTarget restricted to an STM32WB.
func (s *session) wbTarget(ctx context.Context) (*stm32.Flash, *stm32.Profile, error) {
	fl, p, err := s.flashTarget(ctx)
	if err != nil {
		return nil, nil, err
	}
	if p.Family != stm32.WB {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "%s: FUS operations exist on STM32WB only; the target is %s (dev 0x%03x)", unsupportedHW, p.Name, p.DevID)
	}
	return fl, p, nil
}

// fusTiming lets tests shorten the FUS polling intervals.
var fusTiming func(*stm32.FUS)

func (s *session) newFUS(p *stm32.Profile) *stm32.FUS {
	f := stm32.NewFUS(s.core, p.FlashKB)
	f.Reattach = s.reattachSWD
	if fusTiming != nil {
		fusTiming(f)
	}
	return f
}

func fusErr(f *stm32.FUS, err error) error {
	log := ""
	if f != nil && len(f.Log) > 0 {
		log = "; log: " + strings.Join(f.Log, " | ")
	}
	switch {
	case errors.Is(err, stm32.ErrFUS):
		return status.Error(codes.Aborted, err.Error()+log)
	case errors.Is(err, stm32.ErrNotParked), errors.Is(err, stm32.ErrFUSUnexpected):
		return status.Error(codes.FailedPrecondition, err.Error()+log)
	}
	if st, ok := status.FromError(flashErr(err)); ok && st.Code() != codes.Unknown {
		return status.Error(st.Code(), st.Message()+log)
	}
	if log == "" {
		return err
	}
	return fmt.Errorf("%w%s", err, log)
}

func versionArg(args map[string]any, key string, required bool) (stm32.Version, bool, error) {
	v, err := strArg(args, key)
	if err != nil {
		return 0, false, err
	}
	if v == "" {
		if required {
			return 0, false, argErr(fmt.Errorf("%s: required (major.minor.sub, e.g. 1.2.0)", key))
		}
		return 0, false, nil
	}
	ver, err := stm32.ParseVersion(v)
	if err != nil {
		return 0, false, argErr(fmt.Errorf("%s: %w", key, err))
	}
	return ver, true, nil
}

func devInfoMap(d stm32.DeviceInfo) map[string]any {
	m := map[string]any{
		"layout": d.Layout, "table": hexw(d.TablePtr),
		"fus_version": d.FUSVersion.String(), "fus_version_raw": hexw(uint32(d.FUSVersion)),
		"stack_version": d.StackVersion.String(), "stack_version_raw": hexw(uint32(d.StackVersion)),
		"stack_memory_size": hexw(d.StackMemSize),
	}
	if d.StackVersion == 0 {
		m["stack_version"] = "none"
	}
	if d.Layout == "wireless_stack" {
		m["info_stack"] = hexw(d.InfoStack)
	}
	if d.Layout == "fus" {
		// The FUS's own DeviceId field. It is not DBGMCU_IDCODE: an
		// STM32WB55 (DBGMCU 0x495) running FUS 1.1.1 reports 0x0496.
		m["fus_device_id"] = fmt.Sprintf("0x%04x", d.DevID)
	}
	return m
}

func securityMap(sec stm32.Security) map[string]any {
	return map[string]any{
		"flash_sfr": hexw(sec.SFR), "sfsa": fmt.Sprintf("0x%02x", sec.SFR&0xFF), "secure_flash_start": hexw(sec.SecureBase),
		"flash_srrvr": hexw(sec.SRRVR), "secure_sram2a_start": hexw(sec.SRAM2aSecure), "ipccbr": hexw(sec.IPCCBR),
		"pwr_cr4": hexw(sec.PWRCR4), "c2boot": sec.C2Boot,
	}
}

func opMap(f *stm32.FUS, r stm32.OpResult) map[string]any {
	m := map[string]any{
		"before": devInfoMap(r.Before), "after": devInfoMap(r.After),
		"security_before": securityMap(r.SecBefore), "security_after": securityMap(r.SecAfter),
		"log":  anyStrings(f.Log),
		"cpu1": "halted at its reset vector with CPU2 running the FUS; a reset (kind=system) starts the CPU1 application",
	}
	if r.StagedBytes > 0 {
		m["staged_at"], m["staged_bytes"] = hexw(r.StagedAt), float64(r.StagedBytes)
	}
	return m
}

func anyStrings(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

// fusPerms: every FUS method that boots CPU2 writes the mailbox into
// SRAM2a and resets the chip.
func (s *session) fusPerms(modify bool) error {
	if !s.o.perm.reset {
		return denied("FUS methods reset the chip: that needs allow_reset=true in the device configuration")
	}
	if !s.o.perm.memWrite {
		return denied("FUS methods write the IPCC mailbox into SRAM2a: that needs allow_memory_write=true in the device configuration")
	}
	if modify {
		return s.needFlash(true)
	}
	return nil
}

// fusInfo is read only: it decodes what the last CPU2 boot left in the
// device information table, the secure boundary and the boot state.
func (s *session) fusInfo(ctx context.Context, _ map[string]any) (map[string]any, error) {
	_, p, err := s.wbTarget(ctx)
	if err != nil {
		return nil, err
	}
	f := s.newFUS(p)
	sec, err := f.ReadSecurity(ctx)
	if err != nil {
		return nil, err
	}
	di, err := f.ReadDeviceInfo(ctx)
	if err != nil {
		return nil, err
	}
	res := map[string]any{
		"chip": p.Name, "device_info": devInfoMap(di), "security": securityMap(sec),
		"user_flash": fmt.Sprintf("0x%08x..0x%08x", uint32(stm32.FlashBase), sec.SecureBase),
		"note": "read only: the device information table is what CPU2 last wrote; it is current only while CPU2 runs (c2boot=true). " +
			"FUS methods: fus_start, fus_upgrade, fus_delete_stack, fus_install_stack, fus_start_stack",
	}
	return s.mark(res), nil
}

// fusStart resets the chip and brings CPU2 into the FUS.
func (s *session) fusStart(ctx context.Context, _ map[string]any) (map[string]any, error) {
	if err := s.fusPerms(false); err != nil {
		return nil, err
	}
	_, p, err := s.wbTarget(ctx)
	if err != nil {
		return nil, err
	}
	f := s.newFUS(p)
	restore, err := f.Hold(ctx)
	if err != nil {
		return nil, err
	}
	defer restore()
	st, di, err := f.EnterFUS(ctx)
	if err != nil {
		return nil, fusErr(f, err)
	}
	sec, err := f.ReadSecurity(ctx)
	if err != nil {
		return nil, err
	}
	return s.mark(map[string]any{
		"fus_state": fmt.Sprintf("0x%02x", st), "device_info": devInfoMap(di), "security": securityMap(sec), "log": anyStrings(f.Log),
		"cpu1": "halted at its reset vector with CPU2 running the FUS; a reset (kind=system) starts the CPU1 application",
	}), nil
}

// stageArgs reads and checks the image, its SHA-256 and the address, and
// applies the backup gate to the pages the staging erases.
func (s *session) stageArgs(ctx context.Context, args map[string]any, fl *stm32.Flash, p *stm32.Profile) ([]byte, uint32, map[string]any, error) {
	img, path, err := imageArgs(args)
	if err != nil {
		return nil, 0, nil, err
	}
	if err := stm32.CheckImage(img); err != nil {
		return nil, 0, nil, argErr(fmt.Errorf("%s: %w", path, err))
	}
	want, err := strArg(args, "image_sha256")
	if err != nil {
		return nil, 0, nil, err
	}
	if got := sha(img); !strings.EqualFold(want, got) {
		return nil, 0, nil, status.Errorf(codes.FailedPrecondition, "%s has SHA-256 %s, not image_sha256=%q", path, got, want)
	}
	if args["address"] == nil {
		return nil, 0, nil, argErr(fmt.Errorf("address: required; for a wireless stack ST's placement below the current secure start 0x%08x is 0x%08x", p.SecureBase, stm32.StagingAddress(p.SecureBase, len(img))))
	}
	addr, err := toAddr(args["address"])
	if err != nil {
		return nil, 0, nil, argErr(err)
	}
	if addr%p.PageSize != 0 || addr < stm32.FlashBase || uint64(addr)+uint64(len(img)) > uint64(p.SecureBase) {
		return nil, 0, nil, status.Errorf(codes.OutOfRange, "address 0x%08x: the image (%d bytes) must start on a page and end below the secure start 0x%08x", addr, len(img), p.SecureBase)
	}
	var secs []stm32.Sector
	for a := addr; a < p.SecureBase; a += p.PageSize {
		sc, ok := p.SectorAt(a)
		if !ok {
			return nil, 0, nil, status.Errorf(codes.OutOfRange, "no page at 0x%08x", a)
		}
		secs = append(secs, sc)
	}
	bk, err := s.backupGate(ctx, args, fl, secs)
	if err != nil {
		return nil, 0, nil, err
	}
	bk["staging_erases"] = fmt.Sprintf("0x%08x..0x%08x (%d pages)", addr, p.SecureBase, len(secs))
	return img, addr, bk, nil
}

func (s *session) fusUpgrade(ctx context.Context, args map[string]any) (map[string]any, error) {
	if err := s.fusPerms(true); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokFUSUpgrade, "upgrading the FUS"); err != nil {
		return nil, err
	}
	cur, _, err := versionArg(args, "expect_fus_version", true)
	if err != nil {
		return nil, err
	}
	next, _, err := versionArg(args, "expect_new_fus_version", true)
	if err != nil {
		return nil, err
	}
	if next>>8 <= cur>>8 {
		return nil, argErr(fmt.Errorf("expect_new_fus_version %s is not newer than %s: downgrades are not offered", next, cur))
	}
	if cur>>24 == 1 && cur>>8 < fusMinV1>>8 && !next.Same(fusMinV1) {
		return nil, denied("FUS %s must be upgraded to %s first (STM32CubeWB release notes, STEP 6); refusing %s", cur, fusMinV1, next)
	}
	declared, err := strArg(args, "image_sha256")
	if err != nil {
		return nil, err
	}
	if err := checkFUSImage(cur, next, declared); err != nil {
		return nil, err
	}
	fl, p, err := s.wbTarget(ctx)
	if err != nil {
		return nil, err
	}
	img, addr, bk, err := s.stageArgs(ctx, args, fl, p)
	if err != nil {
		return nil, err
	}
	f := s.newFUS(p)
	restore, err := f.Hold(ctx)
	if err != nil {
		return nil, err
	}
	defer restore()
	r, err := f.Upgrade(ctx, img, addr, func(di stm32.DeviceInfo, sec stm32.Security) error {
		if !di.FUSVersion.Same(cur) {
			return status.Errorf(codes.FailedPrecondition, "the FUS is %s, not expect_fus_version=%s; nothing was changed", di.FUSVersion, cur)
		}
		if err := checkFUSImage(di.FUSVersion, next, sha(img)); err != nil {
			return err
		}
		if sec.SecureBase != p.SecureBase {
			return status.Errorf(codes.FailedPrecondition, "the secure start is 0x%08x, not 0x%08x as when the arguments were checked; nothing was changed", sec.SecureBase, p.SecureBase)
		}
		return nil
	})
	if err != nil {
		return nil, fusErr(f, err)
	}
	res := opMap(f, r)
	res["backup"], res["image_sha256"] = bk, sha(img)
	if !r.After.FUSVersion.Same(next) || r.After.StackVersion != r.Before.StackVersion {
		return nil, status.Errorf(codes.DataLoss, "after the upgrade the FUS is %s (expected %s) and the stack %s (was %s); log: %s",
			r.After.FUSVersion, next, r.After.StackVersion, r.Before.StackVersion, strings.Join(f.Log, " | "))
	}
	res["verified"] = fmt.Sprintf("FUS %s -> %s, stack %s unchanged", r.Before.FUSVersion, r.After.FUSVersion, r.After.StackVersion)
	return s.mark(res), nil
}

func (s *session) fusDeleteStack(ctx context.Context, args map[string]any) (map[string]any, error) {
	if err := s.fusPerms(true); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokStackDelete, "deleting the wireless stack"); err != nil {
		return nil, err
	}
	want, _, err := versionArg(args, "expect_stack_version", true)
	if err != nil {
		return nil, err
	}
	_, p, err := s.wbTarget(ctx)
	if err != nil {
		return nil, err
	}
	f := s.newFUS(p)
	restore, err := f.Hold(ctx)
	if err != nil {
		return nil, err
	}
	defer restore()
	r, err := f.DeleteStack(ctx, func(di stm32.DeviceInfo, _ stm32.Security) error {
		if !di.StackVersion.Same(want) {
			return status.Errorf(codes.FailedPrecondition, "the installed stack is %s, not expect_stack_version=%s; nothing was changed", di.StackVersion, want)
		}
		return nil
	})
	if err != nil {
		return nil, fusErr(f, err)
	}
	if r.After.StackVersion != 0 || r.After.FUSVersion != r.Before.FUSVersion {
		return nil, status.Errorf(codes.DataLoss, "after the delete the stack reads %s and the FUS %s (was %s); log: %s",
			r.After.StackVersion, r.After.FUSVersion, r.Before.FUSVersion, strings.Join(f.Log, " | "))
	}
	res := opMap(f, r)
	res["verified"] = fmt.Sprintf("stack %s deleted; secure start 0x%08x -> 0x%08x", r.Before.StackVersion, r.SecBefore.SecureBase, r.SecAfter.SecureBase)
	res["note"] = "the CPU1 user flash grew: earlier backups no longer match the layout; take a new backup before modifying flash"
	return s.mark(res), nil
}

func (s *session) fusInstallStack(ctx context.Context, args map[string]any) (map[string]any, error) {
	if err := s.fusPerms(true); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokStackInstall, "installing a wireless stack"); err != nil {
		return nil, err
	}
	next, _, err := versionArg(args, "expect_new_stack_version", true)
	if err != nil {
		return nil, err
	}
	fl, p, err := s.wbTarget(ctx)
	if err != nil {
		return nil, err
	}
	img, addr, bk, err := s.stageArgs(ctx, args, fl, p)
	if err != nil {
		return nil, err
	}
	f := s.newFUS(p)
	restore, err := f.Hold(ctx)
	if err != nil {
		return nil, err
	}
	defer restore()
	r, err := f.Upgrade(ctx, img, addr, func(di stm32.DeviceInfo, sec stm32.Security) error {
		if di.StackVersion != 0 {
			return status.Errorf(codes.FailedPrecondition, "a wireless stack (%s) is installed: delete it first with fus_delete_stack; nothing was changed", di.StackVersion)
		}
		if sec.SecureBase != p.SecureBase {
			return status.Errorf(codes.FailedPrecondition, "the secure start is 0x%08x, not 0x%08x as when the arguments were checked; nothing was changed", sec.SecureBase, p.SecureBase)
		}
		return nil
	})
	if err != nil {
		return nil, fusErr(f, err)
	}
	res := opMap(f, r)
	res["backup"], res["image_sha256"] = bk, sha(img)
	if !r.After.StackVersion.Same(next) || r.After.FUSVersion != r.Before.FUSVersion {
		return nil, status.Errorf(codes.DataLoss, "after the install the stack is %s (expected %s) and the FUS %s (was %s); log: %s",
			r.After.StackVersion, next, r.After.FUSVersion, r.Before.FUSVersion, strings.Join(f.Log, " | "))
	}
	res["verified"] = fmt.Sprintf("stack %s installed; secure start 0x%08x -> 0x%08x", r.After.StackVersion, r.SecBefore.SecureBase, r.SecAfter.SecureBase)
	res["note"] = "the stack is installed but not started: call fus_start_stack. The CPU1 user flash shrank: take a new backup before modifying flash"
	return s.mark(res), nil
}

func (s *session) fusStartStack(ctx context.Context, args map[string]any) (map[string]any, error) {
	if err := s.fusPerms(false); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokStackStart, "starting the wireless stack"); err != nil {
		return nil, err
	}
	want, have, err := versionArg(args, "expect_stack_version", false)
	if err != nil {
		return nil, err
	}
	_, p, err := s.wbTarget(ctx)
	if err != nil {
		return nil, err
	}
	f := s.newFUS(p)
	restore, err := f.Hold(ctx)
	if err != nil {
		return nil, err
	}
	defer restore()
	_, before, err := f.EnterFUS(ctx)
	if err != nil {
		return nil, fusErr(f, err)
	}
	if before.StackVersion == 0 {
		return nil, status.Error(codes.FailedPrecondition, "no wireless stack is installed")
	}
	if have && !before.StackVersion.Same(want) {
		return nil, status.Errorf(codes.FailedPrecondition, "the installed stack is %s, not expect_stack_version=%s", before.StackVersion, want)
	}
	if err := f.StartStack(ctx); err != nil {
		return nil, fusErr(f, err)
	}
	after, err := f.ReadDeviceInfo(ctx)
	if err != nil {
		return nil, err
	}
	sec, err := f.ReadSecurity(ctx)
	if err != nil {
		return nil, err
	}
	if after.Layout != "wireless_stack" || after.StackVersion != before.StackVersion {
		return nil, status.Errorf(codes.DataLoss, "the wireless stack reported %s in the %s layout (expected %s); log: %s",
			after.StackVersion, after.Layout, before.StackVersion, strings.Join(f.Log, " | "))
	}
	return s.mark(map[string]any{
		"before": devInfoMap(before), "after": devInfoMap(after), "security": securityMap(sec), "log": anyStrings(f.Log),
		"verified": fmt.Sprintf("wireless stack %s running (FUS %s)", after.StackVersion, after.FUSVersion),
		"cpu1":     "halted at its reset vector; the next reset boots the CPU1 application, which boots CPU2 into the wireless stack (the FUS is no longer requested)",
	}), nil
}

// reattachSWD enters SWD again after a reset that reset the debug port and
// refuses a different target.
func (s *session) reattachSWD(ctx context.Context) error {
	info, err := s.conn.Connect(ctx, stlink.ConnectConfig{Transport: s.o.transport, KHz: s.o.clock})
	if err != nil {
		return err
	}
	if info.DPIDCode != s.info.DPIDCode {
		return fmt.Errorf("after re-attaching the DP IDCODE is 0x%08x, not 0x%08x: a different target", info.DPIDCode, s.info.DPIDCode)
	}
	return nil
}

// fusParkCPU1 replaces CPU1's boot page with the park stub so that the
// resets the FUS raises (which reset the debug logic) cannot start an
// application that boots CPU2.
func (s *session) fusParkCPU1(ctx context.Context, args map[string]any) (map[string]any, error) {
	if err := s.fusPerms(true); err != nil {
		return nil, err
	}
	if err := tokenIs(args, "confirm", tokParkCPU1, "replacing CPU1's boot page with the park stub"); err != nil {
		return nil, err
	}
	fl, p, err := s.wbTarget(ctx)
	if err != nil {
		return nil, err
	}
	sc, ok := p.SectorAt(stm32.ParkStubAddr)
	if !ok {
		return nil, status.Errorf(codes.OutOfRange, "no page at 0x%08x", uint32(stm32.ParkStubAddr))
	}
	f := s.newFUS(p)
	if parked, err := f.IsParked(ctx); err != nil {
		return nil, err
	} else if parked {
		return s.mark(map[string]any{"changed": false, "note": "CPU1 is already parked"}), nil
	}
	bk, err := s.backupGate(ctx, args, fl, []stm32.Sector{sc})
	if err != nil {
		return nil, err
	}
	restore, err := f.Hold(ctx)
	if err != nil {
		return nil, err
	}
	defer restore()
	if err := f.SystemReset(ctx); err != nil {
		return nil, fusErr(f, err)
	}
	if err := fl.EraseSector(ctx, sc); err != nil {
		return nil, status.Errorf(codes.DataLoss, "erasing page %d: %v; restore it from %s", sc.Index, flashErr(err), bk["path"])
	}
	if err := fl.Program(ctx, stm32.ParkStubAddr, stm32.ParkStub); err != nil {
		return nil, status.Errorf(codes.DataLoss, "programming the park stub: %v; restore page %d from %s", flashErr(err), sc.Index, bk["path"])
	}
	parked, err := f.IsParked(ctx)
	if err != nil || !parked {
		return nil, status.Errorf(codes.DataLoss, "the park stub did not read back (%v); restore page %d from %s", err, sc.Index, bk["path"])
	}
	return s.mark(map[string]any{
		"changed": true, "backup": bk, "page": fmt.Sprintf("0x%08x (+%d)", sc.Addr, sc.Size), "stub_bytes": float64(len(stm32.ParkStub)),
		"log":  anyStrings(f.Log),
		"note": "CPU1's application no longer boots: its vector table was replaced. Program a new application (or restore the backup) after the FUS work",
	}), nil
}
