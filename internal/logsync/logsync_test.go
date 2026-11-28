package logsync

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"civ6relay/internal/protocol"
	"civ6relay/internal/relay"
	"civ6relay/internal/tunnel"
)

func TestProtocolRoundTripAndPathValidation(t *testing.T) {
	// Valid paths
	for _, valid := range []string{
		"env_check.txt",
		"civ6_logs/Civ6SyncFix.log",
		"civ6_logs/net_message_debug.log",
		"relay_logs/client-20261127.log",
		"relay_logs/config.json",
		"relay_captures/civ6relay-20261127-23-ROOM.pcap",
	} {
		got, err := ValidateRelPath(valid)
		if err != nil || got != valid {
			t.Fatalf("ValidateRelPath(%q) = (%q, %v), want valid", valid, got, err)
		}
	}

	// Rejected paths
	for _, bad := range []string{
		"",
		"../secret.txt",
		"civ6_logs/../evil.txt",
		"civ6_logs/sub/a.log",
		`civ6_logs\a.log`,
		"other_dir/a.log",
		"civ6_logs/CON.log",
		"civ6_logs/NUL",
		"civ6_logs/bad:name.log",
		"civ6_logs/trailingdot.",
		"civ6_logs/ trailingspace",
		"C:/Windows/win.ini",
	} {
		if _, err := ValidateRelPath(bad); err == nil {
			t.Fatalf("ValidateRelPath(%q) unexpectedly succeeded", bad)
		}
	}

	// SanitizeDirComponent
	if got := SanitizeDirComponent(`瓜皮/CON:<>?*`, "Player"); got != "瓜皮_CON" {
		t.Fatalf("SanitizeDirComponent got %q", got)
	}
	if got := SanitizeDirComponent("CON", "Player"); got != "_CON" {
		t.Fatalf("SanitizeDirComponent reserved got %q", got)
	}
	if got := SanitizeDirComponent("   ...  ", "Fallback"); got != "Fallback" {
		t.Fatalf("SanitizeDirComponent empty got %q", got)
	}

	// Chunk round-trip
	chunk := ChunkMessage{
		Nonce:    0x1122334455667788,
		SenderID: 0x8877665544332211,
		Seq:      42,
		Flags:    ChunkFlagReset | ChunkFlagSnapshot,
		Offset:   0,
		RelPath:  "civ6_logs/Civ6SyncFix.log",
		Data:     []byte("hello syncfix\n"),
	}
	raw, err := EncodeChunk(chunk)
	if err != nil {
		t.Fatalf("EncodeChunk: %v", err)
	}
	decoded, err := DecodeChunk(raw)
	if err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}
	if decoded.Nonce != chunk.Nonce || decoded.SenderID != chunk.SenderID ||
		decoded.Seq != chunk.Seq || decoded.Flags != chunk.Flags ||
		decoded.Offset != chunk.Offset || decoded.RelPath != chunk.RelPath ||
		!bytes.Equal(decoded.Data, chunk.Data) {
		t.Fatalf("chunk mismatch: got %+v want %+v", decoded, chunk)
	}

	// Ack round-trip
	ack := AckMessage{
		Nonce:    chunk.Nonce,
		SenderID: chunk.SenderID,
		CumAck:   10,
		SackMask: (1 << 0) | (1 << 3) | (1 << 63),
		Window:   32,
	}
	ackDec, err := DecodeAck(EncodeAck(ack))
	if err != nil {
		t.Fatalf("DecodeAck: %v", err)
	}
	if ackDec != ack {
		t.Fatalf("ack mismatch: got %+v want %+v", ackDec, ack)
	}
}

func startLossyRelay(t *testing.T, dropPct int) (string, int, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	srv := relay.New(relay.Config{Logger: log.New(io.Discard, "", 0)})
	rng := rand.New(rand.NewSource(20261127))
	var delayedMu sync.Mutex
	var delayed []relay.Outbound
	done := make(chan struct{})

	go func() {
		buf := make([]byte, 4096)
		var batch relay.Batch
		for {
			n, peer, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			srv.HandleInto(buf[:n], peer, &batch)
			for _, out := range batch.Datagrams {
				// Only inject loss/reordering on DATA frames so room handshakes stay snappy.
				isData := len(out.Data) > 5 && protocol.Kind(out.Data[5]) == protocol.KindData
				if isData && dropPct > 0 {
					r := rng.Intn(100)
					if r < dropPct {
						continue
					}
					if r < dropPct*2 {
						delayedMu.Lock()
						delayed = append(delayed, relay.Outbound{Addr: out.Addr, Data: bytes.Clone(out.Data)})
						delayedMu.Unlock()
						continue
					}
				}
				_, _ = conn.WriteToUDPAddrPort(out.Data, out.Addr)
				delayedMu.Lock()
				if len(delayed) > 0 {
					d := delayed[0]
					delayed = delayed[1:]
					delayedMu.Unlock()
					_, _ = conn.WriteToUDPAddrPort(d.Data, d.Addr)
				} else {
					delayedMu.Unlock()
				}
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				delayedMu.Lock()
				pending := delayed
				delayed = nil
				delayedMu.Unlock()
				for _, d := range pending {
					_, _ = conn.WriteToUDPAddrPort(d.Data, d.Addr)
				}
			}
		}
	}()

	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String(), addr.Port, func() {
		close(done)
		_ = conn.Close()
	}
}

func waitUntil(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", desc)
}

func makeTempMemberSources(t *testing.T, label string) (dir string, cfg WatcherConfig, appendCiv func(string), truncateCiv func(string), updateConfig func(string)) {
	t.Helper()
	dir = t.TempDir()
	civDir := filepath.Join(dir, "civ6")
	relayDir := filepath.Join(dir, "relay")
	capDir := filepath.Join(dir, "captures")
	for _, d := range []string{civDir, relayDir, capDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	civFile := filepath.Join(civDir, "Civ6SyncFix.log")
	netFile := filepath.Join(civDir, "net_message_debug.log")
	cfgFile := filepath.Join(relayDir, "config.json")
	pcapFile := filepath.Join(capDir, "sample.pcap")

	// Initial content spanning multiple chunks (> 12 KiB for netFile)
	initCiv := fmt.Sprintf("[%s] Civ6SyncFix v1.5 loaded\n", label)
	var bigNet strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&bigNet, "[%s] Turn %03d NetGameSyncCheck hash=0x%016x\n", label, i, uint64(i)*2654435761)
	}
	initCfg := fmt.Sprintf(`{"display_name":%q,"v":1}`, label)
	initPcap := bytes.Repeat([]byte{0xd4, 0xc3, 0xb2, 0xa1, 0x02, 0x00, 0x04, 0x00, 0x11, 0x22, 0x33, 0x44}, 400) // 4800B binary

	if err := os.WriteFile(civFile, []byte(initCiv), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(netFile, []byte(bigNet.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgFile, []byte(initCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pcapFile, initPcap, 0o644); err != nil {
		t.Fatal(err)
	}

	var envGen atomic.Int32
	envGen.Store(1)

	cfg = WatcherConfig{
		DiscoverInterval: 50 * time.Millisecond,
		EnvInterval:      100 * time.Millisecond,
		DiscoverFiles: func() []SourceSpec {
			return []SourceSpec{
				{RelPath: "civ6_logs/Civ6SyncFix.log", FullPath: civFile},
				{RelPath: "civ6_logs/net_message_debug.log", FullPath: netFile},
				{RelPath: "relay_logs/config.json", FullPath: cfgFile, Snapshot: true},
				{RelPath: "relay_captures/sample.pcap", FullPath: pcapFile, Binary: true},
			}
		},
		GenerateEnv: func() []byte {
			return []byte(fmt.Sprintf("CollectedAt: %s\r\nComputer: %s-PC\r\nGen: %d\r\n",
				time.Now().Format(time.RFC3339), label, envGen.Load()))
		},
	}

	appendCiv = func(extra string) {
		f, err := os.OpenFile(civFile, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(extra)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	truncateCiv = func(fresh string) {
		if err := os.WriteFile(civFile, []byte(fresh), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	updateConfig = func(fresh string) {
		envGen.Add(1)
		if err := os.WriteFile(cfgFile, []byte(fresh), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestEndToEndRealTimeStreamingOverLossyRelay(t *testing.T) {
	hostIP, port, stopRelay := startLossyRelay(t, 10) // 10% packet loss + 10% reordering
	defer stopRelay()

	// Create room with host + 2 guests.
	hostTunnel, err := tunnel.New(hostIP, port)
	if err != nil {
		t.Fatal(err)
	}
	defer hostTunnel.Disconnect()
	wHost, err := hostTunnel.CreateRoom("房主A", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	g1Tunnel, err := tunnel.New(hostIP, port)
	if err != nil {
		t.Fatal(err)
	}
	defer g1Tunnel.Disconnect()
	if _, err := g1Tunnel.JoinRoom(wHost.RoomCode, "瓜皮", 3*time.Second); err != nil {
		t.Fatal(err)
	}

	g2Tunnel, err := tunnel.New(hostIP, port)
	if err != nil {
		t.Fatal(err)
	}
	defer g2Tunnel.Disconnect()
	if _, err := g2Tunnel.JoinRoom(wHost.RoomCode, "队友B", 3*time.Second); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 3*time.Second, "all 3 members in peer list", func() bool {
		return len(hostTunnel.Peers()) == 3 && len(g1Tunnel.Peers()) == 3 && len(g2Tunnel.Peers()) == 3
	})

	// Ensure game data handler never receives aux packets.
	var leakedGame atomic.Int64
	hostTunnel.SetDataHandler(func(uint8, uint16, uint16, []byte) { leakedGame.Add(1) })
	g1Tunnel.SetDataHandler(func(uint8, uint16, uint16, []byte) { leakedGame.Add(1) })
	g2Tunnel.SetDataHandler(func(uint8, uint16, uint16, []byte) { leakedGame.Add(1) })

	hostSrcDir, hostWatcherCfg, hostAppend, _, _ := makeTempMemberSources(t, "HOST")
	g1SrcDir, g1WatcherCfg, g1Append, g1Truncate, g1UpdateCfg := makeTempMemberSources(t, "GUAPI")
	g2SrcDir, g2WatcherCfg, g2Append, _, _ := makeTempMemberSources(t, "TEAMB")

	g1Ctrl := NewGuestController(GuestConfig{
		Send:          g1Tunnel.SendAuxData,
		HostID:        g1Tunnel.HostMemberID,
		Identity:      func() (string, string, string) { return "ERYA-D", "erya", "瓜皮" },
		WatcherCfg:    g1WatcherCfg,
		PollInterval:  50 * time.Millisecond,
		HelloInterval: 100 * time.Millisecond,
		RTOTick:       40 * time.Millisecond,
	})
	defer g1Ctrl.Close()
	g1Tunnel.SetAuxHandler(g1Ctrl.HandleAux)

	g2Ctrl := NewGuestController(GuestConfig{
		Send:          g2Tunnel.SendAuxData,
		HostID:        g2Tunnel.HostMemberID,
		Identity:      func() (string, string, string) { return "LAPTOP-B", "teamb", "队友B" },
		WatcherCfg:    g2WatcherCfg,
		PollInterval:  50 * time.Millisecond,
		HelloInterval: 100 * time.Millisecond,
		RTOTick:       40 * time.Millisecond,
	})
	defer g2Ctrl.Close()
	g2Tunnel.SetAuxHandler(g2Ctrl.HandleAux)

	baseDir := filepath.Join(t.TempDir(), "collected_logs")
	hostCtrl, err := StartHostController(HostConfig{
		BaseDir:           baseDir,
		RoomCode:          wHost.RoomCode,
		RelayAddr:         fmt.Sprintf("%s:%d", hostIP, port),
		HostMemberID:      hostTunnel.MemberID(),
		HostName:          "房主A",
		HostComputer:      "HOST-PC",
		Send:              hostTunnel.SendAuxData,
		Peers:             hostTunnel.Peers,
		HostWatcherCfg:    hostWatcherCfg,
		PollInterval:      50 * time.Millisecond,
		ProbeInterval:     150 * time.Millisecond,
		KeepaliveInterval: 400 * time.Millisecond,
		AckDelay:          20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	hostTunnel.SetAuxHandler(hostCtrl.HandleAux)

	// Wait for initial sync of host + both guests.
	sessionDir := hostCtrl.SessionDir()
	var hostMemberDir, g1MemberDir, g2MemberDir string
	waitUntil(t, 8*time.Second, "initial sync of host + 2 guests", func() bool {
		st := hostCtrl.Status()
		if st.ActivePeers != 2 || st.HostFiles < 5 {
			return false
		}
		entries, err := os.ReadDir(sessionDir)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			switch {
			case strings.HasPrefix(name, "01_房主_"):
				hostMemberDir = filepath.Join(sessionDir, name)
			case strings.Contains(name, "瓜皮"):
				g1MemberDir = filepath.Join(sessionDir, name)
			case strings.Contains(name, "队友B"):
				g2MemberDir = filepath.Join(sessionDir, name)
			}
		}
		if hostMemberDir == "" || g1MemberDir == "" || g2MemberDir == "" {
			return false
		}
		return filesEqual(filepath.Join(hostSrcDir, "civ6", "net_message_debug.log"), filepath.Join(hostMemberDir, "civ6_logs", "net_message_debug.log")) &&
			filesEqual(filepath.Join(g1SrcDir, "civ6", "net_message_debug.log"), filepath.Join(g1MemberDir, "civ6_logs", "net_message_debug.log")) &&
			filesEqual(filepath.Join(g2SrcDir, "civ6", "net_message_debug.log"), filepath.Join(g2MemberDir, "civ6_logs", "net_message_debug.log")) &&
			filesEqual(filepath.Join(g1SrcDir, "captures", "sample.pcap"), filepath.Join(g1MemberDir, "relay_captures", "sample.pcap")) &&
			filesEqual(filepath.Join(g2SrcDir, "captures", "sample.pcap"), filepath.Join(g2MemberDir, "relay_captures", "sample.pcap"))
	})

	// 1. Live real-time append on host & both guests.
	hostAppend("[HOST] Turn 27 live host line\n")
	g1Append("[GUAPI] Turn 27 live guest1 line\n")
	g2Append("[TEAMB] Turn 27 live guest2 line\n")
	g1UpdateCfg(`{"display_name":"GUAPI","v":2}`)

	waitUntil(t, 6*time.Second, "live appended lines and config snapshot", func() bool {
		return filesEqual(filepath.Join(hostSrcDir, "civ6", "Civ6SyncFix.log"), filepath.Join(hostMemberDir, "civ6_logs", "Civ6SyncFix.log")) &&
			filesEqual(filepath.Join(g1SrcDir, "civ6", "Civ6SyncFix.log"), filepath.Join(g1MemberDir, "civ6_logs", "Civ6SyncFix.log")) &&
			filesEqual(filepath.Join(g2SrcDir, "civ6", "Civ6SyncFix.log"), filepath.Join(g2MemberDir, "civ6_logs", "Civ6SyncFix.log")) &&
			filesEqual(filepath.Join(g1SrcDir, "relay", "config.json"), filepath.Join(g1MemberDir, "relay_logs", "config.json"))
	})

	// 2. Simulate Civ 6 crash & restart on Guest 1 (file truncated & rewritten).
	preCrashBytes, err := os.ReadFile(filepath.Join(g1SrcDir, "civ6", "Civ6SyncFix.log"))
	if err != nil {
		t.Fatal(err)
	}
	g1Truncate("[GUAPI] Restarted Civ6 fresh log!\n")

	waitUntil(t, 6*time.Second, "truncated file mirrored and .prev archived", func() bool {
		if !filesEqual(filepath.Join(g1SrcDir, "civ6", "Civ6SyncFix.log"), filepath.Join(g1MemberDir, "civ6_logs", "Civ6SyncFix.log")) {
			return false
		}
		matches, _ := filepath.Glob(filepath.Join(g1MemberDir, "civ6_logs", "Civ6SyncFix.log.prev-*"))
		if len(matches) == 0 {
			return false
		}
		prevData, err := os.ReadFile(matches[0])
		return err == nil && bytes.Equal(prevData, preCrashBytes)
	})

	// Verify env_check.txt exists for all 3 members.
	for _, memberDir := range []string{hostMemberDir, g1MemberDir, g2MemberDir} {
		data, err := os.ReadFile(filepath.Join(memberDir, "env_check.txt"))
		if err != nil || !strings.Contains(string(data), "Computer:") {
			t.Fatalf("missing/invalid env_check.txt in %s: err=%v", memberDir, err)
		}
	}

	// Verify zero game bridge leaks and zero game datagram counter pollution.
	if leakedGame.Load() != 0 {
		t.Fatalf("aux frames leaked into game DataHandler: %d", leakedGame.Load())
	}
	if m := hostTunnel.Metrics(); m.GameRxDatagrams != 0 || m.GameTxDatagrams != 0 {
		t.Fatalf("aux frames polluted game metrics: rx=%d tx=%d", m.GameRxDatagrams, m.GameTxDatagrams)
	}

	// 3. Stop host collection -> guests stop automatically and session_info.txt is finalized.
	hostCtrl.Stop()
	waitUntil(t, 4*time.Second, "guests deactivate after host Stop", func() bool {
		return !g1Ctrl.Status().Active && !g2Ctrl.Status().Active
	})
	infoBytes, err := os.ReadFile(filepath.Join(sessionDir, "session_info.txt"))
	if err != nil {
		t.Fatalf("read session_info.txt: %v", err)
	}
	infoText := string(infoBytes)
	for _, want := range []string{wHost.RoomCode, "EndedAt:", "01_房主_", "瓜皮", "队友B"} {
		if !strings.Contains(infoText, want) {
			t.Fatalf("session_info.txt missing %q:\n%s", want, infoText)
		}
	}
}

func TestHostMarksUnsupportedOldClientWithoutSpam(t *testing.T) {
	var sentCount atomic.Int32
	baseDir := filepath.Join(t.TempDir(), "collected")
	peers := []protocol.Peer{
		{ID: 2, Name: "Host", Host: true, Online: true},
		{ID: 3, Name: "OldClient", Host: false, Online: true},
	}
	hc, err := StartHostController(HostConfig{
		BaseDir:       baseDir,
		RoomCode:      "OLDCLIENT1",
		HostMemberID:  2,
		HostName:      "Host",
		HostComputer:  "HOST-PC",
		Send:          func(uint8, []byte) bool { sentCount.Add(1); return true },
		Peers:         func() []protocol.Peer { return peers },
		PollInterval:  20 * time.Millisecond,
		ProbeInterval: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Stop()

	waitUntil(t, 2*time.Second, "peer marked unsupported", func() bool {
		for _, p := range hc.Status().Peers {
			if p.MemberID == 3 && p.State == "unsupported" {
				return true
			}
		}
		return false
	})
	probes := sentCount.Load()
	if probes != maxProbeAttempts {
		t.Fatalf("expected exactly %d probes, got %d", maxProbeAttempts, probes)
	}
	time.Sleep(150 * time.Millisecond)
	if after := sentCount.Load(); after != probes {
		t.Fatalf("expected no further probes after unsupported, grew %d -> %d", probes, after)
	}
}

func TestPruneOldSessions(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 11, 27, 23, 0, 0, 0, time.Local)

	mkSession := func(name string, age time.Duration) string {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return p
	}

	expired := mkSession("20261101_100000_OLDROOM001", 26*24*time.Hour)
	keepActive := mkSession("20261101_090000_ACTIVERM01", 26*24*time.Hour) // old mtime but active!
	s1 := mkSession("20261127_200000_ROOM000001", 3*time.Hour)
	s2 := mkSession("20261127_210000_ROOM000002", 2*time.Hour)
	s3 := mkSession("20261127_220000_ROOM000003", 1*time.Hour)
	unrelated := filepath.Join(base, "my_notes")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := PruneOldSessions(base, now, 14*24*time.Hour, 2, keepActive); err != nil {
		t.Fatalf("PruneOldSessions: %v", err)
	}

	mustExist := []string{keepActive, s2, s3, unrelated}
	for _, p := range mustExist {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s to remain, got err=%v", p, err)
		}
	}
	mustBeGone := []string{expired, s1}
	for _, p := range mustBeGone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be pruned, stat err=%v", p, err)
		}
	}
}

func TestMemberWriterSecurityAndQuota(t *testing.T) {
	mw, err := newMemberWriter(filepath.Join(t.TempDir(), "member"))
	if err != nil {
		t.Fatal(err)
	}
	defer mw.Close()

	if err := mw.WriteChunk(FileChunk{RelPath: "../escape.txt", Data: []byte("x")}, time.Now()); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
	if err := mw.WriteChunk(FileChunk{RelPath: "civ6_logs/a.log", Offset: MaxFileSizeBytes, Data: []byte("x")}, time.Now()); err == nil {
		t.Fatal("expected oversized offset to be rejected")
	}
}

func TestWatcherDormantOldBigFileThenTruncatedToSmall(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "net_message_debug.log")
	oldContent := bytes.Repeat([]byte("OLD-SESSION-LINE-0123456789abcdef\n"), 8000) // ~272 KiB
	if err := os.WriteFile(logPath, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}
	sessionStart := time.Now()
	oldTime := sessionStart.Add(-30 * time.Minute)
	_ = os.Chtimes(logPath, oldTime, oldTime)

	now := sessionStart
	w := NewWatcher(WatcherConfig{
		DiscoverInterval: time.Second,
		Now:              func() time.Time { return now },
		DiscoverFilesForSession: func(start time.Time) []SourceSpec {
			return []SourceSpec{{
				RelPath:         "civ6_logs/net_message_debug.log",
				FullPath:        logPath,
				SkipIfOlderThan: start.Add(-5 * time.Second),
				InitialMaxBytes: 16 << 10,
				SegmentMaxBytes: 64 << 10,
			}}
		},
	})

	hostDir := filepath.Join(t.TempDir(), "member")
	mw, err := newMemberWriter(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer mw.Close()

	// Pre-session large log stays dormant: 0 bytes sent across multiple polls.
	var emitted []FileChunk
	for i := 0; i < 3; i++ {
		w.Poll(func(ch FileChunk) bool {
			emitted = append(emitted, ch)
			return true
		})
	}
	if len(emitted) != 0 {
		t.Fatalf("expected 0 chunks emitted while dormant, got %d", len(emitted))
	}

	// Player launches Civ 6: file is first truncated to 0 bytes, then written with small fresh content,
	// and later overwritten in-place with same-size different content (must capture head fingerprint!).
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	w.Poll(func(ch FileChunk) bool {
		emitted = append(emitted, ch)
		if err := mw.WriteChunk(ch, now); err != nil {
			t.Fatalf("WriteChunk empty: %v", err)
		}
		return true
	})

	freshContent := "[NEW SESSION] Turn 1 GameInit\n[NEW SESSION] Turn 2 SyncOK\n"
	if err := os.WriteFile(logPath, []byte(freshContent), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	for i := 0; i < 4; i++ {
		w.Poll(func(ch FileChunk) bool {
			emitted = append(emitted, ch)
			if err := mw.WriteChunk(ch, now); err != nil {
				t.Fatalf("WriteChunk: %v", err)
			}
			return true
		})
	}
	if len(emitted) == 0 {
		t.Fatal("expected fresh chunks after truncation")
	}
	got, err := os.ReadFile(filepath.Join(hostDir, "civ6_logs", "net_message_debug.log"))
	if err != nil {
		t.Fatalf("read synced file: %v", err)
	}
	if string(got) != freshContent {
		t.Fatalf("synced content = %q, want %q", string(got), freshContent)
	}
	// Ensure no wasteful .prev-* artifact was created from the skipped dormant file.
	matches, _ := filepath.Glob(filepath.Join(hostDir, "civ6_logs", "net_message_debug.log.prev-*"))
	if len(matches) != 0 {
		t.Fatalf("expected 0 .prev-* files after waking dormant file, got %v", matches)
	}

	// Same-size in-place overwrite after 0-byte -> small transition must still trigger head-hash replacement.
	sameSizeFresh := "[ALT SESSION] Turn 1 GameInit\n[ALT SESSION] Turn 2 SyncOK\n"
	if len(sameSizeFresh) != len(freshContent) {
		t.Fatalf("test bug: lengths differ")
	}
	if err := os.WriteFile(logPath, []byte(sameSizeFresh), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	for i := 0; i < 4; i++ {
		w.Poll(func(ch FileChunk) bool {
			if err := mw.WriteChunk(ch, now); err != nil {
				t.Fatalf("WriteChunk same-size replace: %v", err)
			}
			return true
		})
	}
	got2, err := os.ReadFile(filepath.Join(hostDir, "civ6_logs", "net_message_debug.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != sameSizeFresh {
		t.Fatalf("same-size replace synced = %q, want %q", string(got2), sameSizeFresh)
	}
}

func TestWatcherDormantOldFileThenAppendedOnlySendsDelta(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "Lua.log")
	oldContent := bytes.Repeat([]byte("OLD-LUA-LINE-1234567890\n"), 3000) // ~72 KiB
	if err := os.WriteFile(logPath, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}
	sessionStart := time.Now()
	oldTime := sessionStart.Add(-15 * time.Minute)
	_ = os.Chtimes(logPath, oldTime, oldTime)

	w := NewWatcher(WatcherConfig{
		Now: func() time.Time { return sessionStart },
		DiscoverFilesForSession: func(start time.Time) []SourceSpec {
			return []SourceSpec{{
				RelPath:         "civ6_logs/Lua.log",
				FullPath:        logPath,
				SkipIfOlderThan: start.Add(-5 * time.Second),
				InitialMaxBytes: 16 << 10,
				SegmentMaxBytes: 64 << 10,
			}}
		},
	})

	hostDir := filepath.Join(t.TempDir(), "member")
	mw, err := newMemberWriter(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer mw.Close()

	// Initial poll parks file in dormant state.
	var totalSent int
	w.Poll(func(ch FileChunk) bool {
		totalSent += len(ch.Data)
		return true
	})
	if totalSent != 0 {
		t.Fatalf("expected 0 bytes while dormant, got %d", totalSent)
	}

	// Append new line to the same pre-session file without truncating.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	const deltaLine = "LIVE-APPEND-TURN-42\n"
	_, _ = f.WriteString(deltaLine)
	_ = f.Close()

	for i := 0; i < 4; i++ {
		w.Poll(func(ch FileChunk) bool {
			totalSent += len(ch.Data)
			if err := mw.WriteChunk(ch, sessionStart); err != nil {
				t.Fatalf("WriteChunk: %v", err)
			}
			return true
		})
	}
	if totalSent > 1024 {
		t.Fatalf("expected only banner + delta (<1 KiB), sent %d bytes", totalSent)
	}
	got, err := os.ReadFile(filepath.Join(hostDir, "civ6_logs", "Lua.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "[Civ6Relay] skipped pre-session") || !strings.HasSuffix(string(got), deltaLine) {
		t.Fatalf("unexpected delta output:\n%s", string(got))
	}
}

func TestWatcherActiveLargeFileInitialTailAndRestartToSmall(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "net_message_debug.log")
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "Turn %04d sync-line-payload-0123456789abcdef\n", i)
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	w := NewWatcher(WatcherConfig{
		Now: func() time.Time { return now },
		DiscoverFiles: func() []SourceSpec {
			return []SourceSpec{{
				RelPath:         "civ6_logs/net_message_debug.log",
				FullPath:        logPath,
				InitialMaxBytes: 8 << 10,
				SegmentMaxBytes: 32 << 10,
			}}
		},
	})

	hostDir := filepath.Join(t.TempDir(), "member")
	mw, err := newMemberWriter(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer mw.Close()

	for i := 0; i < 10; i++ {
		w.Poll(func(ch FileChunk) bool {
			if err := mw.WriteChunk(ch, now); err != nil {
				t.Fatalf("WriteChunk: %v", err)
			}
			return true
		})
	}

	got, err := os.ReadFile(filepath.Join(hostDir, "civ6_logs", "net_message_debug.log"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.HasPrefix(text, "[Civ6Relay] file was ") {
		t.Fatalf("expected initial tail banner, got prefix %q", text[:min(len(text), 80)])
	}
	if strings.Contains(text, "Turn 0000 ") {
		t.Fatalf("expected early Turn 0000 to be omitted from initial tail")
	}
	if !strings.Contains(text, "Turn 1999 ") {
		t.Fatalf("expected latest Turn 1999 in initial tail")
	}
	// Verify line alignment after the banner line.
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[1], "Turn ") {
		t.Fatalf("expected line-aligned tail after banner, got line[1]=%q", lines[1])
	}

	// Now player restarts game -> large file becomes small fresh file; DropPending must be set.
	const restarted = "Turn 0001 fresh-after-restart\n"
	if err := os.WriteFile(logPath, []byte(restarted), 0o644); err != nil {
		t.Fatal(err)
	}
	var sawDropPending bool
	now = now.Add(time.Second)
	for i := 0; i < 4; i++ {
		w.Poll(func(ch FileChunk) bool {
			if ch.DropPending {
				sawDropPending = true
			}
			if err := mw.WriteChunk(ch, now); err != nil {
				t.Fatalf("WriteChunk after restart: %v", err)
			}
			return true
		})
	}
	if !sawDropPending {
		t.Fatal("expected DropPending=true on mid-session file truncation/replacement")
	}
	gotAfter, err := os.ReadFile(filepath.Join(hostDir, "civ6_logs", "net_message_debug.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotAfter) != restarted {
		t.Fatalf("expected exact restarted content %q, got %q", restarted, string(gotAfter))
	}
	prevs, _ := filepath.Glob(filepath.Join(hostDir, "civ6_logs", "net_message_debug.log.prev-*"))
	if len(prevs) != 1 {
		t.Fatalf("expected 1 rotated .prev-* preserving active in-session tail, got %v", prevs)
	}
}

func TestWatcherSegmentRotationAndPrevPruning(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "net_message_debug.log")
	if err := os.WriteFile(logPath, []byte("SEGMENT-START\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	w := NewWatcher(WatcherConfig{
		Now: func() time.Time { return now },
		DiscoverFiles: func() []SourceSpec {
			return []SourceSpec{{
				RelPath:         "civ6_logs/net_message_debug.log",
				FullPath:        logPath,
				SegmentMaxBytes: 12 << 10,
			}}
		},
	})

	hostDir := filepath.Join(t.TempDir(), "member")
	mw, err := newMemberWriter(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer mw.Close()

	drain := func() {
		for i := 0; i < 20; i++ {
			progressed := false
			w.Poll(func(ch FileChunk) bool {
				progressed = true
				if err := mw.WriteChunk(ch, now); err != nil {
					t.Fatalf("WriteChunk: %v", err)
				}
				return true
			})
			if !progressed {
				break
			}
		}
	}

	drain()
	// Append 4 rounds of ~11 KiB (> 3 segment rotations total).
	for round := 1; round <= 4; round++ {
		now = now.Add(2 * time.Second)
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf("ROUND-%d-%s\n", round, strings.Repeat("X", 100))
		for j := 0; j < 105; j++ {
			_, _ = f.WriteString(line)
		}
		_, _ = fmt.Fprintf(f, "MARKER-END-ROUND-%d\n", round)
		_ = f.Close()
		drain()
	}

	active, err := os.ReadFile(filepath.Join(hostDir, "civ6_logs", "net_message_debug.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(active), "MARKER-END-ROUND-4") {
		t.Fatalf("expected active rotated segment to contain final round marker, got len=%d", len(active))
	}
	if !strings.Contains(string(active), "[Civ6Relay] log segment rotated;") {
		t.Fatalf("expected segment rotation banner in active file")
	}

	prevs, _ := filepath.Glob(filepath.Join(hostDir, "civ6_logs", "net_message_debug.log.prev-*"))
	if len(prevs) > maxPrevRotationsPerFile {
		t.Fatalf("expected at most %d kept .prev-* rotations, got %d (%v)", maxPrevRotationsPerFile, len(prevs), prevs)
	}
	if mw.liveDiskBytes() >= mw.totalWritten {
		t.Fatalf("expected liveDiskBytes (%d) < totalWritten (%d) after pruning old rotations", mw.liveDiskBytes(), mw.totalWritten)
	}
}

func TestWatcherBurstFastForwardAndRoundRobinFairness(t *testing.T) {
	dir := t.TempDir()
	luaPath := filepath.Join(dir, "Lua.log")
	netPath := filepath.Join(dir, "net_message_debug.log")
	modPath := filepath.Join(dir, "Modding.log")

	if err := os.WriteFile(luaPath, []byte("lua-init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(netPath, []byte("net-init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modPath, []byte("mod-init\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	w := NewWatcher(WatcherConfig{
		Now: func() time.Time { return now },
		DiscoverFiles: func() []SourceSpec {
			return []SourceSpec{
				{RelPath: "civ6_logs/Lua.log", FullPath: luaPath, SegmentMaxBytes: 64 << 10, MaxBacklogBytes: 24 << 10, BacklogTailBytes: 6 << 10},
				{RelPath: "civ6_logs/net_message_debug.log", FullPath: netPath, SegmentMaxBytes: 64 << 10},
				{RelPath: "civ6_logs/Modding.log", FullPath: modPath, SegmentMaxBytes: 64 << 10},
			}
		},
	})

	hostDir := filepath.Join(t.TempDir(), "member")
	mw, err := newMemberWriter(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer mw.Close()

	// Verify Modding.log has higher priority than Lua.log / net_message_debug.log.
	var firstPaths []string
	w.Poll(func(ch FileChunk) bool {
		firstPaths = append(firstPaths, ch.RelPath)
		if err := mw.WriteChunk(ch, now); err != nil {
			t.Fatal(err)
		}
		return true
	})
	if len(firstPaths) < 3 || firstPaths[0] != "civ6_logs/Modding.log" {
		t.Fatalf("expected Modding.log prioritized first, got order %v", firstPaths)
	}

	// Simulate sudden 80 KiB Lua spam burst + small critical net_message_debug line.
	fLua, err := os.OpenFile(luaPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1600; i++ {
		_, _ = fmt.Fprintf(fLua, "SPAM-LINE-%04d-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", i)
	}
	_, _ = fLua.WriteString("FINAL-LUA-ERROR-LINE\n")
	_ = fLua.Close()

	fNet, err := os.OpenFile(netPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fNet.WriteString("DESYNC_DELTA_CRITICAL\n")
	_ = fNet.Close()

	// Enforce tight backpressure (max 2 chunks per poll) to verify round-robin fairness.
	netSeenAtPoll := -1
	for poll := 1; poll <= 20; poll++ {
		accepted := 0
		w.Poll(func(ch FileChunk) bool {
			if accepted >= 2 {
				return false
			}
			accepted++
			if ch.RelPath == "civ6_logs/net_message_debug.log" && strings.Contains(string(ch.Data), "DESYNC_DELTA_CRITICAL") {
				if netSeenAtPoll < 0 {
					netSeenAtPoll = poll
				}
			}
			if err := mw.WriteChunk(ch, now); err != nil {
				t.Fatal(err)
			}
			return true
		})
	}
	if netSeenAtPoll < 0 || netSeenAtPoll > 3 {
		t.Fatalf("expected net_message_debug.log not starved under backpressure (seen at poll %d)", netSeenAtPoll)
	}

	luaSynced, err := os.ReadFile(filepath.Join(hostDir, "civ6_logs", "Lua.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(luaSynced), "[Civ6Relay] fast-forwarded ") {
		t.Fatalf("expected burst backlog fast-forward marker in Lua.log")
	}
	if !strings.Contains(string(luaSynced), "FINAL-LUA-ERROR-LINE") {
		t.Fatalf("expected final Lua line after fast-forward")
	}
	if strings.Contains(string(luaSynced), "SPAM-LINE-0001-") {
		t.Fatalf("expected early spam lines to have been skipped by fast-forward")
	}
}

func TestDropPendingForPath(t *testing.T) {
	in := []FileChunk{
		{RelPath: "civ6_logs/a.log", Offset: 0},
		{RelPath: "civ6_logs/b.log", Offset: 0},
		{RelPath: "civ6_logs/a.log", Offset: 4096},
	}
	got := dropPendingForPath(in, "civ6_logs/a.log")
	if len(got) != 1 || got[0].RelPath != "civ6_logs/b.log" {
		t.Fatalf("dropPendingForPath kept %+v, want only b.log", got)
	}
}

func filesEqual(a, b string) bool {
	da, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	db, err := os.ReadFile(b)
	if err != nil {
		return false
	}
	return bytes.Equal(da, db)
}
