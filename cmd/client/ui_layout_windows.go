//go:build windows

package main

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"civ6relay/internal/tunnel"
	"civ6relay/internal/win"
)

const (
	uiMinWidth  = 640
	uiMinHeight = 546
)

type uiPalette struct {
	background, text, muted, active, error uintptr
}

func systemPalette() uiPalette {
	contrast := highContrast{Size: uint32(unsafe.Sizeof(highContrast{}))}
	if ok, _, _ := procSystemParametersInfo.Call(spiGetHighContrast, uintptr(contrast.Size), uintptr(unsafe.Pointer(&contrast)), 0); ok != 0 &&
		contrast.Flags&hcfHighContrastOn != 0 {
		background, _, _ := procGetSysColor.Call(colorWindow)
		text, _, _ := procGetSysColor.Call(colorWindowText)
		return uiPalette{background: background, text: text, muted: text, active: text, error: text}
	}
	return uiPalette{
		background: 0xF8F5F2,
		text:       0x3C281B,
		muted:      0x63513C,
		active:     0x67550F,
		error:      0x272790,
	}
}

func (u *uiWindow) staticColor(dc uintptr, control syscall.Handle) uintptr {
	switch control {
	case u.hCode, u.hRelay, u.hName, u.hLog:
		return 0
	}
	color := u.palette.text
	switch control {
	case u.hStatus:
		color = u.statusColor
	case u.hDetail, u.hRoster, u.hSubnet, u.hP2PStatus:
		color = u.palette.muted
	}
	procSetTextColor.Call(dc, color)
	procSetBkMode.Call(dc, transparent)
	return uintptr(u.bgBrush)
}

func (u *uiWindow) createControls(cfg Config) {
	label := func(text string, style uint32, font syscall.Handle) syscall.Handle {
		handle := createChild(u.hwnd, 0, "STATIC", text, style|ssNoPrefix, 0)
		setFont(handle, font)
		return handle
	}
	edit := func(text string, style uint32, id uint32) syscall.Handle {
		handle := createChild(u.hwnd, wsExClientEdge, "EDIT", text, style|wsTabStop, id)
		setFont(handle, u.fontUI)
		return handle
	}
	button := func(text string, id uint32) syscall.Handle {
		handle := createChild(u.hwnd, 0, "BUTTON", text, wsTabStop, id)
		setFont(handle, u.fontUI)
		return handle
	}

	u.hTitle = label(uiTitle, 0, u.fontTitle)
	u.hStatus = label("", 0, u.fontStrong)
	u.hDetail = label("", 0, u.fontUI)
	u.hDivider = label("", ssEtchedHorz, u.fontUI)
	u.hRelayLabel = label("中转地址", 0, u.fontUI)
	u.hRelay = createChild(u.hwnd, wsExClientEdge, "COMBOBOX", win.FormatRelay(cfg.RelayHost, cfg.RelayPort),
		cbsDropdown|cbsAutoHScroll|wsVScroll|wsTabStop, idRelay)
	setFont(u.hRelay, u.fontUI)
	procSendMessageW.Call(uintptr(u.hRelay), cbSetDroppedHeight, uintptr(px(180)), 0)
	u.setRelayHistory(cfg.RelayHistory)
	u.hNameLabel = label("昵称", 0, u.fontUI)
	u.hName = edit(cfg.DisplayName, esAutoHScroll, idName)
	u.hCreate = button("创建房间", idCreate)
	u.hCodeLabel = label("房间码", 0, u.fontUI)
	u.hCode = edit("", esAutoHScroll, idRoomCode)
	u.hRoomAction = button("加入房间", idRoomAction)
	u.hLeave = button("离开房间", idLeave)
	u.hP2P = createChild(u.hwnd, 0, "BUTTON", "启用 P2P 直连", bsAutoCheck|wsTabStop, idP2PToggle)
	setFont(u.hP2P, u.fontUI)
	u.hP2PStatus = label("", ssEndEllipsis, u.fontUI)
	u.hP2PFirewall = button("配置 P2P 防火墙", idP2PFirewall)
	u.hDiag = button("诊断", idDiagnostics)
	u.hSpeed = button("网络测试", idSpeedTest)
	u.hMembers = label("房间成员", 0, u.fontStrong)
	u.hRoster = label("尚无成员", ssRight, u.fontUI)
	u.hList = createChild(u.hwnd, wsExClientEdge, "SysListView32", "",
		wsTabStop|lvsReport|lvsSingleSel|lvsShowSelAlways|lvsNoSortHeader, 0)
	setFont(u.hList, u.fontUI)
	procSendMessageW.Call(uintptr(u.hList), lvmSetExtendedListViewStyle, 0, lvsExFullRowSelect|lvsExDoubleBuffer|lvsExLabelTip)
	u.insertColumns()

	u.hLog = edit("", wsVScroll|esMultiline|esAutoVScroll|esReadOnly, 0)
	procShowWindow.Call(uintptr(u.hLog), swHide)
	u.hFooterLine = label("", ssEtchedHorz, u.fontUI)
	u.hRTT = label("", ssEndEllipsis, u.fontUI)
	u.hTraffic = label("", ssEndEllipsis, u.fontUI)
	u.hDrops = label("", ssEndEllipsis, u.fontUI)
	u.hGame = label("", ssEndEllipsis, u.fontUI)
	u.hToggleLog = button("查看日志", idLog)
	u.hExport = button("导出日志", idExport)
	u.hCaptureFolder = button("抓包目录", idCaptureFolder)
	u.hSubnet = label("", ssRight|ssEndEllipsis, u.fontUI)
	procSendMessageW.Call(uintptr(u.hRelay), emSetLimitText, 255, 0)
	procSendMessageW.Call(uintptr(u.hName), emSetLimitText, 48, 0)
	procSendMessageW.Call(uintptr(u.hCode), emSetLimitText, 64, 0)
	sendText(u.hCode, emSetCueBanner, 1, "10 位房间码")
}

func windowDimensions(clientWidth, clientHeight int) (int32, int32) {
	bounds := rect{Right: px(clientWidth), Bottom: px(clientHeight)}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&bounds)), windowStyle, 0, 0)
	return bounds.Right - bounds.Left, bounds.Bottom - bounds.Top
}

func setMinimumWindowSize(destination uintptr) {
	var info minMaxInfo
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&info)), destination, unsafe.Sizeof(info))
	info.MinTrackSize.X, info.MinTrackSize.Y = windowDimensions(uiMinWidth, uiMinHeight)
	procRtlMoveMemory.Call(destination, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
}

func (u *uiWindow) layout() {
	var bounds rect
	procGetClientRect.Call(uintptr(u.hwnd), uintptr(unsafe.Pointer(&bounds)))
	width, height := max(bounds.Right, px(uiMinWidth)), max(bounds.Bottom, px(uiMinHeight))
	margin, gap := px(24), px(10)
	right := width - margin
	available := width - 2*margin
	move := func(h syscall.Handle, left, top, w, hgt int32) {
		procMoveWindow.Call(uintptr(h), uintptr(left), uintptr(top), uintptr(w), uintptr(hgt), 0)
	}

	toolWidth := px(112)
	move(u.hTitle, margin, px(20), available-2*(toolWidth+gap), px(38))
	move(u.hDiag, right-2*toolWidth-gap, px(25), toolWidth, px(32))
	move(u.hSpeed, right-toolWidth, px(25), toolWidth, px(32))
	move(u.hStatus, margin, px(65), available, px(20))
	move(u.hDetail, margin, px(89), available, px(37))

	relayWidth := (available - gap) * 61 / 100
	nameLeft := margin + relayWidth + gap
	move(u.hRelayLabel, margin, px(130), relayWidth, px(20))
	move(u.hNameLabel, nameLeft, px(130), right-nameLeft, px(20))
	move(u.hRelay, margin, px(151), relayWidth, px(32))
	move(u.hName, nameLeft, px(151), right-nameLeft, px(32))

	const actionTop = 196
	createWidth, leaveWidth, actionWidth, codeLabelWidth := px(106), px(106), px(112), px(54)
	roomLabelLeft := margin + createWidth + px(22)
	roomInputLeft := roomLabelLeft + codeLabelWidth
	leaveLeft := right - leaveWidth
	actionLeft := leaveLeft - gap - actionWidth
	move(u.hCreate, margin, px(actionTop), createWidth, px(33))
	move(u.hCodeLabel, roomLabelLeft, px(actionTop+8), codeLabelWidth, px(20))
	move(u.hCode, roomInputLeft, px(actionTop), actionLeft-gap-roomInputLeft, px(33))
	move(u.hRoomAction, actionLeft, px(actionTop), actionWidth, px(33))
	move(u.hLeave, leaveLeft, px(actionTop), leaveWidth, px(33))
	move(u.hP2P, margin, px(237), px(129), px(26))
	firewallButtonWidth := px(132)
	move(u.hP2PFirewall, right-firewallButtonWidth, px(237), firewallButtonWidth, px(26))
	move(u.hP2PStatus, margin+px(139), px(242), available-px(139)-firewallButtonWidth-gap, px(20))
	move(u.hDivider, margin, px(275), available, px(2))

	move(u.hMembers, margin, px(291), px(180), px(20))
	move(u.hRoster, right-px(195), px(291), px(195), px(20))
	listTop := px(317)
	contentBottom := height - px(98)
	listHeight := contentBottom - listTop
	if u.logOpen {
		logHeight := min(px(214), (listHeight-gap)/2)
		listHeight -= logHeight + gap
		move(u.hLog, margin, contentBottom-logHeight, available, logHeight)
	}
	move(u.hList, margin, listTop, available, listHeight)

	move(u.hFooterLine, margin, height-px(84), available, px(2))
	metricsY := height - px(74)
	rttWidth, dropWidth, gameWidth := available*22/100, available*22/100, available*21/100
	trafficWidth := available - rttWidth - dropWidth - gameWidth
	move(u.hRTT, margin, metricsY, rttWidth, px(20))
	move(u.hTraffic, margin+rttWidth, metricsY, trafficWidth, px(20))
	move(u.hDrops, right-dropWidth-gameWidth, metricsY, dropWidth, px(20))
	move(u.hGame, right-gameWidth, metricsY, gameWidth, px(20))

	buttonY := height - px(44)
	logWidth, exportWidth, captureWidth := px(92), px(92), px(100)
	move(u.hToggleLog, margin, buttonY, logWidth, px(30))
	move(u.hExport, margin+logWidth+gap, buttonY, exportWidth, px(30))
	captureLeft := margin + logWidth + exportWidth + 2*gap
	move(u.hCaptureFolder, captureLeft, buttonY, captureWidth, px(30))
	move(u.hSubnet, captureLeft+captureWidth+gap, buttonY+px(5), available-logWidth-exportWidth-captureWidth-3*gap, px(20))

	u.resizeColumns()
	procRedrawWindow.Call(uintptr(u.hwnd), uintptr(unsafe.Pointer(&bounds)), 0, rdwInvalidate|rdwErase|rdwFrame|rdwAllChildren)
}

func (u *uiWindow) insertColumns() {
	for index, title := range []string{"昵称", "角色", "状态", "连接 / 延迟", "虚拟地址"} {
		text, _ := syscall.UTF16PtrFromString(title)
		column := lvColumnW{Mask: lvcfText | lvcfSubItem, PszText: text, ISubItem: int32(index)}
		procSendMessageW.Call(uintptr(u.hList), lvmInsertColumnW, uintptr(index), uintptr(unsafe.Pointer(&column)))
	}
}

func (u *uiWindow) resizeColumns() {
	var bounds rect
	procGetClientRect.Call(uintptr(u.hList), uintptr(unsafe.Pointer(&bounds)))
	scrollWidth, _, _ := procGetSystemMetrics.Call(smCxVScroll)
	role, status, path, address := px(96), px(64), px(132), px(134)
	name := max(px(120), bounds.Right-bounds.Left-int32(scrollWidth)-role-status-path-address)
	for index, width := range []int32{name, role, status, path, address} {
		procSendMessageW.Call(uintptr(u.hList), lvmSetColumnWidth, uintptr(index), uintptr(width))
	}
}

func (u *uiWindow) fillMembers(members []memberView, self int) {
	procSendMessageW.Call(uintptr(u.hList), lvmDeleteAllItems, 0, 0)
	online := 0
	for index, member := range members {
		role, status, path := "成员", "离线", "离线"
		if member.ID == self {
			path = "本机"
		}
		if member.Host {
			role = "房主"
		}
		if member.ID == self {
			role += "（你）"
		}
		if member.Online {
			status = "在线"
			online++
			if member.ID != self {
				switch member.Path {
				case tunnel.Direct:
					path = "直连"
				case tunnel.Punching:
					path = "打洞中"
				default:
					path = "中转"
				}
				if member.Latency > 0 {
					latency := "< 1 ms"
					if member.Latency >= time.Millisecond {
						latency = fmt.Sprintf("%d ms", member.Latency.Milliseconds())
					}
					path += " · " + latency
				} else {
					path += " · 测量中"
				}
			}
		}
		for column, value := range []string{member.Name, role, status, path, member.Virtual} {
			text, _ := syscall.UTF16PtrFromString(value)
			item := lvItemW{Mask: lvifText, IItem: int32(index), ISubItem: int32(column), PszText: text}
			if column == 0 {
				procSendMessageW.Call(uintptr(u.hList), lvmInsertItemW, 0, uintptr(unsafe.Pointer(&item)))
			} else {
				procSendMessageW.Call(uintptr(u.hList), lvmSetItemTextW, uintptr(index), uintptr(unsafe.Pointer(&item)))
			}
		}
	}
	status := "尚无成员"
	if len(members) != 0 {
		status = fmt.Sprintf("%d / %d 在线", online, len(members))
	}
	u.setText(u.hRoster, status)
}
