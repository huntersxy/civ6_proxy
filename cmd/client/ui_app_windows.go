//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"civ6relay/internal/logsync"
	"civ6relay/internal/protocol"
	"civ6relay/internal/win"

	"github.com/go-gui-org/go-gui/gui"
	"github.com/go-gui-org/go-gui/gui/backend"
)

//go:embed app_icon.png
var appIconPNG []byte

const uiTitle = "文明 VI 联机中继"

type navPage uint8

const (
	pageLobby navPage = iota
	pageNetTest
	pageDiagLog
	pageModPatch
)

type diagState struct {
	busy       bool
	hasResult  bool
	ok         bool
	summary    string
	report     string
	reportPath string
	showFull   bool
}

type clientApp struct {
	s *session

	page navPage

	// Form inputs
	relayText      string
	relayHistory   []string
	historyOpen    bool
	nameText       string
	maxMembersText string
	joinDraft      string
	activeCode     string

	// Cached session snapshot & log entries
	snap        sessionSnapshot
	prevState   connectionState
	hostPCAP    bool
	hostP2P     bool
	hostLogSync bool
	logEntries  []logEntry
	logSeq      int64
	autoScroll  bool

	// Transient states
	closing         bool
	exportBusy      bool
	diag             diagState
	netTest          netTestState
	syncFixMod       syncFixModStatus
	modPatches       []modPatchStatus
	modPatchExpanded map[string]bool
	modPatchNotice   string
	openPathFn       func(string) error     // optional test override for openFolder
	openURLFn        func(string) error     // optional test override for openURL
	modsRootFn       func() (string, error) // optional test override for defaultCiv6ModsRoot
	workshopRootsFn  func() []string        // optional test override for defaultSteamLibraryRoots
	patchBackupDirFn func() string          // optional test override for modPatchBackupRoot
	modPatchSpecsFn  func() []modPatchSpec  // optional test override for modPatchSpecs

	refreshQueued atomic.Bool
	stopped       atomic.Bool
}

func newClientApp(s *session, cfg Config) *clientApp {
	maxMembersText := ""
	if cfg.MaxMembers > 0 {
		maxMembersText = strconv.Itoa(cfg.MaxMembers)
	}
	app := &clientApp{
		s:              s,
		page:           pageLobby,
		relayText:      win.FormatRelay(cfg.RelayHost, cfg.RelayPort),
		relayHistory:   append([]string(nil), cfg.RelayHistory...),
		nameText:       cfg.DisplayName,
		maxMembersText: maxMembersText,
		hostPCAP:       cfg.HostPCAP,
		hostP2P:        cfg.HostP2P,
		hostLogSync:      cfg.HostLogSync,
		autoScroll:       true,
		modPatchExpanded: make(map[string]bool),
	}
	app.netTest.init()
	app.refreshSyncFixStatus()
	app.refreshModPatches()
	if s != nil {
		app.snap = s.Snapshot()
		app.prevState = app.snap.State
		app.hostPCAP = app.snap.HostPCAP
		app.hostP2P = app.snap.HostP2P
		app.hostLogSync = app.snap.HostLogSync
		if entries, seq, ok := s.logEntries(-1); ok {
			app.logEntries = entries
			app.logSeq = seq
		}
	}
	return app
}

func runNativeUI(s *session, cfg Config) error {
	app := newClientApp(s, cfg)
	lightTheme, darkTheme := civ6Themes()

	stopTicker := make(chan struct{})
	defer close(stopTicker)

	var w *gui.Window
	w = gui.NewWindow(gui.WindowCfg{
		Title:             uiTitle,
		IconPNG:           appIconPNG,
		Width:             1020,
		Height:            700,
		MinWidth:          860,
		MinHeight:         600,
		EnableTextHinting: true,
		State:             app,
		OnInit: func(win *gui.Window) {
			win.SetView(func(vw *gui.Window) gui.View {
				return gui.State[clientApp](vw).view(vw)
			})
			win.QueueCommand(func(vw *gui.Window) {
				if ids := vw.ResolveID("relay-input"); len(ids) > 0 {
					vw.SetFocus(ids[0])
				}
			})
		},
		OnEvent: func(ev *gui.Event, win *gui.Window) {
			if ev.Type != gui.EventKeyDown || ev.KeyCode != gui.KeyEscape {
				return
			}
			a := gui.State[clientApp](win)
			if a.closing {
				return
			}
			if a.historyOpen {
				a.historyOpen = false
				win.InvalidateLayout()
				return
			}
			if a.page == pageNetTest && a.netTest.running() {
				a.netTest.stop(win)
				return
			}
			if a.snap.State == stateConnecting {
				a.s.Disconnect()
				a.syncSnapshot(win)
			}
		},
		OnCloseRequest: func(win *gui.Window) {
			a := gui.State[clientApp](win)
			if a.closing {
				return
			}
			a.closing = true
			speedDone := a.netTest.stopForClose()
			win.InvalidateLayout()
			go func() {
				if speedDone != nil {
					<-speedDone
				}
				if a.s != nil {
					a.s.Shutdown()
				}
				win.Close()
			}()
		},
	})

	// Configure system light/dark tracking before Run so Windows paints a
	// matching dark titlebar from the very first frame.
	w.FollowSystemAppearance(lightTheme, darkTheme)

	notifyUI := func() {
		if app.stopped.Load() {
			return
		}
		if app.refreshQueued.CompareAndSwap(false, true) {
			w.QueueCommand(func(vw *gui.Window) {
				app.refreshQueued.Store(false)
				app.syncSnapshot(vw)
			})
		}
	}

	s.startBackground(notifyUI)
	defer func() {
		app.stopped.Store(true)
		s.Shutdown()
	}()

	go func() {
		ticker := time.NewTicker(750 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopTicker:
				return
			case <-ticker.C:
				notifyUI()
			}
		}
	}()

	backend.Run(w)
	return nil
}

func (a *clientApp) syncSnapshot(w *gui.Window) {
	if a.s == nil {
		return
	}
	snap := a.s.Snapshot()
	prevState := a.prevState
	a.snap = snap
	a.prevState = snap.State
	a.hostPCAP = snap.HostPCAP
	a.hostP2P = snap.HostP2P
	a.hostLogSync = snap.HostLogSync

	if snap.State == stateJoined && snap.RoomCode != "" {
		a.activeCode = snap.RoomCode
	} else if snap.State != stateJoined {
		a.activeCode = ""
	}

	a.s.SyncP2PFirewall(snap.P2P)

	if prevState == stateConnecting && snap.State == stateJoined && snap.RoomCode != "" {
		a.relayHistory = loadConfig().RelayHistory
	}

	if entries, seq, changed := a.s.logEntries(a.logSeq); changed {
		a.logEntries = entries
		a.logSeq = seq
		if a.page == pageDiagLog && a.autoScroll && len(entries) > 0 {
			w.QueueCommand(func(vw *gui.Window) {
				if ids := vw.ResolveID("log-list"); len(ids) > 0 {
					vw.ScrollToEnd(ids[0])
				}
			})
		}
	}

	w.InvalidateLayout()
}

func (a *clientApp) canConnect() bool {
	return !a.closing && !a.diag.busy && (a.snap.State == stateIdle || a.snap.State == stateFailed)
}

func (a *clientApp) relayAddressForTest() (string, error) {
	host, port, err := parseRelayAddress(a.relayText)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func (a *clientApp) toast(w *gui.Window, severity gui.ToastSeverity, title, body string) {
	if w == nil {
		return
	}
	w.Toast(gui.ToastCfg{
		Title:    title,
		Body:     body,
		Severity: severity,
		Duration: 3500 * time.Millisecond,
	})
}

func (a *clientApp) connect(w *gui.Window, join bool) {
	if !a.canConnect() || a.s == nil {
		return
	}
	a.historyOpen = false
	if err := a.s.Connect(join, a.relayText, a.nameText, a.joinDraft, a.maxMembersText); err != nil {
		a.toast(w, gui.ToastWarning, "无法发起连接", err.Error())
		return
	}
	a.syncSnapshot(w)
}

func (a *clientApp) copyRoomCode(w *gui.Window) {
	if a.activeCode == "" {
		return
	}
	w.SetClipboard(a.activeCode)
}

func (a *clientApp) runDiagnostics(w *gui.Window) {
	if a.diag.busy || a.closing || a.snap.State == stateConnecting || a.snap.State == stateLeaving {
		return
	}
	a.diag.busy = true
	relay := a.relayText
	logPath := ""
	if a.s != nil {
		logPath = a.s.logLocation()
	}
	w.InvalidateLayout()

	go func() {
		summary, report, path, ok := collectDiagnostics(relay, logPath)
		w.QueueCommand(func(vw *gui.Window) {
			app := gui.State[clientApp](vw)
			app.diag.busy = false
			app.diag.hasResult = true
			app.diag.ok = ok
			app.diag.summary = summary
			app.diag.report = report
			app.diag.reportPath = path
			if app.closing {
				return
			}
			if !ok {
				app.toast(vw, gui.ToastError, "诊断发现问题", "请查看下方诊断结果")
			}
			vw.InvalidateLayout()
		})
	}()
}

func (a *clientApp) exportLog(w *gui.Window) {
	if a.exportBusy || a.closing || a.s == nil {
		return
	}
	a.exportBusy = true
	w.InvalidateLayout()
	defaultName := fmt.Sprintf("civ6relay-%s.log", time.Now().Format("20060102-150405"))
	go func() {
		path, ok := saveFileDialog(0, defaultName)
		var saveErr error
		if ok && path != "" {
			text, _ := a.s.logSnapshot(-1)
			saveErr = writeClientFile(path, []byte("\uFEFF"+text))
		}
		w.QueueCommand(func(vw *gui.Window) {
			app := gui.State[clientApp](vw)
			app.exportBusy = false
			if !ok || path == "" {
				vw.InvalidateLayout()
				return
			}
			if saveErr != nil {
				app.s.logError("导出日志失败: %v", saveErr)
				app.toast(vw, gui.ToastError, "导出失败", saveErr.Error())
			}
			vw.InvalidateLayout()
		})
	}()
}

func (a *clientApp) openCapturesDir(w *gui.Window) {
	dir := captureDirectory()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		a.toast(w, gui.ToastError, "打开失败", err.Error())
		return
	}
	a.openPath(w, dir)
}

func (a *clientApp) openCollectedLogsDir(w *gui.Window, preferSession bool) {
	dir := collectedLogsDirectory()
	if preferSession && a.snap.LogSyncDir != "" {
		if info, err := os.Stat(a.snap.LogSyncDir); err == nil && info.IsDir() {
			a.openPath(w, a.snap.LogSyncDir)
			return
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.toast(w, gui.ToastError, "打开失败", err.Error())
		return
	}
	a.openPath(w, dir)
}

func (a *clientApp) modsRoot() (string, error) {
	if a.modsRootFn != nil {
		return a.modsRootFn()
	}
	return defaultCiv6ModsRoot()
}

func (a *clientApp) steamLibraryRoots() []string {
	if a.workshopRootsFn != nil {
		return a.workshopRootsFn()
	}
	return defaultSteamLibraryRoots()
}

func (a *clientApp) refreshSyncFixStatus() {
	roots := a.steamLibraryRoots()
	modRoot, err := a.modsRoot()
	if err != nil {
		st := syncFixModStatus{Err: err.Error()}
		if workshopDir, ok := findWorkshopModDir(roots, civ6SteamAppID, syncFixWorkshopID); ok {
			st.WorkshopDir = workshopDir
			st.WorkshopInstalled = true
		}
		a.syncFixMod = st
		return
	}
	a.syncFixMod = inspectSyncFixMod(modRoot, roots)
}

const civ6LaunchSteamURI = "steam://rungameid/" + civ6SteamAppID

func (a *clientApp) openWorkshopURL(w *gui.Window) {
	openFn := openURL
	if a.openURLFn != nil {
		openFn = a.openURLFn
	}
	if err := openFn(syncFixWorkshopURL); err != nil {
		a.toast(w, gui.ToastError, "打开失败", err.Error())
	}
}

func (a *clientApp) launchCiv6(w *gui.Window) {
	if a.closing {
		return
	}
	if a.snap.CivRunning {
		a.toast(w, gui.ToastInfo, "游戏已在运行", "检测到文明 VI 进程正在运行中")
		return
	}
	openFn := openURL
	if a.openURLFn != nil {
		openFn = a.openURLFn
	}
	err := openFn(civ6LaunchSteamURI)
	if err != nil && a.openURLFn == nil {
		err = launchCiv6Executable(a.steamLibraryRoots())
	}
	if err != nil {
		if a.s != nil {
			a.s.logError("启动文明 VI 失败: %v", err)
		}
		a.toast(w, gui.ToastError, "启动游戏失败", err.Error())
		return
	}
	if a.s != nil {
		a.s.logInfo("已发送启动文明 VI 请求")
	}
	a.toast(w, gui.ToastSuccess, "正在启动文明 VI", "已唤起 Steam 启动游戏，请等待游戏窗口打开")
}

func (a *clientApp) openPath(w *gui.Window, dir string) {
	openFn := openFolder
	if a.openPathFn != nil {
		openFn = a.openPathFn
	}
	if err := openFn(dir); err != nil {
		a.toast(w, gui.ToastError, "打开失败", err.Error())
	}
}

func (a *clientApp) installSyncFixFromUI(w *gui.Window) {
	root, err := a.modsRoot()
	if err != nil {
		a.toast(w, gui.ToastError, "安装失败", err.Error())
		return
	}
	installDir, err := installSyncFixMod(root)
	a.refreshSyncFixStatus()
	if err != nil {
		if a.s != nil {
			a.s.logError("安装 Civ6SyncFix 失败: %v", err)
		}
		a.toast(w, gui.ToastError, "安装失败", err.Error())
		w.InvalidateLayout()
		return
	}
	if a.s != nil {
		a.s.logOK("已安装 Civ6SyncFix: %s", installDir)
	}
	w.DialogDismiss()
	w.InvalidateLayout()
}

func (a *clientApp) uninstallSyncFixFromUI(w *gui.Window) {
	root, err := a.modsRoot()
	if err != nil {
		a.toast(w, gui.ToastError, "卸载失败", err.Error())
		return
	}
	if err := uninstallSyncFixMod(root); err != nil {
		if a.s != nil {
			a.s.logError("卸载 Civ6SyncFix 失败: %v", err)
		}
		a.toast(w, gui.ToastError, "卸载失败", err.Error())
		return
	}
	a.refreshSyncFixStatus()
	if a.s != nil {
		a.s.logInfo("已卸载 Civ6SyncFix")
	}
	w.DialogDismiss()
	w.InvalidateLayout()
}

func (a *clientApp) patchBackupRootDir() string {
	if a.patchBackupDirFn != nil {
		return a.patchBackupDirFn()
	}
	return modPatchBackupRoot()
}

func (a *clientApp) refreshModPatches() {
	specs := modPatchSpecs()
	if a.modPatchSpecsFn != nil {
		specs = a.modPatchSpecsFn()
	}
	roots := a.steamLibraryRoots()
	backupRoot := a.patchBackupRootDir()
	out := make([]modPatchStatus, 0, len(specs))
	for _, spec := range specs {
		out = append(out, inspectModPatch(spec, roots, backupRoot))
	}
	a.modPatches = out
}

func (a *clientApp) findModPatchStatus(workshopID string) (modPatchStatus, bool) {
	for _, st := range a.modPatches {
		if st.Spec.WorkshopID == workshopID {
			return st, true
		}
	}
	return modPatchStatus{}, false
}

func (a *clientApp) applyModPatchFromUI(w *gui.Window, workshopID string, force bool) {
	st, ok := a.findModPatchStatus(workshopID)
	if !ok {
		return
	}
	updated, err := applyModPatch(st.Spec, a.steamLibraryRoots(), a.patchBackupRootDir(), force)
	a.refreshModPatches()
	if err != nil {
		if a.s != nil {
			a.s.logError("应用 Mod 补丁失败 (%s): %v", workshopID, err)
		}
		a.toast(w, gui.ToastError, "打补丁失败", err.Error())
		if w != nil {
			w.InvalidateLayout()
		}
		return
	}
	if a.s != nil {
		a.s.logOK("已应用 Mod 补丁: %s (%s)", updated.Spec.Name, workshopID)
	}
	a.modPatchNotice = "已应用补丁: " + updated.Spec.Name + "（已自动备份原文件）"
	if w != nil {
		w.InvalidateLayout()
	}
}

func (a *clientApp) applyAllReadyModPatchesFromUI(w *gui.Window) {
	applied := 0
	var firstErr error
	for _, st := range a.modPatches {
		if st.State != modPatchReady {
			continue
		}
		if _, err := applyModPatch(st.Spec, a.steamLibraryRoots(), a.patchBackupRootDir(), false); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		applied++
	}
	a.refreshModPatches()
	if firstErr != nil {
		a.modPatchNotice = "批量打补丁存在失败项: " + firstErr.Error()
		a.toast(w, gui.ToastError, "部分补丁失败", firstErr.Error())
	} else if applied > 0 {
		if a.s != nil {
			a.s.logOK("已批量应用 %d 个 MD5 匹配的 Mod 补丁", applied)
		}
		a.modPatchNotice = fmt.Sprintf("批量打补丁完成：已为 %d 个 Mod 应用联机补丁", applied)
	}
	if w != nil {
		w.InvalidateLayout()
	}
}

func (a *clientApp) restoreModPatchFromUI(w *gui.Window, workshopID string) {
	st, ok := a.findModPatchStatus(workshopID)
	if !ok {
		return
	}
	updated, err := restoreModPatch(st.Spec, a.steamLibraryRoots(), a.patchBackupRootDir())
	a.refreshModPatches()
	if err != nil {
		if a.s != nil {
			a.s.logError("还原 Mod 失败 (%s): %v", workshopID, err)
		}
		a.toast(w, gui.ToastError, "还原失败", err.Error())
		if w != nil {
			w.InvalidateLayout()
		}
		return
	}
	if a.s != nil {
		a.s.logInfo("已还原 Mod 原版文件: %s (%s)", updated.Spec.Name, workshopID)
	}
	a.modPatchNotice = "已还原原版文件: " + updated.Spec.Name
	if w != nil {
		w.InvalidateLayout()
	}
}

func (a *clientApp) openSyncFixDialog(w *gui.Window) {
	if w == nil || a.closing {
		return
	}
	a.historyOpen = false
	a.refreshSyncFixStatus()
	w.Dialog(gui.DialogCfg{
		ID:         "syncfix-dialog",
		DialogType: gui.DialogCustom,
		Title:      "Civ6SyncFix",
		Width:      520,
		FocusID:    "syncfix-confirm-install",
		CustomView: func(vw *gui.Window) gui.View {
			app := gui.State[clientApp](vw)
			t := gui.CurrentTheme()
			st := app.syncFixMod

			workshopLabel := "未检测"
			workshopVariant := gui.BadgeDefault
			if st.WorkshopInstalled {
				workshopLabel = "已订阅"
				workshopVariant = gui.BadgeSuccess
			}

			localLabel := "未安装"
			localVariant := gui.BadgeDefault
			confirmLabel := "本地安装"
			confirmVariant := gui.ButtonPrimary
			if st.Err != "" {
				localLabel = "目录异常"
				localVariant = gui.BadgeError
			} else if st.UpToDate {
				localLabel = "已装最新"
				localVariant = gui.BadgeSuccess
				confirmLabel = "重新安装"
			} else if st.Installed {
				localLabel = "有更新"
				localVariant = gui.BadgeWarning
				confirmLabel = "更新"
			}
			if st.WorkshopInstalled {
				confirmVariant = gui.ButtonSecondary
				if !st.Installed && st.Err == "" {
					confirmLabel = "本地安装"
				}
			}

			targetText := st.InstallDir
			if st.Err != "" {
				targetText = st.Err
			}

			workshopItems := []gui.View{
				gui.Row(gui.ContainerCfg{
					Sizing:     gui.FillFit,
					Padding:    gui.NoPadding,
					SizeBorder: gui.NoBorder,
					Spacing:    gui.SpacingSmall,
					VAlign:     gui.VAlignMiddle,
					Content: []gui.View{
						gui.Text(gui.TextCfg{
							Text:      "Steam 创意工坊",
							TextStyle: t.TextStyleLabel,
							Sizing:    gui.FillFit,
						}),
						gui.Badge(gui.BadgeCfg{
							Label:   workshopLabel,
							Variant: workshopVariant,
						}),
					},
				}),
				gui.Row(gui.ContainerCfg{
					Sizing:     gui.FillFit,
					Padding:    gui.NoPadding,
					SizeBorder: gui.NoBorder,
					Spacing:    gui.SpacingSmall,
					VAlign:     gui.VAlignMiddle,
					Content: []gui.View{
						gui.Row(gui.ContainerCfg{
							ID:           "syncfix-workshop-link",
							Focusable:    true,
							ClickOnEnter: true,
							ClickOnSpace: true,
							Sizing:       gui.FillFit,
							Padding:      gui.NewPadding(2, 4, 2, 0),
							SizeBorder:   gui.NoBorder,
							Hover: gui.HoverStyle{
								Cursor: gui.CursorPointingHand,
							},
							OnClick: func(ctx gui.EventCtx) {
								ctx.Consume()
								gui.State[clientApp](ctx.Window).openWorkshopURL(ctx.Window)
							},
							Content: []gui.View{
								gui.Text(gui.TextCfg{
									Text:      syncFixWorkshopURL,
									TextStyle: t.TextStyleLink,
									Mode:      gui.TextModeWrap,
									Sizing:    gui.FillFit,
								}),
							},
						}),
						gui.Button(gui.ButtonCfg{
							ID:      "syncfix-copy-workshop",
							Label:   "复制",
							Variant: gui.ButtonGhost,
							OnClick: func(ctx gui.EventCtx) {
								ctx.Consume()
								ctx.Window.SetClipboard(syncFixWorkshopURL)
							},
						}),
						gui.Button(gui.ButtonCfg{
							ID:      "syncfix-open-workshop",
							Label:   "打开工坊",
							Variant: gui.ButtonPrimary,
							OnClick: func(ctx gui.EventCtx) {
								ctx.Consume()
								gui.State[clientApp](ctx.Window).openWorkshopURL(ctx.Window)
							},
						}),
					},
				}),
			}
			if st.WorkshopInstalled {
				workshopItems = append(workshopItems,
					gui.Text(gui.TextCfg{
						Text:      st.WorkshopDir,
						TextStyle: t.TextStyleCodeSmall,
						Mode:      gui.TextModeWrap,
						Sizing:    gui.FillFit,
					}),
				)
			}

			bodyItems := []gui.View{
				subtlePanelView("syncfix-workshop-info", gui.FillFit, workshopItems...),
				subtlePanelView("syncfix-dialog-info", gui.FillFit,
					gui.Row(gui.ContainerCfg{
						Sizing:     gui.FillFit,
						Padding:    gui.NoPadding,
						SizeBorder: gui.NoBorder,
						Spacing:    gui.SpacingSmall,
						VAlign:     gui.VAlignMiddle,
						Content: []gui.View{
							gui.Text(gui.TextCfg{
								Text:      "本地安装",
								TextStyle: t.TextStyleLabel,
								Sizing:    gui.FillFit,
							}),
							gui.Badge(gui.BadgeCfg{
								Label:   localLabel,
								Variant: localVariant,
							}),
						},
					}),
					gui.Text(gui.TextCfg{
						Text:      targetText,
						TextStyle: t.TextStyleCodeSmall,
						Mode:      gui.TextModeWrap,
						Sizing:    gui.FillFit,
					}),
				),
			}

			if st.WorkshopInstalled && st.Installed {
				bodyItems = append(bodyItems, gui.Text(gui.TextCfg{
					Text:      "工坊与本地版共存，建议卸载本地版。",
					TextStyle: t.TextStyleWarning,
					Mode:      gui.TextModeWrap,
					Sizing:    gui.FillFit,
				}))
			}

			if app.snap.CivRunning {
				bodyItems = append(bodyItems, gui.Text(gui.TextCfg{
					Text:      "文明 VI 运行中，写入文件可能被占用。",
					TextStyle: t.TextStyleWarning,
					Mode:      gui.TextModeWrap,
					Sizing:    gui.FillFit,
				}))
			}

			actions := make([]gui.View, 0, 5)
			if st.Installed {
				actions = append(actions,
					gui.Button(gui.ButtonCfg{
						ID:      "syncfix-open-dir",
						Label:   "打开目录",
						Variant: gui.ButtonGhost,
						OnClick: func(ctx gui.EventCtx) {
							ctx.Consume()
							a := gui.State[clientApp](ctx.Window)
							a.openPath(ctx.Window, a.syncFixMod.InstallDir)
						},
					}),
					gui.Button(gui.ButtonCfg{
						ID:      "syncfix-uninstall",
						Label:   "卸载",
						Variant: gui.ButtonDanger,
						OnClick: func(ctx gui.EventCtx) {
							ctx.Consume()
							gui.State[clientApp](ctx.Window).uninstallSyncFixFromUI(ctx.Window)
						},
					}),
				)
			}
			actions = append(actions,
				gui.Row(gui.ContainerCfg{
					Sizing:     gui.FillFit,
					Padding:    gui.NoPadding,
					SizeBorder: gui.NoBorder,
				}),
				gui.Button(gui.ButtonCfg{
					ID:      "syncfix-cancel",
					Label:   "关闭",
					Variant: gui.ButtonSecondary,
					OnClick: func(ctx gui.EventCtx) {
						ctx.Consume()
						ctx.Window.DialogDismiss()
					},
				}),
				gui.Button(gui.ButtonCfg{
					ID:       "syncfix-confirm-install",
					Label:    confirmLabel,
					Variant:  confirmVariant,
					Disabled: st.Err != "",
					OnClick: func(ctx gui.EventCtx) {
						ctx.Consume()
						gui.State[clientApp](ctx.Window).installSyncFixFromUI(ctx.Window)
					},
				}),
			)

			bodyItems = append(bodyItems, gui.Row(gui.ContainerCfg{
				Sizing:     gui.FillFit,
				Padding:    gui.NewPadding(4, 0, 0, 0),
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingSmall,
				VAlign:     gui.VAlignMiddle,
				Content:    actions,
			}))

			return gui.Column(gui.ContainerCfg{
				Sizing:     gui.FillFit,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingMedium,
				Content:    bodyItems,
			})
		},
	})
}

func (a *clientApp) statusMeta() (title, detail string, variant gui.BadgeVariant, accentColor gui.Color) {
	t := gui.CurrentTheme()
	if a.closing {
		return "正在关闭", "", gui.BadgeWarning, t.Cfg.ColorWarning
	}
	snap := a.snap
	detail = snap.Detail
	if first, _, multi := strings.Cut(detail, "\n"); multi {
		detail = first
	}
	switch snap.State {
	case stateConnecting:
		return "正在连接", detail, gui.BadgeInfo, t.ColorAccent
	case stateLeaving:
		return "正在离开", "", gui.BadgeWarning, t.Cfg.ColorWarning
	case stateFailed:
		return "连接失败", detail, gui.BadgeError, t.Cfg.ColorError
	case stateJoined:
		switch {
		case snap.Tunnel.Silent:
			return "中转无响应", "正在重试探测", gui.BadgeError, t.Cfg.ColorError
		case snap.Capture && !snap.BridgeRunning:
			return "转发已停止", "请查看诊断与日志", gui.BadgeError, t.Cfg.ColorError
		case !snap.Capture:
			return "已进入房间", "测试模式（未抓包）", gui.BadgeSuccess, t.Cfg.ColorSuccess
		default:
			if snap.PCAPError != "" {
				detail = "PCAP 记录异常"
			} else if snap.Host && snap.PCAPActive && snap.PCAPPath != "" {
				detail = fmt.Sprintf("PCAP 已写入 %d 包", snap.PCAPPackets)
			}
			return "运行中", detail, gui.BadgeSuccess, t.Cfg.ColorSuccess
		}
	default:
		return "未连接", detail, gui.BadgeDefault, t.ColorBorder
	}
}

func (a *clientApp) view(w *gui.Window) gui.View {
	t := gui.CurrentTheme()

	var pageView gui.View
	switch a.page {
	case pageNetTest:
		pageView = a.netTestView(w)
	case pageDiagLog:
		pageView = a.diagLogView(w)
	case pageModPatch:
		pageView = a.modPatchView(w)
	default:
		pageView = a.lobbyView(w)
	}

	return gui.Row(gui.ContainerCfg{
		ID:         "app-root",
		Sizing:     gui.FillFill,
		Color:      t.ColorBackground,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.NoSpacing,
		OnClick: func(ctx gui.EventCtx) {
			app := gui.State[clientApp](ctx.Window)
			if app.historyOpen {
				ctx.Consume()
				app.historyOpen = false
				ctx.Window.InvalidateLayout()
			}
		},
		Content: []gui.View{
			a.sidebarView(w),
			gui.Separator(gui.SeparatorCfg{Vertical: true}),
			gui.Column(gui.ContainerCfg{
				ID:         "main-shell",
				Sizing:     gui.FillFill,
				Color:      t.ColorBackground,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.NoSpacing,
				Content: []gui.View{
					gui.Column(gui.ContainerCfg{
						ID:         "page-host",
						Sizing:     gui.FillFill,
						Padding:    gui.NewPadding(18, 22, 16, 22),
						SizeBorder: gui.NoBorder,
						Spacing:    gui.SpacingMedium,
						Content:    []gui.View{pageView},
					}),
					gui.Separator(gui.SeparatorCfg{}),
					a.footerBarView(w),
				},
			}),
		},
	})
}

func (a *clientApp) sidebarView(w *gui.Window) gui.View {
	t := gui.CurrentTheme()
	statusTitle, _, statusVariant, _ := a.statusMeta()

	navBtn := func(id string, page navPage, label string, badgeText string, badgeVariant gui.BadgeVariant) gui.View {
		active := a.page == page
		bg := gui.ColorTransparent
		if active {
			bg = t.ColorAccentSubtle
		}
		indicatorColor := gui.ColorTransparent
		if active {
			indicatorColor = t.ColorAccent
		}
		labelStyle := t.TextStyleBody
		if active {
			labelStyle = t.TextStyleBody.Bold()
		}

		rowItems := []gui.View{
			gui.Rectangle(gui.RectangleCfg{
				Width:  3,
				Height: 16,
				Sizing: gui.FixedFixed,
				Radius: t.RadiusSmall,
				Color:  indicatorColor,
			}),
			gui.Text(gui.TextCfg{
				Text:      label,
				TextStyle: labelStyle,
				Sizing:    gui.FillFit,
			}),
		}
		if badgeText != "" {
			rowItems = append(rowItems, gui.Badge(gui.BadgeCfg{
				Label:   badgeText,
				Variant: badgeVariant,
			}))
		}

		return gui.Row(gui.ContainerCfg{
			ID:           id,
			Focusable:    true,
			ClickOnEnter: true,
			ClickOnSpace: true,
			Sizing:       gui.FillFit,
			Color:        bg,
			Hover: gui.HoverStyle{
				Color:  t.ColorHover,
				Cursor: gui.CursorPointingHand,
			},
			Radius:     gui.RadiusMedium,
			SizeBorder: gui.NoBorder,
			Padding:    gui.NewPadding(9, 10, 9, 8),
			Spacing:    gui.SpacingSmall,
			VAlign:     gui.VAlignMiddle,
			A11YRole:   gui.AccessRoleTab,
			A11YCfg:    gui.A11YCfg{A11YLabel: label},
			OnClick: func(ctx gui.EventCtx) {
				ctx.Consume()
				app := gui.State[clientApp](ctx.Window)
				app.historyOpen = false
				app.page = page
				if page == pageNetTest {
					app.netTest.syncTarget(app.relayText)
				}
				if page == pageDiagLog && app.autoScroll && len(app.logEntries) > 0 {
					ctx.Window.QueueCommand(func(vw *gui.Window) {
						if ids := vw.ResolveID("log-list"); len(ids) > 0 {
							vw.ScrollToEnd(ids[0])
						}
					})
				}
				if page == pageModPatch {
					app.refreshModPatches()
				}
				ctx.Window.InvalidateLayout()
			},
			Content: rowItems,
		})
	}

	netTestBadge := ""
	netTestVariant := gui.BadgeInfo
	if a.netTest.running() {
		netTestBadge = "运行中"
		netTestVariant = gui.BadgeWarning
	}

	diagBadge := ""
	diagVariant := gui.BadgeDefault
	if a.diag.busy {
		diagBadge = "诊断中"
		diagVariant = gui.BadgeInfo
	} else if a.diag.hasResult && !a.diag.ok {
		diagBadge = "异常"
		diagVariant = gui.BadgeError
	}

	modPatchBadge, modPatchVariant := modPatchNavBadge(a.modPatches)

	lobbyBadge := ""
	lobbyVariant := gui.BadgeSuccess
	if a.snap.State == stateJoined && len(a.snap.Members) > 0 {
		online := 0
		for _, m := range a.snap.Members {
			if m.Online {
				online++
			}
		}
		lobbyBadge = fmt.Sprintf("%d/%d", online, len(a.snap.Members))
	}

	civBadgeLabel, civBadgeVariant := "未运行", gui.BadgeDefault
	if a.snap.CivRunning {
		civBadgeLabel, civBadgeVariant = "运行中", gui.BadgeSuccess
	}

	rttText := "—"
	if a.snap.Tunnel.HasRTT {
		if a.snap.Tunnel.RTT < time.Millisecond {
			rttText = "< 1 ms"
		} else {
			rttText = fmt.Sprintf("%d ms", a.snap.Tunnel.RTT.Milliseconds())
		}
	}

	summaryItems := []gui.View{
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingSmall,
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Badge(gui.BadgeCfg{Dot: true, Variant: statusVariant}),
				gui.Text(gui.TextCfg{
					Text:      statusTitle,
					TextStyle: t.TextStyleBodySmall.Bold(),
					Mode:      gui.TextModeWrap,
					Sizing:    gui.FillFit,
				}),
			},
		}),
	}

	if a.snap.State == stateJoined && a.activeCode != "" {
		summaryItems = append(summaryItems, gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NewPadding(4, 0, 0, 0),
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingSmall,
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Text(gui.TextCfg{
					Text:      a.activeCode,
					TextStyle: t.TextStyleCode.Bold(),
					Sizing:    gui.FillFit,
				}),
				gui.Button(gui.ButtonCfg{
					ID:      "sidebar-copy-code",
					Label:   "复制",
					Variant: gui.ButtonGhost,
					Padding: gui.NewPadding(3, 6, 3, 6),
					OnClick: func(ctx gui.EventCtx) {
						ctx.Consume()
						gui.State[clientApp](ctx.Window).copyRoomCode(ctx.Window)
					},
				}),
			},
		}))
	}

	summaryItems = append(summaryItems,
		gui.Separator(gui.SeparatorCfg{}),
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Text(gui.TextCfg{
					Text:      "文明 VI",
					TextStyle: t.TextStyleCaption,
					Sizing:    gui.FillFit,
				}),
				gui.Badge(gui.BadgeCfg{
					Label:   civBadgeLabel,
					Variant: civBadgeVariant,
				}),
			},
		}),
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Text(gui.TextCfg{
					Text:      "中转延迟",
					TextStyle: t.TextStyleCaption,
					Sizing:    gui.FillFit,
				}),
				gui.Text(gui.TextCfg{
					Text:      rttText,
					TextStyle: t.TextStyleCodeSmall,
				}),
			},
		}),
		func() gui.View {
			launchLabel := "启动游戏"
			launchVariant := gui.ButtonPrimary
			launchDisabled := a.closing
			if a.snap.CivRunning {
				launchLabel = "游戏运行中"
				launchVariant = gui.ButtonSecondary
				launchDisabled = true
			}
			return gui.Button(gui.ButtonCfg{
				ID:       "sidebar-launch-civ6",
				Label:    launchLabel,
				Variant:  launchVariant,
				Disabled: launchDisabled,
				Sizing:   gui.FillFit,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					gui.State[clientApp](ctx.Window).launchCiv6(ctx.Window)
				},
			})
		}(),
	)

	return gui.Column(gui.ContainerCfg{
		ID:         "nav-rail",
		Width:      224,
		Sizing:     gui.FixedFill,
		Color:      t.ColorPanel,
		Padding:    gui.NewPadding(18, 14, 14, 14),
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		Content: []gui.View{
			// Brand header
			gui.Column(gui.ContainerCfg{
				Sizing:     gui.FillFit,
				Padding:    gui.NewPadding(0, 4, 6, 4),
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingPx(4),
				Content: []gui.View{
					gui.Row(gui.ContainerCfg{
						Sizing:     gui.FillFit,
						Padding:    gui.NoPadding,
						SizeBorder: gui.NoBorder,
						Spacing:    gui.SpacingSmall,
						VAlign:     gui.VAlignMiddle,
						Content: []gui.View{
							gui.Rectangle(gui.RectangleCfg{
								Width:  4,
								Height: 22,
								Sizing: gui.FixedFixed,
								Radius: t.RadiusSmall,
								Color:  t.ColorAccent,
							}),
							gui.Text(gui.TextCfg{
								Text:      "文明 VI 中继",
								TextStyle: t.TextStyleTitleSmall,
							}),
						},
					}),
					gui.Text(gui.TextCfg{
						Text:      "Civ6Relay · " + version,
						TextStyle: t.TextStyleCaption,
					}),
				},
			}),
			gui.Separator(gui.SeparatorCfg{}),
			// Navigation items
			gui.Column(gui.ContainerCfg{
				Sizing:     gui.FillFit,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingPx(4),
				Content: []gui.View{
					navBtn("nav-lobby", pageLobby, "联机大厅", lobbyBadge, lobbyVariant),
					navBtn("nav-nettest", pageNetTest, "网络测试", netTestBadge, netTestVariant),
					navBtn("nav-diaglog", pageDiagLog, "诊断与日志", diagBadge, diagVariant),
					navBtn("nav-modpatch", pageModPatch, "Mod 补丁", modPatchBadge, modPatchVariant),
				},
			}),
			// Spacer
			gui.Column(gui.ContainerCfg{
				Sizing:     gui.FillFill,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
			}),
			// One-click SyncFix Mod installer card
			a.sidebarSyncFixCard(w),
			// Persistent live status summary card
			subtlePanelView("sidebar-status-card", gui.FillFit, summaryItems...),
		},
	})
}

func (a *clientApp) sidebarSyncFixCard(w *gui.Window) gui.View {
	t := gui.CurrentTheme()
	st := a.syncFixMod
	badgeLabel := "未装"
	badgeVariant := gui.BadgeDefault
	btnLabel := "安装 / 订阅"
	btnVariant := gui.ButtonPrimary
	if st.WorkshopInstalled && st.Installed {
		badgeLabel = "工坊+本地"
		badgeVariant = gui.BadgeWarning
		btnLabel = "管理 Mod"
		btnVariant = gui.ButtonSecondary
	} else if st.WorkshopInstalled {
		badgeLabel = "已订阅"
		badgeVariant = gui.BadgeSuccess
		btnLabel = "管理 Mod"
		btnVariant = gui.ButtonSecondary
	} else if st.UpToDate {
		badgeLabel = "已装"
		badgeVariant = gui.BadgeSuccess
		btnLabel = "管理 Mod"
		btnVariant = gui.ButtonSecondary
	} else if st.Installed {
		badgeLabel = "可更新"
		badgeVariant = gui.BadgeWarning
		btnLabel = "更新 Mod"
		btnVariant = gui.ButtonPrimary
	}

	return subtlePanelView("sidebar-syncfix-card", gui.FillFit,
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Text(gui.TextCfg{
					Text:      "同步修复 Mod",
					TextStyle: t.TextStyleCaption,
					Sizing:    gui.FillFit,
				}),
				gui.Badge(gui.BadgeCfg{
					Label:   badgeLabel,
					Variant: badgeVariant,
				}),
			},
		}),
		gui.Button(gui.ButtonCfg{
			ID:       "sidebar-syncfix-btn",
			Label:    btnLabel,
			Variant:  btnVariant,
			Disabled: a.closing,
			Sizing:   gui.FillFit,
			OnClick: func(ctx gui.EventCtx) {
				ctx.Consume()
				gui.State[clientApp](ctx.Window).openSyncFixDialog(ctx.Window)
			},
		}),
	)
}

func (a *clientApp) footerBarView(w *gui.Window) gui.View {
	t := gui.CurrentTheme()
	snap := a.snap

	rtt, traffic, drops := "—", "—", "—"
	if snap.Tunnel.HasRTT {
		if snap.Tunnel.RTT < time.Millisecond {
			rtt = "< 1 ms"
		} else {
			rtt = fmt.Sprintf("%d ms", snap.Tunnel.RTT.Milliseconds())
		}
	}
	if snap.State == stateJoined {
		traffic = fmt.Sprintf("%d / %d", snap.Tunnel.GameRxDatagrams, snap.Tunnel.GameTxDatagrams)
		if snap.Capture {
			drops = fmt.Sprint(snap.Bridge.Dropped)
		} else {
			drops = fmt.Sprint(snap.Tunnel.Dropped + snap.Tunnel.AuthFailures + snap.Tunnel.ReplayDrops)
		}
	}

	rightInfo := ""
	if snap.Subnet != "" {
		rightInfo = "网段 " + snap.Subnet
	}
	if snap.PCAPPath != "" {
		if rightInfo != "" {
			rightInfo += " · "
		}
		rightInfo += fmt.Sprintf("PCAP %d 包", snap.PCAPPackets)
		if snap.PCAPDropped != 0 {
			rightInfo += fmt.Sprintf(" (丢 %d)", snap.PCAPDropped)
		}
	}
	if snap.LogHost.Enabled {
		if rightInfo != "" {
			rightInfo += " · "
		}
		rightInfo += fmt.Sprintf("日志 %s", logsync.FormatByteSize(snap.LogHost.TotalBytes))
	} else if snap.LogGuest.Active {
		if rightInfo != "" {
			rightInfo += " · "
		}
		rightInfo += fmt.Sprintf("上传日志 %s", logsync.FormatByteSize(snap.LogGuest.BytesSent))
	}

	metric := func(label, val string) gui.View {
		return gui.Row(gui.ContainerCfg{
			Sizing:     gui.FitFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingPx(6),
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Text(gui.TextCfg{Text: label, TextStyle: t.TextStyleCaption}),
				gui.Text(gui.TextCfg{Text: val, TextStyle: t.TextStyleCodeSmall}),
			},
		})
	}

	items := []gui.View{
		metric("延迟", rtt),
		gui.Separator(gui.SeparatorCfg{Vertical: true, Length: 12}),
		metric("收/发", traffic),
		gui.Separator(gui.SeparatorCfg{Vertical: true, Length: 12}),
		metric("丢弃", drops),
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
		}),
	}
	if rightInfo != "" {
		items = append(items, gui.Text(gui.TextCfg{
			Text:      rightInfo,
			TextStyle: t.TextStyleCaption,
		}))
	}
	items = append(items, gui.Button(gui.ButtonCfg{
		ID:      "footer-capture-dir",
		Label:   "抓包目录",
		Variant: gui.ButtonGhost,
		Padding: gui.NewPadding(3, 8, 3, 8),
		OnClick: func(ctx gui.EventCtx) {
			ctx.Consume()
			gui.State[clientApp](ctx.Window).openCapturesDir(ctx.Window)
		},
	}))

	return gui.Row(gui.ContainerCfg{
		ID:         "footer-bar",
		Sizing:     gui.FillFit,
		Color:      t.ColorPanel,
		Padding:    gui.NewPadding(8, 18, 8, 18),
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		VAlign:     gui.VAlignMiddle,
		Content:    items,
	})
}

func canJoinWithDraft(canConnect bool, draft string) bool {
	if !canConnect {
		return false
	}
	_, err := protocol.NormalizeRoomCode(draft)
	return err == nil
}
