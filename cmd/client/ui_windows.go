//go:build windows

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"civ6relay/internal/win"
)

const (
	uiClassName = "Civ6RelayMainWnd"
	uiTitle     = "Civ 6 UDP Relay (Go)"

	timerRefresh = 1

	idCreate     = 100
	idJoin       = 101
	idCopy       = 102
	idDisconnect = 103
	idDiag       = 104
	idExport     = 105

	wmCtlColorStatic = 0x0138
	transparent      = 1
	lvmGetHeader     = lvmFirst + 31
)

var (
	uiWndProcCallback uintptr
	appUI             *uiWindow
	uiBgBrush         syscall.Handle
)

type uiWindow struct {
	hwnd      syscall.Handle
	s         *session
	fontUI    syscall.Handle
	fontMono  syscall.Handle
	fontTitle syscall.Handle

	hTitle, hSubtitle, hStatus, hFooter syscall.Handle
	hMembersLabel, hLogLabel            syscall.Handle
	hRelayLabel, hNameLabel, hCodeLabel syscall.Handle
	hRelay, hName, hCode                syscall.Handle
	hCreate, hJoin, hCopy, hDisconnect  syscall.Handle
	hDiag, hExport                      syscall.Handle
	hList, hLog                         syscall.Handle

	lastLogSeq  int64
	lastErrSeq  int64
	lastMembers string
	lastCode    string
	diagBusy    bool

	tasksMu sync.Mutex
	tasks   []func()
}

func runNativeUI(s *session, cfg Config) error {
	initDPI()
	initCommonControls()
	uiBgBrush = createSolidBrush(0x00FAF7F5)

	u := &uiWindow{s: s}
	u.fontUI = createFont(9, false, "Microsoft YaHei UI")
	u.fontMono = createFont(9, false, "Consolas")
	u.fontTitle = createFont(15, true, "Microsoft YaHei UI")

	className, _ := syscall.UTF16PtrFromString(uiClassName)
	hInstance, _, _ := procGetModuleHandleW.Call(0)
	if uiWndProcCallback == 0 {
		uiWndProcCallback = syscall.NewCallback(uiWndProc)
	}
	var wc wndClassExW
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.LpfnWndProc = uiWndProcCallback
	wc.HInstance = syscall.Handle(hInstance)
	wc.HbrBackground = uiBgBrush
	wc.LpszClassName = className
	atom, _, regErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		return fmt.Errorf("注册窗口类失败: %v", regErr)
	}

	clientW, clientH := px(920), px(648)
	style := uint32(wsOverlapped | wsCaption | wsSysMenu | wsMinimizeBox | wsClipChildren | wsClipSiblings)
	r := rect{0, 0, clientW, clientH}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&r)), uintptr(style), 0, 0)
	winW := r.Right - r.Left
	winH := r.Bottom - r.Top
	screenW, _, _ := procGetSystemMetrics.Call(smCxScreen)
	screenH, _, _ := procGetSystemMetrics.Call(smCyScreen)
	x := (int32(screenW) - winW) / 2
	y := (int32(screenH) - winH) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	titlePtr, _ := syscall.UTF16PtrFromString(uiTitle)
	hwnd, _, createErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(winW), uintptr(winH),
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("创建窗口失败: %v", createErr)
	}
	u.hwnd = syscall.Handle(hwnd)
	appUI = u
	u.createControls(cfg)

	s.logInfo("客户端已就绪（原生版）")
	procSetTimer.Call(uintptr(u.hwnd), timerRefresh, 400, 0)
	procShowWindow.Call(uintptr(u.hwnd), swShowNormal)
	procUpdateWindow.Call(uintptr(u.hwnd))
	procSetFocus.Call(uintptr(u.hRelay))
	u.refresh()

	var msg msgW
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	procKillTimer.Call(uintptr(u.hwnd), timerRefresh)
	appUI = nil
	for _, font := range []syscall.Handle{u.fontUI, u.fontMono, u.fontTitle} {
		if font != 0 {
			procDeleteObject.Call(uintptr(font))
		}
	}
	if uiBgBrush != 0 {
		procDeleteObject.Call(uintptr(uiBgBrush))
		uiBgBrush = 0
	}
	return nil
}

func (u *uiWindow) createControls(cfg Config) {
	static := uint32(0x80) // SS_NOPREFIX
	editStyle := uint32(esAutoHScroll)
	buttonStyle := uint32(bsPushButton)

	u.hTitle = createChild(u.hwnd, 0, "STATIC", "Civ 6 UDP Relay", static, px(18), px(10), px(600), px(30), 0)
	u.hSubtitle = createChild(u.hwnd, 0, "STATIC", "Windows x64 · 公网 UDP 房间中转 · 原生版", static, px(18), px(42), px(700), px(18), 0)

	u.hRelayLabel = createChild(u.hwnd, 0, "STATIC", "中转地址", static, px(18), px(74), px(70), px(22), 0)
	u.hRelay = createChild(u.hwnd, wsExClientEdge, "EDIT", win.FormatRelay(cfg.RelayHost, cfg.RelayPort), editStyle, px(92), px(72), px(330), px(26), 0)
	u.hNameLabel = createChild(u.hwnd, 0, "STATIC", "昵称", static, px(440), px(74), px(40), px(22), 0)
	u.hName = createChild(u.hwnd, wsExClientEdge, "EDIT", cfg.DisplayName, editStyle, px(484), px(72), px(200), px(26), 0)

	u.hCodeLabel = createChild(u.hwnd, 0, "STATIC", "房间码", static, px(18), px(108), px(70), px(22), 0)
	u.hCode = createChild(u.hwnd, wsExClientEdge, "EDIT", "", editStyle, px(92), px(106), px(330), px(26), 0)
	u.hCreate = createChild(u.hwnd, 0, "BUTTON", "创建房间", buttonStyle, px(440), px(104), px(88), px(28), idCreate)
	u.hJoin = createChild(u.hwnd, 0, "BUTTON", "加入房间", buttonStyle, px(534), px(104), px(88), px(28), idJoin)
	u.hCopy = createChild(u.hwnd, 0, "BUTTON", "复制房间码", buttonStyle, px(628), px(104), px(100), px(28), idCopy)
	u.hDisconnect = createChild(u.hwnd, 0, "BUTTON", "断开", buttonStyle, px(734), px(104), px(88), px(28), idDisconnect)

	u.hStatus = createChild(u.hwnd, 0, "STATIC", "", static, px(18), px(142), px(884), px(22), 0)

	u.hMembersLabel = createChild(u.hwnd, 0, "STATIC", "房间成员", static, px(18), px(172), px(200), px(20), 0)
	u.hList = createChild(u.hwnd, wsExClientEdge, "SysListView32", "", lvsReport|lvsSingleSel|lvsShowSelAlways, px(18), px(194), px(884), px(150), 0)

	u.hLogLabel = createChild(u.hwnd, 0, "STATIC", "运行日志", static, px(18), px(352), px(200), px(20), 0)
	u.hDiag = createChild(u.hwnd, 0, "BUTTON", "诊断", buttonStyle, px(742), px(348), px(74), px(28), idDiag)
	u.hExport = createChild(u.hwnd, 0, "BUTTON", "导出日志", buttonStyle, px(822), px(348), px(80), px(28), idExport)
	u.hLog = createChild(u.hwnd, wsExClientEdge, "EDIT", "", wsVScroll|esMultiline|esAutoVScroll|esReadOnly, px(18), px(380), px(884), px(230), 0)

	u.hFooter = createChild(u.hwnd, 0, "STATIC", fmt.Sprintf("Civ6RelayClient %s · 配置保存在 %%APPDATA%%\\Civ6Relay\\config.json", version), static, px(18), px(618), px(884), px(18), 0)

	setFont(u.hTitle, u.fontTitle)
	setFont(u.hSubtitle, u.fontUI)
	setFont(u.hRelayLabel, u.fontUI)
	setFont(u.hRelay, u.fontUI)
	setFont(u.hNameLabel, u.fontUI)
	setFont(u.hName, u.fontUI)
	setFont(u.hCodeLabel, u.fontUI)
	setFont(u.hCode, u.fontUI)
	setFont(u.hCreate, u.fontUI)
	setFont(u.hJoin, u.fontUI)
	setFont(u.hCopy, u.fontUI)
	setFont(u.hDisconnect, u.fontUI)
	setFont(u.hStatus, u.fontUI)
	setFont(u.hMembersLabel, u.fontUI)
	setFont(u.hList, u.fontUI)
	setFont(u.hLogLabel, u.fontUI)
	setFont(u.hDiag, u.fontUI)
	setFont(u.hExport, u.fontUI)
	setFont(u.hLog, u.fontMono)
	setFont(u.hFooter, u.fontUI)

	procSendMessageW.Call(uintptr(u.hList), lvmSetExtendedListViewStyle, 0,
		uintptr(lvsExFullRowSelect|lvsExGridLines|lvsExDoubleBuffer))
	header, _, _ := procSendMessageW.Call(uintptr(u.hList), lvmGetHeader, 0, 0)
	if header != 0 {
		setFont(syscall.Handle(header), u.fontUI)
	}
	u.insertColumns()

	enableWindow(u.hCopy, false)
	enableWindow(u.hDisconnect, false)
	setWindowText(u.hStatus, "状态：未连接    Civ 6：未运行    虚拟网段：-    RTT：-    收发：0 / 0")
}

func (u *uiWindow) insertColumns() {
	columns := []struct {
		title  string
		width  int
		center bool
	}{
		{"ID", 60, true},
		{"昵称", 290, false},
		{"角色", 80, true},
		{"状态", 80, true},
		{"虚拟地址", 330, true},
	}
	for i, c := range columns {
		text, _ := syscall.UTF16FromString(c.title)
		fmtValue := int32(lvcfmtLeft)
		if c.center {
			fmtValue = lvcfmtCenter
		}
		col := lvColumnW{
			Mask:     lvcfFmt | lvcfWidth | lvcfText | lvcfSubItem,
			Fmt:      fmtValue,
			Cx:       px(c.width),
			PszText:  &text[0],
			CchText:  -1,
			ISubItem: int32(i),
		}
		procSendMessageW.Call(uintptr(u.hList), lvmInsertColumnW, uintptr(i), uintptr(unsafe.Pointer(&col)))
	}
}

func (u *uiWindow) refresh() {
	snap := u.s.Snapshot()

	status := snap.Status
	if snap.Connected && snap.Capture && !snap.BridgeRunning && status == "中继运行中" {
		status = "抓包驱动已停止"
	}
	setWindowText(u.hStatus, formatStatus(snap, status))

	canStart := !snap.Busy && !snap.Connected
	enableWindow(u.hCreate, canStart)
	enableWindow(u.hJoin, canStart)
	enableWindow(u.hDisconnect, !snap.Busy && snap.Connected)
	enableWindow(u.hCopy, snap.RoomCode != "")
	enableWindow(u.hRelay, !snap.Connected && !snap.Busy)
	enableWindow(u.hName, !snap.Connected && !snap.Busy)
	enableWindow(u.hCode, !snap.Connected && !snap.Busy)
	enableWindow(u.hDiag, !u.diagBusy)
	enableWindow(u.hExport, true)

	if snap.Connected && snap.RoomCode != "" && snap.RoomCode != u.lastCode {
		u.lastCode = snap.RoomCode
		if windowText(u.hCode) != snap.RoomCode {
			setWindowText(u.hCode, snap.RoomCode)
		}
	}

	key := membersKey(snap.Members)
	if key != u.lastMembers {
		u.lastMembers = key
		u.fillMembers(snap.Members)
	}

	u.appendNewLogs(snap)

	if snap.ErrorSeq > u.lastErrSeq {
		u.lastErrSeq = snap.ErrorSeq
		if snap.Error != "" {
			msgBox(u.hwnd, "连接失败", snap.Error, mbOK|mbIconError)
		}
	}
}

func formatStatus(snap sessionSnapshot, status string) string {
	civ := "未运行"
	if len(snap.Civ) > 0 {
		parts := make([]string, 0, len(snap.Civ))
		for _, p := range snap.Civ {
			parts = append(parts, fmt.Sprintf("%s(%d)", p.Name, p.PID))
		}
		civ = strings.Join(parts, ", ")
	}
	rtt := "-"
	if snap.HasRTT {
		rtt = fmt.Sprintf("%d ms", snap.RTTms)
	}
	subnet := "-"
	if snap.Subnet != "" {
		subnet = snap.Subnet
	}
	return fmt.Sprintf("状态：%s    Civ 6：%s    虚拟网段：%s    RTT：%s    收发：%d / %d", status, civ, subnet, rtt, snap.Rx, snap.Tx)
}

func membersKey(members []memberView) string {
	var b strings.Builder
	for _, m := range members {
		fmt.Fprintf(&b, "%d|%s|%t|%t|%s;", m.ID, m.Name, m.Host, m.Online, m.Virtual)
	}
	return b.String()
}

func (u *uiWindow) fillMembers(members []memberView) {
	procSendMessageW.Call(uintptr(u.hList), lvmDeleteAllItems, 0, 0)
	for i, m := range members {
		role := "成员"
		if m.Host {
			role = "房主"
		}
		status := "离线"
		if m.Online {
			status = "在线"
		}
		values := []string{strconv.Itoa(m.ID), m.Name, role, status, m.Virtual}
		for col, value := range values {
			text, err := syscall.UTF16FromString(value)
			if err != nil {
				continue
			}
			item := lvItemW{
				Mask:     lvifText,
				IItem:    int32(i),
				ISubItem: int32(col),
				PszText:  &text[0],
				CchText:  -1,
			}
			if col == 0 {
				procSendMessageW.Call(uintptr(u.hList), lvmInsertItemW, 0, uintptr(unsafe.Pointer(&item)))
			} else {
				procSendMessageW.Call(uintptr(u.hList), lvmSetItemTextW, uintptr(i), uintptr(unsafe.Pointer(&item)))
			}
		}
	}
}

func (u *uiWindow) appendNewLogs(snap sessionSnapshot) {
	var b strings.Builder
	for _, entry := range snap.Logs {
		if entry.Seq <= u.lastLogSeq {
			continue
		}
		fmt.Fprintf(&b, "%s [%s] %s\r\n", entry.Time, entry.Level, entry.Message)
		u.lastLogSeq = entry.Seq
	}
	if b.Len() == 0 {
		return
	}
	text, err := syscall.UTF16FromString(b.String())
	if err != nil {
		return
	}
	procSendMessageW.Call(uintptr(u.hLog), emSetReadOnly, 0, 0)
	procSendMessageW.Call(uintptr(u.hLog), emSetSel, ^uintptr(0), ^uintptr(0))
	procSendMessageW.Call(uintptr(u.hLog), emReplaceSel, 0, uintptr(unsafe.Pointer(&text[0])))
	procSendMessageW.Call(uintptr(u.hLog), emSetReadOnly, 1, 0)
	procSendMessageW.Call(uintptr(u.hLog), emScrollCaret, 0, 0)
}

func (u *uiWindow) onCommand(id uint32) {
	switch id {
	case idCreate:
		if err := u.s.Connect("create", windowText(u.hRelay), windowText(u.hName), ""); err != nil {
			msgBox(u.hwnd, "参数错误", err.Error(), mbOK|mbIconError)
		}
	case idJoin:
		if err := u.s.Connect("join", windowText(u.hRelay), windowText(u.hName), windowText(u.hCode)); err != nil {
			msgBox(u.hwnd, "参数错误", err.Error(), mbOK|mbIconError)
		}
	case idCopy:
		code := strings.TrimSpace(windowText(u.hCode))
		if code != "" {
			if err := copyToClipboard(u.hwnd, code); err != nil {
				msgBox(u.hwnd, "复制失败", err.Error(), mbOK|mbIconError)
			} else {
				u.s.logInfo("房间码已复制")
			}
		}
	case idDisconnect:
		u.s.Disconnect()
	case idDiag:
		u.runDiagnostics()
	case idExport:
		u.exportLog()
	}
	u.refresh()
}

func (u *uiWindow) runDiagnostics() {
	if u.diagBusy {
		return
	}
	u.diagBusy = true
	enableWindow(u.hDiag, false)
	relay := windowText(u.hRelay)
	go func() {
		summary, _, path, ok := collectDiagnostics(relay)
		u.post(func() {
			u.diagBusy = false
			enableWindow(u.hDiag, true)
			flags := uint32(mbOK | mbIconInformation)
			if !ok {
				flags = mbOK | mbIconError
			}
			msgBox(u.hwnd, "诊断结果", summary+"\n\n完整报告已保存:\n"+path, flags)
		})
	}()
}

func (u *uiWindow) exportLog() {
	defaultName := fmt.Sprintf("civ6relay-%s.log", time.Now().Format("20060102-150405"))
	path, ok := saveFileDialog(u.hwnd, defaultName)
	if !ok || path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(u.s.logText()), 0o644); err != nil {
		msgBox(u.hwnd, "导出失败", err.Error(), mbOK|mbIconError)
		return
	}
	msgBox(u.hwnd, "导出成功", "运行日志已保存到:\n"+path, mbOK|mbIconInformation)
}

func (u *uiWindow) post(fn func()) {
	u.tasksMu.Lock()
	u.tasks = append(u.tasks, fn)
	u.tasksMu.Unlock()
	procPostMessageW.Call(uintptr(u.hwnd), wmAppInvoke, 0, 0)
}

func (u *uiWindow) drainTasks() {
	u.tasksMu.Lock()
	tasks := u.tasks
	u.tasks = nil
	u.tasksMu.Unlock()
	for _, fn := range tasks {
		fn()
	}
}

func uiWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	u := appUI
	switch uint32(msg) {
	case wmCtlColorStatic:
		if uiBgBrush != 0 {
			procSetBkMode.Call(wparam, transparent)
			return uintptr(uiBgBrush)
		}
	case wmTimer:
		if u != nil {
			u.refresh()
		}
		return 0
	case wmCommand:
		if u != nil {
			id := uint32(wparam) & 0xffff
			code := uint32(wparam) >> 16
			if code == bnClicked {
				u.onCommand(id)
			}
		}
		return 0
	case wmAppInvoke:
		if u != nil {
			u.drainTasks()
		}
		return 0
	case wmClose:
		if u != nil {
			u.s.Shutdown()
		}
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func initCommonControls() {
	icc := initCommonControlsEx{
		Size: uint32(unsafe.Sizeof(initCommonControlsEx{})),
		ICC:  iccListViewClasses | iccStandardClasses,
	}
	procInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&icc)))
}

func createSolidBrush(color uint32) syscall.Handle {
	r, _, _ := procCreateSolidBrush.Call(uintptr(color))
	return syscall.Handle(r)
}
