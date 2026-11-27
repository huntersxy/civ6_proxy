//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDocumentsDir(t *testing.T) {
	docs, err := resolveDocumentsDir()
	if err != nil {
		t.Fatalf("resolveDocumentsDir failed: %v", err)
	}
	info, err := os.Stat(docs)
	if err != nil || !info.IsDir() {
		t.Fatalf("resolved documents dir %q does not exist as dir: err=%v", docs, err)
	}
}

func TestSyncFixModInstallInspectUninstall(t *testing.T) {
	modsRoot := filepath.Join(t.TempDir(), "Mods")

	st := inspectSyncFixMod(modsRoot, nil)
	if st.Err != "" {
		t.Fatalf("inspectSyncFixMod error: %s", st.Err)
	}
	if st.FileCount != 4 {
		t.Fatalf("expected 4 embedded mod files, got %d", st.FileCount)
	}
	if st.Installed || st.UpToDate || st.WorkshopInstalled {
		t.Fatalf("expected not installed initially, got %+v", st)
	}

	installDir, err := installSyncFixMod(modsRoot)
	if err != nil {
		t.Fatalf("installSyncFixMod failed: %v", err)
	}
	expectedDir := filepath.Join(modsRoot, "Civ6SyncFix")
	if installDir != expectedDir {
		t.Fatalf("expected installDir %q, got %q", expectedDir, installDir)
	}

	for _, rel := range []string{
		"Civ6SyncFix.modinfo",
		filepath.Join("Config", "Civ6SyncFix_Config.sql"),
		filepath.Join("Binaries", "Win64", "GameCore_XP2_Civ6SyncFix_FinalRelease.dll"),
		filepath.Join("Binaries", "Win64", "c6se", "Civ6SyncFix.dll"),
	} {
		if _, err := os.Stat(filepath.Join(installDir, rel)); err != nil {
			t.Fatalf("expected installed file %s: %v", rel, err)
		}
	}

	st = inspectSyncFixMod(modsRoot, nil)
	if !st.Installed || !st.UpToDate {
		t.Fatalf("expected installed & up to date, got %+v", st)
	}

	// Corrupt one file and ensure inspect detects outdated state, then reinstall repairs it.
	sqlPath := filepath.Join(installDir, "Config", "Civ6SyncFix_Config.sql")
	if err := os.WriteFile(sqlPath, []byte("-- modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	st = inspectSyncFixMod(modsRoot, nil)
	if !st.Installed || st.UpToDate {
		t.Fatalf("expected installed=true upToDate=false after edit, got %+v", st)
	}
	if _, err := installSyncFixMod(modsRoot); err != nil {
		t.Fatalf("reinstall failed: %v", err)
	}
	st = inspectSyncFixMod(modsRoot, nil)
	if !st.UpToDate {
		t.Fatalf("expected upToDate=true after reinstall, got %+v", st)
	}

	if err := uninstallSyncFixMod(modsRoot); err != nil {
		t.Fatalf("uninstallSyncFixMod failed: %v", err)
	}
	st = inspectSyncFixMod(modsRoot, nil)
	if st.Installed || st.UpToDate {
		t.Fatalf("expected uninstalled after removal, got %+v", st)
	}
}

func TestSteamWorkshopDetectionAndVDFParsing(t *testing.T) {
	vdf := `
"libraryfolders"
{
	"0"
	{
		"path"		"C:\\Program Files (x86)\\Steam"
		"label"		""
	}
	"1"
	{
		"path"		"D:\\SteamLibrary"
	}
}
`
	libs := parseLibraryFoldersVDF(vdf)
	if len(libs) != 2 || libs[0] != `C:\Program Files (x86)\Steam` || libs[1] != `D:\SteamLibrary` {
		t.Fatalf("unexpected VDF library paths: %#v", libs)
	}

	modsRoot := filepath.Join(t.TempDir(), "Mods")
	steamLib := t.TempDir()
	workshopItemDir := filepath.Join(steamLib, "steamapps", "workshop", "content", civ6SteamAppID, syncFixWorkshopID)

	// Empty workshop directory does not count as downloaded.
	if err := os.MkdirAll(workshopItemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := inspectSyncFixMod(modsRoot, []string{steamLib})
	if st.WorkshopInstalled {
		t.Fatalf("expected empty workshop dir not to report WorkshopInstalled, got %+v", st)
	}

	// Non-empty workshop directory reports subscribed & downloaded.
	if err := os.WriteFile(filepath.Join(workshopItemDir, "Civ6SyncFix.modinfo"), []byte("<Mod/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	st = inspectSyncFixMod(modsRoot, []string{steamLib})
	if !st.WorkshopInstalled || st.WorkshopDir != workshopItemDir {
		t.Fatalf("expected WorkshopInstalled=true and WorkshopDir=%q, got %+v", workshopItemDir, st)
	}
}
