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
	relayText    string
	relayHistory []string
	historyOpen  bool
	nameText     string
	joinDraft    string
	activeCode   string

	// Cached session snapshot & log entries
	snap       sessionSnapshot
	prevState  connectionState
	hostPCAP   bool
	hostP2P    bool
	logEntries []logEntry
	logSeq     int64
	autoScroll bool

	// Transient states
	closing    bool
	exportBusy bool
	diag       diagState
	netTest    netTestState
	syncFixMod syncFixModStatus
	openPathFn func(string) error       // optional test override for openFolder
	modsRootFn func() (string, error)   // optional test override for defaultCiv6ModsRoot

	refreshQueued atomic.Bool
	stopped       atomic.Bool
}

func newClientApp(s *session, cfg Config) *clientApp {
	app := &clientApp{
		s:            s,
		page:         pageLobby,
		relayText:    win.FormatRelay(cfg.RelayHost, cfg.RelayPort),
		relayHistory: append([]string(nil), cfg.RelayHistory...),
		nameText:     cfg.DisplayName,
		hostPCAP:     cfg.HostPCAP,
		hostP2P:      cfg.HostP2P,
		autoScroll:   true,
	}
	app.netTest.init()
	app.refreshSyncFixStatus()
	if s != nil {
		app.snap = s.Snapshot()
		app.prevState = app.snap.State
		app.hostPCAP = app.snap.HostPCAP
		app.hostP2P = app.snap.HostP2P
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

	if snap.State == stateJoined && snap.RoomCode != "" {
		a.activeCode = snap.RoomCode
	} else if snap.State != stateJoined {
		a.activeCode = ""
	}

	a.s.SyncP2PFirewall(snap.P2P)

	if prevState == stateConnecting && snap.State == stateJoined && snap.RoomCode != "" {
		a.relayHistory = loadConfig().RelayHistory
		a.toast(w, gui.ToastSuccess, "已进入房间", "房间码 "+snap.RoomCode+" 已就绪，可复制分享给朋友。")
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
	if err := a.s.Connect(join, a.relayText, a.nameText, a.joinDraft); err != nil {
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
	a.toast(w, gui.ToastSuccess, "房间码已复制", a.activeCode)
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
			if ok {
				app.toast(vw, gui.ToastSuccess, "诊断通过", "WinDivert 驱动和网络环境正常。")
			} else {
				app.toast(vw, gui.ToastError, "诊断发现问题", "请查看下方诊断结果与完整报告。")
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
				app.toast(vw, gui.ToastError, "导出失败", "请检查保存目录的写入权限。")
			} else {
				app.toast(vw, gui.ToastSuccess, "运行日志已导出", path)
			}
			vw.InvalidateLayout()
		})
	}()
}

func (a *clientApp) openCapturesDir(w *gui.Window) {
	dir := captureDirectory()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		a.toast(w, gui.ToastError, "打开失败", "无法创建抓包目录，请检查本地磁盘权限。")
		return
	}
	if a.openPathFn != nil {
		if err := a.openPathFn(dir); err != nil {
			a.toast(w, gui.ToastError, "打开失败", err.Error())
			return
		}
		a.toast(w, gui.ToastInfo, "本地抓包目录已打开", dir)
		return
	}
	if err := openFolder(dir); err != nil {
		a.toast(w, gui.ToastError, "打开失败", err.Error())
		return
	}
	a.toast(w, gui.ToastInfo, "本地抓包目录已打开", dir)
}

func (a *clientApp) modsRoot() (string, error) {
	if a.modsRootFn != nil {
		return a.modsRootFn()
	}
	return defaultCiv6ModsRoot()
}

func (a *clientApp) refreshSyncFixStatus() {
	root, err := a.modsRoot()
	if err != nil {
		a.syncFixMod = syncFixModStatus{Err: err.Error()}
		return
	}
	a.syncFixMod = inspectSyncFixMod(root)
}

func (a *clientApp) openPath(w *gui.Window, dir, title string) {
	if a.openPathFn != nil {
		if err := a.openPathFn(dir); err != nil {
			a.toast(w, gui.ToastError, "打开失败", err.Error())
			return
		}
		a.toast(w, gui.ToastInfo, title, dir)
		return
	}
	if err := openFolder(dir); err != nil {
		a.toast(w, gui.ToastError, "打开失败", err.Error())
		return
	}
	a.toast(w, gui.ToastInfo, title, dir)
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
			a.s.logError("安装同步修复 Mod 失败: %v", err)
		}
		a.toast(w, gui.ToastError, "同步修复 Mod 安装失败", err.Error())
		w.InvalidateLayout()
		return
	}
	if a.s != nil {
		a.s.logOK("已安装同步修复 Mod 到 %s", installDir)
	}
	w.DialogDismiss()
	a.toast(w, gui.ToastSuccess, "同步修复 Mod 安装成功", "请确保已取消订阅旧版 FastSync，并在游戏「额外内容 › 模组」勾选 Civ6SyncFix。")
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
			a.s.logError("卸载同步修复 Mod 失败: %v", err)
		}
		a.toast(w, gui.ToastError, "卸载失败", err.Error())
		return
	}
	a.refreshSyncFixStatus()
	if a.s != nil {
		a.s.logInfo("已卸载同步修复 Mod (%s)", a.syncFixMod.InstallDir)
	}
	w.DialogDismiss()
	a.toast(w, gui.ToastInfo, "同步修复 Mod 已卸载", "已移除本地 Civ6SyncFix 模组目录。")
	w.InvalidateLayout()
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
		Title:      "一键安装同步修复 Mod（Civ6SyncFix）",
		Width:      500,
		FocusID:    "syncfix-confirm-install",
		CustomView: func(vw *gui.Window) gui.View {
			app := gui.State[clientApp](vw)
			t := gui.CurrentTheme()
			st := app.syncFixMod

			statusLabel := "尚未安装"
			statusVariant := gui.BadgeDefault
			confirmLabel := "确定安装"
			if st.Err != "" {
				statusLabel = "目录异常"
				statusVariant = gui.BadgeError
			} else if st.UpToDate {
				statusLabel = "已安装最新"
				statusVariant = gui.BadgeSuccess
				confirmLabel = "重新安装"
			} else if st.Installed {
				statusLabel = "检测到旧版"
				statusVariant = gui.BadgeWarning
				confirmLabel = "确定更新"
			}

			targetText := st.InstallDir
			if st.Err != "" {
				targetText = st.Err
			}

			bodyItems := []gui.View{
				gui.Text(gui.TextCfg{
					Text:      "Civ6SyncFix v1.3（DX11 / DX12 全支持）：修复 64 位哈希碰撞卡顿、局内重载隔回合死循环、后期 AI 重排巨作严重卡顿、国家奇观被抢打断、原版宗教 AI 未初始化不同步，并内置全对象字段级不同步诊断日志。",
					TextStyle: t.TextStyleBody,
					Mode:      gui.TextModeWrap,
					Sizing:    gui.FillFit,
				}),
				subtlePanelView("syncfix-dialog-info", gui.FillFit,
					gui.Row(gui.ContainerCfg{
						Sizing:     gui.FillFit,
						Padding:    gui.NoPadding,
						SizeBorder: gui.NoBorder,
						Spacing:    gui.SpacingSmall,
						VAlign:     gui.VAlignMiddle,
						Content: []gui.View{
							gui.Text(gui.TextCfg{
								Text:      "安装状态",
								TextStyle: t.TextStyleLabel,
								Sizing:    gui.FillFit,
							}),
							gui.Badge(gui.BadgeCfg{
								Label:   statusLabel,
								Variant: statusVariant,
							}),
						},
					}),
					gui.Text(gui.TextCfg{
						Text:      "目标路径",
						TextStyle: t.TextStyleCaption,
					}),
					gui.Text(gui.TextCfg{
						Text:      targetText,
						TextStyle: t.TextStyleCodeSmall,
						Mode:      gui.TextModeWrap,
						Sizing:    gui.FillFit,
					}),
				),
				gui.Text(gui.TextCfg{
					Text:      "安装后请在 Steam 创意工坊取消订阅旧版 FastSync 与 DX12 独立内核修复（3823991616），并在游戏「额外内容 › 模组」勾选「Civ6SyncFix」即可。",
					TextStyle: t.TextStyleSecondary,
					Mode:      gui.TextModeWrap,
					Sizing:    gui.FillFit,
				}),
			}

			if app.snap.CivRunning {
				bodyItems = append(bodyItems, gui.Text(gui.TextCfg{
					Text:      "提示：当前检测到文明 VI 正在运行，若写入 DLL 提示被占用，请先退出游戏后再点确定。",
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
							a.openPath(ctx.Window, a.syncFixMod.InstallDir, "Mod 目录已打开")
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
					Label:   "取消",
					Variant: gui.ButtonSecondary,
					OnClick: func(ctx gui.EventCtx) {
						ctx.Consume()
						ctx.Window.DialogDismiss()
					},
				}),
				gui.Button(gui.ButtonCfg{
					ID:       "syncfix-confirm-install",
					Label:    confirmLabel,
					Variant:  gui.ButtonPrimary,
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
		return "正在关闭", "正在结束房间会话并释放网络资源…", gui.BadgeWarning, t.Cfg.ColorWarning
	}
	snap := a.snap
	detail = snap.Detail
	if first, _, multi := strings.Cut(detail, "\n"); multi {
		detail = first + "。完整信息见「诊断与日志」。"
	}
	switch snap.State {
	case stateConnecting:
		if detail == "" {
			detail = "正在联系中转服务器…"
		}
		return "正在连接", detail, gui.BadgeInfo, t.ColorAccent
	case stateLeaving:
		if detail == "" {
			detail = "正在离开房间…"
		}
		return "正在结束会话", detail, gui.BadgeWarning, t.Cfg.ColorWarning
	case stateFailed:
		return "连接失败", detail, gui.BadgeError, t.Cfg.ColorError
	case stateJoined:
		switch {
		case snap.Tunnel.Silent:
			return "中转暂时无响应", "正在继续探测中转。连接恢复后会自动更新。", gui.BadgeError, t.Cfg.ColorError
		case snap.Capture && !snap.BridgeRunning:
			return "游戏转发已停止", "请前往「诊断与日志」查看运行日志或运行诊断。", gui.BadgeError, t.Cfg.ColorError
		case !snap.Capture:
			return "已进入房间", "隧道测试模式，游戏流量未接入。", gui.BadgeSuccess, t.Cfg.ColorSuccess
		default:
			if snap.PCAPError != "" {
				detail = "本地 PCAP 记录异常，请查看运行日志。"
			} else if snap.Host && snap.PCAPActive && snap.PCAPPath != "" {
				detail = fmt.Sprintf("房主端正在记录游戏 UDP（%d 包），自动保留最近 3 天。", snap.PCAPPackets)
			}
			if detail == "" {
				detail = "成员上线后，在文明 VI 的「多人游戏 › 局域网」创建或加入游戏。"
			}
			return "游戏转发运行中", detail, gui.BadgeSuccess, t.Cfg.ColorSuccess
		}
	default:
		if detail == "" {
			detail = "和朋友使用同一中转地址，再创建房间或输入房间码加入。"
		}
		return "尚未连接", detail, gui.BadgeDefault, t.ColorBorder
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
	badgeLabel := "未安装"
	badgeVariant := gui.BadgeDefault
	btnLabel := "一键安装修复 Mod"
	btnVariant := gui.ButtonPrimary
	if st.UpToDate {
		badgeLabel = "已安装"
		badgeVariant = gui.BadgeSuccess
		btnLabel = "同步修复 Mod"
		btnVariant = gui.ButtonSecondary
	} else if st.Installed {
		badgeLabel = "可更新"
		badgeVariant = gui.BadgeWarning
		btnLabel = "更新修复 Mod"
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
					Text:      "联机同步修复",
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
		metric("游戏收/发", traffic),
		gui.Separator(gui.SeparatorCfg{Vertical: true, Length: 12}),
		metric("本地丢弃", drops),
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
