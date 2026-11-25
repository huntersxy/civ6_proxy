//go:build windows

package main

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"civ6relay/internal/playtest"
	"civ6relay/internal/speedtest"
	"civ6relay/internal/tunnel"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/go-gui-org/go-gui/gui/backend/soft"
)

func newTestClientWindow(t *testing.T, dark bool) (*gui.Window, *clientApp) {
	t.Helper()
	cfg := Config{
		RelayHost:    "203.0.113.10",
		RelayPort:    42056,
		DisplayName:  "秦始皇",
		RelayHistory: []string{"203.0.113.10:42056", "198.51.100.7:42056"},
	}
	app := newClientApp(nil, cfg)
	app.logEntries = []logEntry{
		{Time: "22:00:01", Level: "INFO", Message: "Civ6RelayClient 启动"},
		{Time: "22:00:02", Level: "OK", Message: "已进入房间 ABCD123456"},
		{Time: "22:00:03", Level: "WARN", Message: "防火墙规则检查中"},
		{Time: "22:00:04", Level: "ERROR", Message: "示例错误消息"},
	}

	lightTheme, darkTheme := civ6Themes()
	w := gui.NewTestWindow(t, gui.WindowCfg{
		Title:  uiTitle,
		Width:  980,
		Height: 680,
		State:  app,
		OnInit: func(win *gui.Window) {
			if dark {
				win.SetTheme(darkTheme)
			} else {
				win.SetTheme(lightTheme)
			}
			win.SetView(func(vw *gui.Window) gui.View {
				return gui.State[clientApp](vw).view(vw)
			})
		},
	})
	w.TestRender(nil)
	return w, app
}

func TestClientUINavigationAndThemes(t *testing.T) {
	for _, tc := range []struct {
		name string
		dark bool
	}{
		{name: "light", dark: false},
		{name: "dark", dark: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, app := newTestClientWindow(t, tc.dark)

			if dups := w.TestDuplicateIDs(); len(dups) != 0 {
				t.Fatalf("lobby duplicate IDs: %v", dups)
			}

			// Navigate to Network Test
			netNav := w.ResolveID("nav-nettest")
			if len(netNav) != 1 {
				t.Fatalf("expected 1 nav-nettest ID, got %v", netNav)
			}
			if err := w.TestClick(netNav[0]); err != nil {
				t.Fatalf("click nav-nettest: %v", err)
			}
			if app.page != pageNetTest {
				t.Fatalf("expected pageNetTest, got %v", app.page)
			}
			if dups := w.TestDuplicateIDs(); len(dups) != 0 {
				t.Fatalf("nettest play mode duplicate IDs: %v", dups)
			}

			// Switch to bandwidth mode and re-render
			app.netTest.mode = bandwidthMode
			w.TestRender(nil)
			if dups := w.TestDuplicateIDs(); len(dups) != 0 {
				t.Fatalf("nettest bandwidth mode duplicate IDs: %v", dups)
			}

			// Navigate to Diagnostics & Logs
			diagNav := w.ResolveID("nav-diaglog")
			if len(diagNav) != 1 {
				t.Fatalf("expected 1 nav-diaglog ID, got %v", diagNav)
			}
			if err := w.TestClick(diagNav[0]); err != nil {
				t.Fatalf("click nav-diaglog: %v", err)
			}
			if app.page != pageDiagLog {
				t.Fatalf("expected pageDiagLog, got %v", app.page)
			}
			app.diag = diagState{
				hasResult:  true,
				ok:         true,
				summary:    "驱动加载: 成功",
				report:     "Civ6Relay 诊断报告\n驱动加载: 成功\n",
				reportPath: `C:\Temp\Civ6RelayDiag.txt`,
				showFull:   true,
			}
			w.TestRender(nil)
			if dups := w.TestDuplicateIDs(); len(dups) != 0 {
				t.Fatalf("diaglog duplicate IDs: %v", dups)
			}

			// Navigate back to Lobby
			lobbyNav := w.ResolveID("nav-lobby")
			if err := w.TestClick(lobbyNav[0]); err != nil {
				t.Fatalf("click nav-lobby: %v", err)
			}
			if app.page != pageLobby {
				t.Fatalf("expected pageLobby, got %v", app.page)
			}
			if unconsumed := w.TestUnconsumedEvents(); len(unconsumed) != 0 {
				t.Fatalf("unconsumed events: %v", unconsumed)
			}
		})
	}
}

func TestClientUIJoinedLobbyState(t *testing.T) {
	w, app := newTestClientWindow(t, false)
	var copied string
	w.SetClipboardFn(func(s string) { copied = s })

	app.snap = sessionSnapshot{
		State:         stateJoined,
		RoomCode:      "K7M2P9Q4X8",
		MemberID:      1,
		Host:          true,
		Capture:       true,
		BridgeRunning: true,
		Subnet:        "10.66.0.0/24",
		CivRunning:    true,
		Tunnel: tunnel.Metrics{
			HasRTT:          true,
			RTT:             18 * time.Millisecond,
			GameRxDatagrams: 120,
			GameTxDatagrams: 145,
		},
		P2P: tunnel.P2PStatus{
			Supported: true,
			Enabled:   true,
		},
		Members: []memberView{
			{ID: 1, Name: "秦始皇", Host: true, Online: true, Virtual: "10.66.0.1"},
			{ID: 2, Name: "亚历山大", Host: false, Online: true, Virtual: "10.66.0.2", Path: tunnel.Direct, Latency: 14 * time.Millisecond},
			{ID: 3, Name: "维多利亚", Host: false, Online: true, Virtual: "10.66.0.3", Path: tunnel.ViaRelay, Latency: 42 * time.Millisecond},
		},
	}
	app.activeCode = "K7M2P9Q4X8"
	w.TestRender(nil)

	if dups := w.TestDuplicateIDs(); len(dups) != 0 {
		t.Fatalf("joined lobby duplicate IDs: %v", dups)
	}
	if ids := w.ResolveID("members-table"); len(ids) != 1 {
		t.Fatalf("expected members-table to render, got %v", ids)
	}

	copyBtns := w.ResolveID("banner-copy-code")
	if len(copyBtns) != 1 {
		t.Fatalf("expected banner-copy-code button, got %v", copyBtns)
	}
	if err := w.TestClick(copyBtns[0]); err != nil {
		t.Fatalf("click banner-copy-code: %v", err)
	}
	if copied != "K7M2P9Q4X8" {
		t.Fatalf("expected clipboard K7M2P9Q4X8, got %q", copied)
	}
}

func TestCanJoinWithDraft(t *testing.T) {
	if canJoinWithDraft(false, "K7M2P9Q4X8") {
		t.Fatal("should not allow join when canConnect is false")
	}
	if canJoinWithDraft(true, "short") {
		t.Fatal("should not allow join with invalid room code")
	}
	if !canJoinWithDraft(true, "K7M2P9Q4X8") {
		t.Fatal("expected valid 10-char room code to be accepted")
	}
}

func TestClientUISnapshots(t *testing.T) {
	outDir := os.Getenv("CIV6_SNAPSHOT_DIR")
	if outDir == "" {
		t.Skip("set CIV6_SNAPSHOT_DIR to export UI snapshots")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	save := func(name string, w *gui.Window) {
		t.Helper()
		img, err := soft.RenderToImage(w, 1.0)
		if err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		f, err := os.Create(filepath.Join(outDir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Light Lobby (joined with members)
	wLight, appLight := newTestClientWindow(t, false)
	appLight.snap = sessionSnapshot{
		State:         stateJoined,
		RoomCode:      "K7M2P9Q4X8",
		MemberID:      1,
		Host:          true,
		Capture:       true,
		BridgeRunning: true,
		Subnet:        "10.66.0.0/24",
		CivRunning:    true,
		PCAPPath:      `C:\Users\demo\AppData\Local\Civ6Relay\captures\room.pcap`,
		PCAPPackets:   1842,
		Tunnel: tunnel.Metrics{
			HasRTT:          true,
			RTT:             18 * time.Millisecond,
			GameRxDatagrams: 4210,
			GameTxDatagrams: 3985,
		},
		P2P: tunnel.P2PStatus{Supported: true, Enabled: true},
		Members: []memberView{
			{ID: 1, Name: "秦始皇", Host: true, Online: true, Virtual: "10.66.0.1"},
			{ID: 2, Name: "亚历山大", Host: false, Online: true, Virtual: "10.66.0.2", Path: tunnel.Direct, Latency: 14 * time.Millisecond},
			{ID: 3, Name: "维多利亚", Host: false, Online: true, Virtual: "10.66.0.3", Path: tunnel.Punching},
			{ID: 4, Name: "凯撒", Host: false, Online: true, Virtual: "10.66.0.4", Path: tunnel.ViaRelay, Latency: 46 * time.Millisecond},
		},
	}
	appLight.activeCode = "K7M2P9Q4X8"
	save("lobby_light", wLight)

	// 2. Dark Lobby (joined)
	wDark, appDark := newTestClientWindow(t, true)
	appDark.snap = appLight.snap
	appDark.activeCode = "K7M2P9Q4X8"
	save("lobby_dark", wDark)

	// 3. Network Test (play mode + bandwidth mode)
	appDark.page = pageNetTest
	appDark.netTest.relay = "203.0.113.10:42056"
	appDark.netTest.mode = playMode
	appDark.netTest.displays[playMode] = testDisplay{status: "联机模拟完成", progress: 1}
	appDark.netTest.playRes = playtest.Result{
		Create: 22 * time.Millisecond,
		Join:   19 * time.Millisecond,
		Discovery: playtest.Discovery{
			Attempts: 3, Broadcasts: 3, Replies: 3,
		},
		HostToGuest: playtest.Delivery{Sent: 240, Received: 240, Settled: 240},
		GuestToHost: playtest.Delivery{Sent: 240, Received: 239, Settled: 240, Missing: 1},
		SteadyRTT:   playtest.Latency{Median: 21 * time.Millisecond, P95: 28 * time.Millisecond, Jitter: 2500 * time.Microsecond, Probes: 30, Replies: 30},
		SyncRTT:     playtest.Latency{Median: 34 * time.Millisecond, P95: 52 * time.Millisecond, Jitter: 6100 * time.Microsecond, Probes: 12, Replies: 12},
		Fragmented:  playtest.Delivery{Sent: 18, Received: 18},
	}
	save("nettest_play_dark", wDark)

	appLight.page = pageNetTest
	appLight.netTest.relay = "203.0.113.10:42056"
	appLight.netTest.mode = bandwidthMode
	appLight.netTest.displays[bandwidthMode] = testDisplay{status: "压测完成", progress: 1}
	appLight.netTest.bwRes = speedtest.Result{
		MaxMbps:  100,
		Download: speedtest.Transfer{Sent: 5000, Received: 4992, Final: true, AverageMbps: 84.35, PeakMbps: 96.20},
		Upload:   speedtest.Transfer{Sent: 3200, Received: 3180, Final: true, AverageMbps: 42.18, PeakMbps: 48.90},
		Latency: [3]speedtest.Latency{
			{Samples: 20, Median: 18 * time.Millisecond, P95: 23 * time.Millisecond, Jitter: 1400 * time.Microsecond},
			{Samples: 20, Median: 29 * time.Millisecond, P95: 44 * time.Millisecond, Jitter: 5200 * time.Microsecond},
			{Samples: 20, Median: 25 * time.Millisecond, P95: 38 * time.Millisecond, Jitter: 3800 * time.Microsecond},
		},
	}
	save("nettest_bw_light", wLight)

	// 4. Diagnostics & Logs (dark)
	appDark.page = pageDiagLog
	appDark.diag = diagState{
		hasResult:  true,
		ok:         true,
		summary:    "系统: Windows 11\n管理员权限: true\n驱动文件: 完整且哈希匹配\n驱动加载: 成功\n防火墙规则: 已存在\n\n诊断通过：WinDivert 驱动可以正常加载。",
		report:     "Civ6Relay 诊断报告\n驱动加载: 成功\n",
		reportPath: `C:\Temp\Civ6RelayDiag.txt`,
	}
	save("diaglog_dark", wDark)
}
