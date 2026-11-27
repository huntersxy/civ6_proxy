//go:build windows

package main

import (
	"context"
	"image/png"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"civ6relay/internal/playtest"
	"civ6relay/internal/relay"
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
	testModsRoot := filepath.Join(t.TempDir(), "Mods")
	app.modsRootFn = func() (string, error) { return testModsRoot, nil }
	app.workshopRootsFn = func() []string { return nil }
	app.refreshSyncFixStatus()
	app.logEntries = []logEntry{
		{Time: "22:00:01", Level: "INFO", Message: "Civ6RelayClient 启动"},
		{Time: "22:00:02", Level: "OK", Message: "已进入房间 ABCD123456"},
		{Time: "22:00:03", Level: "WARN", Message: "防火墙规则检查中"},
		{Time: "22:00:04", Level: "ERROR", Message: "示例错误消息"},
	}

	lightTheme, darkTheme := civ6Themes()
	w := gui.NewTestWindow(t, gui.WindowCfg{
		Title:  uiTitle,
		Width:  1020,
		Height: 700,
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

func TestClientThemesUseReadableFontScale(t *testing.T) {
	light, dark := civ6Themes()
	for _, theme := range []gui.Theme{light, dark} {
		if theme.TextStyleDef.Size != uiFontSize || theme.TextStyleBody.Size != uiFontSize {
			t.Fatalf("%s body font size = %.1f/%.1f, want %.1f", theme.Name, theme.TextStyleDef.Size, theme.TextStyleBody.Size, uiFontSize)
		}
		if theme.TextStyleCaption.Size < 12 {
			t.Fatalf("%s caption font size = %.1f, want >= 12", theme.Name, theme.TextStyleCaption.Size)
		}
		if theme.TextStyleCodeSmall.Size < 13 {
			t.Fatalf("%s code small font size = %.1f, want >= 13", theme.Name, theme.TextStyleCodeSmall.Size)
		}
	}
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

func TestHostPCAPSwitchDefaultsOffAndToggles(t *testing.T) {
	w, app := newTestClientWindow(t, false)
	if app.hostPCAP {
		t.Fatal("expected hostPCAP to default to false")
	}

	switches := w.ResolveID("host-pcap-switch")
	if len(switches) != 1 {
		t.Fatalf("expected 1 host-pcap-switch, got %v", switches)
	}
	if err := w.TestClick(switches[0]); err != nil {
		t.Fatalf("click host-pcap-switch on: %v", err)
	}
	w.TestRender(nil)
	if !app.hostPCAP {
		t.Fatal("expected hostPCAP to toggle to true")
	}

	switches = w.ResolveID("host-pcap-switch")
	if err := w.TestClick(switches[0]); err != nil {
		t.Fatalf("click host-pcap-switch off: %v", err)
	}
	w.TestRender(nil)
	if app.hostPCAP {
		t.Fatal("expected hostPCAP to toggle back to false")
	}

	// While joined as a non-host member, clicking the disabled switch must not toggle.
	app.snap = sessionSnapshot{
		State:    stateJoined,
		RoomCode: "K7M2P9Q4X8",
		MemberID: 2,
		Host:     false,
		Capture:  true,
	}
	app.activeCode = "K7M2P9Q4X8"
	w.TestRender(nil)
	switches = w.ResolveID("host-pcap-switch")
	_ = w.TestClick(switches[0])
	if app.hostPCAP {
		t.Fatal("expected disabled host-pcap-switch not to toggle when non-host")
	}
}

func TestSessionSetHostPCAPPersistsAndDefaultsOff(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	s := &session{
		capture:          true,
		stop:             make(chan struct{}),
		p2pFirewallReady: make(chan struct{}),
	}
	if s.Snapshot().HostPCAP {
		t.Fatal("expected session HostPCAP to default to false")
	}
	if err := s.SetHostPCAP(true); err != nil {
		t.Fatalf("SetHostPCAP(true): %v", err)
	}
	if !s.Snapshot().HostPCAP {
		t.Fatal("expected session Snapshot().HostPCAP to be true")
	}
	if !loadConfig().HostPCAP {
		t.Fatal("expected config HostPCAP to persist as true")
	}
	if err := s.SetHostPCAP(false); err != nil {
		t.Fatalf("SetHostPCAP(false): %v", err)
	}
	if s.Snapshot().HostPCAP || loadConfig().HostPCAP {
		t.Fatal("expected HostPCAP to persist as false")
	}
}

func TestHostP2PSwitchPreRoomTogglesAndPersists(t *testing.T) {
	w, app := newTestClientWindow(t, false)
	if app.hostP2P {
		t.Fatal("expected hostP2P to default to false")
	}

	switches := w.ResolveID("p2p-switch")
	if len(switches) != 1 {
		t.Fatalf("expected 1 p2p-switch, got %v", switches)
	}
	if err := w.TestClick(switches[0]); err != nil {
		t.Fatalf("click p2p-switch on before joining: %v", err)
	}
	w.TestRender(nil)
	if !app.hostP2P {
		t.Fatal("expected hostP2P to toggle to true before joining")
	}

	switches = w.ResolveID("p2p-switch")
	if err := w.TestClick(switches[0]); err != nil {
		t.Fatalf("click p2p-switch off before joining: %v", err)
	}
	w.TestRender(nil)
	if app.hostP2P {
		t.Fatal("expected hostP2P to toggle back to false")
	}

	// While joined as a non-host member, clicking the disabled switch must not toggle.
	app.snap = sessionSnapshot{
		State:    stateJoined,
		RoomCode: "K7M2P9Q4X8",
		MemberID: 2,
		Host:     false,
		P2P:      tunnel.P2PStatus{Supported: true, Enabled: false},
	}
	app.activeCode = "K7M2P9Q4X8"
	w.TestRender(nil)
	switches = w.ResolveID("p2p-switch")
	_ = w.TestClick(switches[0])
	if app.hostP2P {
		t.Fatal("expected disabled p2p-switch not to toggle when non-host")
	}
}

func TestSessionHostP2PPreferenceAutoEnablesOnCreateRoom(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	srv := relay.New(relay.Config{Logger: log.New(io.Discard, "", 0)})
	go func() {
		buf := make([]byte, 2048)
		var batch relay.Batch
		for {
			n, peer, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			srv.HandleInto(buf[:n], peer, &batch)
			for _, out := range batch.Datagrams {
				_, _ = conn.WriteToUDPAddrPort(out.Data, out.Addr)
			}
		}
	}()
	addr := conn.LocalAddr().(*net.UDPAddr)

	s := &session{
		capture:          false,
		stop:             make(chan struct{}),
		p2pFirewallReady: make(chan struct{}),
	}
	if err := s.SetP2PEnabled(true); err != nil {
		t.Fatalf("pre-room SetP2PEnabled(true): %v", err)
	}
	if !s.Snapshot().HostP2P || !loadConfig().HostP2P {
		t.Fatal("expected HostP2P preference to persist as true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.openRoom(ctx, Config{RelayHost: "127.0.0.1", RelayPort: addr.Port, DisplayName: "房主", HostP2P: true}, ""); err != nil {
		t.Fatalf("openRoom: %v", err)
	}
	defer func() {
		s.mu.Lock()
		c := s.client
		s.client = nil
		s.mu.Unlock()
		if c != nil {
			c.Disconnect()
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := s.Snapshot()
		if snap.P2P.Supported && snap.P2P.Enabled && !snap.P2P.Pending {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected P2P to auto-enable after room creation, got %+v", s.Snapshot().P2P)
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

func TestCaptureDirButtonsInvokeOpen(t *testing.T) {
	w, app := newTestClientWindow(t, false)
	var opened []string
	app.openPathFn = func(p string) error {
		opened = append(opened, p)
		return nil
	}

	footerBtns := w.ResolveID("footer-capture-dir")
	if len(footerBtns) != 1 {
		t.Fatalf("expected 1 footer-capture-dir button, got %v", footerBtns)
	}
	if err := w.TestClick(footerBtns[0]); err != nil {
		t.Fatalf("click footer-capture-dir: %v", err)
	}

	// Switch to Diagnostics & Logs page and click log-captures.
	diagNav := w.ResolveID("nav-diaglog")
	if err := w.TestClick(diagNav[0]); err != nil {
		t.Fatalf("click nav-diaglog: %v", err)
	}
	logBtns := w.ResolveID("log-captures")
	if len(logBtns) != 1 {
		t.Fatalf("expected 1 log-captures button, got %v", logBtns)
	}
	if err := w.TestClick(logBtns[0]); err != nil {
		t.Fatalf("click log-captures: %v", err)
	}

	if len(opened) != 2 || opened[0] != captureDirectory() || opened[1] != captureDirectory() {
		t.Fatalf("unexpected opened capture dirs: %v", opened)
	}
}

func TestSyncFixModDialogInstallAndUninstall(t *testing.T) {
	w, app := newTestClientWindow(t, false)
	if app.syncFixMod.Installed {
		t.Fatalf("expected syncFixMod not installed in fresh temp dir")
	}

	// Open confirmation dialog via sidebar button.
	btns := w.ResolveID("sidebar-syncfix-btn")
	if len(btns) != 1 {
		t.Fatalf("expected 1 sidebar-syncfix-btn, got %v", btns)
	}
	if err := w.TestClick(btns[0]); err != nil {
		t.Fatalf("click sidebar-syncfix-btn: %v", err)
	}
	w.TestRender(nil)
	if dups := w.TestDuplicateIDs(); len(dups) != 0 {
		t.Fatalf("dialog duplicate IDs: %v", dups)
	}

	// Confirm installation.
	confirmBtns := w.ResolveID("syncfix-confirm-install")
	if len(confirmBtns) != 1 {
		t.Fatalf("expected 1 syncfix-confirm-install button, got %v", confirmBtns)
	}
	if err := w.TestClick(confirmBtns[0]); err != nil {
		t.Fatalf("click syncfix-confirm-install: %v", err)
	}
	w.TestRender(nil)

	if !app.syncFixMod.Installed || !app.syncFixMod.UpToDate {
		t.Fatalf("expected syncFixMod installed & upToDate after confirm, got %+v", app.syncFixMod)
	}
	if _, err := os.Stat(filepath.Join(app.syncFixMod.InstallDir, "Civ6SyncFix.modinfo")); err != nil {
		t.Fatalf("expected Civ6SyncFix.modinfo on disk: %v", err)
	}

	// Re-open dialog and uninstall cleanly.
	btns = w.ResolveID("sidebar-syncfix-btn")
	if err := w.TestClick(btns[0]); err != nil {
		t.Fatalf("re-click sidebar-syncfix-btn: %v", err)
	}
	w.TestRender(nil)
	unBtns := w.ResolveID("syncfix-uninstall")
	if len(unBtns) != 1 {
		t.Fatalf("expected 1 syncfix-uninstall button, got %v", unBtns)
	}
	if err := w.TestClick(unBtns[0]); err != nil {
		t.Fatalf("click syncfix-uninstall: %v", err)
	}
	w.TestRender(nil)
	if app.syncFixMod.Installed {
		t.Fatalf("expected syncFixMod uninstalled, got %+v", app.syncFixMod)
	}
}

func TestSyncFixWorkshopLinkAndDetectionInUI(t *testing.T) {
	w, app := newTestClientWindow(t, false)
	var copied string
	var openedURL string
	w.SetClipboardFn(func(s string) { copied = s })
	app.openURLFn = func(u string) error {
		openedURL = u
		return nil
	}

	steamLib := t.TempDir()
	workshopDir := filepath.Join(steamLib, "steamapps", "workshop", "content", civ6SteamAppID, syncFixWorkshopID)
	if err := os.MkdirAll(workshopDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workshopDir, "Civ6SyncFix.modinfo"), []byte("<Mod/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	app.workshopRootsFn = func() []string { return []string{steamLib} }

	btns := w.ResolveID("sidebar-syncfix-btn")
	if len(btns) != 1 {
		t.Fatalf("expected 1 sidebar-syncfix-btn, got %v", btns)
	}
	if err := w.TestClick(btns[0]); err != nil {
		t.Fatalf("click sidebar-syncfix-btn: %v", err)
	}
	w.TestRender(nil)

	if !app.syncFixMod.WorkshopInstalled || app.syncFixMod.WorkshopDir != workshopDir {
		t.Fatalf("expected WorkshopInstalled=true, got %+v", app.syncFixMod)
	}
	if dups := w.TestDuplicateIDs(); len(dups) != 0 {
		t.Fatalf("syncfix workshop dialog duplicate IDs: %v", dups)
	}

	copyBtns := w.ResolveID("syncfix-copy-workshop")
	if len(copyBtns) != 1 {
		t.Fatalf("expected 1 syncfix-copy-workshop, got %v", copyBtns)
	}
	if err := w.TestClick(copyBtns[0]); err != nil {
		t.Fatalf("click syncfix-copy-workshop: %v", err)
	}
	if copied != syncFixWorkshopURL {
		t.Fatalf("expected copied URL %q, got %q", syncFixWorkshopURL, copied)
	}

	openBtns := w.ResolveID("syncfix-open-workshop")
	if len(openBtns) != 1 {
		t.Fatalf("expected 1 syncfix-open-workshop, got %v", openBtns)
	}
	if err := w.TestClick(openBtns[0]); err != nil {
		t.Fatalf("click syncfix-open-workshop: %v", err)
	}
	if openedURL != syncFixWorkshopURL {
		t.Fatalf("expected opened URL %q, got %q", syncFixWorkshopURL, openedURL)
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
		HostToGuest:   playtest.Delivery{Sent: 10240, Received: 10240, Settled: 10240},
		GuestToHost:   playtest.Delivery{Sent: 8160, Received: 8159, Settled: 8160, Missing: 1},
		SteadyRTT:     playtest.Latency{Median: 21 * time.Millisecond, P95: 28 * time.Millisecond, Jitter: 2500 * time.Microsecond, Probes: 50, Replies: 50},
		SyncRTT:       playtest.Latency{Median: 34 * time.Millisecond, P95: 52 * time.Millisecond, Jitter: 6100 * time.Microsecond, Probes: 100, Replies: 100},
		Fragmented:    playtest.Delivery{Sent: 40, Received: 40},
		HostRawBytes:  11_800_000,
		HostWireBytes: 5_850_000,
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
