//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"civ6relay/internal/playtest"
	"civ6relay/internal/speedtest"
)

const (
	speedClassName   = "Civ6RelaySpeedWnd"
	speedWindowStyle = wsOverlapped | wsCaption | wsSysMenu | wsClipChildren
	idSpeedStart     = 200
	idSpeedStop      = 201
	idPlayMode       = 202
	idBandwidthMode  = 203
	wsGroup          = 0x00020000
	bsAutoRadio      = 0x00000009
)

type testMode uint8

const (
	playMode testMode = iota
	bandwidthMode
)

type testDisplay struct {
	status   string
	progress uintptr
	failed   bool
}

type testLabel func(text string, font syscall.Handle, style uint32, x, y, width, height int) syscall.Handle

type speedColumn struct {
	rate, summary, volume syscall.Handle
}

type speedWindow struct {
	owner *uiWindow
	hwnd  syscall.Handle
	relay string

	target, status, progress, hint, cap, startButton, stopButton syscall.Handle
	modes                                                        [2]syscall.Handle
	panels                                                       [2][]syscall.Handle
	displays                                                     [2]testDisplay
	mode                                                         testMode
	play                                                         playPanel
	directions                                                   [2]speedColumn
	latency                                                      [3][3]syscall.Handle

	cancel   context.CancelFunc
	done     chan struct{}
	stopping bool
}

func (u *uiWindow) speedAddress() (string, error) {
	host, port, err := parseRelayAddress(windowText(u.hRelay))
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func (u *uiWindow) showSpeedWindow() {
	if u.speed != nil && u.speed.cancel != nil {
		u.speed.show()
		return
	}
	address, err := u.speedAddress()
	if err != nil {
		u.showFeedback(err.Error())
		return
	}
	if u.speed == nil {
		u.speed, err = u.createSpeedWindow()
		if err != nil {
			u.showFeedback(err.Error())
			return
		}
	}
	if u.speed.relay != address {
		u.speed.selectMode(playMode)
		u.speed.start(address)
	}
	u.speed.show()
}

func (u *uiWindow) createSpeedWindow() (*speedWindow, error) {
	const clientWidth, clientHeight = 644, 456
	bounds := rect{Right: px(clientWidth), Bottom: px(clientHeight)}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&bounds)), speedWindowStyle, 0, 0)
	width, height := bounds.Right-bounds.Left, bounds.Bottom-bounds.Top
	var parent, area rect
	procGetWindowRect.Call(uintptr(u.hwnd), uintptr(unsafe.Pointer(&parent)))
	procSystemParametersInfo.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&area)), 0)
	left := max(area.Left, min(area.Right-width, (parent.Left+parent.Right-width)/2))
	top := max(area.Top, min(area.Bottom-height, (parent.Top+parent.Bottom-height)/2))

	class, _ := syscall.UTF16PtrFromString(speedClassName)
	title, _ := syscall.UTF16PtrFromString("网络测试")
	instance, _, _ := procGetModuleHandleW.Call(0)
	handle, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)),
		speedWindowStyle, uintptr(left), uintptr(top), uintptr(width), uintptr(height), uintptr(u.hwnd), 0, instance, 0)
	if handle == 0 {
		return nil, fmt.Errorf("创建网络测试窗口失败: %v", err)
	}
	window := &speedWindow{owner: u, hwnd: syscall.Handle(handle)}

	position := func(control syscall.Handle, x, y, wide, tall int) {
		procMoveWindow.Call(uintptr(control), uintptr(px(x)), uintptr(px(y)), uintptr(px(wide)), uintptr(px(tall)), 1)
	}
	label := func(text string, font syscall.Handle, style uint32, x, y, wide, tall int) syscall.Handle {
		control := createChild(window.hwnd, 0, "STATIC", text, style|ssNoPrefix, 0)
		setFont(control, font)
		position(control, x, y, wide, tall)
		return control
	}
	button := func(text string, id uint32, style uint32, x, y, wide, tall int) syscall.Handle {
		control := createChild(window.hwnd, 0, "BUTTON", text, style, id)
		setFont(control, u.fontUI)
		position(control, x, y, wide, tall)
		return control
	}
	panelLabel := func(mode testMode) testLabel {
		return func(text string, font syscall.Handle, style uint32, x, y, width, height int) syscall.Handle {
			control := label(text, font, style, x, y, width, height)
			window.panels[mode] = append(window.panels[mode], control)
			return control
		}
	}

	window.modes[playMode] = button("联机模拟", idPlayMode, wsGroup|wsTabStop|bsAutoRadio, 22, 12, 106, 26)
	window.modes[bandwidthMode] = button("带宽压测", idBandwidthMode, bsAutoRadio, 142, 12, 106, 26)
	window.target = label("", u.fontUI, ssEndEllipsis, 22, 47, 600, 20)
	window.status = label("", u.fontStrong, 0, 22, 72, 600, 26)
	window.progress = createChild(window.hwnd, 0, "msctls_progress32", "", 0, 0)
	position(window.progress, 22, 106, 600, 8)
	procSendMessageW.Call(uintptr(window.progress), pbmSetRange32, 0, 1000)
	window.play = createPlayPanel(panelLabel(playMode), u)
	window.createBandwidthPanel(panelLabel(bandwidthMode))
	label("", u.fontUI, ssEtchedHorz, 22, 397, 600, 2)
	window.hint = label("", u.fontUI, 0, 22, 419, 326, 24)
	window.startButton = button("", idSpeedStart, wsGroup|wsTabStop, 382, 410, 122, 34)
	window.stopButton = button("停止", idSpeedStop, wsTabStop, 516, 410, 106, 34)
	return window, nil
}

func (window *speedWindow) createBandwidthPanel(label testLabel) {
	u := window.owner
	for index, title := range []string{"下行", "上行"} {
		left := 22 + index*310
		label(title, u.fontStrong, 0, left, 133, 290, 22)
		window.directions[index] = speedColumn{
			rate:    label("", u.fontTitle, 0, left, 158, 290, 37),
			summary: label("", u.fontUI, 0, left, 199, 290, 20),
			volume:  label("", u.fontUI, 0, left, 222, 290, 20),
		}
	}
	label("", u.fontUI, ssEtchedHorz, 22, 254, 600, 2)
	label("中转往返", u.fontStrong, 0, 22, 268, 132, 20)
	for column, heading := range []string{"RTT 中位数", "RTT P95", "RTT 抖动"} {
		left := 166 + column*152
		label(heading, u.fontStrong, 0, left, 268, 144, 20)
		for row := range window.latency {
			window.latency[row][column] = label("—", u.fontUI, 0, left, 294+row*22, 144, 20)
		}
	}
	for row, heading := range []string{"空载", "下行负载", "上行负载"} {
		label(heading, u.fontUI, 0, 22, 294+row*22, 132, 20)
	}
	window.cap = label("", u.fontUI, 0, 22, 366, 600, 20)
}

func (window *speedWindow) owns(handle syscall.Handle) bool {
	if handle == window.hwnd {
		return true
	}
	child, _, _ := procIsChild.Call(uintptr(window.hwnd), uintptr(handle))
	return child != 0
}

func (window *speedWindow) show() {
	procShowWindow.Call(uintptr(window.hwnd), swShowNormal)
	procSetForegroundWindow.Call(uintptr(window.hwnd))
	switch {
	case window.cancel == nil:
		procSetFocus.Call(uintptr(window.startButton))
	case !window.stopping:
		procSetFocus.Call(uintptr(window.stopButton))
	}
}

func (window *speedWindow) selectMode(mode testMode) {
	if window.cancel != nil {
		return
	}
	window.mode = mode
	for index, radio := range window.modes {
		show, check := uintptr(swHide), uintptr(0)
		if testMode(index) == mode {
			show, check = swShow, 1
		}
		procSendMessageW.Call(uintptr(radio), bmSetCheck, check, 0)
		for _, control := range window.panels[index] {
			procShowWindow.Call(uintptr(control), show)
		}
	}
	startLabel, hint := "重新模拟", "同机双端 · 合成流量"
	if mode == bandwidthMode {
		startLabel, hint = "开始压测", "压测丢包不代表联机丢包"
	}
	window.owner.setText(window.startButton, startLabel)
	window.owner.setText(window.hint, hint)
	window.renderDisplay()
	procRedrawWindow.Call(uintptr(window.hwnd), 0, 0, rdwInvalidate|rdwErase|rdwAllChildren)
}

func (window *speedWindow) display(status string, progress uintptr, failed bool) {
	window.displays[window.mode] = testDisplay{status: status, progress: progress, failed: failed}
	window.renderDisplay()
}

func (window *speedWindow) renderDisplay() {
	current := window.displays[window.mode]
	window.owner.setText(window.status, current.status)
	procSendMessageW.Call(uintptr(window.progress), pbmSetPos, current.progress, 0)
}

func (window *speedWindow) start(address string) {
	if window.cancel != nil {
		return
	}
	u := window.owner
	if window.relay != address {
		window.relay = address
		u.setText(window.target, "中转  "+address)
		window.displays = [2]testDisplay{{status: "尚未开始模拟"}, {status: "尚未开始压测"}}
		window.play.render(u, playtest.Result{})
		window.bandwidthMetrics(speedtest.Result{})
	}
	mode := window.mode
	if mode == playMode {
		window.play.render(u, playtest.Result{})
		window.display("正在创建临时房间…", 0, false)
	} else {
		window.bandwidthMetrics(speedtest.Result{})
		window.display("正在连接中转服…", 0, false)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	window.cancel, window.done, window.stopping = cancel, done, false
	for _, radio := range window.modes {
		u.enable(radio, false)
	}
	u.enable(window.startButton, false)
	u.enable(window.stopButton, true)
	post := func(final bool, fn func()) {
		u.post(func() {
			if !u.closing && window.done == done && (final || !window.stopping) {
				fn()
			}
		})
	}
	go func() {
		defer close(done)
		if mode == playMode {
			result, err := playtest.Run(ctx, address, func(update playtest.Result) {
				post(false, func() { window.updatePlay(update) })
			})
			post(true, func() { window.finishPlay(result, err) })
			return
		}
		result, err := speedtest.Run(ctx, address, speedtest.Options{}, func(update speedtest.Result) {
			post(false, func() { window.updateBandwidth(update) })
		})
		post(true, func() { window.finishBandwidth(result, err) })
	}()
}

func (window *speedWindow) stop() {
	if window.cancel == nil || window.stopping {
		return
	}
	window.stopping = true
	window.cancel()
	window.display("正在停止…", window.displays[window.mode].progress, false)
	window.owner.enable(window.stopButton, false)
}

func (window *speedWindow) updateBandwidth(result speedtest.Result) {
	window.bandwidthMetrics(result)
	phase := [...]string{"空载延迟", "下行压测", "上行压测"}[result.Phase]
	progress := result.Elapsed.Seconds() / result.PhaseDuration.Seconds()
	window.display(fmt.Sprintf("%s   %.1f / %.1f 秒", phase, result.Elapsed.Seconds(), result.PhaseDuration.Seconds()), uintptr(1000*progress), false)
}

func (window *speedWindow) finishBandwidth(result speedtest.Result, err error) {
	window.bandwidthMetrics(result)
	text := "压测完成"
	switch {
	case errors.Is(err, speedtest.ErrBusy):
		text = "中转服正在压测或刚刚结束，稍等几秒再试。"
	case errors.Is(err, speedtest.ErrUnavailable):
		text = "中转服尚未启用带宽压测。"
	case errors.Is(err, speedtest.ErrNoReply):
		text = "中转服没有响应。请检查地址、UDP 端口和服务端版本。"
	case errors.Is(err, speedtest.ErrInterrupted):
		text = "中转服停止响应，已采集的数据保留在窗口内。"
	case err != nil:
		text = "压测失败。请查看运行日志。"
	}
	window.finish(text, err)
}

func (window *speedWindow) finish(text string, err error) {
	window.cancel()
	window.cancel = nil
	u := window.owner
	progress := window.displays[window.mode].progress
	stopped := window.stopping || errors.Is(err, context.Canceled)
	if stopped {
		text = "已停止，保留本次结果。"
	} else if err == nil {
		progress = 1000
	} else {
		u.s.logError("网络测试失败，中转 %s: %v", window.relay, err)
	}
	window.display(text, progress, err != nil && !stopped)
	for _, radio := range window.modes {
		u.enable(radio, true)
	}
	u.enable(window.startButton, true)
	focus, _, _ := procGetFocus.Call()
	if window.owns(syscall.Handle(focus)) {
		procSetFocus.Call(uintptr(window.startButton))
	}
	u.enable(window.stopButton, false)
}

func (window *speedWindow) bandwidthMetrics(result speedtest.Result) {
	u := window.owner
	capText := "UDP 测速会占用带宽"
	if result.MaxMbps > 0 {
		capText = fmt.Sprintf("测速上限  %d Mbps", result.MaxMbps)
	}
	u.setText(window.cap, capText)

	for index, transfer := range []speedtest.Transfer{result.Download, result.Upload} {
		column := window.directions[index]
		rate, summary, volume := "— Mbps", "平均 —   峰值 —", "接收 —   丢包 —"
		if transfer.Sent > 0 || transfer.Final {
			rate = fmt.Sprintf("%.2f Mbps", transfer.CurrentMbps)
			summary = fmt.Sprintf("平均 %.2f   峰值 %.2f Mbps", transfer.AverageMbps, transfer.PeakMbps)
			volume = fmt.Sprintf("接收 %.2f MB   丢包 —", float64(transfer.Bytes())/1e6)
			if transfer.Sent > 0 {
				loss := "丢包约"
				if transfer.Final {
					loss = "丢包"
				}
				volume = fmt.Sprintf("接收 %.2f MB   %s %.2f%%", float64(transfer.Bytes())/1e6, loss, transfer.Loss())
			}
			if transfer.Final {
				rate = fmt.Sprintf("%.2f Mbps", transfer.AverageMbps)
				summary = fmt.Sprintf("平均速率   峰值 %.2f Mbps", transfer.PeakMbps)
			}
		}
		u.setText(column.rate, rate)
		u.setText(column.summary, summary)
		u.setText(column.volume, volume)
	}
	for row, latency := range result.Latency {
		for column, value := range []time.Duration{latency.Median, latency.P95, latency.Jitter} {
			text := "—"
			if latency.Samples > 0 && (column != 2 || latency.Samples > 1) {
				text = fmt.Sprintf("%.2f ms", float64(value)/float64(time.Millisecond))
			}
			u.setText(window.latency[row][column], text)
		}
	}
}

func speedWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	if u := appUI; u != nil && u.speed != nil && hwnd == uintptr(u.speed.hwnd) {
		window := u.speed
		switch uint32(msg) {
		case wmCtlColorStatic:
			color := u.palette.text
			switch syscall.Handle(lparam) {
			case window.status:
				if window.displays[window.mode].failed {
					color = u.palette.error
				}
			case window.target, window.hint:
				color = u.palette.muted
			}
			procSetTextColor.Call(wparam, color)
			procSetBkMode.Call(wparam, transparent)
			return uintptr(u.bgBrush)
		case wmCommand:
			id, event := uint32(wparam)&0xffff, uint32(wparam)>>16
			if event == bnClicked && !u.closing {
				switch id {
				case idPlayMode:
					window.selectMode(playMode)
				case idBandwidthMode:
					window.selectMode(bandwidthMode)
				case idSpeedStart:
					address, err := u.speedAddress()
					if err != nil {
						window.display(err.Error(), window.displays[window.mode].progress, true)
					} else {
						window.start(address)
						window.show()
					}
				case idSpeedStop:
					window.stop()
				}
			}
			return 0
		case wmClose:
			window.stop()
			procShowWindow.Call(hwnd, swHide)
			return 0
		}
	}
	result, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return result
}
