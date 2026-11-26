//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"civ6relay/internal/playtest"
	"civ6relay/internal/speedtest"

	"github.com/go-gui-org/go-gui/gui"
)

type testMode uint8

const (
	playMode testMode = iota
	bandwidthMode
)

const noSample = "—"

type testDisplay struct {
	status   string
	progress float32
	failed   bool
}

type netTestState struct {
	mode      testMode
	relay     string
	displays  [2]testDisplay
	playRes   playtest.Result
	bwRes     speedtest.Result
	cancel    context.CancelFunc
	done      chan struct{}
	stopping  bool
}

func (nt *netTestState) init() {
	nt.mode = playMode
	nt.displays = [2]testDisplay{
		{status: "尚未开始联机模拟"},
		{status: "尚未开始带宽压测"},
	}
}

func (nt *netTestState) running() bool {
	return nt.cancel != nil
}

func (nt *netTestState) syncTarget(relayInput string) {
	if nt.running() {
		return
	}
	host, port, err := parseRelayAddress(relayInput)
	if err != nil {
		return
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	if nt.relay != addr && nt.relay != "" {
		nt.relay = addr
		nt.displays = [2]testDisplay{
			{status: "目标中转已变更，可重新开始模拟"},
			{status: "目标中转已变更，可重新开始压测"},
		}
		nt.playRes = playtest.Result{}
		nt.bwRes = speedtest.Result{}
	}
}

func (nt *netTestState) start(app *clientApp, w *gui.Window) {
	if nt.running() || app.closing {
		return
	}
	address, err := app.relayAddressForTest()
	if err != nil {
		nt.setDisplay(err.Error(), nt.displays[nt.mode].progress, true)
		app.toast(w, gui.ToastWarning, "中转地址无效", err.Error())
		w.InvalidateLayout()
		return
	}
	if nt.relay != address {
		nt.relay = address
		nt.displays = [2]testDisplay{
			{status: "尚未开始联机模拟"},
			{status: "尚未开始带宽压测"},
		}
		nt.playRes = playtest.Result{}
		nt.bwRes = speedtest.Result{}
	}
	mode := nt.mode
	if mode == playMode {
		nt.playRes = playtest.Result{}
		nt.setDisplay("正在创建临时房间…", 0, false)
	} else {
		nt.bwRes = speedtest.Result{}
		nt.setDisplay("正在连接中转服…", 0, false)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	nt.cancel, nt.done, nt.stopping = cancel, done, false
	w.InvalidateLayout()

	post := func(final bool, fn func(*clientApp, *gui.Window)) {
		w.QueueCommand(func(vw *gui.Window) {
			a := gui.State[clientApp](vw)
			if !a.closing && a.netTest.done == done && (final || !a.netTest.stopping) {
				fn(a, vw)
				vw.InvalidateLayout()
			}
		})
	}

	go func() {
		defer close(done)
		if mode == playMode {
			result, runErr := playtest.Run(ctx, address, func(update playtest.Result) {
				post(false, func(a *clientApp, _ *gui.Window) {
					a.netTest.updatePlay(update)
				})
			})
			post(true, func(a *clientApp, vw *gui.Window) {
				a.netTest.finishPlay(a, vw, result, runErr)
			})
			return
		}
		result, runErr := speedtest.Run(ctx, address, speedtest.Options{}, func(update speedtest.Result) {
			post(false, func(a *clientApp, _ *gui.Window) {
				a.netTest.updateBandwidth(update)
			})
		})
		post(true, func(a *clientApp, vw *gui.Window) {
			a.netTest.finishBandwidth(a, vw, result, runErr)
		})
	}()
}

func (nt *netTestState) stop(w *gui.Window) {
	if nt.cancel == nil || nt.stopping {
		return
	}
	nt.stopping = true
	nt.cancel()
	nt.setDisplay("正在停止…", nt.displays[nt.mode].progress, false)
	if w != nil {
		w.InvalidateLayout()
	}
}

func (nt *netTestState) stopForClose() <-chan struct{} {
	if nt.cancel != nil && !nt.stopping {
		nt.stopping = true
		nt.cancel()
	}
	return nt.done
}

func (nt *netTestState) setDisplay(status string, progress float32, failed bool) {
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	nt.displays[nt.mode] = testDisplay{
		status:   status,
		progress: progress,
		failed:   failed,
	}
}

func (nt *netTestState) updatePlay(result playtest.Result) {
	nt.playRes = result
	var status string
	var progress float32
	switch result.Phase {
	case playtest.Creating:
		status = "正在创建临时房间…"
	case playtest.Joining:
		status = "测试玩家正在入房…"
	case playtest.Discovering:
		status = "正在广播发现房间…"
	case playtest.Playing:
		status = fmt.Sprintf("正在模拟对局  %.1f / %.0f 秒", result.PhaseElapsed.Seconds(), result.PhaseDuration.Seconds())
		if result.PhaseDuration > 0 {
			progress = float32(result.PhaseElapsed.Seconds() / result.PhaseDuration.Seconds())
		}
	case playtest.Settling:
		status = "正在结算在途数据报…"
		progress = 1
	}
	nt.setDisplay(status, progress, false)
}

func (nt *netTestState) finishPlay(app *clientApp, w *gui.Window, result playtest.Result, err error) {
	nt.playRes = result
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
	nt.finish(app, w, text, err)
}

func (nt *netTestState) updateBandwidth(result speedtest.Result) {
	nt.bwRes = result
	phases := [...]string{"空载延迟", "下行压测", "上行压测"}
	phase := "压测中"
	if int(result.Phase) >= 0 && int(result.Phase) < len(phases) {
		phase = phases[result.Phase]
	}
	var progress float32
	if result.PhaseDuration > 0 {
		progress = float32(result.Elapsed.Seconds() / result.PhaseDuration.Seconds())
	}
	nt.setDisplay(fmt.Sprintf("%s  %.1f / %.1f 秒", phase, result.Elapsed.Seconds(), result.PhaseDuration.Seconds()), progress, false)
}

func (nt *netTestState) finishBandwidth(app *clientApp, w *gui.Window, result speedtest.Result, err error) {
	nt.bwRes = result
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
	nt.finish(app, w, text, err)
}

func (nt *netTestState) finish(app *clientApp, w *gui.Window, text string, err error) {
	if nt.cancel != nil {
		nt.cancel()
		nt.cancel = nil
	}
	progress := nt.displays[nt.mode].progress
	stopped := nt.stopping || errors.Is(err, context.Canceled)
	nt.stopping = false
	if stopped {
		text = "已停止，保留本次结果。"
	} else if err == nil {
		progress = 1
	} else if app.s != nil {
		app.s.logError("网络测试失败，中转 %s: %v", nt.relay, err)
	}
	nt.setDisplay(text, progress, err != nil && !stopped)
	if !stopped && err == nil {
		app.toast(w, gui.ToastSuccess, text, "测试数据已更新。")
	}
}

func latencyText(d time.Duration) string {
	if d <= 0 {
		return noSample
	}
	return formatMilliseconds(d)
}

func formatMilliseconds(d time.Duration) string {
	return fmt.Sprintf("%.2f ms", float64(d)/float64(time.Millisecond))
}

func (a *clientApp) netTestView(w *gui.Window) gui.View {
	t := gui.CurrentTheme()
	nt := &a.netTest
	running := nt.running()
	modeVal := "play"
	if nt.mode == bandwidthMode {
		modeVal = "bandwidth"
	}

	targetAddr := nt.relay
	if targetAddr == "" {
		if addr, err := a.relayAddressForTest(); err == nil {
			targetAddr = addr
		} else {
			targetAddr = "未配置有效地址"
		}
	}

	header := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		VAlign:     gui.VAlignMiddle,
		Content: []gui.View{
			gui.Column(gui.ContainerCfg{
				Sizing:     gui.FillFit,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingPx(2),
				Content: []gui.View{
					gui.Text(gui.TextCfg{Text: "网络测试", TextStyle: t.TextStyleTitle}),
					gui.Text(gui.TextCfg{Text: "目标中转  " + targetAddr, TextStyle: t.TextStyleSecondary}),
				},
			}),
			gui.SegmentedControl(gui.SegmentedControlCfg{
				ID:       "nettest-mode",
				Value:    modeVal,
				Disabled: running,
				Options: []gui.SegmentOption{
					gui.NewSegmentOption("联机模拟", "play"),
					gui.NewSegmentOption("带宽压测", "bandwidth"),
				},
				OnSelect: func(val string, ctx gui.EventCtx) {
					ctx.Consume()
					app := gui.State[clientApp](ctx.Window)
					if app.netTest.running() {
						return
					}
					if val == "bandwidth" {
						app.netTest.mode = bandwidthMode
					} else {
						app.netTest.mode = playMode
					}
					ctx.Window.InvalidateLayout()
				},
			}),
		},
	})

	disp := nt.displays[nt.mode]
	statusStyle := t.TextStyleBody.Bold()
	if disp.failed {
		statusStyle = t.TextStyleError.Bold()
	} else if !running && disp.progress >= 1 {
		statusStyle = t.TextStyleSuccess.Bold()
	}

	startLabel, hint := "开始模拟", "同机双端 · 文明 VI 实测流量模型（1180 B 同步突发 + 14 B 高频回执）"
	if nt.mode == playMode && (nt.playRes.Create > 0 || nt.playRes.Discovery.Attempts > 0) {
		startLabel = "重新模拟"
	}
	if nt.mode == bandwidthMode {
		startLabel, hint = "开始压测", "压测丢包不代表联机丢包"
		if nt.bwRes.Download.Sent > 0 || nt.bwRes.Upload.Sent > 0 {
			startLabel = "重新压测"
		}
	}

	canStart := !running && !a.closing && a.snap.State != stateConnecting && a.snap.State != stateLeaving

	controlCard := cardView("nettest-control-card", gui.FillFit,
		gui.Row(gui.ContainerCfg{
			Sizing:     gui.FillFit,
			Padding:    gui.NoPadding,
			SizeBorder: gui.NoBorder,
			Spacing:    gui.SpacingMedium,
			VAlign:     gui.VAlignMiddle,
			Content: []gui.View{
				gui.Column(gui.ContainerCfg{
					Sizing:     gui.FillFit,
					Padding:    gui.NoPadding,
					SizeBorder: gui.NoBorder,
					Spacing:    gui.SpacingPx(2),
					Content: []gui.View{
						gui.Text(gui.TextCfg{
							Text:      disp.status,
							TextStyle: statusStyle,
							Mode:      gui.TextModeWrap,
						}),
						gui.Text(gui.TextCfg{
							Text:      hint,
							TextStyle: t.TextStyleCaption,
						}),
					},
				}),
				gui.Button(gui.ButtonCfg{
					ID:       "nettest-start",
					Label:    startLabel,
					Variant:  gui.ButtonPrimary,
					Disabled: !canStart,
					Loading:  running && !nt.stopping,
					MinWidth: 96,
					OnClick: func(ctx gui.EventCtx) {
						ctx.Consume()
						app := gui.State[clientApp](ctx.Window)
						app.netTest.start(app, ctx.Window)
					},
				}),
				gui.Button(gui.ButtonCfg{
					ID:       "nettest-stop",
					Label:    "停止",
					Variant:  gui.ButtonDanger,
					Disabled: !running || nt.stopping,
					MinWidth: 76,
					OnClick: func(ctx gui.EventCtx) {
						ctx.Consume()
						gui.State[clientApp](ctx.Window).netTest.stop(ctx.Window)
					},
				}),
			},
		}),
		gui.ProgressBar(gui.ProgressBarCfg{
			ID:       "nettest-progress",
			Percent:  disp.progress,
			Height:   8,
			Sizing:   gui.FillFit,
			TextShow: true,
		}),
	)

	var detailView gui.View
	if nt.mode == playMode {
		detailView = a.playModeResultsView()
	} else {
		detailView = a.bandwidthModeResultsView()
	}

	return gui.Column(gui.ContainerCfg{
		ID:         "nettest-page",
		Sizing:     gui.FillFill,
		Scrollable: true,
		ScrollMode: gui.ScrollVerticalOnly,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		Content: []gui.View{
			header,
			controlCard,
			detailView,
		},
	})
}

func (a *clientApp) playModeResultsView() gui.View {
	t := gui.CurrentTheme()
	res := a.netTest.playRes

	discovery := noSample
	if res.Discovery.Attempts > 0 {
		discovery = fmt.Sprintf("广播 %d/%d · 应答 %d/%d",
			res.Discovery.Broadcasts, res.Discovery.Attempts,
			res.Discovery.Replies, res.Discovery.Attempts)
	}

	kpiRow := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		Content: []gui.View{
			kpiTileView("创建中转房间", latencyText(res.Create), t.TextStyleCode.Bold()),
			kpiTileView("加入中转房间", latencyText(res.Join), t.TextStyleCode.Bold()),
			kpiTileView("房间发现", discovery, t.TextStyleBody.Bold()),
		},
	})

	deliveryRow := func(label string, stats playtest.Delivery) []gui.View {
		sent, received, loss := noSample, noSample, noSample
		if stats.Sent > 0 {
			sent, received = fmt.Sprint(stats.Sent), fmt.Sprint(stats.Received)
		}
		if stats.Settled > 0 {
			loss = fmt.Sprintf("%d / %d (%.1f%%)", stats.Missing, stats.Settled, 100*float64(stats.Missing)/float64(stats.Settled))
		}
		return []gui.View{
			gui.Text(gui.TextCfg{Text: label, TextStyle: t.TextStyleBody}),
			gui.Text(gui.TextCfg{Text: sent, TextStyle: t.TextStyleCodeSmall}),
			gui.Text(gui.TextCfg{Text: received, TextStyle: t.TextStyleCodeSmall}),
			gui.Text(gui.TextCfg{Text: loss, TextStyle: t.TextStyleCodeSmall}),
		}
	}

	deliveryTable := flexTableView("play-delivery-table", gui.FillFit, false,
		[]tableColSpec{
			{title: "游戏数据报", width: 0},
			{title: "已发送", width: 110},
			{title: "已接收", width: 110},
			{title: "丢包 / 已结算", width: 170},
		},
		[][]gui.View{
			deliveryRow("房主 → 玩家", res.HostToGuest),
			deliveryRow("玩家 → 房主", res.GuestToHost),
		},
	)

	latencyRow := func(label string, stats playtest.Latency) []gui.View {
		jitter, replies := noSample, noSample
		if stats.Replies > 1 {
			jitter = formatMilliseconds(stats.Jitter)
		}
		if stats.Probes > 0 {
			replies = fmt.Sprintf("%d / %d", stats.Replies, stats.Probes)
		}
		return []gui.View{
			gui.Text(gui.TextCfg{Text: label, TextStyle: t.TextStyleBody}),
			gui.Text(gui.TextCfg{Text: latencyText(stats.Median), TextStyle: t.TextStyleCodeSmall}),
			gui.Text(gui.TextCfg{Text: latencyText(stats.P95), TextStyle: t.TextStyleCodeSmall}),
			gui.Text(gui.TextCfg{Text: jitter, TextStyle: t.TextStyleCodeSmall}),
			gui.Text(gui.TextCfg{Text: replies, TextStyle: t.TextStyleCodeSmall}),
		}
	}

	latencyTable := flexTableView("play-latency-table", gui.FillFit, false,
		[]tableColSpec{
			{title: "游戏消息往返", width: 0},
			{title: "中位数", width: 110},
			{title: "P95", width: 110},
			{title: "抖动", width: 110},
			{title: "应答 / 探测", width: 120},
		},
		[][]gui.View{
			latencyRow("常态交互", res.SteadyRTT),
			latencyRow("同步期间", res.SyncRTT),
		},
	)

	fragments, compression, failures := noSample, noSample, noSample
	if res.Fragmented.Sent > 0 {
		fragments = fmt.Sprintf("%d / %d", res.Fragmented.Received, res.Fragmented.Sent)
	}
	if ratio := res.CompressionRatio(); ratio > 0 {
		compression = fmt.Sprintf("%.1f%% (%.2f → %.2f MB)",
			ratio*100, float64(res.HostRawBytes)/1e6, float64(res.HostWireBytes)/1e6)
	}
	if res.Discovery.Attempts > 0 || res.SendFailures > 0 {
		failures = fmt.Sprint(res.SendFailures)
	}

	footerRow := gui.Row(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		Content: []gui.View{
			kpiTileView("超长分片接收", fragments, t.TextStyleCode.Bold()),
			kpiTileView("压缩线速占比", compression, t.TextStyleCode.Bold()),
			kpiTileView("本地发送失败", failures, t.TextStyleCode.Bold()),
		},
	})

	return gui.Column(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		Content: []gui.View{
			kpiRow,
			cardView("play-delivery-card", gui.FillFit, deliveryTable),
			cardView("play-latency-card", gui.FillFit, latencyTable),
			footerRow,
		},
	})
}

func (a *clientApp) bandwidthModeResultsView() gui.View {
	t := gui.CurrentTheme()
	res := a.netTest.bwRes

	transferCard := func(id, title string, tr speedtest.Transfer) gui.View {
		rate, summary, volume := "— Mbps", "平均 —   峰值 —", "接收 —   丢包 —"
		if tr.Sent > 0 || tr.Final {
			rate = fmt.Sprintf("%.2f Mbps", tr.CurrentMbps)
			summary = fmt.Sprintf("平均 %.2f · 峰值 %.2f Mbps", tr.AverageMbps, tr.PeakMbps)
			volume = fmt.Sprintf("接收 %.2f MB · 丢包 —", float64(tr.Bytes())/1e6)
			if tr.Sent > 0 {
				loss := "丢包约"
				if tr.Final {
					loss = "丢包"
				}
				volume = fmt.Sprintf("接收 %.2f MB · %s %.2f%%", float64(tr.Bytes())/1e6, loss, tr.Loss())
			}
			if tr.Final {
				rate = fmt.Sprintf("%.2f Mbps", tr.AverageMbps)
				summary = fmt.Sprintf("平均速率 · 峰值 %.2f Mbps", tr.PeakMbps)
			}
		}
		return cardView(id, gui.FillFit,
			gui.Text(gui.TextCfg{Text: title, TextStyle: t.TextStyleLabel}),
			gui.Text(gui.TextCfg{Text: rate, TextStyle: t.TextStyleDisplay}),
			gui.Text(gui.TextCfg{Text: summary, TextStyle: t.TextStyleSecondary}),
			gui.Text(gui.TextCfg{Text: volume, TextStyle: t.TextStyleCaption}),
		)
	}

	bwLatRow := func(label string, lat speedtest.Latency) []gui.View {
		vals := []time.Duration{lat.Median, lat.P95, lat.Jitter}
		cells := []gui.View{gui.Text(gui.TextCfg{Text: label, TextStyle: t.TextStyleBody})}
		for col, v := range vals {
			txt := noSample
			if lat.Samples > 0 && (col != 2 || lat.Samples > 1) {
				txt = formatMilliseconds(v)
			}
			cells = append(cells, gui.Text(gui.TextCfg{Text: txt, TextStyle: t.TextStyleCodeSmall}))
		}
		return cells
	}

	latTable := flexTableView("bw-latency-table", gui.FillFit, false,
		[]tableColSpec{
			{title: "中转往返", width: 0},
			{title: "RTT 中位数", width: 140},
			{title: "RTT P95", width: 140},
			{title: "RTT 抖动", width: 140},
		},
		[][]gui.View{
			bwLatRow("空载", res.Latency[0]),
			bwLatRow("下行负载", res.Latency[1]),
			bwLatRow("上行负载", res.Latency[2]),
		},
	)

	capText := "UDP 测速会占用带宽，测试时建议暂停下载或直播。"
	if res.MaxMbps > 0 {
		capText = fmt.Sprintf("服务端测速上限 %d Mbps", res.MaxMbps)
	}

	return gui.Column(gui.ContainerCfg{
		Sizing:     gui.FillFit,
		Padding:    gui.NoPadding,
		SizeBorder: gui.NoBorder,
		Spacing:    gui.SpacingMedium,
		Content: []gui.View{
			gui.Row(gui.ContainerCfg{
				Sizing:     gui.FillFit,
				Padding:    gui.NoPadding,
				SizeBorder: gui.NoBorder,
				Spacing:    gui.SpacingMedium,
				Content: []gui.View{
					transferCard("bw-download-card", "下行带宽", res.Download),
					transferCard("bw-upload-card", "上行带宽", res.Upload),
				},
			}),
			cardView("bw-latency-card", gui.FillFit,
				latTable,
				gui.Text(gui.TextCfg{Text: capText, TextStyle: t.TextStyleCaption}),
			),
		},
	})
}
