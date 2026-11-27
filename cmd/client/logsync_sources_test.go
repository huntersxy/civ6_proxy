//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoverLocalLogSourcesAndEnvCheckParity(t *testing.T) {
	localApp := t.TempDir()
	roamingApp := t.TempDir()
	tmpDir := t.TempDir()
	t.Setenv("LOCALAPPDATA", localApp)
	t.Setenv("APPDATA", roamingApp)
	t.Setenv("TEMP", tmpDir)
	t.Setenv("TMP", tmpDir)

	civLogs := filepath.Join(localApp, "Firaxis Games", "Sid Meier's Civilization VI", "Logs")
	relayLogs := filepath.Join(localApp, "Civ6Relay", "logs")
	relayCaps := filepath.Join(localApp, "Civ6Relay", "captures")
	relayCfgDir := filepath.Join(roamingApp, "Civ6Relay")
	for _, d := range []string{civLogs, relayLogs, relayCaps, relayCfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Dynamic Civ 6 logs matching Sync|Desync|C6SE plus an unrelated file.
	for _, name := range []string{"Civ6SyncFix.log", "Custom_Desync_Dump.log", "C6SE_loader.log", "unrelated.txt"} {
		if err := os.WriteFile(filepath.Join(civLogs, name), []byte("data-"+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 5 relay logs -> should pick only the newest 4.
	baseTime := time.Now().Add(-10 * time.Minute)
	for i := 0; i < 5; i++ {
		p := filepath.Join(relayLogs, fmt.Sprintf("client-2026112%d.log", i))
		if err := os.WriteFile(p, []byte("relay-log\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := baseTime.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(p, mt, mt)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "Civ6RelayDiag.txt"), []byte("diag"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relayCfgDir, "config.json"), []byte(`{"relay_host":"1.2.3.4"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3 PCAPs: newest small (1KB), second huge (35MB > 30MB cap), third small (2KB, skipped because only top 2 considered).
	p1 := filepath.Join(relayCaps, "newest.pcap")
	p2 := filepath.Join(relayCaps, "huge.pcap")
	p3 := filepath.Join(relayCaps, "oldest.pcap")
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
	_ = os.Chtimes(p3, baseTime, baseTime)
	_ = os.Chtimes(p2, baseTime.Add(1*time.Minute), baseTime.Add(1*time.Minute))
	_ = os.Chtimes(p1, baseTime.Add(2*time.Minute), baseTime.Add(2*time.Minute))

	specs := discoverLocalLogSources()
	got := make(map[string]bool, len(specs))
	for _, s := range specs {
		got[s.RelPath] = true
	}

	for _, want := range []string{
		"civ6_logs/Civ6SyncFix.log",
		"civ6_logs/net_message_debug.log",
		"civ6_logs/Custom_Desync_Dump.log",
		"civ6_logs/C6SE_loader.log",
		"relay_logs/client-20261124.log",
		"relay_logs/client-20261123.log",
		"relay_logs/client-20261122.log",
		"relay_logs/client-20261121.log",
		"relay_logs/Civ6RelayDiag.txt",
		"relay_logs/config.json",
		"relay_captures/newest.pcap",
	} {
		if !got[want] {
			t.Fatalf("expected source %q in discovered list, got %v", want, got)
		}
	}
	for _, unwanted := range []string{
		"civ6_logs/unrelated.txt",
		"relay_logs/client-20261120.log",
		"relay_captures/huge.pcap",
		"relay_captures/oldest.pcap",
	} {
		if got[unwanted] {
			t.Fatalf("unexpected source %q in discovered list", unwanted)
		}
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
