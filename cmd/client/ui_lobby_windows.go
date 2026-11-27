//go:build windows

package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"civ6relay/internal/logsync"
	"civ6relay/internal/tunnel"

	"github.com/go-gui-org/go-gui/gui"
)

func (a *clientApp) lobbyView(w *gui.Window) gui.View {
	return gui.Column(gui.ContainerCfg{
		ID:         "lobby-page",
		Sizing:     gui.FillFill,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		Content: []gui.View{
			a.lobbyStatusBanner(w),
			a.lobbyConnectionCard(w),
			a.lobbyP2PCard(w),
			a.lobbyMembersCard(w),
		},
	})
}

func (a *clientApp) lobbyStatusBanner(w *gui.Window) gui.View {
	t := gui.CurrentTheme()
	title, detail, variant, accent := a.statusMeta()
	joined := a.snap.State == stateJoined && a.activeCode != ""

	headerLeft := []gui.View{
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingSmall,
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Badge(gui.BadgeCfg{Label: title, Variant: variant}),
			},
		}),
	}
	if detail != "" {
		headerLeft = append(headerLeft, gui.Text(gui.TextCfg{
			Text:      detail,
			TextStyle: t.TextStyleSecondary,
			Mode:      gui.TextModeWrap,
		}))
	}

	bannerContent := []gui.View{
		gui.Rectangle(gui.RectangleCfg{
			Width:  4,
			Sizing: gui.FixedFill,
			Radius: t.RadiusSmall,
			Color:  accent,
		}),
		gui.Column(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingPx(6),
			Content:    headerLeft,
		}),
	}

	if joined {
		bannerContent = append(bannerContent, gui.Row(gui.ContainerCfg{
			Sizing:      gui.FitFit,
			Color:       t.ColorBackground,
			ColorBorder: t.ColorBorder,
			SizeBorder:  gui.BorderThin,
			Radius:      gui.RadiusMedium,
			Padding:     gui.NewPadding(6, 10, 6, 12),
			Spacing:     gui.SpacingSmall,
			VAlign:      gui.VAlignMiddle,
			Content: []gui.View{
				gui.Column(gui.ContainerCfg{
					Sizing:     gui.FitFit,
					Padding:    gui.NoPadding,
					SizeBorder: gui.NoBorder,
					Spacing:    gui.SpacingPx(2),
					Content: []gui.View{
						gui.Text(gui.TextCfg{Text: "当前房间码", TextStyle: t.TextStyleCaption}),
						gui.Text(gui.TextCfg{Text: a.activeCode, TextStyle: t.TextStyleCode.Bold()}),
					},
				}),
				gui.Button(gui.ButtonCfg{
					ID:      "banner-copy-code",
					Label:   "复制",
					Variant: gui.ButtonPrimary,
					OnClick: func(ctx gui.EventCtx) {
						ctx.Consume()
						gui.State[clientApp](ctx.Window).copyRoomCode(ctx.Window)
					},
				}),
			},
		}))
	}

	return gui.Row(gui.ContainerCfg{
		ID:          "status-banner",
		Sizing:      gui.FillFit,
		Color:       t.ColorPanel,
		ColorBorder: t.ColorBorder,
		SizeBorder:  gui.BorderThin,
		Radius:      gui.RadiusLarge,
		Padding:     gui.NewPadding(12, 16, 12, 14),
		Spacing:     gui.SpacingMedium,
		VAlign:      gui.VAlignMiddle,
		Content:     bannerContent,
	})
}

func (a *clientApp) lobbyConnectionCard(w *gui.Window) gui.View {
	t := gui.CurrentTheme()
	canConnect := a.canConnect()
	joined := a.snap.State == stateJoined
	connecting := a.snap.State == stateConnecting

	relayFieldItems := []gui.View{
		gui.Input(gui.InputCfg{
			ID:          "relay-input",
			Text:        a.relayText,
			Placeholder: "IP:端口",
			MaxLength:   255,
			Disabled:    !canConnect,
			Sizing:      gui.FillFit,
			OnTextChanged: func(val string, ctx gui.EventCtx) {
				ctx.Consume()
				app := gui.State[clientApp](ctx.Window)
				app.relayText = val
				ctx.Window.InvalidateLayout()
			},
			OnEnter: func(ctx gui.EventCtx) {
				ctx.Consume()
				if ids := ctx.Window.ResolveID("create-room"); len(ids) > 0 {
					ctx.Window.SetFocus(ids[0])
				}
			},
		}),
	}

	if len(a.relayHistory) > 0 {
		btnLabel := "历史 ▾"
		if a.historyOpen && canConnect {
			btnLabel = "历史 ▴"
		}
		anchorChildren := []gui.View{
			gui.Button(gui.ButtonCfg{
				ID:       "relay-history-btn",
				Label:    btnLabel,
				Variant:  gui.ButtonSecondary,
				Disabled: !canConnect,
				MinWidth: 74,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					app.historyOpen = !app.historyOpen
					ctx.Window.InvalidateLayout()
				},
			}),
		}

		if a.historyOpen && canConnect {
			current := strings.TrimSpace(a.relayText)
			menuRows := make([]gui.View, 0, len(a.relayHistory)+1)
			menuRows = append(menuRows, gui.Row(gui.ContainerCfg{
				Sizing:     gui.FillFit,
				Padding:    gui.NewPadding(4, 8, 4, 8),
				SizeBorder: gui.NoBorder,
				Content: []gui.View{
					gui.Text(gui.TextCfg{
						Text:      "历史记录",
						TextStyle: t.TextStyleCaption,
					}),
				},
			}))
			for idx, addr := range a.relayHistory {
				selected := addr == current
				rowBg := gui.ColorTransparent
				if selected {
					rowBg = t.ColorAccentSubtle
				}
				addrCopy := addr
				rowContent := []gui.View{
					gui.Text(gui.TextCfg{
						Text:      addr,
						TextStyle: t.TextStyleCodeSmall,
						Sizing:    gui.FillFit,
					}),
				}
				if selected {
					rowContent = append(rowContent, gui.Badge(gui.BadgeCfg{
						Dot:   true,
						Color: t.ColorAccent,
					}))
				}
				menuRows = append(menuRows, gui.Row(gui.ContainerCfg{
					ID:           fmt.Sprintf("relay-history-item-%d", idx),
					Focusable:    true,
					ClickOnEnter: true,
					ClickOnSpace: true,
					Sizing:       gui.FillFit,
					Color:        rowBg,
					Hover: gui.HoverStyle{
						Color:  t.ColorHover,
						Cursor: gui.CursorPointingHand,
					},
					Radius:     gui.RadiusSmall,
					SizeBorder: gui.NoBorder,
					Padding:    gui.NewPadding(7, 10, 7, 10),
					Spacing:    gui.SpacingSmall,
					VAlign:     gui.VAlignMiddle,
					A11YRole:   gui.AccessRoleMenuItem,
					A11YCfg:    gui.A11YCfg{A11YLabel: addr},
					OnClick: func(ctx gui.EventCtx) {
						ctx.Consume()
						app := gui.State[clientApp](ctx.Window)
						app.relayText = addrCopy
						app.historyOpen = false
						ctx.Window.InvalidateLayout()
					},
					Content: rowContent,
				}))
			}

			anchorChildren = append(anchorChildren, gui.Column(gui.ContainerCfg{
				ID:            "relay-history-popover",
				Float:         true,
				FloatAnchor:   gui.FloatBottomRight,
				FloatTieOff:   gui.FloatTopRight,
				FloatAutoFlip: true,
				FloatOffsetY:  4,
				FloatZIndex:   100,
				MinWidth:      260,
				MaxWidth:      380,
				MaxHeight:     220,
				Sizing:        gui.FitFit,
				Scrollable:    true,
				ScrollMode:    gui.ScrollVerticalOnly,
				Color:         t.ColorPanel,
				ColorBorder:   t.ColorBorder,
				SizeBorder:    gui.BorderThin,
				Radius:        gui.RadiusMedium,
				Shadow:        t.Cfg.ShadowPopover,
				Padding:       gui.NewPadding(6, 6, 6, 6),
				Spacing:       gui.SpacingPx(2),
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
				},
				Content: menuRows,
			}))
		}

		relayFieldItems = append(relayFieldItems, gui.Row(gui.ContainerCfg{
			ID:         "relay-history-anchor",
			Sizing:     gui.FitFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			Content:    anchorChildren,
		}))
	}

	maxMembersDisplay := a.maxMembersText
	if joined {
		if a.snap.MaxMembers > 0 {
			maxMembersDisplay = strconv.Itoa(a.snap.MaxMembers)
		} else {
			maxMembersDisplay = "无限"
		}
	}

	row1 := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		VAlign:     gui.VAlignBottom,
		Content: []gui.View{
			gui.Column(gui.ContainerCfg{
				Sizing:     gui.FillFit,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingPx(4),
				Content: []gui.View{
					gui.Text(gui.TextCfg{Text: "中转地址", TextStyle: t.TextStyleLabel}),
					gui.Row(gui.ContainerCfg{
						Sizing:     gui.FillFit,
						Padding:    gui.NoPadding,
						SizeBorder: gui.NoBorder,
						Spacing:    gui.SpacingSmall,
						VAlign:     gui.VAlignMiddle,
						Content:    relayFieldItems,
					}),
				},
			}),
			gui.Column(gui.ContainerCfg{
				Width:      180,
				Sizing:     gui.FixedFit,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingPx(4),
				Content: []gui.View{
					gui.Text(gui.TextCfg{Text: "昵称", TextStyle: t.TextStyleLabel}),
					gui.Input(gui.InputCfg{
						ID:          "name-input",
						Text:        a.nameText,
						Placeholder: "昵称",
						MaxLength:   48,
						Disabled:    !canConnect,
						Sizing:      gui.FillFit,
						OnTextChanged: func(val string, ctx gui.EventCtx) {
							ctx.Consume()
							gui.State[clientApp](ctx.Window).nameText = val
							ctx.Window.InvalidateLayout()
						},
						OnEnter: func(ctx gui.EventCtx) {
							ctx.Consume()
							if ids := ctx.Window.ResolveID("create-room"); len(ids) > 0 {
								ctx.Window.SetFocus(ids[0])
							}
						},
					}),
				},
			}),
			gui.Column(gui.ContainerCfg{
				Width:      104,
				Sizing:     gui.FixedFit,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingPx(4),
				Content: []gui.View{
					gui.Text(gui.TextCfg{Text: "人数上限", TextStyle: t.TextStyleLabel}),
					gui.Input(gui.InputCfg{
						ID:          "max-members-input",
						Text:        maxMembersDisplay,
						Placeholder: "无限",
						MaxLength:   8,
						Disabled:    !canConnect,
						Sizing:      gui.FillFit,
						OnTextChanged: func(val string, ctx gui.EventCtx) {
							ctx.Consume()
							gui.State[clientApp](ctx.Window).maxMembersText = val
							ctx.Window.InvalidateLayout()
						},
						OnEnter: func(ctx gui.EventCtx) {
							ctx.Consume()
							if ids := ctx.Window.ResolveID("create-room"); len(ids) > 0 {
								ctx.Window.SetFocus(ids[0])
							}
						},
					}),
				},
			}),
		},
	})

	roomCodeValue := a.joinDraft
	if joined && a.activeCode != "" {
		roomCodeValue = a.activeCode
	}

	roomActionLabel := "加入房间"
	roomActionEnabled := canJoinWithDraft(canConnect, a.joinDraft)
	roomActionVariant := gui.ButtonSecondary
	if joined {
		roomActionLabel = "复制房间码"
		roomActionEnabled = a.activeCode != ""
		roomActionVariant = gui.ButtonPrimary
	}

	leaveLabel := "离开房间"
	if connecting {
		leaveLabel = "取消连接"
	}
	leaveEnabled := !a.closing && (joined || connecting)

	createVariant := gui.ButtonPrimary
	if joined {
		createVariant = gui.ButtonSecondary
	}

	row2 := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingSmall,
		VAlign:     gui.VAlignMiddle,
		Content: []gui.View{
			gui.Button(gui.ButtonCfg{
				ID:       "create-room",
				Label:    "创建房间",
				Variant:  createVariant,
				Disabled: !canConnect,
				Loading:  connecting && a.joinDraft == "",
				MinWidth: 96,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					gui.State[clientApp](ctx.Window).connect(ctx.Window, false)
				},
			}),
			gui.Separator(gui.SeparatorCfg{Vertical: true, Length: 22}),
			gui.Text(gui.TextCfg{Text: "房间码", TextStyle: t.TextStyleLabel}),
			gui.Input(gui.InputCfg{
				ID:          "room-code",
				Text:        roomCodeValue,
				Placeholder: "10 位房间码",
				MaxLength:   64,
				ReadOnly:    !canConnect,
				TextStyle:   t.TextStyleCode,
				Sizing:      gui.FillFit,
				OnTextChanged: func(val string, ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					if app.canConnect() {
						app.joinDraft = val
						ctx.Window.InvalidateLayout()
					}
				},
				OnEnter: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					if app.canConnect() {
						app.connect(ctx.Window, true)
					}
				},
			}),
			gui.Button(gui.ButtonCfg{
				ID:       "room-action",
				Label:    roomActionLabel,
				Variant:  roomActionVariant,
				Disabled: !roomActionEnabled,
				MinWidth: 96,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					if app.snap.State == stateJoined && app.activeCode != "" {
						app.copyRoomCode(ctx.Window)
					} else {
						app.connect(ctx.Window, true)
					}
				},
			}),
			gui.Button(gui.ButtonCfg{
				ID:       "leave-room",
				Label:    leaveLabel,
				Variant:  gui.ButtonDanger,
				Disabled: !leaveEnabled,
				MinWidth: 88,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					if app.s != nil {
						app.s.Disconnect()
						app.syncSnapshot(ctx.Window)
					}
				},
			}),
		},
	})

	return cardView("connection-card", gui.FillFit, row1, row2)
}

func (a *clientApp) lobbyP2PCard(w *gui.Window) gui.View {
	t := gui.CurrentTheme()
	snap := a.snap
	joined := snap.State == stateJoined

	p2pSelected := a.hostP2P
	status := "关闭"
	statusStyle := t.TextStyleSecondary
	canToggle := !a.closing && !a.diag.busy && snap.State != stateConnecting && snap.State != stateLeaving
	if joined {
		p2pSelected = snap.P2P.Enabled
		canToggle = snap.Host && snap.P2P.Supported && !a.closing
		switch {
		case snap.P2P.Checking:
			status = "检测中…"
		case !snap.P2P.Supported:
			status = "中转不支持"
		case snap.P2P.Pending:
			status = "等待确认…"
		case snap.P2P.Enabled:
			direct, punching, total := 0, 0, 0
			for _, peer := range snap.Members {
				if peer.ID != snap.MemberID && peer.Online {
					total++
					if peer.Path == tunnel.Direct {
						direct++
					} else if peer.Path == tunnel.Punching {
						punching++
					}
				}
			}
			status = "已开启"
			if total != 0 {
				if direct == 0 && punching == 0 {
					status = fmt.Sprintf("直连失败 · %d 人", total)
					statusStyle = t.TextStyleWarning
				} else {
					status = fmt.Sprintf("直连 %d/%d", direct, total)
					statusStyle = t.TextStyleSuccess
				}
				if punching > 0 {
					status += fmt.Sprintf(" · 打洞 %d", punching)
				} else if direct < total && direct != 0 {
					status += " · 其余中转"
				}
			}
			if snap.P2PFirewallError != "" {
				status += " · 防火墙未同步"
				statusStyle = t.TextStyleWarning
			}
		case snap.P2PFirewallError != "":
			status = "防火墙未同步"
			statusStyle = t.TextStyleWarning
		case !snap.Host:
			status = "房主未开"
		default:
			status = "关闭"
		}
	} else if a.hostP2P {
		status = "开启（建房后自动启用）"
		statusStyle = t.TextStyleSuccess
	}

	canFirewall := joined && snap.P2P.Enabled && len(snap.P2P.Endpoints) > 0 && !a.closing

	hostPCAPSelected := a.hostPCAP
	canTogglePCAP := !a.closing && !a.diag.busy && snap.State != stateConnecting && snap.State != stateLeaving
	pcapStatus := "关闭"
	pcapStatusStyle := t.TextStyleSecondary
	if joined {
		switch {
		case !snap.Host:
			canTogglePCAP = false
			pcapStatus = "仅房主可用"
		case !snap.Capture:
			canTogglePCAP = false
			pcapStatus = "测试模式不可用"
		case snap.PCAPError != "":
			pcapStatus = "PCAP 异常"
			pcapStatusStyle = t.TextStyleError
		case hostPCAPSelected && snap.PCAPActive:
			pcapStatus = fmt.Sprintf("记录中 · %d 包", snap.PCAPPackets)
			pcapStatusStyle = t.TextStyleSuccess
		case hostPCAPSelected:
			pcapStatus = "准备中…"
		case snap.PCAPPath != "":
			pcapStatus = fmt.Sprintf("已停止 · %d 包", snap.PCAPPackets)
		default:
			pcapStatus = "关闭"
		}
	} else if a.s != nil && !snap.Capture {
		canTogglePCAP = false
		pcapStatus = "测试模式不可用"
	} else if hostPCAPSelected {
		pcapStatus = "开启（建房自动记录）"
		pcapStatusStyle = t.TextStyleSuccess
	}

	p2pRow := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		VAlign:     gui.VAlignMiddle,
		Content: []gui.View{
			gui.Switch(gui.SwitchCfg{
				ID:       "p2p-switch",
				Label:    "P2P 直连",
				Selected: p2pSelected,
				Disabled: !canToggle,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					if app.snap.State == stateJoined {
						if app.s == nil {
							return
						}
						next := !app.snap.P2P.Enabled
						if err := app.s.SetP2PEnabled(next); err != nil {
							app.toast(ctx.Window, gui.ToastWarning, "P2P 设置失败", err.Error())
						}
						app.syncSnapshot(ctx.Window)
						return
					}
					next := !app.hostP2P
					if app.s == nil {
						app.hostP2P = next
						app.snap.HostP2P = next
						ctx.Window.InvalidateLayout()
						return
					}
					if err := app.s.SetP2PEnabled(next); err != nil {
						app.toast(ctx.Window, gui.ToastWarning, "P2P 设置失败", err.Error())
					}
					app.syncSnapshot(ctx.Window)
				},
			}),
			gui.Text(gui.TextCfg{
				Text:      status,
				TextStyle: statusStyle,
				Mode:      gui.TextModeWrap,
				Sizing:    gui.FillFit,
			}),
			gui.Button(gui.ButtonCfg{
				ID:       "p2p-firewall",
				Label:    "P2P 防火墙",
				Variant:  gui.ButtonSecondary,
				Disabled: !canFirewall,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					if app.s == nil {
						return
					}
					if err := app.s.RetryP2PFirewall(app.snap.P2P); err != nil {
						app.toast(ctx.Window, gui.ToastWarning, "同步失败", err.Error())
					}
					app.syncSnapshot(ctx.Window)
				},
			}),
		},
	})

	pcapRow := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		VAlign:     gui.VAlignMiddle,
		Content: []gui.View{
			gui.Switch(gui.SwitchCfg{
				ID:       "host-pcap-switch",
				Label:    "房主抓包",
				Selected: hostPCAPSelected,
				Disabled: !canTogglePCAP,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					next := !app.hostPCAP
					if app.s == nil {
						app.hostPCAP = next
						app.snap.HostPCAP = next
						ctx.Window.InvalidateLayout()
						return
					}
					if err := app.s.SetHostPCAP(next); err != nil {
						app.toast(ctx.Window, gui.ToastWarning, "抓包设置失败", err.Error())
					}
					app.syncSnapshot(ctx.Window)
				},
			}),
			gui.Text(gui.TextCfg{
				Text:      pcapStatus,
				TextStyle: pcapStatusStyle,
				Mode:      gui.TextModeWrap,
				Sizing:    gui.FillFit,
			}),
		},
	})

	hostLogSyncSelected := a.hostLogSync
	canToggleLogSync := !a.closing && !a.diag.busy && snap.State != stateConnecting && snap.State != stateLeaving
	logSyncStatus := "关闭"
	logSyncStyle := t.TextStyleSecondary
	if joined {
		if !snap.Host {
			canToggleLogSync = false
			hostLogSyncSelected = snap.LogGuest.Active
			if snap.LogGuest.Active {
				logSyncStatus = fmt.Sprintf("向房主实时上传 · %d 文件 · %s",
					snap.LogGuest.FilesSynced, logsync.FormatByteSize(snap.LogGuest.BytesSent))
				logSyncStyle = t.TextStyleSuccess
			} else {
				logSyncStatus = "房主未开"
			}
		} else {
			switch {
			case snap.LogSyncError != "":
				logSyncStatus = "收集异常"
				logSyncStyle = t.TextStyleError
			case hostLogSyncSelected && snap.LogHost.Enabled:
				if snap.LogHost.OnlineGuests > 0 {
					logSyncStatus = fmt.Sprintf("收集中 · 房主 + %d/%d 队友 · %s",
						snap.LogHost.ActivePeers, snap.LogHost.OnlineGuests, logsync.FormatByteSize(snap.LogHost.TotalBytes))
				} else {
					logSyncStatus = fmt.Sprintf("收集中 · 房主本机 · %d 文件 · %s",
						snap.LogHost.HostFiles, logsync.FormatByteSize(snap.LogHost.HostBytes))
				}
				logSyncStyle = t.TextStyleSuccess
			case hostLogSyncSelected:
				logSyncStatus = "准备中…"
			case snap.LogSyncDir != "":
				logSyncStatus = fmt.Sprintf("已停止 · %d 文件 · %s",
					snap.LogHost.TotalFiles, logsync.FormatByteSize(snap.LogHost.TotalBytes))
			default:
				logSyncStatus = "关闭"
			}
		}
	} else if hostLogSyncSelected {
		logSyncStatus = "开启（建房自动收集）"
		logSyncStyle = t.TextStyleSuccess
	}

	logSyncRow := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		VAlign:     gui.VAlignMiddle,
		Content: []gui.View{
			gui.Switch(gui.SwitchCfg{
				ID:       "host-logsync-switch",
				Label:    "日志收集",
				Selected: hostLogSyncSelected,
				Disabled: !canToggleLogSync,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					next := !app.hostLogSync
					if app.s == nil {
						app.hostLogSync = next
						app.snap.HostLogSync = next
						ctx.Window.InvalidateLayout()
						return
					}
					if err := app.s.SetHostLogSync(next); err != nil {
						app.toast(ctx.Window, gui.ToastWarning, "日志收集设置失败", err.Error())
					}
					app.syncSnapshot(ctx.Window)
				},
			}),
			gui.Text(gui.TextCfg{
				Text:      logSyncStatus,
				TextStyle: logSyncStyle,
				Mode:      gui.TextModeWrap,
				Sizing:    gui.FillFit,
			}),
			gui.Button(gui.ButtonCfg{
				ID:       "host-logsync-dir",
				Label:    "收集目录",
				Variant:  gui.ButtonSecondary,
				Disabled: a.closing,
				OnClick: func(ctx gui.EventCtx) {
					ctx.Consume()
					gui.State[clientApp](ctx.Window).openCollectedLogsDir(ctx.Window, true)
				},
			}),
		},
	})

	return gui.Column(gui.ContainerCfg{
		ID:          "p2p-card",
		Sizing:      gui.FillFit,
		Color:       t.ColorPanel,
		ColorBorder: t.ColorBorder,
		SizeBorder:  gui.BorderThin,
		Radius:      gui.RadiusLarge,
		Padding:     gui.NewPadding(10, 16, 10, 16),
		Spacing:     gui.SpacingSmall,
		Content: []gui.View{
			p2pRow,
			gui.Separator(gui.SeparatorCfg{}),
			pcapRow,
			gui.Separator(gui.SeparatorCfg{}),
			logSyncRow,
		},
	})
}

func (a *clientApp) lobbyMembersCard(w *gui.Window) gui.View {
	t := gui.CurrentTheme()
	members := a.snap.Members
	self := a.snap.MemberID

	online := 0
	for _, m := range members {
		if m.Online {
			online++
		}
	}
	rosterLabel := "尚无成员"
	rosterVariant := gui.BadgeDefault
	if len(members) > 0 {
		if a.snap.MaxMembers > 0 {
			rosterLabel = fmt.Sprintf("%d / %d 在线 · 上限 %d", online, len(members), a.snap.MaxMembers)
		} else {
			rosterLabel = fmt.Sprintf("%d / %d 在线 · 无限", online, len(members))
		}
		rosterVariant = gui.BadgeSuccess
	}

	header := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		VAlign:     gui.VAlignMiddle,
		Content: []gui.View{
			gui.Text(gui.TextCfg{
				Text:      "房间成员",
				TextStyle: t.TextStyleTitleSmall,
				Sizing:    gui.FillFit,
			}),
			gui.Badge(gui.BadgeCfg{
				Label:   rosterLabel,
				Variant: rosterVariant,
			}),
		},
	})

	var body gui.View
	if len(members) == 0 {
		body = gui.Column(gui.ContainerCfg{
			Sizing:     gui.FillFill,
			HAlign:     gui.HAlignCenter,
			VAlign:     gui.VAlignMiddle,
			Padding:    gui.NewPadding(20, 16, 20, 16),
			SizeBorder: gui.NoBorder,
			Content: []gui.View{
				gui.Text(gui.TextCfg{
					Text:      "未加入房间",
					TextStyle: t.TextStyleSecondary,
				}),
			},
		})
	} else {
		cols := []tableColSpec{
			{title: "昵称", width: 0},
			{title: "角色", width: 108},
			{title: "状态", width: 76},
			{title: "连接 / 延迟", width: 156},
			{title: "虚拟地址", width: 120},
		}
		rows := make([][]gui.View, 0, len(members))
		for _, m := range members {
			role := "成员"
			if m.Host {
				role = "房主"
			}
			if m.ID == self {
				role += "（你）"
			}

			stateBadge := gui.Badge(gui.BadgeCfg{Label: "离线", Variant: gui.BadgeDefault})
			pathView := gui.Badge(gui.BadgeCfg{Label: "离线", Variant: gui.BadgeDefault})

			if m.ID == self {
				stateBadge = gui.Badge(gui.BadgeCfg{Label: "在线", Variant: gui.BadgeSuccess})
				pathView = gui.Badge(gui.BadgeCfg{Label: "本机", Variant: gui.BadgeInfo})
			} else if m.Online {
				stateBadge = gui.Badge(gui.BadgeCfg{Label: "在线", Variant: gui.BadgeSuccess})
				pathLabel := "中转"
				pathVariant := gui.BadgeDefault
				switch m.Path {
				case tunnel.Direct:
					pathLabel = "直连"
					pathVariant = gui.BadgeSuccess
				case tunnel.Punching:
					pathLabel = "打洞中"
					pathVariant = gui.BadgeWarning
				}
				latText := "测量中"
				if m.Latency > 0 {
					if m.Latency < time.Millisecond {
						latText = "< 1 ms"
					} else {
						latText = fmt.Sprintf("%d ms", m.Latency.Milliseconds())
					}
				}
				pathView = gui.Row(gui.ContainerCfg{
					Sizing:     gui.FitFit,
					Padding:    gui.NoPadding,
					SizeBorder: gui.NoBorder,
					Spacing:    gui.SpacingSmall,
					VAlign:     gui.VAlignMiddle,
					Content: []gui.View{
						gui.Badge(gui.BadgeCfg{Label: pathLabel, Variant: pathVariant}),
						gui.Text(gui.TextCfg{Text: latText, TextStyle: t.TextStyleCodeSmall}),
					},
				})
			}

			rows = append(rows, []gui.View{
				gui.Text(gui.TextCfg{Text: m.Name, TextStyle: t.TextStyleBody.Bold()}),
				gui.Text(gui.TextCfg{Text: role, TextStyle: t.TextStyleBody}),
				stateBadge,
				pathView,
				gui.Text(gui.TextCfg{Text: m.Virtual, TextStyle: t.TextStyleCodeSmall}),
			})
		}

		body = flexTableView("members-table", gui.FillFill, true, cols, rows)
	}

	return cardView("members-card", gui.FillFill, header, body)
}
