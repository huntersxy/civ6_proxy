//go:build windows

package main

import (
	"fmt"
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
					if err := openFolder(app.diag.reportPath); err != nil {
						app.toast(ctx.Window, gui.ToastError, "打开报告失败", err.Error())
					}
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
