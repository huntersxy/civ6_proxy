//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModPatchSpecsMetadata(t *testing.T) {
	specs := modPatchSpecs()
	if len(specs) != 3 {
		t.Fatalf("expected 3 workshop mod patch specs, got %d", len(specs))
	}
	seenIDs := make(map[string]bool)
	for _, spec := range specs {
		if spec.WorkshopID == "" || spec.Name == "" || len(spec.Files) == 0 {
			t.Fatalf("incomplete mod patch spec: %+v", spec)
		}
		if seenIDs[spec.WorkshopID] {
			t.Fatalf("duplicate workshop ID %s", spec.WorkshopID)
		}
		seenIDs[spec.WorkshopID] = true
		for _, f := range spec.Files {
			if len(f.ExpectedOrigMD5) != 32 {
				t.Fatalf("%s/%s: invalid expected orig MD5 %q", spec.WorkshopID, f.RelPath, f.ExpectedOrigMD5)
			}
			if strings.TrimSpace(f.PatchedContent) == "" {
				t.Fatalf("%s/%s: empty patched content", spec.WorkshopID, f.RelPath)
			}
			if strings.EqualFold(md5Hex([]byte(f.PatchedContent)), f.ExpectedOrigMD5) {
				t.Fatalf("%s/%s: patched content MD5 must differ from expected original MD5", spec.WorkshopID, f.RelPath)
			}
		}
	}
}

func TestModPatchSafeApplyForceAndRestore(t *testing.T) {
	libRoot := filepath.Join(t.TempDir(), "SteamLibrary")
	backupRoot := filepath.Join(t.TempDir(), "backups")
	workshopID := "999001"
	modDir := filepath.Join(libRoot, "steamapps", "workshop", "content", civ6SteamAppID, workshopID)
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatalf("mkdir modDir: %v", err)
	}

	origBytes := []byte("function orig()\r\n    return 1\r\nend\r\n")
	patchedText := "function patched()\r\n    return 2\r\nend\r\n"
	spec := modPatchSpec{
		WorkshopID:  workshopID,
		Name:        "Test Mod",
		Description: "test spec",
		Files: []modPatchFileSpec{
			{
				RelPath:         "test_script.lua",
				ExpectedOrigMD5: md5Hex(origBytes),
				PatchedContent:  patchedText,
			},
		},
	}

	targetFile := filepath.Join(modDir, "test_script.lua")
	if err := os.WriteFile(targetFile, origBytes, 0o644); err != nil {
		t.Fatalf("write orig file: %v", err)
	}

	// 1. Exact MD5 match -> modPatchReady -> safe apply succeeds -> backup created -> modPatchApplied.
	st := inspectModPatch(spec, []string{libRoot}, backupRoot)
	if !st.WorkshopFound || st.State != modPatchReady {
		t.Fatalf("expected modPatchReady, got %+v", st)
	}
	st, err := applyModPatch(spec, []string{libRoot}, backupRoot, false)
	if err != nil {
		t.Fatalf("safe applyModPatch: %v", err)
	}
	if st.State != modPatchApplied || !st.HasBackup {
		t.Fatalf("expected modPatchApplied with backup, got %+v", st)
	}
	got, err := os.ReadFile(targetFile)
	if err != nil || string(got) != patchedText {
		t.Fatalf("patched content mismatch: got %q, err=%v", string(got), err)
	}

	// 2. Restore original file exactly and remove backup.
	st, err = restoreModPatch(spec, []string{libRoot}, backupRoot)
	if err != nil {
		t.Fatalf("restoreModPatch: %v", err)
	}
	if st.State != modPatchReady || st.HasBackup {
		t.Fatalf("expected modPatchReady without backup after restore, got %+v", st)
	}
	restored, err := os.ReadFile(targetFile)
	if err != nil || !bytes.Equal(restored, origBytes) {
		t.Fatalf("restored bytes mismatch: got %q want %q (err=%v)", restored, origBytes, err)
	}

	// 3. Modified content (MD5 mismatch) -> safe apply fails -> force apply succeeds & backs up modified bytes.
	modifiedBytes := []byte("function modified()\r\n    return 99\r\nend\r\n")
	if err := os.WriteFile(targetFile, modifiedBytes, 0o644); err != nil {
		t.Fatalf("write modified file: %v", err)
	}
	st = inspectModPatch(spec, []string{libRoot}, backupRoot)
	if st.State != modPatchMismatch {
		t.Fatalf("expected modPatchMismatch, got %+v", st)
	}
	if _, err := applyModPatch(spec, []string{libRoot}, backupRoot, false); err == nil {
		t.Fatalf("expected non-forced apply on MD5 mismatch to fail")
	}
	st, err = applyModPatch(spec, []string{libRoot}, backupRoot, true)
	if err != nil {
		t.Fatalf("forced applyModPatch: %v", err)
	}
	if st.State != modPatchApplied || !st.HasBackup {
		t.Fatalf("expected modPatchApplied after force patch, got %+v", st)
	}

	// Restoring returns the pre-force bytes cleanly.
	st, err = restoreModPatch(spec, []string{libRoot}, backupRoot)
	if err != nil {
		t.Fatalf("restore after force patch: %v", err)
	}
	restoredMod, err := os.ReadFile(targetFile)
	if err != nil || !bytes.Equal(restoredMod, modifiedBytes) {
		t.Fatalf("expected modified bytes restored, got %q (err=%v)", restoredMod, err)
	}
}

func TestModPatchUIFlowSafeForceAndRestore(t *testing.T) {
	w, app := newTestClientWindow(t, false)
	libRoot := filepath.Join(t.TempDir(), "SteamLibrary")

	readyID := "888001"
	mismatchID := "888002"
	readyDir := filepath.Join(libRoot, "steamapps", "workshop", "content", civ6SteamAppID, readyID)
	mismatchDir := filepath.Join(libRoot, "steamapps", "workshop", "content", civ6SteamAppID, mismatchID)
	if err := os.MkdirAll(readyDir, 0o755); err != nil {
		t.Fatalf("mkdir readyDir: %v", err)
	}
	if err := os.MkdirAll(mismatchDir, 0o755); err != nil {
		t.Fatalf("mkdir mismatchDir: %v", err)
	}

	readyOrig := []byte("-- orig ready\r\n")
	readyPatched := "-- patched ready\r\n"
	mismatchExpectedOrig := []byte("-- expected mismatch orig\r\n")
	mismatchActual := []byte("-- custom local edit\r\n")
	mismatchPatched := "-- patched mismatch\r\n"

	if err := os.WriteFile(filepath.Join(readyDir, "ready.lua"), readyOrig, 0o644); err != nil {
		t.Fatalf("write ready.lua: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mismatchDir, "mismatch.lua"), mismatchActual, 0o644); err != nil {
		t.Fatalf("write mismatch.lua: %v", err)
	}

	app.workshopRootsFn = func() []string { return []string{libRoot} }
	app.modPatchSpecsFn = func() []modPatchSpec {
		return []modPatchSpec{
			{
				WorkshopID:  readyID,
				Name:        "Ready Mod",
				Description: "Exact MD5 match test mod",
				Files: []modPatchFileSpec{
					{
						RelPath:         "ready.lua",
						ExpectedOrigMD5: md5Hex(readyOrig),
						PatchedContent:  readyPatched,
					},
				},
			},
			{
				WorkshopID:  mismatchID,
				Name:        "Mismatch Mod",
				Description: "Modified same-ID test mod",
				Files: []modPatchFileSpec{
					{
						RelPath:         "mismatch.lua",
						ExpectedOrigMD5: md5Hex(mismatchExpectedOrig),
						PatchedContent:  mismatchPatched,
					},
				},
			},
		}
	}

	navBtns := w.ResolveID("nav-modpatch")
	if len(navBtns) != 1 {
		t.Fatalf("expected 1 nav-modpatch button, got %v", navBtns)
	}
	if err := w.TestClick(navBtns[0]); err != nil {
		t.Fatalf("click nav-modpatch: %v", err)
	}
	w.TestRender(nil)
	if dups := w.TestDuplicateIDs(); len(dups) != 0 {
		t.Fatalf("modpatch page duplicate IDs: %v", dups)
	}

	// Technical details are collapsed by default and can be expanded/collapsed cleanly.
	if details := w.ResolveID("modpatch-details-" + readyID); len(details) != 0 {
		t.Fatalf("expected technical details collapsed by default, got %v", details)
	}
	toggleBtns := w.ResolveID("modpatch-toggle-" + readyID)
	if len(toggleBtns) != 1 {
		t.Fatalf("expected 1 modpatch-toggle-%s button, got %v", readyID, toggleBtns)
	}
	if err := w.TestClick(toggleBtns[0]); err != nil {
		t.Fatalf("click expand details: %v", err)
	}
	w.TestRender(nil)
	if details := w.ResolveID("modpatch-details-" + readyID); len(details) != 1 {
		t.Fatalf("expected 1 expanded details panel, got %v", details)
	}
	if dups := w.TestDuplicateIDs(); len(dups) != 0 {
		t.Fatalf("expanded modpatch page duplicate IDs: %v", dups)
	}
	toggleBtns = w.ResolveID("modpatch-toggle-" + readyID)
	if err := w.TestClick(toggleBtns[0]); err != nil {
		t.Fatalf("click collapse details: %v", err)
	}
	w.TestRender(nil)
	if details := w.ResolveID("modpatch-details-" + readyID); len(details) != 0 {
		t.Fatalf("expected details collapsed after second toggle, got %v", details)
	}

	// Safe patch on MD5-matched mod via button click.
	applyBtns := w.ResolveID("modpatch-apply-" + readyID)
	if len(applyBtns) != 1 {
		t.Fatalf("expected 1 modpatch-apply-%s button, got %v", readyID, applyBtns)
	}
	if err := w.TestClick(applyBtns[0]); err != nil {
		t.Fatalf("click safe apply: %v", err)
	}
	w.TestRender(nil)
	if got, _ := os.ReadFile(filepath.Join(readyDir, "ready.lua")); string(got) != readyPatched {
		t.Fatalf("expected ready.lua patched, got %q", string(got))
	}

	// Restore ready mod via UI button.
	restoreBtns := w.ResolveID("modpatch-restore-" + readyID)
	if len(restoreBtns) != 1 {
		t.Fatalf("expected 1 modpatch-restore-%s button, got %v", readyID, restoreBtns)
	}
	if err := w.TestClick(restoreBtns[0]); err != nil {
		t.Fatalf("click restore ready mod: %v", err)
	}
	w.TestRender(nil)
	if got, _ := os.ReadFile(filepath.Join(readyDir, "ready.lua")); !bytes.Equal(got, readyOrig) {
		t.Fatalf("expected ready.lua restored to original, got %q", string(got))
	}

	// Scroll down to bottom card and test force patch dialog on MD5-mismatched mod.
	pageIDs := w.ResolveID("modpatch-page")
	if len(pageIDs) != 1 {
		t.Fatalf("expected 1 modpatch-page, got %v", pageIDs)
	}
	w.ScrollVerticalToPct(pageIDs[0], 1.0)
	w.TestRender(nil)

	forceBtns := w.ResolveID("modpatch-force-" + mismatchID)
	if len(forceBtns) != 1 {
		t.Fatalf("expected 1 modpatch-force-%s button, got %v", mismatchID, forceBtns)
	}
	if err := w.TestClick(forceBtns[0]); err != nil {
		t.Fatalf("click force patch: %v", err)
	}
	w.TestRender(nil)
	if dups := w.TestDuplicateIDs(); len(dups) != 0 {
		t.Fatalf("force patch dialog duplicate IDs: %v", dups)
	}

	confirmBtns := w.ResolveID("modpatch-force-confirm")
	if len(confirmBtns) != 1 {
		t.Fatalf("expected 1 modpatch-force-confirm button, got %v", confirmBtns)
	}
	if err := w.TestClick(confirmBtns[0]); err != nil {
		t.Fatalf("click force confirm: %v", err)
	}
	w.TestRender(nil)
	if got, _ := os.ReadFile(filepath.Join(mismatchDir, "mismatch.lua")); string(got) != mismatchPatched {
		t.Fatalf("expected mismatch.lua force patched, got %q", string(got))
	}

	// Restore mismatched mod via UI button.
	w.ScrollVerticalToPct(pageIDs[0], 1.0)
	w.TestRender(nil)
	restoreMismatchBtns := w.ResolveID("modpatch-restore-" + mismatchID)
	if len(restoreMismatchBtns) != 1 {
		t.Fatalf("expected 1 modpatch-restore-%s button, got %v", mismatchID, restoreMismatchBtns)
	}
	if err := w.TestClick(restoreMismatchBtns[0]); err != nil {
		t.Fatalf("click restore mismatch mod: %v", err)
	}
	w.TestRender(nil)
	if got, _ := os.ReadFile(filepath.Join(mismatchDir, "mismatch.lua")); !bytes.Equal(got, mismatchActual) {
		t.Fatalf("expected mismatch.lua restored to pre-patch bytes, got %q", string(got))
	}
	if unconsumed := w.TestUnconsumedEvents(); len(unconsumed) != 0 {
		t.Fatalf("unconsumed events on modpatch page: %v", unconsumed)
	}
}

func TestOpenFolderAndURLSuppressedDuringGoTest(t *testing.T) {
	if err := openFolder(t.TempDir()); err != nil {
		t.Fatalf("expected openFolder to be no-op under go test, got %v", err)
	}
	if err := openURL("https://example.com"); err != nil {
		t.Fatalf("expected openURL to be no-op under go test, got %v", err)
	}

	w, app := newTestClientWindow(t, false)
	var openedPath string
	app.openPathFn = func(p string) error {
		openedPath = p
		return nil
	}
	app.diag = diagState{
		hasResult:  true,
		ok:         true,
		summary:    "OK",
		report:     "Diagnostic Report",
		reportPath: `C:\Temp\Civ6RelayDiag.txt`,
	}

	diagNav := w.ResolveID("nav-diaglog")
	if err := w.TestClick(diagNav[0]); err != nil {
		t.Fatalf("click nav-diaglog: %v", err)
	}
	w.TestRender(nil)

	openBtns := w.ResolveID("diag-open-report")
	if len(openBtns) != 1 {
		t.Fatalf("expected 1 diag-open-report button, got %v", openBtns)
	}
	if err := w.TestClick(openBtns[0]); err != nil {
		t.Fatalf("click diag-open-report: %v", err)
	}
	if openedPath != `C:\Temp\Civ6RelayDiag.txt` {
		t.Fatalf("expected openPathFn to receive report path, got %q", openedPath)
	}
}

func TestAIDiagPromptButtonCopiesPaths(t *testing.T) {
	w, app := newTestClientWindow(t, false)
	var copied string
	w.SetClipboardFn(func(s string) { copied = s })

	app.snap = sessionSnapshot{
		State:       stateJoined,
		Host:        true,
		RoomCode:    "TESTROOM88",
		PCAPActive:  true,
		PCAPPath:    `D:\Captures\TESTROOM88.pcap`,
		HostLogSync: true,
		LogSyncDir:  `D:\CollectedLogs\Session_TESTROOM88`,
		CivRunning:  true,
	}

	diagNav := w.ResolveID("nav-diaglog")
	if len(diagNav) != 1 {
		t.Fatalf("expected 1 nav-diaglog button, got %v", diagNav)
	}
	if err := w.TestClick(diagNav[0]); err != nil {
		t.Fatalf("click nav-diaglog: %v", err)
	}
	w.TestRender(nil)
	if dups := w.TestDuplicateIDs(); len(dups) != 0 {
		t.Fatalf("diaglog duplicate IDs: %v", dups)
	}

	promptBtns := w.ResolveID("log-ai-prompt")
	if len(promptBtns) != 1 {
		t.Fatalf("expected 1 log-ai-prompt button, got %v", promptBtns)
	}
	if err := w.TestClick(promptBtns[0]); err != nil {
		t.Fatalf("click log-ai-prompt: %v", err)
	}
	w.TestRender(nil)

	for _, want := range []string{
		`Sid Meier's Civilization VI\Logs`,
		`D:\Captures\TESTROOM88.pcap`,
		`D:\CollectedLogs\Session_TESTROOM88`,
		"net_message_debug.log",
		"DESYNC_DELTA",
		"TESTROOM88",
	} {
		if !strings.Contains(copied, want) {
			t.Fatalf("expected AI diagnostic prompt to contain %q, got:\n%s", want, copied)
		}
	}
	if unconsumed := w.TestUnconsumedEvents(); len(unconsumed) != 0 {
		t.Fatalf("unconsumed events: %v", unconsumed)
	}
}
