//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-gui-org/go-gui/gui"
)

func (a *clientApp) diagLogView(w *gui.Window) gui.View {
	return gui.Column(gui.ContainerCfg{
		ID:         "diaglog-page",
		Sizing:     gui.FillFill,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		Content: []gui.View{
			a.diagnosticsCard(w),
			a.logsCard(w),
		},
	})
}

func (a *clientApp) diagnosticsCard(w *gui.Window) gui.View {
	t := gui.CurrentTheme()
	d := &a.diag

	statusLabel, statusVariant := "未运行", gui.BadgeDefault
	if d.busy {
		statusLabel, statusVariant = "诊断中…", gui.BadgeInfo
	} else if d.hasResult {
		if d.ok {
			statusLabel, statusVariant = "通过", gui.BadgeSuccess
		} else {
			statusLabel, statusVariant = "异常", gui.BadgeError
		}
	}

	canDiag := !d.busy && !a.closing && (a.canConnect() || a.snap.State == stateJoined)

	actions := []gui.View{
		gui.Text(gui.TextCfg{Text: "驱动与网络诊断", TextStyle: t.TextStyleTitleSmall}),
		gui.Badge(gui.BadgeCfg{Label: statusLabel, Variant: statusVariant}),
		gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder}),
	}
	if d.hasResult && d.report != "" {
		toggleText := "完整报告"
		if d.showFull {
			toggleText = "收起报告"
		}
		actions = append(actions,
			gui.Button(gui.ButtonCfg{
				ID:      "diag-toggle-full",
				Label:   toggleText,
				Variant: gui.ButtonGhost,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					app.diag.showFull = !app.diag.showFull
					ctx.Window.InvalidateLayout()
				},
			}),
			gui.Button(gui.ButtonCfg{
				ID:      "diag-open-report",
				Label:   "打开报告",
				Variant: gui.ButtonSecondary,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					if app.diag.reportPath == "" {
						return
					}
					app.openPath(ctx.Window, app.diag.reportPath)
				},
			}),
		)
	}
	actions = append(actions, gui.Button(gui.ButtonCfg{
		ID:       "diag-run",
		Label:    "运行诊断",
		Variant:  gui.ButtonPrimary,
		Disabled: !canDiag,
		Loading:  d.busy,
		MinWidth: 92,
		OnClick: func(ctx gui.EventCtx) {
			ctx.Consume()
			gui.State[clientApp](ctx.Window).runDiagnostics(ctx.Window)
		},
	}))

	items := []gui.View{
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingSmall,
			VAlign:     gui.VAlignMiddle,
			Content:    actions,
		}),
	}

	if d.hasResult {
		summaryText := d.summary
		if d.reportPath != "" {
			summaryText += "\n报告: " + d.reportPath
		}
		items = append(items, subtlePanelView("diag-summary-box", gui.FillFit,
			gui.Text(gui.TextCfg{
				Text:      summaryText,
				TextStyle: t.TextStyleCodeSmall,
				Mode:      gui.TextModeWrap,
			}),
		))
		if d.showFull && d.report != "" {
			items = append(items, gui.Column(gui.ContainerCfg{
				ID:          "diag-full-report-scroll",
				Sizing:      gui.FillFixed,
				Height:      180,
				Scrollable:  true,
				ScrollMode:  gui.ScrollVerticalOnly,
				Color:       t.ColorBackground,
				ColorBorder: t.ColorBorder,
				SizeBorder:  gui.BorderThin,
				Radius:      gui.RadiusMedium,
				Padding:     gui.NewPadding(10, 12, 10, 12),
				Content: []gui.View{
					gui.Text(gui.TextCfg{
						ID:        "diag-full-report-text",
						Text:      d.report,
						TextStyle: t.TextStyleCodeSmall,
						Mode:      gui.TextModeWrap,
						Focusable: true,
					}),
				},
			}))
		}
	}

	return cardView("diag-card", gui.FillFit, items...)
}

func (a *clientApp) logsCard(w *gui.Window) gui.View {
	t := gui.CurrentTheme()
	entries := a.logEntries

	toolbar := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingSmall,
		VAlign:     gui.VAlignMiddle,
		Content: []gui.View{
			gui.Text(gui.TextCfg{Text: "运行日志", TextStyle: t.TextStyleTitleSmall}),
			gui.Badge(gui.BadgeCfg{Label: fmt.Sprintf("%d 条", len(entries)), Variant: gui.BadgeDefault}),
			gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder}),
			gui.Switch(gui.SwitchCfg{
				ID:       "log-autoscroll",
				Label:    "自动滚动",
				Selected: a.autoScroll,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					app.autoScroll = !app.autoScroll
					if app.autoScroll && len(app.logEntries) > 0 {
						ctx.Window.QueueCommand(func(vw *gui.Window) {
							if ids := vw.ResolveID("log-list"); len(ids) > 0 {
								vw.ScrollToEnd(ids[0])
							}
						})
					}
					ctx.Window.InvalidateLayout()
				},
			}),
			gui.Button(gui.ButtonCfg{
				ID:       "log-copy",
				Label:    "复制",
				Variant:  gui.ButtonGhost,
				Disabled: len(entries) == 0,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					if app.s == nil {
						return
					}
					text, _ := app.s.logSnapshot(-1)
					ctx.Window.SetClipboard(text)
				},
			}),
			gui.Button(gui.ButtonCfg{
				ID:       "log-export",
				Label:    "导出日志",
				Variant:  gui.ButtonSecondary,
				Disabled: a.exportBusy || a.closing,
				Loading:  a.exportBusy,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					gui.State[clientApp](ctx.Window).exportLog(ctx.Window)
				},
			}),
			gui.Button(gui.ButtonCfg{
				ID:      "log-captures",
				Label:   "抓包目录",
				Variant: gui.ButtonSecondary,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					gui.State[clientApp](ctx.Window).openCapturesDir(ctx.Window)
				},
			}),
			gui.Button(gui.ButtonCfg{
				ID:      "log-collected-dir",
				Label:   "收集目录",
				Variant: gui.ButtonSecondary,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					gui.State[clientApp](ctx.Window).openCollectedLogsDir(ctx.Window, false)
				},
			}),
			gui.Button(gui.ButtonCfg{
				ID:      "log-ai-prompt",
				Label:   "ai诊断提示词",
				Variant: gui.ButtonPrimary,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					prompt := app.buildAIDiagPrompt()
					ctx.Window.SetClipboard(prompt)
					app.toast(ctx.Window, gui.ToastSuccess, "已复制 ai诊断提示词", "已将包含日志、抓包与 Mod 路径的排查提示词复制到剪贴板")
				},
			}),
		},
	})

	var logBody gui.View
	if len(entries) == 0 {
		logBody = gui.Column(gui.ContainerCfg{
			Sizing:     gui.FillFill,
			HAlign:     gui.HAlignCenter,
			VAlign:     gui.VAlignMiddle,
			SizeBorder: gui.NoBorder,
			Content: []gui.View{
				gui.Text(gui.TextCfg{Text: "暂无运行日志", TextStyle: t.TextStyleSecondary}),
			},
		})
	} else {
		logBody = gui.VirtualList(gui.VirtualListCfg{
			ID:        "log-list",
			ItemCount: len(entries),
			Sizing:    gui.FillFill,
			Color:     t.ColorBackground,
			Padding:   gui.NewPadding(6, 10, 6, 10),
			ItemKey: func(i int) string {
				e := entries[i]
				return fmt.Sprintf("%d-%s-%s", i, e.Time, e.Level)
			},
			ItemView: func(i int, _ float32) gui.View {
				e := entries[i]
				lvlColor := t.TextStyleSecondary.Color
				switch e.Level {
				case "OK":
					lvlColor = t.TextStyleSuccess.Color
				case "WARN":
					lvlColor = t.TextStyleWarning.Color
				case "ERROR":
					lvlColor = t.TextStyleError.Color
				}
				msg := strings.ReplaceAll(e.Message, "\r\n", "\n")
				return gui.Row(gui.ContainerCfg{
					Sizing:     gui.FillFit,
					Padding:    gui.NewPadding(3, 2, 3, 2),
					SizeBorder: gui.NoBorder,
					Spacing:    gui.SpacingSmall,
					VAlign:     gui.VAlignTop,
					Content: []gui.View{
						gui.Text(gui.TextCfg{
							Text:      e.Time,
							TextStyle: textStyleWithColor(t.TextStyleCodeSmall, t.TextStyleSecondary.Color),
						}),
						gui.Text(gui.TextCfg{
							Text:      fmt.Sprintf("[%s]", e.Level),
							TextStyle: textStyleWithColor(t.TextStyleCodeSmall.Bold(), lvlColor),
						}),
						gui.Text(gui.TextCfg{
							Text:      msg,
							TextStyle: t.TextStyleCodeSmall,
							Mode:      gui.TextModeWrap,
							Sizing:    gui.FillFit,
						}),
					},
				})
			},
		})
	}

	return cardView("logs-card", gui.FillFill, toolbar, logBody)
}

func (a *clientApp) buildAIDiagPrompt() string {
	civLogs, relayLogs, relayCaptures, _, defaultDiagReport := localLogPaths()
	collectedDir := collectedLogsDirectory()

	pcapPath := strings.TrimSpace(a.snap.PCAPPath)
	if pcapPath == "" {
		pcapPath = latestEntryPath(relayCaptures, ".pcap", false)
	}
	logSyncDir := strings.TrimSpace(a.snap.LogSyncDir)
	if logSyncDir == "" {
		logSyncDir = latestEntryPath(collectedDir, "", true)
	}
	diagPath := strings.TrimSpace(a.diag.reportPath)
	if diagPath == "" {
		diagPath = defaultDiagReport
	}

	var wsContentDirs []string
	for _, root := range a.steamLibraryRoots() {
		dir := filepath.Join(root, "steamapps", "workshop", "content", civ6SteamAppID)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			wsContentDirs = append(wsContentDirs, dir)
		}
	}
	localModsDir, _ := a.modsRoot()

	role := "未进入房间"
	if a.snap.State == stateJoined {
		if a.snap.Host {
			role = "房主"
		} else {
			role = "队员"
		}
	}
	roomCode := strings.TrimSpace(a.snap.RoomCode)
	if roomCode == "" {
		roomCode = strings.TrimSpace(a.activeCode)
	}
	if roomCode == "" {
		roomCode = "无"
	}
	relayAddr := strings.TrimSpace(a.relayText)
	if relayAddr == "" {
		relayAddr = "未填写"
	}

	var b strings.Builder
	b.WriteString("请帮我排查《文明 VI》（Sid Meier's Civilization VI）联机过程中出现的不同步（DESYNC / DESYNC_DELTA）、加载卡死、掉线或中继转发异常。\n")
	b.WriteString("请直接读取我本机以下日志与抓包目录中的真实文件内容，结合代码或 Mod 脚本分析根因。\n\n")

	b.WriteString("## 一、当前联机运行环境\n")
	fmt.Fprintf(&b, "- 客户端版本: Civ6RelayClient %s\n", version)
	fmt.Fprintf(&b, "- 当前身份/状态: %s（房号: %s，中继地址: %s，游戏进程运行中: %v）\n",
		role, roomCode, relayAddr, a.snap.CivRunning)
	fmt.Fprintf(&b, "- 会话开关: P2P=%v | PCAP抓包=%v | 自动收集队友日志=%v\n\n",
		a.snap.HostP2P, a.snap.PCAPActive, a.snap.HostLogSync)

	b.WriteString("## 二、本机日志与抓包路径\n")
	fmt.Fprintf(&b, "1. 文明 VI 游戏日志目录: `%s`\n", civLogs)
	b.WriteString("   - 重点关注: `net_message_debug.log`（同步/不同步明细）、`Lua.log`、`Modding.log`、`net_connection_debug.log`、`Civ6SyncFix.log`、`Database.log`，以及文件名包含 `Desync`/`Sync`/`C6SE` 的日志。\n")
	fmt.Fprintf(&b, "2. 抓包目录 (PCAP): `%s`\n", relayCaptures)
	if pcapPath != "" {
		fmt.Fprintf(&b, "   - 最新/当前抓包文件: `%s`\n", pcapPath)
	}
	fmt.Fprintf(&b, "3. 房主收集的联机日志目录: `%s`\n", collectedDir)
	if logSyncDir != "" {
		fmt.Fprintf(&b, "   - 最新/当前房间收集目录: `%s`（含房主与各队友的 `civ6_logs/` 及 `env_check.txt`）\n", logSyncDir)
	}
	fmt.Fprintf(&b, "4. Civ6Relay 客户端日志目录: `%s`\n", relayLogs)
	fmt.Fprintf(&b, "5. 网络与驱动诊断报告路径: `%s`\n", diagPath)
	if localModsDir != "" {
		fmt.Fprintf(&b, "6. 本地 Mods 目录: `%s`\n", localModsDir)
	}
	if len(wsContentDirs) > 0 {
		fmt.Fprintf(&b, "7. Steam 创意工坊 Mod 目录: `%s`\n", strings.Join(wsContentDirs, "`, `"))
	}
	fmt.Fprintf(&b, "8. 客户端 Mod 补丁备份目录: `%s`\n\n", a.patchBackupRootDir())

	b.WriteString("## 三、排查要求与重点\n")
	b.WriteString("1. **先定性问题类型**：区分是「游戏逻辑不同步（DESYNC_DELTA / Hash 不匹配）」、「Mod 版本或启用状态不一致（Modding.log）」还是「网络丢包/端口未转发/隧道断连（PCAP 与 net_connection_debug.log）」。\n")
	b.WriteString("2. **若存在不同步（Desync）**：请对照房主与队友的 `net_message_debug.log`、`Lua.log`、`Modding.log`，精确指出是哪个 Mod（含 Workshop ID、Lua 文件名、函数/事件与关键字段），具体说明它为什么会在多人端产生不同结果（如非本地玩家误触发 UI 指令、随机遍历 `pairs` 顺序不一致、跨 UI/Gameplay 直接修改状态未走同步事件等）。\n")
	b.WriteString("3. **若存在网络/掉线异常**：请解析 `.pcap` 抓包与 `net_connection_debug.log`，检查 UDP 端口、数据包收发断档时间点、RTT 抖动或中继转发异常。\n")
	b.WriteString("4. **输出格式**：请用清晰易懂的中文给出「问题发生模块与直接证据 -> 根本原因 -> 无损修复方案（保留 Mod 原有玩法功能）」。\n")

	if len(a.logEntries) > 0 {
		start := 0
		if len(a.logEntries) > 15 {
			start = len(a.logEntries) - 15
		}
		b.WriteString("\n## 四、客户端最近运行日志摘录\n```text\n")
		for _, entry := range a.logEntries[start:] {
			fmt.Fprintf(&b, "%s [%s] %s\n", entry.Time, entry.Level, strings.ReplaceAll(entry.Message, "\r\n", "\n"))
		}
		b.WriteString("```\n")
	}

	return b.String()
}

// latestEntryPath returns the most recently modified file or subdirectory in dir.
func latestEntryPath(dir, ext string, wantDir bool) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var bestPath string
	var bestMod int64
	for _, entry := range entries {
		if entry.IsDir() != wantDir {
			continue
		}
		if ext != "" && !strings.HasSuffix(strings.ToLower(entry.Name()), strings.ToLower(ext)) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		mt := info.ModTime().UnixNano()
		if bestPath == "" || mt > bestMod {
			bestMod = mt
			bestPath = filepath.Join(dir, entry.Name())
		}
	}
	return bestPath
}
