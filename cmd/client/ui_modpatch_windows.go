//go:build windows

package main

import (
	"fmt"

	"github.com/go-gui-org/go-gui/gui"
)

func modPatchNavBadge(statuses []modPatchStatus) (string, gui.BadgeVariant) {
	var readyCount, appliedCount, mismatchCount int
	for _, st := range statuses {
		switch st.State {
		case modPatchMismatch:
			mismatchCount++
		case modPatchReady:
			readyCount++
		case modPatchApplied:
			appliedCount++
		}
	}
	switch {
	case mismatchCount > 0:
		return fmt.Sprintf("%d 差异", mismatchCount), gui.BadgeWarning
	case readyCount > 0:
		return fmt.Sprintf("%d 待打", readyCount), gui.BadgeInfo
	case appliedCount > 0:
		return fmt.Sprintf("%d 已打", appliedCount), gui.BadgeSuccess
	default:
		return "", gui.BadgeDefault
	}
}

func modPatchStateMeta(state modPatchStateKind) (string, gui.BadgeVariant) {
	switch state {
	case modPatchApplied:
		return "已打补丁", gui.BadgeSuccess
	case modPatchReady:
		return "可打补丁", gui.BadgeInfo
	case modPatchMismatch:
		return "版本差异", gui.BadgeWarning
	default:
		return "未检测到", gui.BadgeDefault
	}
}

func modPatchSummaryLine(st modPatchStatus) string {
	id := st.Spec.WorkshopID
	switch st.State {
	case modPatchApplied:
		if st.HasBackup {
			return "已应用联机修复补丁（保留 Mod 原有玩法）· 已保留原版备份"
		}
		return "已应用联机修复补丁（保留 Mod 原有玩法）"
	case modPatchReady:
		return fmt.Sprintf("Workshop %s · 原版校验一致，可安全打补丁", id)
	case modPatchMismatch:
		return fmt.Sprintf("Workshop %s · 本地文件与预期原版不一致，如需覆盖可使用“强制打补丁”", id)
	default:
		return fmt.Sprintf("Workshop %s · 未在本地创意工坊检测到", id)
	}
}

func modPatchFileBadge(f modPatchFileState) (string, gui.BadgeVariant) {
	switch {
	case !f.Exists:
		return "文件缺失", gui.BadgeDefault
	case f.MatchesPatched:
		return "已应用补丁", gui.BadgeSuccess
	case f.MatchesOriginal:
		return "原版 MD5 一致", gui.BadgeInfo
	default:
		return "MD5 不一致", gui.BadgeWarning
	}
}

func (a *clientApp) modPatchView(w *gui.Window) gui.View {
	t := gui.CurrentTheme()

	var foundCount, readyCount, appliedCount int
	for _, st := range a.modPatches {
		if st.WorkshopFound {
			foundCount++
		}
		switch st.State {
		case modPatchReady:
			readyCount++
		case modPatchApplied:
			appliedCount++
		}
	}

	summaryLabel := fmt.Sprintf("已打 %d / 检测到 %d", appliedCount, foundCount)
	summaryVariant := gui.BadgeDefault
	if foundCount > 0 && appliedCount == foundCount {
		summaryVariant = gui.BadgeSuccess
	} else if readyCount > 0 {
		summaryVariant = gui.BadgeInfo
	}

	headerItems := []gui.View{
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingSmall,
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Text(gui.TextCfg{Text: "Mod 联机补丁", TextStyle: t.TextStyleTitleSmall}),
				gui.Badge(gui.BadgeCfg{Label: summaryLabel, Variant: summaryVariant}),
				gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder}),
				gui.Button(gui.ButtonCfg{
					ID:      "modpatch-refresh",
					Label:   "刷新",
					Variant: gui.ButtonSecondary,
					OnClick: func(ctx gui.EventCtx) {
						ctx.Consume()
						app := gui.State[clientApp](ctx.Window)
						app.modPatchNotice = ""
						app.refreshModPatches()
						ctx.Window.InvalidateLayout()
					},
				}),
				gui.Button(gui.ButtonCfg{
					ID:       "modpatch-apply-all",
					Label:    "一键安全打补丁",
					Variant:  gui.ButtonPrimary,
					Disabled: readyCount == 0,
					OnClick: func(ctx gui.EventCtx) {
						ctx.Consume()
						gui.State[clientApp](ctx.Window).applyAllReadyModPatchesFromUI(ctx.Window)
					},
				}),
			},
		}),
		gui.Text(gui.TextCfg{
			Text:      "开局前请退出文明 VI，确保房主与队友状态一致；写入前会自动备份原文件，可随时还原。",
			TextStyle: t.TextStyleCaption,
			Mode:      gui.TextModeWrap,
		}),
	}
	if a.modPatchNotice != "" {
		headerItems = append(headerItems, gui.Text(gui.TextCfg{
			ID:        "modpatch-notice",
			Text:      a.modPatchNotice,
			TextStyle: t.TextStyleSuccess,
			Mode:      gui.TextModeWrap,
		}))
	}
	headerCard := cardView("modpatch-header-card", gui.FillFit, headerItems...)

	cards := make([]gui.View, 0, len(a.modPatches))
	for _, st := range a.modPatches {
		cards = append(cards, a.modPatchCardView(w, st))
	}

	return gui.Column(gui.ContainerCfg{
		ID:         "modpatch-page",
		Sizing:     gui.FillFill,
		Scrollable: true,
		ScrollMode: gui.ScrollVerticalOnly,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		Content: []gui.View{
			headerCard,
			gui.Column(gui.ContainerCfg{
				ID:         "modpatch-list-scroll",
				Sizing:     gui.FillFit,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingMedium,
				Content:    cards,
			}),
		},
	})
}

func (a *clientApp) modPatchCardView(w *gui.Window, st modPatchStatus) gui.View {
	t := gui.CurrentTheme()
	id := st.Spec.WorkshopID
	stateLabel, stateVariant := modPatchStateMeta(st.State)
	expanded := a.modPatchExpanded != nil && a.modPatchExpanded[id]

	toggleLabel := "展开详情"
	if expanded {
		toggleLabel = "收起详情"
	}

	actionButtons := []gui.View{
		gui.Button(gui.ButtonCfg{
			ID:      "modpatch-toggle-" + id,
			Label:   toggleLabel,
			Variant: gui.ButtonGhost,
			OnClick: func(ctx gui.EventCtx) {
				ctx.Consume()
				app := gui.State[clientApp](ctx.Window)
				if app.modPatchExpanded == nil {
					app.modPatchExpanded = make(map[string]bool)
				}
				app.modPatchExpanded[id] = !app.modPatchExpanded[id]
				ctx.Window.InvalidateLayout()
			},
		}),
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
		}),
	}
	if st.HasBackup {
		actionButtons = append(actionButtons, gui.Button(gui.ButtonCfg{
			ID:      "modpatch-restore-" + id,
			Label:   "还原原版",
			Variant: gui.ButtonSecondary,
			OnClick: func(ctx gui.EventCtx) {
				ctx.Consume()
				gui.State[clientApp](ctx.Window).restoreModPatchFromUI(ctx.Window, id)
			},
		}))
	}

	switch st.State {
	case modPatchReady:
		actionButtons = append(actionButtons, gui.Button(gui.ButtonCfg{
			ID:      "modpatch-apply-" + id,
			Label:   "打补丁",
			Variant: gui.ButtonPrimary,
			OnClick: func(ctx gui.EventCtx) {
				ctx.Consume()
				gui.State[clientApp](ctx.Window).applyModPatchFromUI(ctx.Window, id, false)
			},
		}))
	case modPatchMismatch:
		actionButtons = append(actionButtons, gui.Button(gui.ButtonCfg{
			ID:      "modpatch-force-" + id,
			Label:   "强制打补丁",
			Variant: gui.ButtonDanger,
			OnClick: func(ctx gui.EventCtx) {
				ctx.Consume()
				gui.State[clientApp](ctx.Window).openForceModPatchDialog(ctx.Window, st)
			},
		}))
	}

	items := []gui.View{
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingSmall,
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Text(gui.TextCfg{
					Text:      st.Spec.Name,
					TextStyle: t.TextStyleTitleSmall,
					Sizing:    gui.FillFit,
				}),
				gui.Badge(gui.BadgeCfg{Label: stateLabel, Variant: stateVariant}),
			},
		}),
		gui.Text(gui.TextCfg{
			Text:      modPatchSummaryLine(st),
			TextStyle: t.TextStyleCaption,
			Mode:      gui.TextModeWrap,
		}),
	}
	if expanded {
		items = append(items, a.modPatchDetailsPanel(w, st))
	}
	items = append(items, gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingSmall,
		VAlign:     gui.VAlignMiddle,
		Content:    actionButtons,
	}))

	return cardView("modpatch-card-"+id, gui.FillFit, items...)
}

func (a *clientApp) modPatchDetailsPanel(w *gui.Window, st modPatchStatus) gui.View {
	t := gui.CurrentTheme()
	id := st.Spec.WorkshopID

	dirText := fmt.Sprintf("目录: 未在 Steam 创意工坊 (%s/%s) 中检测到完整文件", civ6SteamAppID, id)
	if st.WorkshopFound {
		dirText = "目录: " + st.WorkshopDir
	}

	detailRows := []gui.View{
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingSmall,
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Text(gui.TextCfg{
					Text:      "创意工坊 ID: " + id,
					TextStyle: t.TextStyleLabel,
					Sizing:    gui.FillFit,
				}),
				func() gui.View {
					if !st.WorkshopFound {
						return gui.Row(gui.ContainerCfg{Padding: gui.NoPadding, SizeBorder: gui.NoBorder})
					}
					return gui.Button(gui.ButtonCfg{
						ID:      "modpatch-open-" + id,
						Label:   "打开目录",
						Variant: gui.ButtonGhost,
						OnClick: func(ctx gui.EventCtx) {
							ctx.Consume()
							gui.State[clientApp](ctx.Window).openPath(ctx.Window, st.WorkshopDir)
						},
					})
				}(),
			},
		}),
		gui.Text(gui.TextCfg{
			Text:      "修复内容: " + st.Spec.Description,
			TextStyle: t.TextStyleCaption,
			Mode:      gui.TextModeWrap,
		}),
		gui.Text(gui.TextCfg{
			Text:      dirText,
			TextStyle: t.TextStyleCaption,
			Mode:      gui.TextModeWrap,
		}),
	}
	if st.HasBackup {
		backupMsg := "备份状态: 已备份原文件，可随时还原"
		if st.BackupTime != "" {
			backupMsg = "备份时间: " + st.BackupTime
		}
		detailRows = append(detailRows, gui.Text(gui.TextCfg{
			Text:      backupMsg,
			TextStyle: t.TextStyleCaption,
			Mode:      gui.TextModeWrap,
		}))
	}

	for idx, f := range st.Files {
		badgeLabel, badgeVariant := modPatchFileBadge(f)
		md5Detail := fmt.Sprintf("预期原版 MD5: %s", f.ExpectedOrigMD5)
		if f.Exists {
			md5Detail = fmt.Sprintf("当前: %s\n预期: %s", f.ActualMD5, f.ExpectedOrigMD5)
		}
		detailRows = append(detailRows, gui.Column(gui.ContainerCfg{
			ID:         fmt.Sprintf("modpatch-file-%s-%d", id, idx),
			Sizing:     gui.FillFit,
			Padding:    gui.NewPadding(4, 0, 0, 0),
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingPx(2),
			Content: []gui.View{
				gui.Row(gui.ContainerCfg{
					Sizing:     gui.FillFit,
					Padding:    gui.NoPadding,
					SizeBorder: gui.NoBorder,
					Spacing:    gui.SpacingSmall,
					VAlign:     gui.VAlignMiddle,
					Content: []gui.View{
						gui.Text(gui.TextCfg{
							Text:      f.RelPath,
							TextStyle: t.TextStyleLabel,
							Sizing:    gui.FillFit,
						}),
						gui.Badge(gui.BadgeCfg{
							Label:   badgeLabel,
							Variant: badgeVariant,
						}),
					},
				}),
				gui.Text(gui.TextCfg{
					Text:      md5Detail,
					TextStyle: t.TextStyleCodeSmall,
					Mode:      gui.TextModeWrap,
				}),
			},
		}))
	}

	return subtlePanelView("modpatch-details-"+id, gui.FillFit, detailRows...)
}

func (a *clientApp) openForceModPatchDialog(w *gui.Window, st modPatchStatus) {
	if w == nil || a.closing {
		return
	}
	id := st.Spec.WorkshopID
	w.Dialog(gui.DialogCfg{
		ID:         "modpatch-force-dialog",
		DialogType: gui.DialogCustom,
		Title:      "确认强制打补丁",
		Width:      520,
		FocusID:    "modpatch-force-cancel",
		CustomView: func(vw *gui.Window) gui.View {
			t := gui.CurrentTheme()
			rows := []gui.View{
				gui.Text(gui.TextCfg{
					Text:      fmt.Sprintf("%s (ID %s) 的本地文件 MD5 与预期原版不完全一致，可能是工坊已更新或本地修改过。", st.Spec.Name, id),
					TextStyle: t.TextStyleBody,
					Mode:      gui.TextModeWrap,
				}),
				gui.Text(gui.TextCfg{
					Text:      "继续强制打补丁将备份当前文件并用修复版脚本覆盖对应 Lua 文件；若游戏版本差异过大，可通过“还原原版”恢复。",
					TextStyle: t.TextStyleCaption,
					Mode:      gui.TextModeWrap,
				}),
			}
			for _, f := range st.Files {
				rows = append(rows, gui.Text(gui.TextCfg{
					Text:      fmt.Sprintf("• %s\n  当前 MD5: %s\n  预期原版: %s", f.RelPath, f.ActualMD5, f.ExpectedOrigMD5),
					TextStyle: t.TextStyleCodeSmall,
					Mode:      gui.TextModeWrap,
				}))
			}
			rows = append(rows, gui.Row(gui.ContainerCfg{
				Sizing:     gui.FillFit,
				Padding:    gui.NewPadding(6, 0, 0, 0),
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingSmall,
				VAlign:     gui.VAlignMiddle,
				Content: []gui.View{
					gui.Row(gui.ContainerCfg{Sizing: gui.FillFit, Padding: gui.NoPadding, SizeBorder: gui.NoBorder}),
					gui.Button(gui.ButtonCfg{
						ID:      "modpatch-force-cancel",
						Label:   "取消",
						Variant: gui.ButtonSecondary,
						OnClick: func(ctx gui.EventCtx) {
							ctx.Consume()
							ctx.Window.DialogDismiss()
						},
					}),
					gui.Button(gui.ButtonCfg{
						ID:      "modpatch-force-confirm",
						Label:   "确认强制覆盖",
						Variant: gui.ButtonDanger,
						OnClick: func(ctx gui.EventCtx) {
							ctx.Consume()
							ctx.Window.DialogDismiss()
							gui.State[clientApp](ctx.Window).applyModPatchFromUI(ctx.Window, id, true)
						},
					}),
				},
			}))
			return gui.Column(gui.ContainerCfg{
				Sizing:     gui.FillFit,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingMedium,
				Content:    rows,
			})
		},
	})
}
