//go:build windows

package main

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"civ6relay/internal/playtest"
)

type playPanel struct {
	room      [3]syscall.Handle
	delivery  [2][3]syscall.Handle
	latency   [2][4]syscall.Handle
	fragments syscall.Handle
	failures  syscall.Handle
}

const noSample = "—"

func createPlayPanel(label testLabel, ui *uiWindow) playPanel {
	var panel playPanel
	for index, heading := range []string{"创建中转房间", "加入中转房间", "房间发现"} {
		left := 22 + 200*index
		label(heading, ui.fontUI, 0, left, 132, 190, 20)
		panel.room[index] = label(noSample, ui.fontStrong, 0, left, 156, 190, 22)
	}
	label("", ui.fontUI, ssEtchedHorz, 22, 186, 600, 2)
	label("游戏数据报", ui.fontStrong, 0, 22, 198, 130, 20)
	for column, heading := range []string{"已发送", "已接收", "丢包 / 已结算"} {
		left, width := 166+145*column, 137
		if column == 2 {
			width = 166
		}
		label(heading, ui.fontStrong, 0, left, 198, width, 20)
		for row := range panel.delivery {
			panel.delivery[row][column] = label(noSample, ui.fontUI, 0, left, 221+row*22, width, 20)
		}
	}
	label("房主 → 玩家", ui.fontUI, 0, 22, 221, 130, 20)
	label("玩家 → 房主", ui.fontUI, 0, 22, 243, 130, 20)
	label("", ui.fontUI, ssEtchedHorz, 22, 270, 600, 2)
	label("游戏消息往返", ui.fontStrong, 0, 22, 282, 130, 20)
	for column, heading := range []string{"中位数", "P95", "抖动", "应答 / 探测"} {
		left := 166 + 115*column
		label(heading, ui.fontStrong, 0, left, 282, 111, 20)
		for row := range panel.latency {
			panel.latency[row][column] = label(noSample, ui.fontUI, 0, left, 305+row*22, 111, 20)
		}
	}
	label("常态交互", ui.fontUI, 0, 22, 305, 130, 20)
	label("同步期间", ui.fontUI, 0, 22, 327, 130, 20)
	panel.fragments = label("", ui.fontUI, 0, 22, 365, 285, 20)
	panel.failures = label("", ui.fontUI, 0, 332, 365, 290, 20)
	return panel
}

func (panel *playPanel) render(ui *uiWindow, result playtest.Result) {
	ui.setText(panel.room[0], latencyText(result.Create))
	ui.setText(panel.room[1], latencyText(result.Join))
	discovery := noSample
	if result.Discovery.Attempts > 0 {
		discovery = fmt.Sprintf("广播 %d/%d   应答 %d/%d", result.Discovery.Broadcasts, result.Discovery.Attempts, result.Discovery.Replies, result.Discovery.Attempts)
	}
	ui.setText(panel.room[2], discovery)

	for row, stats := range []playtest.Delivery{result.HostToGuest, result.GuestToHost} {
		sent, received, loss := noSample, noSample, noSample
		if stats.Sent > 0 {
			sent, received = fmt.Sprint(stats.Sent), fmt.Sprint(stats.Received)
		}
		if stats.Settled > 0 {
			loss = fmt.Sprintf("%d / %d  (%.1f%%)", stats.Missing, stats.Settled, 100*float64(stats.Missing)/float64(stats.Settled))
		}
		for column, text := range []string{sent, received, loss} {
			ui.setText(panel.delivery[row][column], text)
		}
	}
	for row, stats := range []playtest.Latency{result.SteadyRTT, result.SyncRTT} {
		jitter, replies := noSample, noSample
		if stats.Replies > 1 {
			jitter = formatMilliseconds(stats.Jitter)
		}
		if stats.Probes > 0 {
			replies = fmt.Sprintf("%d / %d", stats.Replies, stats.Probes)
		}
		for column, text := range []string{latencyText(stats.Median), latencyText(stats.P95), jitter, replies} {
			ui.setText(panel.latency[row][column], text)
		}
	}
	fragments, failures := noSample, noSample
	if result.Fragmented.Sent > 0 {
		fragments = fmt.Sprintf("%d / %d", result.Fragmented.Received, result.Fragmented.Sent)
	}
	if result.Discovery.Attempts > 0 || result.SendFailures > 0 {
		failures = fmt.Sprint(result.SendFailures)
	}
	ui.setText(panel.fragments, "同步大包接收  "+fragments)
	ui.setText(panel.failures, "本地发送失败  "+failures)
}

func latencyText(duration time.Duration) string {
	if duration <= 0 {
		return noSample
	}
	return formatMilliseconds(duration)
}

func formatMilliseconds(duration time.Duration) string {
	return fmt.Sprintf("%.2f ms", float64(duration)/float64(time.Millisecond))
}

func (window *speedWindow) updatePlay(result playtest.Result) {
	window.play.render(window.owner, result)
	var status string
	var progress uintptr
	switch result.Phase {
	case playtest.Creating:
		status = "正在创建临时房间…"
	case playtest.Joining:
		status = "测试玩家正在入房…"
	case playtest.Discovering:
		status = "正在广播发现房间…"
	case playtest.Playing:
		status = fmt.Sprintf("正在模拟对局  %.1f / %.0f 秒", result.PhaseElapsed.Seconds(), result.PhaseDuration.Seconds())
		progress = uintptr(1000 * result.PhaseElapsed / result.PhaseDuration)
	case playtest.Settling:
		status = "正在结算在途数据报…"
		progress = 1000
	}
	window.display(status, progress, false)
}

func (window *speedWindow) finishPlay(result playtest.Result, err error) {
	window.play.render(window.owner, result)
	text := "联机模拟完成"
	switch {
	case errors.Is(err, playtest.ErrRoomClosed):
		text = "临时房间已断开，保留本次结果。"
	case err != nil && result.Phase == playtest.Creating:
		text = "无法创建临时房间。请检查中转地址、UDP 端口或运行日志。"
	case err != nil && result.Phase == playtest.Joining:
		text = "临时房间已创建，但测试玩家未能加入。请查看运行日志。"
	case err != nil:
		text = "联机模拟中断，保留本次结果。"
	case result.Discovery.Replies == 0:
		text = "联机模拟完成，未收到房间发现应答。"
	case result.SteadyRTT.Replies+result.SyncRTT.Replies == 0:
		text = "联机模拟完成，未收到游戏消息应答。"
	}
	window.finish(text, err)
}
