//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"civ6relay/internal/logsync"
	"civ6relay/internal/win"
)

const (
	legacyDX12SyncFixWorkshopID = "3823991616"
	civ6InitialTailBytes        = 8 << 20  // tail latest 8 MiB if active log is already huge at sync start
	civ6SegmentMaxBytes         = 32 << 20 // rotate remote segment every 32 MiB during marathon games
	civ6MaxBacklogBytes         = 16 << 20 // fast-forward if unread burst backlog exceeds 16 MiB
	civ6BacklogTailBytes        = 4 << 20  // retain latest 4 MiB after a burst fast-forward
	relayLogInitialTailBytes    = 512 << 10
	relayLogSegmentMaxBytes     = 16 << 20
	relayLogMaxBacklogBytes     = 4 << 20
	relayLogBacklogTailBytes    = 512 << 10
	maxTotalPCAPCollectBytes    = 30 << 20
	sessionCutoffGrace          = 5 * time.Second
	pcapSessionGrace            = 30 * time.Second
	relayLogActiveGrace         = 5 * time.Minute
	relayLogLookback            = 24 * time.Hour
)

var civ6StartTimeFn = win.FindCiv6EarliestStartTime

var fixedCiv6LogNames = []string{
	"Civ6SyncFix.log",
	"FastSync.log",
	"net_message_debug.log",
	"net_connection_debug.log",
	"Modding.log",
	"Lua.log",
	"Database.log",
	"Renderer.log",
	"App.log",
}

func collectedLogsDirectory() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "Civ6Relay", "collected_logs")
}

func localLogPaths() (civLogs, relayLogs, relayCaptures, relayRoaming, diagReport string) {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		localAppData = os.TempDir()
	}
	civLogs = filepath.Join(localAppData, "Firaxis Games", "Sid Meier's Civilization VI", "Logs")
	relayLocal := filepath.Join(localAppData, "Civ6Relay")
	relayLogs = filepath.Join(relayLocal, "logs")
	relayCaptures = filepath.Join(relayLocal, "captures")
	roaming := os.Getenv("APPDATA")
	if roaming == "" {
		home, _ := os.UserHomeDir()
		roaming = filepath.Join(home, "AppData", "Roaming")
	}
	relayRoaming = filepath.Join(roaming, "Civ6Relay")
	diagReport = filepath.Join(os.TempDir(), "Civ6RelayDiag.txt")
	return
}

func localIdentity(memberName string) (computer, user, displayName string) {
	computer = strings.TrimSpace(os.Getenv("COMPUTERNAME"))
	if computer == "" {
		if h, err := os.Hostname(); err == nil {
			computer = strings.TrimSpace(h)
		}
	}
	user = strings.TrimSpace(os.Getenv("USERNAME"))
	displayName = strings.TrimSpace(memberName)
	return
}

// buildLocalWatcherConfig creates a WatcherConfig with session-aware filtering,
// dormant pre-session tracking, large-file tailing, and segment rotation.
func buildLocalWatcherConfig() logsync.WatcherConfig {
	return logsync.WatcherConfig{
		DiscoverFilesForSession: discoverLocalLogSourcesForSession,
		GenerateEnv:             generateLocalEnvCheck,
	}
}

func discoverLocalLogSourcesForSession(sessionStart time.Time) []logsync.SourceSpec {
	if sessionStart.IsZero() {
		sessionStart = time.Now()
	}
	civLogs, relayLogs, relayCaptures, relayRoaming, diagReport := localLogPaths()
	var specs []logsync.SourceSpec

	// Determine freshness cutoff for Civ 6 logs:
	// - Game not running yet -> park pre-session files dormant (0 bytes sent) until Civ 6 writes/truncates them.
	// - Game already running -> sync files touched since Civ 6 started (keeps earlier Modding.log of this game)
	//   while keeping untouched leftovers from older sessions dormant.
	civStart, civRunning := civ6StartTimeFn()
	civ6Cutoff := sessionStart.Add(-sessionCutoffGrace)
	if civRunning {
		if !civStart.IsZero() {
			civ6Cutoff = civStart.Add(-sessionCutoffGrace)
		} else {
			civ6Cutoff = time.Time{}
		}
	}

	makeCiv6Spec := func(name string) logsync.SourceSpec {
		return logsync.SourceSpec{
			RelPath:          "civ6_logs/" + name,
			FullPath:         filepath.Join(civLogs, name),
			SkipIfOlderThan:  civ6Cutoff,
			InitialMaxBytes:  civ6InitialTailBytes,
			SegmentMaxBytes:  civ6SegmentMaxBytes,
			MaxBacklogBytes:  civ6MaxBacklogBytes,
			BacklogTailBytes: civ6BacklogTailBytes,
		}
	}

	// 1. Civ 6 logs (fixed targets + dynamic Sync|Desync|C6SE; large files are tracked too so
	// they stay dormant if stale, tail cleanly if active, and reset immediately if rewritten smaller).
	seenCiv := make(map[string]struct{}, len(fixedCiv6LogNames)+8)
	for _, name := range fixedCiv6LogNames {
		lower := strings.ToLower(name)
		seenCiv[lower] = struct{}{}
		specs = append(specs, makeCiv6Spec(name))
	}
	if entries, err := os.ReadDir(civLogs); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			lower := strings.ToLower(name)
			if _, dup := seenCiv[lower]; dup {
				continue
			}
			if !strings.Contains(lower, "sync") && !strings.Contains(lower, "desync") && !strings.Contains(lower, "c6se") {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			seenCiv[lower] = struct{}{}
			specs = append(specs, makeCiv6Spec(name))
		}
	}

	// 2. Civ6Relay logs: only current/recent (within 24h, up to 2 files); active file tails last 512 KiB,
	// older inactive files stay dormant unless written during this session.
	relayCutoff := sessionStart.Add(-relayLogLookback)
	relayActiveCutoff := sessionStart.Add(-relayLogActiveGrace)
	for idx, fi := range recentMatchingFiles(relayLogs, func(name string) bool {
		lower := strings.ToLower(name)
		return strings.HasPrefix(lower, "client-") && strings.Contains(lower, ".log")
	}, 2) {
		if fi.modTime.Before(relayCutoff) {
			continue
		}
		skipOlder := relayActiveCutoff
		if idx == 0 && !fi.modTime.Before(relayActiveCutoff) {
			skipOlder = time.Time{}
		}
		specs = append(specs, logsync.SourceSpec{
			RelPath:          "relay_logs/" + fi.name,
			FullPath:         fi.fullPath,
			SkipIfOlderThan:  skipOlder,
			InitialMaxBytes:  relayLogInitialTailBytes,
			SegmentMaxBytes:  relayLogSegmentMaxBytes,
			MaxBacklogBytes:  relayLogMaxBacklogBytes,
			BacklogTailBytes: relayLogBacklogTailBytes,
		})
	}
	specs = append(specs,
		logsync.SourceSpec{
			RelPath:  "relay_logs/Civ6RelayDiag.txt",
			FullPath: diagReport,
			Snapshot: true,
		},
		logsync.SourceSpec{
			RelPath:  "relay_logs/config.json",
			FullPath: filepath.Join(relayRoaming, "config.json"),
			Snapshot: true,
		},
	)

	// 3. Current-session PCAP captures only (modified on or after sessionStart - 30s, <= 30 MiB total).
	pcapCutoff := sessionStart.Add(-pcapSessionGrace)
	var pcapTotal int64
	for _, fi := range recentMatchingFiles(relayCaptures, func(name string) bool {
		return strings.EqualFold(filepath.Ext(name), ".pcap")
	}, 2) {
		if fi.modTime.Before(pcapCutoff) {
			continue
		}
		if pcapTotal+fi.size > maxTotalPCAPCollectBytes {
			continue
		}
		remaining := maxTotalPCAPCollectBytes - pcapTotal
		pcapTotal += fi.size
		specs = append(specs, logsync.SourceSpec{
			RelPath:  "relay_captures/" + fi.name,
			FullPath: fi.fullPath,
			Binary:   true,
			MaxBytes: remaining,
		})
	}

	return specs
}

type matchedFile struct {
	name     string
	fullPath string
	size     int64
	modTime  time.Time
}

func recentMatchingFiles(dir string, match func(string) bool, limit int) []matchedFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []matchedFile
	for _, entry := range entries {
		if entry.IsDir() || !match(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, matchedFile{
			name:     entry.Name(),
			fullPath: filepath.Join(dir, entry.Name()),
			size:     info.Size(),
			modTime:  info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].modTime.Equal(out[j].modTime) {
			return out[i].modTime.After(out[j].modTime)
		}
		return out[i].name < out[j].name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func generateLocalEnvCheck() []byte {
	civLogs, relayLogs, relayCaptures, _, _ := localLogPaths()
	computer, user, _ := localIdentity("")

	var b strings.Builder
	b.WriteString("\uFEFF")
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\r\n", args...)
	}

	line("CollectedAt: %s", time.Now().Format("2006-01-02 15:04:05 -07:00"))
	line("Computer: %s", computer)
	line("User: %s", user)
	line("OS: %s", win.OSVersion())
	line("IsAdmin: %v", win.IsAdmin())

	line("")
	line("=== Running Processes ===")
	procs := win.FindRelevantProcesses()
	if len(procs) == 0 {
		line("(None)")
	} else {
		sort.Slice(procs, func(i, j int) bool {
			if procs[i].Name != procs[j].Name {
				return procs[i].Name < procs[j].Name
			}
			return procs[i].PID < procs[j].PID
		})
		for _, p := range procs {
			line("%s (PID=%d)", strings.TrimSuffix(p.Name, filepath.Ext(p.Name)), p.PID)
		}
	}

	line("")
	line("=== Civ6SyncFix Mod Status ===")
	localModsDir, modErr := defaultCiv6ModsRoot()
	if modErr != nil {
		line("LocalModsDir: (Error: %v)", modErr)
	} else {
		syncFixLocalDir := filepath.Join(localModsDir, syncFixModFolderName)
		line("LocalModsDir: %s (Exists=%v)", localModsDir, pathExists(localModsDir))
		line("LocalCiv6SyncFix: %s (Exists=%v)", syncFixLocalDir, pathExists(syncFixLocalDir))
		if pathExists(syncFixLocalDir) {
			var relFiles []string
			_ = filepath.WalkDir(syncFixLocalDir, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if rel, rerr := filepath.Rel(syncFixLocalDir, p); rerr == nil {
					relFiles = append(relFiles, rel)
				}
				return nil
			})
			sort.Strings(relFiles)
			for _, rel := range relFiles {
				full := filepath.Join(syncFixLocalDir, rel)
				size, hash := fileSizeAndSHA256(full)
				line("  LocalFile: %s (%d bytes, SHA256=%s)", rel, size, hash)
			}
		}
	}
	steamRoots := defaultSteamLibraryRoots()
	for _, ws := range []struct {
		id    string
		label string
	}{
		{syncFixWorkshopID, "Civ6SyncFix(Workshop)"},
		{legacyDX12SyncFixWorkshopID, "Legacy-DX12-SyncFix(3823991616)"},
	} {
		if wsDir, ok := findWorkshopModDir(steamRoots, civ6SteamAppID, ws.id); ok {
			line("  Found %s: %s", ws.label, wsDir)
		}
	}

	line("")
	line("=== WinDivert Driver & Service ===")
	seenDrv := make(map[string]struct{})
	for _, base := range []string{os.Getenv("ProgramData"), os.Getenv("LOCALAPPDATA")} {
		if strings.TrimSpace(base) == "" {
			continue
		}
		drv := filepath.Clean(filepath.Join(base, "Civ6Relay", "windivert-2.2.2"))
		key := strings.ToLower(drv)
		if _, dup := seenDrv[key]; dup {
			continue
		}
		seenDrv[key] = struct{}{}
		entries, err := os.ReadDir(drv)
		if err != nil {
			continue
		}
		line("DriverDir: %s", drv)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			full := filepath.Join(drv, entry.Name())
			size, hash := fileSizeAndSHA256(full)
			line("  %s (%d bytes, SHA256=%s)", entry.Name(), size, hash)
		}
	}
	line("--- sc.exe qc WinDivert ---")
	if out, _ := win.RunHiddenCommand(3*time.Second, "sc.exe", "qc", "WinDivert"); strings.TrimSpace(out) != "" {
		line("%s", normalizeCRLF(strings.TrimSpace(out)))
	}
	line("--- sc.exe query WinDivert ---")
	if out, _ := win.RunHiddenCommand(3*time.Second, "sc.exe", "query", "WinDivert"); strings.TrimSpace(out) != "" {
		line("%s", normalizeCRLF(strings.TrimSpace(out)))
	}

	line("")
	line("=== Firewall Rules ===")
	for _, ruleName := range []string{"Civ6Relay-Game-Inbound", "Civ6Relay-P2P-Inbound", "Civ6Relay-P2P-Outbound"} {
		line("--- %s ---", ruleName)
		out, _ := win.RunHiddenCommand(3*time.Second, "netsh", "advfirewall", "firewall", "show", "rule", "name="+ruleName)
		line("%s", normalizeCRLF(strings.TrimSpace(out)))
	}

	writeDirListing(&b, "Civ6 Logs", civLogs)
	writeDirListing(&b, "Civ6Relay Logs", relayLogs)
	writeDirListing(&b, "Civ6Relay Captures", relayCaptures)

	// Record any skipped large PCAPs just like collect_teammate_logs.bat.
	var pcapTotal int64
	for _, fi := range recentMatchingFiles(relayCaptures, func(name string) bool {
		return strings.EqualFold(filepath.Ext(name), ".pcap")
	}, 2) {
		if pcapTotal+fi.size <= maxTotalPCAPCollectBytes {
			pcapTotal += fi.size
		} else {
			line("Skipped large PCAP: %s (%d bytes)", fi.name, fi.size)
		}
	}

	return []byte(b.String())
}

func writeDirListing(b *strings.Builder, title, dir string) {
	fmt.Fprintf(b, "\r\n=== %s (%s) ===\r\n", title, dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		b.WriteString("(Not found)\r\n")
		return
	}
	if len(entries) == 0 {
		b.WriteString("(Empty)\r\n")
		return
	}
	type item struct {
		name    string
		isDir   bool
		size    int64
		modTime time.Time
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{
			name:    e.Name(),
			isDir:   e.IsDir(),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].modTime.Equal(items[j].modTime) {
			return items[i].modTime.After(items[j].modTime)
		}
		return items[i].name < items[j].name
	})
	for _, it := range items {
		sizeStr := fmt.Sprintf("%10d", it.size)
		if it.isDir {
			sizeStr = "     <DIR>"
		}
		fmt.Fprintf(b, "%s  %s  %s\r\n", it.modTime.Format("2006-01-02 15:04:05"), sizeStr, it.name)
	}
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fileSizeAndSHA256(p string) (int64, string) {
	f, err := os.Open(p)
	if err != nil {
		return 0, ""
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return n, ""
	}
	return n, strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
}

func normalizeCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
