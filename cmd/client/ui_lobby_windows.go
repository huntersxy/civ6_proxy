//go:build windows

package main

import (
	"fmt"
	"strings"
	"time"

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
		gui.Text(gui.TextCfg{
			Text:      detail,
			TextStyle: t.TextStyleSecondary,
			Mode:      gui.TextModeWrap,
		}),
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
			Placeholder: "例如 203.0.113.10:42056",
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
						Text:      "历史中转地址",
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
				Width:      220,
				Sizing:     gui.FixedFit,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingPx(4),
				Content: []gui.View{
					gui.Text(gui.TextCfg{Text: "昵称", TextStyle: t.TextStyleLabel}),
					gui.Input(gui.InputCfg{
						ID:          "name-input",
						Text:        a.nameText,
						Placeholder: "游戏内显示昵称",
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
				Placeholder: "输入 10 位房间码",
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
	status := "未开启（可提前开启，建房后对支持的中转自动生效）"
	statusStyle := t.TextStyleSecondary
	canToggle := !a.closing && !a.diag.busy && snap.State != stateConnecting && snap.State != stateLeaving
	if joined {
		p2pSelected = snap.P2P.Enabled
		canToggle = snap.Host && snap.P2P.Supported && !a.closing
		switch {
		case snap.P2P.Checking:
			status = "正在检测中转支持…"
		case !snap.P2P.Supported:
			status = "当前中转不支持直连"
		case snap.P2P.Pending:
			status = "等待中转确认…"
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
			status = "已开启，等待其他玩家"
			if total != 0 {
				if direct == 0 && punching == 0 {
					status = fmt.Sprintf("直连未建立，可能受 NAT/运营商 UDP 限制（%d 人）", total)
					statusStyle = t.TextStyleWarning
				} else {
					status = fmt.Sprintf("%d / %d 位玩家已直连", direct, total)
					statusStyle = t.TextStyleSuccess
				}
				if punching > 0 {
					status += fmt.Sprintf("，%d 位正在打洞", punching)
				} else if direct < total && direct != 0 {
					status += "，其余走中转"
				}
			}
			if snap.P2PFirewallError != "" {
				status += " · 规则未同步，仍会尝试直连"
				statusStyle = t.TextStyleWarning
			}
		case snap.P2PFirewallError != "":
			status = "防火墙规则未同步；直连失败时自动走中转"
			statusStyle = t.TextStyleWarning
		case !snap.Host:
			status = "房主未开启"
		default:
			status = "已关闭"
		}
	} else if a.hostP2P {
		status = "已开启：建房后若中转支持，将自动启用 P2P 直连"
		statusStyle = t.TextStyleSuccess
	}

	canFirewall := joined && snap.P2P.Enabled && len(snap.P2P.Endpoints) > 0 && !a.closing

	hostPCAPSelected := a.hostPCAP
	canTogglePCAP := !a.closing && !a.diag.busy && snap.State != stateConnecting && snap.State != stateLeaving
	pcapStatus := "默认关闭；仅需排查联机丢包或异常时再由房主开启"
	pcapStatusStyle := t.TextStyleSecondary
	if joined {
		switch {
		case !snap.Host:
			canTogglePCAP = false
			pcapStatus = "仅房主端支持自动抓包（当前为成员）"
		case !snap.Capture:
			canTogglePCAP = false
			pcapStatus = "当前为隧道测试模式，游戏流量未接入"
		case snap.PCAPError != "":
			pcapStatus = "本地 PCAP 记录异常，请查看运行日志"
			pcapStatusStyle = t.TextStyleError
		case hostPCAPSelected && snap.PCAPActive:
			pcapStatus = fmt.Sprintf("正在记录游戏 UDP（已写入 %d 包），自动保留 72 小时", snap.PCAPPackets)
			pcapStatusStyle = t.TextStyleSuccess
		case hostPCAPSelected:
			pcapStatus = "已开启，正在准备本地 PCAP 记录…"
		case snap.PCAPPath != "":
			pcapStatus = fmt.Sprintf("已关闭（本次已停止，共记录 %d 包）", snap.PCAPPackets)
		default:
			pcapStatus = "已关闭（开启后将记录原始游戏 UDP 到本地目录）"
		}
	} else if a.s != nil && !snap.Capture {
		canTogglePCAP = false
		pcapStatus = "当前为隧道测试模式，游戏流量未接入"
	} else if hostPCAPSelected {
		pcapStatus = "已开启：建房后仅房主自动记录原始游戏 UDP（保留 72 小时）"
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
				Label:    "启用 P2P 直连",
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
				Label:    "配置 P2P 防火墙",
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
					} else {
						app.toast(ctx.Window, gui.ToastInfo, "防火墙规则同步中", "正在为本机 UDP 端口和已验证对端同步规则。")
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
				Label:    "房主自动抓包",
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
		rosterLabel = fmt.Sprintf("%d / %d 在线", online, len(members))
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
			Spacing:    gui.SpacingSmall,
			Content: []gui.View{
				gui.Text(gui.TextCfg{
					Text:      "当前尚未加入房间",
					TextStyle: t.TextStyleBodyLarge.Bold(),
				}),
				gui.Text(gui.TextCfg{
					Text:      "填写中转地址与昵称后，点击「创建房间」或输入朋友的 10 位房间码加入。",
					TextStyle: t.TextStyleSecondary,
					Mode:      gui.TextModeWrap,
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
