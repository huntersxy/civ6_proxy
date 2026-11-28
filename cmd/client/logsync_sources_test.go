//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"civ6relay/internal/logsync"
)

func TestDiscoverLocalLogSourcesAndEnvCheckParity(t *testing.T) {
	localApp := t.TempDir()
	roamingApp := t.TempDir()
	tmpDir := t.TempDir()
	t.Setenv("LOCALAPPDATA", localApp)
	t.Setenv("APPDATA", roamingApp)
	t.Setenv("TEMP", tmpDir)
	t.Setenv("TMP", tmpDir)

	origCivStartFn := civ6StartTimeFn
	defer func() { civ6StartTimeFn = origCivStartFn }()
	civ6StartTimeFn = func() (time.Time, bool) { return time.Time{}, false }

	civLogs := filepath.Join(localApp, "Firaxis Games", "Sid Meier's Civilization VI", "Logs")
	relayLogs := filepath.Join(localApp, "Civ6Relay", "logs")
	relayCaps := filepath.Join(localApp, "Civ6Relay", "captures")
	relayCfgDir := filepath.Join(roamingApp, "Civ6Relay")
	for _, d := range []string{civLogs, relayLogs, relayCaps, relayCfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	sessionStart := time.Now()

	// Dynamic Civ 6 logs matching Sync|Desync|C6SE (including a huge 70 MiB pre-session file) plus unrelated file.
	for _, name := range []string{"Civ6SyncFix.log", "Custom_Desync_Dump.log", "C6SE_loader.log", "unrelated.txt"} {
		if err := os.WriteFile(filepath.Join(civLogs, name), []byte("data-"+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hugeDesync := filepath.Join(civLogs, "Huge_Desync_Old.log")
	fh, err := os.Create(hugeDesync)
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.Truncate(70 << 20); err != nil {
		_ = fh.Close()
		t.Fatal(err)
	}
	_ = fh.Close()
	oldTime := sessionStart.Add(-2 * time.Hour)
	_ = os.Chtimes(hugeDesync, oldTime, oldTime)

	// 5 relay logs: 1 very old (>24h), 4 recent -> should pick only newest 2 within 24h.
	relayBase := sessionStart.Add(-3 * time.Minute)
	for i := 0; i < 5; i++ {
		p := filepath.Join(relayLogs, fmt.Sprintf("client-2026112%d.log", i))
		if err := os.WriteFile(p, []byte("relay-log\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := relayBase.Add(time.Duration(i) * time.Minute)
		if i == 0 {
			mt = sessionStart.Add(-48 * time.Hour)
		}
		_ = os.Chtimes(p, mt, mt)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "Civ6RelayDiag.txt"), []byte("diag"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relayCfgDir, "config.json"), []byte(`{"relay_host":"1.2.3.4"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3 PCAPs:
	// - newest.pcap: fresh within current session (small, 1KB) -> included
	// - huge.pcap: fresh within current session (35MB > 30MB cap) -> skipped
	// - stale.pcap: from previous session (10m ago, 2KB) -> skipped by sessionStart cutoff
	p1 := filepath.Join(relayCaps, "newest.pcap")
	p2 := filepath.Join(relayCaps, "huge.pcap")
	p3 := filepath.Join(relayCaps, "stale.pcap")
	if err := os.WriteFile(p1, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	f2, err := os.Create(p2)
	if err != nil {
		t.Fatal(err)
	}
	if err := f2.Truncate(35 << 20); err != nil {
		_ = f2.Close()
		t.Fatal(err)
	}
	_ = f2.Close()
	if err := os.WriteFile(p3, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p3, sessionStart.Add(-10*time.Minute), sessionStart.Add(-10*time.Minute))
	_ = os.Chtimes(p2, sessionStart.Add(-5*time.Second), sessionStart.Add(-5*time.Second))
	_ = os.Chtimes(p1, sessionStart, sessionStart)

	specs := discoverLocalLogSourcesForSession(sessionStart)
	byRel := make(map[string]logsync.SourceSpec, len(specs))
	for _, s := range specs {
		byRel[s.RelPath] = s
	}

	for _, want := range []string{
		"civ6_logs/Civ6SyncFix.log",
		"civ6_logs/net_message_debug.log",
		"civ6_logs/Custom_Desync_Dump.log",
		"civ6_logs/C6SE_loader.log",
		"civ6_logs/Huge_Desync_Old.log",
		"relay_logs/client-20261124.log",
		"relay_logs/client-20261123.log",
		"relay_logs/Civ6RelayDiag.txt",
		"relay_logs/config.json",
		"relay_captures/newest.pcap",
	} {
		if _, ok := byRel[want]; !ok {
			t.Fatalf("expected source %q in discovered list, got %v", want, byRel)
		}
	}
	for _, unwanted := range []string{
		"civ6_logs/unrelated.txt",
		"relay_logs/client-20261120.log",
		"relay_logs/client-20261121.log",
		"relay_logs/client-20261122.log",
		"relay_captures/huge.pcap",
		"relay_captures/stale.pcap",
	} {
		if _, ok := byRel[unwanted]; ok {
			t.Fatalf("unexpected source %q in discovered list", unwanted)
		}
	}

	// Verify Civ 6 specs use session cutoff when game is not running, plus tail/segment/backlog caps.
	netSpec := byRel["civ6_logs/net_message_debug.log"]
	if netSpec.SkipIfOlderThan.IsZero() || netSpec.InitialMaxBytes != civ6InitialTailBytes || netSpec.SegmentMaxBytes != civ6SegmentMaxBytes {
		t.Fatalf("unexpected Civ 6 spec policy: %+v", netSpec)
	}
	// Verify active relay log tails 512 KiB.
	relaySpec := byRel["relay_logs/client-20261124.log"]
	if relaySpec.InitialMaxBytes != relayLogInitialTailBytes {
		t.Fatalf("unexpected relay log tail limit: %+v", relaySpec)
	}

	// Verify Civ 6 already running shifts cutoff to civStartTime - 5s (preserving earlier Modding.log from current game).
	civStart := sessionStart.Add(-20 * time.Minute)
	civ6StartTimeFn = func() (time.Time, bool) { return civStart, true }
	runningSpecs := discoverLocalLogSourcesForSession(sessionStart)
	var moddingSpec logsync.SourceSpec
	for _, s := range runningSpecs {
		if s.RelPath == "civ6_logs/Modding.log" {
			moddingSpec = s
			break
		}
	}
	wantCutoff := civStart.Add(-sessionCutoffGrace)
	if !moddingSpec.SkipIfOlderThan.Equal(wantCutoff) {
		t.Fatalf("expected Modding.log SkipIfOlderThan=%v when Civ6 running, got %v", wantCutoff, moddingSpec.SkipIfOlderThan)
	}

	envBytes := generateLocalEnvCheck()
	envText := string(envBytes)
	for _, section := range []string{
		"CollectedAt:",
		"Computer:",
		"User:",
		"OS:",
		"IsAdmin:",
		"=== Running Processes ===",
		"=== Civ6SyncFix Mod Status ===",
		"=== WinDivert Driver & Service ===",
		"=== Firewall Rules ===",
		"=== Civ6 Logs",
		"=== Civ6Relay Logs",
		"=== Civ6Relay Captures",
		"Skipped large PCAP: huge.pcap",
	} {
		if !strings.Contains(envText, section) {
			t.Fatalf("env_check.txt missing %q", section)
		}
	}
}
