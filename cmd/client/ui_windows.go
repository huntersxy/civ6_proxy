//go:build windows

package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"civ6relay/internal/protocol"
	"civ6relay/internal/tunnel"
)

const (
	uiClassName = "Civ6RelayMainWnd"
	uiTitle     = "文明 VI 联机中继"
)

const (
	idCreate = 100 + iota
	idRoomAction
	idLeave
	idDiagnostics
	idLog
	idExport
	idRoomCode
	idRelay
	idName
	idSpeedTest
	idP2PToggle
	idCaptureFolder
	idP2PFirewall
)

const (
	timerRefresh     = 1
	wmCtlColorStatic = 0x0138
	transparent      = 1
	windowStyle      = wsOverlapped | wsCaption | wsSysMenu | wsMinimizeBox | wsMaximizeBox | wsThickFrame | wsClipChildren
)

var appUI *uiWindow

type uiWindow struct {
	hwnd syscall.Handle
	s    *session

	fontUI, fontStrong, fontTitle syscall.Handle
	bgBrush                       syscall.Handle
	palette                       uiPalette

	hTitle, hStatus, hDetail, hDivider, hFooterLine syscall.Handle
	hRelayLabel, hNameLabel, hCodeLabel, hRelay     syscall.Handle
	hName, hCode, hCreate, hRoomAction, hLeave      syscall.Handle
	hP2P, hP2PStatus, hP2PFirewall                  syscall.Handle
	hDiag, hSpeed, hMembers, hRoster, hList         syscall.Handle
	hRTT, hTraffic, hDrops, hGame, hSubnet          syscall.Handle
	hToggleLog, hExport, hCaptureFolder, hLog       syscall.Handle
	speed                                           *speedWindow

	state       connectionState
	statusColor uintptr
	joinDraft   string
	activeCode  string
	codeLocked  bool
	editingCode bool
	closing     bool
	diagBusy    bool
	exportBusy  bool
	feedback    string
	feedbackEnd time.Time
	logOpen     bool
	lastLogSeq  int64
	lastMembers []memberView
	lastSelf    int
	text        map[syscall.Handle]string
	enabled     map[syscall.Handle]bool

	refreshQueued atomic.Bool
	stopped       atomic.Bool
	tasksMu       sync.Mutex
	tasks         []func()
}

func runNativeUI(s *session, cfg Config) error {
	initDPI()
	icc := initCommonControlsEx{Size: uint32(unsafe.Sizeof(initCommonControlsEx{})),
		ICC: iccListViewClasses | iccStandardClasses | iccProgressClass}
	procInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&icc)))

	u := &uiWindow{
		s: s, palette: systemPalette(),
		text: make(map[syscall.Handle]string), enabled: make(map[syscall.Handle]bool),
	}
	u.fontUI = createFont(10, false, "Microsoft YaHei UI")
	u.fontStrong = createFont(10, true, "Microsoft YaHei UI")
	u.fontTitle = createFont(19, true, "Microsoft YaHei UI")
	brush, _, _ := procCreateSolidBrush.Call(u.palette.background)
	u.bgBrush = syscall.Handle(brush)
	defer func() {
		for _, object := range []syscall.Handle{u.fontUI, u.fontStrong, u.fontTitle, u.bgBrush} {
			if object != 0 {
				procDeleteObject.Call(uintptr(object))
			}
		}
	}()

	className, _ := syscall.UTF16PtrFromString(uiClassName)
	instance, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, 32512)
	class := wndClassExW{
		CbSize: uint32(unsafe.Sizeof(wndClassExW{})), LpfnWndProc: syscall.NewCallback(uiWndProc),
		HInstance: syscall.Handle(instance), HbrBackground: u.bgBrush, HCursor: syscall.Handle(cursor), LpszClassName: className,
	}
	if atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return fmt.Errorf("注册窗口类失败: %v", err)
	}
	speedClass, _ := syscall.UTF16PtrFromString(speedClassName)
	class.LpszClassName, class.LpfnWndProc = speedClass, syscall.NewCallback(speedWndProc)
	if atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return fmt.Errorf("注册网络测试窗口类失败: %v", err)
	}

	width, height := windowDimensions(852, 618)
	var workArea rect
	procSystemParametersInfo.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&workArea)), 0)
	width = min(width, max(workArea.Right-workArea.Left, px(uiMinWidth)))
	height = min(height, max(workArea.Bottom-workArea.Top, px(uiMinHeight)))
	x := workArea.Left + max(0, (workArea.Right-workArea.Left-width)/2)
	y := workArea.Top + max(0, (workArea.Bottom-workArea.Top-height)/2)
	title, _ := syscall.UTF16PtrFromString(uiTitle)
	hwnd, _, err := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)), windowStyle,
		uintptr(x), uintptr(y), uintptr(width), uintptr(height), 0, 0, instance, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("创建窗口失败: %v", err)
	}
	u.hwnd = syscall.Handle(hwnd)
	appUI = u
	defer func() { appUI = nil }()
	u.createControls(cfg)
	u.layout()
	u.refresh()
	s.startBackground(u.requestRefresh)
	defer s.Shutdown()
	procSetTimer.Call(hwnd, timerRefresh, 750, 0)
	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)
	procSetFocus.Call(uintptr(u.hRelay))

	var message msgW
	for {
		result, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(result) <= 0 {
			break
		}
		dialog := hwnd
		if w := u.speed; w != nil && w.owns(message.HWnd) {
			if message.Message == wmKeyDown && message.WParam == vkEscape {
				if w.cancel != nil {
					w.stop()
				} else {
					procShowWindow.Call(uintptr(w.hwnd), swHide)
				}
				continue
			}
			dialog = uintptr(w.hwnd)
		} else if u.handleKey(message) {
			continue
		}
		handled, _, _ := procIsDialogMessageW.Call(dialog, uintptr(unsafe.Pointer(&message)))
		if handled == 0 {
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&message)))
		}
	}
	return nil
}

func (u *uiWindow) refresh() {
	if u.closing {
		u.setText(u.hStatus, "正在关闭")
		u.setText(u.hDetail, "正在结束房间会话并释放网络资源…")
		return
	}
	snap := u.s.Snapshot()
	previousState := u.state
	if snap.State != u.state {
		u.feedback = ""
		u.state = snap.State
	}
	u.statusColor = u.palette.muted
	status, detail := u.statusText(snap)
	if u.feedback != "" && time.Now().Before(u.feedbackEnd) {
		detail = u.feedback
	}
	u.setText(u.hStatus, status)
	u.setText(u.hDetail, detail)
	u.updateRoomControls(snap.State, snap.RoomCode)
	u.updateP2PControls(snap)
	u.s.SyncP2PFirewall(snap.P2P)
	u.enable(u.hDiag, !u.diagBusy && (u.canConnect() || snap.State == stateJoined))
	u.enable(u.hSpeed, snap.State != stateConnecting && snap.State != stateLeaving)
	u.enable(u.hExport, !u.exportBusy)
	diagLabel := "诊断"
	if u.diagBusy {
		diagLabel = "正在诊断…"
	}
	u.setText(u.hDiag, diagLabel)
	if previousState == stateConnecting && snap.State == stateJoined && snap.RoomCode != "" {
		u.refreshRelayHistory()
		procSetFocus.Call(uintptr(u.hRoomAction))
	}

	if snap.MemberID != u.lastSelf || !slices.Equal(snap.Members, u.lastMembers) {
		u.lastSelf, u.lastMembers = snap.MemberID, snap.Members
		u.fillMembers(snap.Members, snap.MemberID)
	}
	u.refreshMetrics(snap)
	if u.logOpen {
		u.refreshLog()
	}
}

func (u *uiWindow) statusText(snap sessionSnapshot) (string, string) {
	detail := snap.Detail
	if first, _, multi := strings.Cut(detail, "\n"); multi {
		detail = first + "。完整信息见运行日志。"
	}
	switch snap.State {
	case stateConnecting:
		return "正在连接", detail
	case stateLeaving:
		return "正在结束会话", detail
	case stateFailed:
		u.statusColor = u.palette.error
		return "连接失败", detail
	case stateJoined:
		switch {
		case snap.Tunnel.Silent:
			u.statusColor = u.palette.error
			return "中转暂时无响应", "正在继续探测中转。连接恢复后会自动更新。"
		case snap.Capture && !snap.BridgeRunning:
			u.statusColor = u.palette.error
			return "游戏转发已停止", "请查看运行日志或运行诊断。"
		case !snap.Capture:
			u.statusColor = u.palette.active
			return "已进入房间", "隧道测试模式，游戏流量未接入。"
		default:
			u.statusColor = u.palette.active
			if snap.PCAPError != "" {
				detail = "本地 PCAP 记录异常，请查看运行日志。"
			} else if snap.Host && snap.PCAPPath != "" {
				detail = fmt.Sprintf("房主端正在记录游戏 UDP（%d 包），自动保留最近 3 天。", snap.PCAPPackets)
			}
			if detail == "" {
				detail = "成员上线后，在文明 VI 的「多人游戏 › 局域网」创建或加入游戏。"
			}
			return "游戏转发运行中", detail
		}
	default:
		if detail == "" {
			detail = "和朋友使用同一中转地址，再创建房间或输入房间码加入。"
		}
		return "尚未连接", detail
	}
}

func (u *uiWindow) canConnect() bool {
	return !u.closing && !u.diagBusy && (u.state == stateIdle || u.state == stateFailed)
}

func (u *uiWindow) updateRoomControls(state connectionState, roomCode string) {
	joined := state == stateJoined
	if joined && roomCode != "" && roomCode != u.activeCode {
		u.activeCode = roomCode
		u.writeRoomCode(roomCode)
	} else if (!joined || roomCode == "") && u.activeCode != "" {
		u.activeCode = ""
		u.writeRoomCode(u.joinDraft)
	}
	canConnect := u.canConnect()
	u.enable(u.hRelay, canConnect)
	u.enable(u.hName, canConnect)
	u.enable(u.hCreate, canConnect)
	if locked := !canConnect; locked != u.codeLocked {
		u.codeLocked = locked
		var readOnly uintptr
		if locked {
			readOnly = 1
		}
		procSendMessageW.Call(uintptr(u.hCode), emSetReadOnly, readOnly, 0)
	}

	if joined {
		u.setText(u.hRoomAction, "复制房间码")
		u.enable(u.hRoomAction, u.activeCode != "")
	} else {
		u.setText(u.hRoomAction, "加入房间")
		_, err := protocol.NormalizeRoomCode(u.joinDraft)
		u.enable(u.hRoomAction, canConnect && err == nil)
	}
	leaveLabel := "离开房间"
	if state == stateConnecting {
		leaveLabel = "取消连接"
	}
	u.setText(u.hLeave, leaveLabel)
	u.enable(u.hLeave, joined || state == stateConnecting)
}

func (u *uiWindow) writeRoomCode(code string) {
	u.editingCode = true
	setWindowText(u.hCode, code)
	u.editingCode = false
}

func (u *uiWindow) updateP2PControls(snap sessionSnapshot) {
	status := "未入房"
	joined := snap.State == stateJoined
	if joined {
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
				} else {
					status = fmt.Sprintf("%d / %d 位玩家已直连", direct, total)
				}
				if punching > 0 {
					status += fmt.Sprintf("，%d 位正在打洞", punching)
				} else if direct < total && direct != 0 {
					status += "，其余走中转"
				}
			}
			if snap.P2PFirewallError != "" {
				status += " · 规则未同步，仍会尝试直连"
			}
		case snap.P2PFirewallError != "":
			status = "防火墙规则未同步；直连失败时自动走中转"
		case !snap.Host:
			status = "房主未开启"
		default:
			status = "已关闭"
		}
	}
	var check uintptr
	if snap.P2P.Enabled {
		check = 1
	}
	procSendMessageW.Call(uintptr(u.hP2P), bmSetCheck, check, 0)
	u.enable(u.hP2P, joined && snap.Host && snap.P2P.Supported)
	u.enable(u.hP2PFirewall, joined && snap.P2P.Enabled && len(snap.P2P.Endpoints) > 0)
	u.setText(u.hP2PStatus, status)
}

func (u *uiWindow) refreshMetrics(snap sessionSnapshot) {
	rtt, traffic, drops, game := "—", "—", "—", "未运行"
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
	if snap.CivRunning {
		game = "运行中"
	}
	u.setText(u.hRTT, "中转延迟  "+rtt)
	u.setText(u.hTraffic, "游戏收 / 发  "+traffic)
	u.setText(u.hDrops, "本地丢弃  "+drops)
	u.setText(u.hGame, "文明 VI  "+game)
	subnet := ""
	if snap.Subnet != "" {
		subnet = "虚拟网段  " + snap.Subnet
	}
	if snap.PCAPPath != "" {
		if subnet != "" {
			subnet += "   ·   "
		}
		subnet += fmt.Sprintf("PCAP %d 包", snap.PCAPPackets)
		if snap.PCAPDropped != 0 {
			subnet += fmt.Sprintf("（丢 %d）", snap.PCAPDropped)
		}
	}
	u.setText(u.hSubnet, subnet)
}

func (u *uiWindow) refreshLog() {
	focus, _, _ := procGetFocus.Call()
	if focus == uintptr(u.hLog) {
		return
	}
	text, seq := u.s.logSnapshot(u.lastLogSeq)
	if seq != u.lastLogSeq {
		u.lastLogSeq = seq
		setWindowText(u.hLog, text)
		end, _, _ := procGetWindowTextLengthW.Call(uintptr(u.hLog))
		procSendMessageW.Call(uintptr(u.hLog), emSetSel, end, end)
		procSendMessageW.Call(uintptr(u.hLog), emScrollCaret, 0, 0)
	}
}

func (u *uiWindow) setText(handle syscall.Handle, text string) {
	if previous, ok := u.text[handle]; !ok || previous != text {
		u.text[handle] = text
		setWindowText(handle, text)
	}
}

func (u *uiWindow) setRelayHistory(history []string) {
	current := windowText(u.hRelay)
	procSendMessageW.Call(uintptr(u.hRelay), cbResetContent, 0, 0)
	for _, address := range history {
		text, err := syscall.UTF16PtrFromString(address)
		if err == nil {
			procSendMessageW.Call(uintptr(u.hRelay), cbAddString, 0, uintptr(unsafe.Pointer(text)))
		}
	}
	setWindowText(u.hRelay, current)
}

func (u *uiWindow) refreshRelayHistory() {
	u.setRelayHistory(loadConfig().RelayHistory)
}

func (u *uiWindow) enable(handle syscall.Handle, enabled bool) {
	if previous, ok := u.enabled[handle]; !ok || previous != enabled {
		u.enabled[handle] = enabled
		enableWindow(handle, enabled)
	}
}

func (u *uiWindow) showFeedback(text string) {
	u.feedback, u.feedbackEnd = text, time.Now().Add(5*time.Second)
	u.refresh()
}

func (u *uiWindow) handleKey(message msgW) bool {
	if message.Message != wmKeyDown || u.closing {
		return false
	}
	if message.WParam == vkEscape && u.state == stateConnecting {
		u.onCommand(idLeave)
		return true
	}
	if message.WParam == vkReturn && u.canConnect() {
		switch message.HWnd {
		case u.hCode:
			u.connect(true)
			return true
		case u.hRelay, u.hName:
			procSetFocus.Call(uintptr(u.hCreate))
			return true
		}
	}
	return false
}

func (u *uiWindow) connect(join bool) {
	if !u.canConnect() {
		return
	}
	if err := u.s.Connect(join, windowText(u.hRelay), windowText(u.hName), u.joinDraft); err != nil {
		u.showFeedback(err.Error())
		return
	}
	u.refresh()
	if u.state == stateConnecting {
		procSetFocus.Call(uintptr(u.hLeave))
	}
}

func (u *uiWindow) onCommand(id uint32) {
	if u.closing {
		return
	}
	switch id {
	case idCreate:
		u.connect(false)
	case idRoomAction:
		if u.state == stateJoined && u.activeCode != "" {
			if err := copyToClipboard(u.hwnd, u.activeCode); err != nil {
				u.showFeedback("复制失败。请在房间码框内手动复制。")
			} else {
				u.showFeedback("房间码已复制。")
			}
		} else {
			u.connect(true)
		}
	case idLeave:
		u.s.Disconnect()
	case idDiagnostics:
		u.runDiagnostics()
	case idSpeedTest:
		u.showSpeedWindow()
	case idP2PToggle:
		checked, _, _ := procSendMessageW.Call(uintptr(u.hP2P), bmGetCheck, 0, 0)
		if err := u.s.SetP2PEnabled(checked == 1); err != nil {
			u.showFeedback(err.Error())
		}
	case idP2PFirewall:
		snap := u.s.Snapshot()
		if err := u.s.RetryP2PFirewall(snap.P2P); err != nil {
			u.showFeedback(err.Error())
		} else {
			u.showFeedback("正在为本机 UDP 端口和已验证对端同步 P2P 防火墙规则。")
		}
	case idLog:
		u.logOpen = !u.logOpen
		label, command := "查看日志", uintptr(swHide)
		if u.logOpen {
			label, command = "收起日志", swShow
		}
		u.setText(u.hToggleLog, label)
		procShowWindow.Call(uintptr(u.hLog), command)
		u.layout()
	case idExport:
		u.exportLog()
	case idCaptureFolder:
		dir := captureDirectory()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			u.showFeedback("无法创建抓包目录，请检查本地磁盘权限。")
		} else if err := openFolder(dir); err != nil {
			u.showFeedback("打开抓包目录失败: " + err.Error())
		} else {
			u.showFeedback("本地抓包目录已打开。")
		}
	}
	u.refresh()
}

func (u *uiWindow) runDiagnostics() {
	if u.diagBusy || u.state == stateConnecting || u.state == stateLeaving {
		return
	}
	u.diagBusy = true
	relay := windowText(u.hRelay)
	go func() {
		summary, _, path, ok := collectDiagnostics(relay, u.s.logLocation())
		u.post(func() {
			u.diagBusy = false
			if u.closing {
				return
			}
			u.refresh()
			flags := uint32(mbOK | mbIconInformation)
			if !ok {
				flags = mbOK | mbIconError
			}
			msgBox(u.hwnd, "诊断结果", summary+"\n\n完整报告已保存到\n"+path, flags)
		})
	}()
}

func (u *uiWindow) exportLog() {
	if u.exportBusy {
		return
	}
	defaultName := fmt.Sprintf("civ6relay-%s.log", time.Now().Format("20060102-150405"))
	path, ok := saveFileDialog(u.hwnd, defaultName)
	if !ok || path == "" {
		return
	}
	u.exportBusy = true
	go func() {
		text, _ := u.s.logSnapshot(-1)
		err := writeClientFile(path, []byte("\uFEFF"+text))
		u.post(func() {
			u.exportBusy = false
			if err != nil {
				u.s.logError("导出日志失败: %v", err)
				u.showFeedback("导出失败。请检查保存目录的写入权限。")
			} else {
				u.showFeedback("运行日志已导出。")
			}
		})
	}()
}

func (u *uiWindow) requestRefresh() {
	if !u.stopped.Load() && u.refreshQueued.CompareAndSwap(false, true) {
		procPostMessageW.Call(uintptr(u.hwnd), wmAppRefresh, 0, 0)
	}
}

func (u *uiWindow) post(fn func()) {
	if u.stopped.Load() {
		return
	}
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
	case wmGetMinMax:
		setMinimumWindowSize(lparam)
		return 0
	case wmSize:
		if u != nil && u.hList != 0 && wparam != 1 {
			u.layout()
		}
	case wmCtlColorStatic:
		if u != nil {
			if brush := u.staticColor(wparam, syscall.Handle(lparam)); brush != 0 {
				return brush
			}
		}
	case wmAppRefresh, wmTimer:
		if u != nil {
			u.refreshQueued.Store(false)
			u.refresh()
		}
		return 0
	case wmCommand:
		if u != nil {
			id, event := uint32(wparam)&0xffff, uint32(wparam)>>16
			if id == idRoomCode && event == enChange && !u.editingCode && !u.codeLocked {
				u.joinDraft = windowText(u.hCode)
				u.updateRoomControls(u.state, u.activeCode)
			} else if event == bnClicked {
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
		if u != nil && !u.closing {
			u.closing = true
			var speedDone <-chan struct{}
			if w := u.speed; w != nil {
				w.stop()
				procShowWindow.Call(uintptr(w.hwnd), swHide)
				speedDone = w.done
			}
			u.refresh()
			go func() {
				if speedDone != nil {
					<-speedDone
				}
				u.s.Shutdown()
				u.post(func() { procDestroyWindow.Call(hwnd) })
			}()
		}
		return 0
	case wmDestroy:
		if u != nil {
			u.stopped.Store(true)
		}
		procKillTimer.Call(hwnd, timerRefresh)
		procPostQuitMessage.Call(0)
		return 0
	}
	result, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return result
}
