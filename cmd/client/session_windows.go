//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"civ6relay/internal/pcap"
	"civ6relay/internal/protocol"
	"civ6relay/internal/tunnel"
	"civ6relay/internal/win"
)

type logEntry struct {
	Time    string
	Level   string
	Message string
}

type memberView struct {
	ID      int
	Name    string
	Host    bool
	Online  bool
	Virtual string
	Path    tunnel.PeerPath
	Latency time.Duration
}

type connectionState uint8

const (
	stateIdle connectionState = iota
	stateConnecting
	stateJoined
	stateLeaving
	stateFailed
)

type sessionSnapshot struct {
	State            connectionState
	Detail           string
	RoomCode         string
	MemberID         int
	Host             bool
	P2P              tunnel.P2PStatus
	P2PFirewallError string
	Subnet           string
	Tunnel           tunnel.Metrics
	Bridge           win.BridgeStats
	BridgeRunning    bool
	Capture          bool
	PCAPPath         string
	PCAPPackets      uint64
	PCAPDropped      uint64
	PCAPError        string
	Members          []memberView
	CivRunning       bool
}

type session struct {
	mu      sync.Mutex
	capture bool

	client   *tunnel.Client
	bridge   *win.Bridge
	recorder *pcap.Recorder
	subnet   *net.IPNet
	state    connectionState
	detail   string
	cancel   context.CancelFunc
	changed  func()

	logSeq  int64
	logs    []logEntry
	logPath string
	logFile *os.File

	pcapPath    string
	pcapPackets uint64
	pcapDropped uint64
	pcapError   string

	quitting         bool
	civRunning       bool
	stop             chan struct{}
	work             sync.WaitGroup
	p2pFirewallReady chan struct{}
	p2pFirewallKey   string
	p2pFirewallBusy  bool
	p2pFirewallError string
}

func newSession(capture bool) *session {
	s := &session{
		capture:          capture,
		stop:             make(chan struct{}),
		p2pFirewallReady: make(chan struct{}),
	}
	s.openLog()
	if s.logPath == "" {
		s.logAt("WARN", "无法创建日志文件，请检查 %LOCALAPPDATA% 权限")
	} else {
		s.logAt("INFO", "日志文件: "+s.logPath)
	}
	s.work.Add(1)
	go s.captureRetentionLoop()
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		defer close(s.p2pFirewallReady)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := win.EnsureP2PFirewallRule(ctx, 0, nil); err != nil {
			s.logWarn("清理上次运行留下的 P2P 防火墙规则失败: %v", err)
		}
	}()
	return s
}

func (s *session) captureRetentionLoop() {
	defer s.work.Done()
	s.pruneCaptures()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.pruneCaptures()
		}
	}
}

func (s *session) pruneCaptures() {
	s.mu.Lock()
	recorder := s.recorder
	s.mu.Unlock()
	activePath := ""
	if recorder != nil {
		activePath = recorder.Path()
	}
	if err := pcap.PruneExpired(captureDirectory(), time.Now(), activePath); err != nil {
		s.logWarn("清理过期 PCAP 文件失败: %v", err)
	}
}

func (s *session) openLog() {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "Civ6Relay", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "client-"+time.Now().Format("20060102")+".log")
	needBOM := true
	if info, err := os.Stat(path); err == nil {
		needBOM = info.Size() == 0
		missingBOM := false
		if info.Size() >= 3 {
			if f, err := os.Open(path); err == nil {
				var head [3]byte
				_, _ = f.Read(head[:])
				missingBOM = !(head[0] == 0xEF && head[1] == 0xBB && head[2] == 0xBF)
				_ = f.Close()
			}
		}
		if info.Size() > 5<<20 || missingBOM {
			_ = os.Remove(path + ".1")
			_ = os.Rename(path, path+".1")
			needBOM = true
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	if needBOM {
		_, _ = f.Write([]byte{0xEF, 0xBB, 0xBF})
	}
	s.logFile = f
	s.logPath = path
}

// CloseLog flushes and closes the automatic log file.
func (s *session) CloseLog() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
}

func (s *session) logLocation() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logPath == "" {
		return "未创建（请检查 %LOCALAPPDATA% 权限）"
	}
	return s.logPath
}

// logEnvironment writes startup facts that support needs when diagnosing.
func (s *session) logEnvironment() {
	exe, _ := os.Executable()
	s.logInfo("Civ6RelayClient %s 启动", version)
	s.logInfo("系统: %s, 管理员: %v, 抓包: %v", win.OSVersion(), win.IsAdmin(), s.capture)
	s.logInfo("程序: %s", exe)
	s.logInfo("配置: %s", configPath())
	s.logInfo("日志: %s", s.logLocation())
	if av := win.SecuritySoftware(); len(av) > 0 {
		s.logWarn("检测到安全软件: %s", strings.Join(av, "、"))
	}
	svc := win.QueryWinDivertService()
	s.logInfo("WinDivert 服务: %s", svc.Summary())
	s.logInfo("驱动目录: %s", win.DriverDir())
}

func (s *session) logAt(level, message string) {
	now := time.Now()
	s.mu.Lock()
	s.logSeq++
	entry := logEntry{Time: now.Format("15:04:05"), Level: level, Message: message}
	s.logs = append(s.logs, entry)
	if len(s.logs) > 500 {
		s.logs = s.logs[len(s.logs)-500:]
	}
	if s.logFile != nil {
		text := message
		if strings.Contains(text, "\n") {
			text = strings.ReplaceAll(text, "\n", "\n    ")
		}
		fmt.Fprintf(s.logFile, "%s [%s] %s\n", now.Format("2006-01-02 15:04:05"), level, text)
	}
	s.mu.Unlock()
}

func (s *session) logInfo(format string, args ...any) { s.logAt("INFO", fmt.Sprintf(format, args...)) }
func (s *session) logOK(format string, args ...any)   { s.logAt("OK", fmt.Sprintf(format, args...)) }
func (s *session) logWarn(format string, args ...any) { s.logAt("WARN", fmt.Sprintf(format, args...)) }
func (s *session) logError(format string, args ...any) {
	s.logAt("ERROR", fmt.Sprintf(format, args...))
}

func (s *session) startBackground(changed func()) {
	s.changed = changed
	s.work.Add(2)
	go func() {
		defer s.work.Done()
		s.logEnvironment()
	}()
	go s.watchGame()
}

func (s *session) watchGame() {
	defer s.work.Done()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		running := len(win.FindCiv6Processes()) > 0
		s.mu.Lock()
		changed := running != s.civRunning
		s.civRunning = running
		s.mu.Unlock()
		if changed {
			s.notify()
		}
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

func (s *session) notify() {
	if s.changed != nil {
		s.changed()
	}
}

// Connect validates the inputs and starts an asynchronous connection.
func (s *session) Connect(join bool, relayText, nameText, codeText string) error {
	host, port, err := parseRelayAddress(relayText)
	if err != nil {
		return err
	}
	name, err := protocol.NormalizeName(nameText)
	if err != nil {
		return errors.New("昵称长度必须为 1 到 24 个可打印字符")
	}
	code := ""
	if join {
		code, err = protocol.NormalizeRoomCode(codeText)
		if err != nil {
			return errors.New("请输入 10 位房间码")
		}
	}
	s.mu.Lock()
	if s.quitting || (s.state != stateIdle && s.state != stateFailed) {
		s.mu.Unlock()
		return errors.New("当前已在连接或房间中")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.state = stateConnecting
	s.detail = "正在联系中转服务器…"
	s.pcapPath, s.pcapError = "", ""
	s.pcapPackets, s.pcapDropped = 0, 0
	s.work.Add(1)
	s.mu.Unlock()
	stored := loadConfig()
	cfg := Config{RelayHost: host, RelayPort: port, DisplayName: name, RelayHistory: stored.RelayHistory}
	go s.connectWorker(ctx, cancel, cfg, code)
	s.notify()
	return nil
}

func (s *session) connectWorker(ctx context.Context, cancel context.CancelFunc, cfg Config, code string) {
	defer s.work.Done()
	defer cancel()
	if err := saveConfig(cfg); err != nil {
		s.logWarn("保存配置失败: %v", err)
	}
	s.logInfo("正在连接中转 %s", win.FormatRelay(cfg.RelayHost, cfg.RelayPort))
	if err := s.openRoom(ctx, cfg, code); err != nil {
		s.mu.Lock()
		s.cancel = nil
		message := describeTunnelError(err)
		if ctx.Err() != nil {
			s.state, s.detail = stateIdle, "连接已取消。"
		} else {
			s.state, s.detail = stateFailed, message
		}
		cancelled := ctx.Err() != nil
		s.mu.Unlock()
		if cancelled {
			s.logInfo("连接已取消")
		} else {
			s.logAt("ERROR", message)
		}
		s.notify()
	}
}

func (s *session) openRoom(ctx context.Context, cfg Config, code string) error {
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, 5*time.Second)
	defer cancelHandshake()
	client, err := tunnel.NewContext(handshakeCtx, cfg.RelayHost, cfg.RelayPort)
	if err != nil {
		return err
	}
	var bridge *win.Bridge
	var recorder *pcap.Recorder
	transferred := false
	defer func() {
		if !transferred {
			s.finishCapture(recorder, stopConnection(client, bridge, recorder))
		}
	}()
	client.SetEventHandler(func(ev tunnel.Event) { s.onTunnelEvent(client, ev) })
	var welcome *protocol.WelcomeMessage
	if code == "" {
		welcome, err = client.CreateRoomContext(handshakeCtx, cfg.DisplayName)
	} else {
		welcome, err = client.JoinRoomContext(handshakeCtx, code, cfg.DisplayName)
	}
	cancelHandshake()
	if err != nil {
		return err
	}
	if code == "" {
		cfg.RelayHistory = addRelayHistory(cfg.RelayHistory, win.FormatRelay(cfg.RelayHost, cfg.RelayPort))
		if err := saveConfig(cfg); err != nil {
			s.logWarn("保存建房中转地址历史失败: %v", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var subnet *net.IPNet
	detail := ""
	captureError := ""
	if s.capture {
		s.mu.Lock()
		if s.state == stateConnecting {
			s.detail = "已进入房间，正在启动游戏转发…"
		}
		s.mu.Unlock()
		s.notify()
		var route win.RouteInfo
		subnet, err = win.SelectVirtualSubnet(client.RelayAddr().IP)
		if err != nil {
			return err
		}
		route, err = win.RouteTo(win.VirtualMemberIP(subnet, 2))
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		driverDir, err := win.EnsureDriver()
		if err != nil {
			return fmt.Errorf("释放 WinDivert 驱动失败: %w\n驱动目录: %s\n日志: %s", err, win.DriverDir(), s.logLocation())
		}
		status := win.InspectDriver(driverDir)
		s.logInfo("驱动目录: %s", driverDir)
		s.logInfo("WinDivert64.dll: %s", status.DLL.Describe())
		s.logInfo("WinDivert64.sys: %s", status.SYS.Describe())
		if !status.Complete {
			return errors.New(s.driverIncompleteText(status))
		}
		if err := win.EnsureFirewallRule(ctx, subnet); err != nil {
			s.logWarn("%v", err)
			detail = "防火墙规则配置失败。游戏无法加入时，请查看运行日志或运行诊断。"
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if welcome.Host {
			recorder, err = pcap.NewRecorder(captureDirectory(), welcome.RoomCode)
			if err != nil {
				captureError = err.Error()
				s.logWarn("无法创建本地 PCAP 记录: %v", err)
				recorder = nil
			} else {
				s.logOK("本房间游戏 UDP 已开始记录: %s", recorder.Path())
			}
		}
		var recordPacket func([]byte)
		if recorder != nil {
			recordPacket = recorder.Record
		}
		bridge, err = win.StartBridge(client, subnet, route, driverDir, func(level, message string) {
			s.logAt(upperLevel(level), message)
		}, recordPacket)
		if err != nil {
			return errors.New(s.driverFailureText(err, driverDir))
		}
		s.logOK("本地出口 %s，虚拟网段 %s", route.LocalIP, subnet)
	}
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		return ctx.Err()
	}
	if !client.Connected() {
		s.mu.Unlock()
		return errors.New("连接完成前，房间已经关闭")
	}
	s.client, s.bridge, s.recorder, s.subnet = client, bridge, recorder, subnet
	s.pcapError = captureError
	if recorder != nil {
		s.pcapPath = recorder.Path()
	}
	s.state, s.detail, s.cancel = stateJoined, detail, nil
	transferred = true
	s.mu.Unlock()
	s.logOK("已进入房间 %s，成员 ID %d", welcome.RoomCode, welcome.MemberID)
	s.notify()
	return nil
}

func upperLevel(level string) string {
	switch level {
	case "info":
		return "INFO"
	case "warn":
		return "WARN"
	case "error":
		return "ERROR"
	}
	return "INFO"
}

func (s *session) SetP2PEnabled(enabled bool) error {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return errors.New("当前没有房间。")
	}
	return client.SetP2PEnabled(enabled)
}

func (s *session) RetryP2PFirewall(status tunnel.P2PStatus) error {
	if !status.Enabled || len(status.Endpoints) == 0 {
		return errors.New("房主开启 P2P 且检测到对端后，才能配置直连规则。")
	}
	s.mu.Lock()
	if s.quitting {
		s.mu.Unlock()
		return errors.New("程序正在退出。")
	}
	if s.p2pFirewallBusy {
		s.mu.Unlock()
		return errors.New("P2P 防火墙规则正在配置。")
	}
	s.p2pFirewallKey = ""
	s.p2pFirewallError = ""
	s.mu.Unlock()
	s.SyncP2PFirewall(status)
	return nil
}

func (s *session) SyncP2PFirewall(status tunnel.P2PStatus) {
	addresses := make([]netip.Addr, 0, len(status.Endpoints))
	for _, endpoint := range status.Endpoints {
		ip := endpoint.Addr().Unmap()
		if ip.Is4() && ip.IsGlobalUnicast() {
			addresses = append(addresses, ip)
		}
	}
	slices.SortFunc(addresses, func(a, b netip.Addr) int { return a.Compare(b) })
	addresses = slices.Compact(addresses)
	key := ""
	port := uint16(0)
	if len(addresses) != 0 {
		port = status.LocalPort
		key = fmt.Sprintf("%d|", port)
		for _, ip := range addresses {
			key += ip.String() + ","
		}
	}
	s.mu.Lock()
	if s.quitting || s.p2pFirewallBusy || s.p2pFirewallKey == key {
		s.mu.Unlock()
		return
	}
	s.p2pFirewallKey = key
	s.p2pFirewallBusy = true
	s.work.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.work.Done()
		select {
		case <-s.p2pFirewallReady:
		case <-s.stop:
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		err := win.EnsureP2PFirewallRule(ctx, port, addresses)
		s.mu.Lock()
		s.p2pFirewallBusy = false
		s.p2pFirewallError = ""
		if err != nil && key != "" {
			s.p2pFirewallError = err.Error()
		}
		s.mu.Unlock()
		if err != nil && key != "" {
			s.logWarn("P2P 防火墙规则同步失败；软件仍尝试 UDP 直连，失败时自动走中转: %v", err)
		} else if key != "" {
			s.logInfo("P2P 防火墙规则同步完成：UDP 本地端口 %d，对端 %d 个", port, len(addresses))
		}
		s.notify()
	}()
}

// Disconnect cancels an in-flight connection or leaves the active room.
func (s *session) Disconnect() {
	s.mu.Lock()
	if s.quitting {
		s.mu.Unlock()
		return
	}
	switch s.state {
	case stateConnecting:
		s.state, s.detail = stateLeaving, "正在取消连接…"
		s.cancel()
	case stateJoined:
		s.teardownLocked("已离开房间。")
	}
	s.mu.Unlock()
	s.notify()
}

func (s *session) teardownLocked(detail string) {
	client, bridge, recorder := s.client, s.bridge, s.recorder
	s.client, s.bridge, s.recorder, s.subnet = nil, nil, nil, nil
	if recorder != nil {
		s.pcapPath = recorder.Path()
	}
	s.state, s.detail = stateLeaving, "正在离开房间…"
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		s.finishCapture(recorder, stopConnection(client, bridge, recorder))
		s.logInfo("%s", detail)
		s.mu.Lock()
		s.state, s.detail = stateIdle, detail
		s.mu.Unlock()
		s.notify()
	}()
}

func stopConnection(client *tunnel.Client, bridge *win.Bridge, recorder *pcap.Recorder) error {
	if client != nil {
		client.Disconnect()
	}
	bridge.Stop()
	if recorder != nil {
		return recorder.Close()
	}
	return nil
}

func (s *session) finishCapture(recorder *pcap.Recorder, closeErr error) {
	if recorder == nil {
		return
	}
	written, dropped := recorder.Stats()
	s.mu.Lock()
	s.pcapPath = recorder.Path()
	s.pcapPackets, s.pcapDropped = written, dropped
	if closeErr != nil {
		s.pcapError = closeErr.Error()
	}
	s.mu.Unlock()
	if dropped != 0 {
		s.logWarn("本地 PCAP 队列繁忙，丢弃 %d 个数据包", dropped)
	}
	if closeErr != nil {
		s.logWarn("关闭本地 PCAP 记录时发生错误: %v", closeErr)
	}
}

func captureDirectory() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "Civ6Relay", "captures")
}

func (s *session) onTunnelEvent(client *tunnel.Client, ev tunnel.Event) {
	s.mu.Lock()
	if s.quitting || s.client != client {
		s.mu.Unlock()
		return
	}
	if ev.Type == tunnel.EventRoomClosed {
		reason := "房间已关闭。"
		switch ev.Reason {
		case "host_left":
			reason = "房主已离开，房间已关闭。"
		case "host_timeout":
			reason = "房主长时间离线，房间已关闭。"
		}
		s.teardownLocked(reason)
	}
	s.mu.Unlock()
	if ev.Type == tunnel.EventTransportError && ev.Err != nil {
		s.logError("网络错误: %v", ev.Err)
	}
	s.notify()
}

// Shutdown stops everything; used when the window closes.
func (s *session) Shutdown() {
	s.mu.Lock()
	if s.quitting {
		s.mu.Unlock()
		return
	}
	s.quitting = true
	close(s.stop)
	if s.cancel != nil {
		s.cancel()
	}
	client, bridge, recorder := s.client, s.bridge, s.recorder
	s.client, s.bridge, s.recorder, s.subnet = nil, nil, nil, nil
	if recorder != nil {
		s.pcapPath = recorder.Path()
	}
	s.mu.Unlock()
	s.logInfo("正在退出，停止会话…")
	s.finishCapture(recorder, stopConnection(client, bridge, recorder))
	s.work.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	if err := win.EnsureP2PFirewallRule(ctx, 0, nil); err != nil {
		s.logWarn("清理 P2P 防火墙规则失败: %v", err)
	}
	cancel()
}

// Snapshot returns a consistent copy of the current state for the UI.
func (s *session) Snapshot() sessionSnapshot {
	s.mu.Lock()
	snap := sessionSnapshot{
		State: s.state, Detail: s.detail, Capture: s.capture, CivRunning: s.civRunning,
		P2PFirewallError: s.p2pFirewallError,
		PCAPPath:         s.pcapPath, PCAPPackets: s.pcapPackets, PCAPDropped: s.pcapDropped, PCAPError: s.pcapError,
	}
	subnet := s.subnet
	client := s.client
	bridge := s.bridge
	recorder := s.recorder
	s.mu.Unlock()

	if subnet != nil {
		snap.Subnet = subnet.String()
	}
	if bridge != nil {
		snap.BridgeRunning = bridge.Running()
		snap.Bridge = bridge.Stats()
	}
	if recorder != nil {
		snap.PCAPPath = recorder.Path()
		snap.PCAPPackets, snap.PCAPDropped = recorder.Stats()
		if err := recorder.Error(); err != nil {
			snap.PCAPError = err.Error()
		}
	}
	if client == nil {
		return snap
	}
	snap.Tunnel = client.Metrics()
	snap.MemberID = int(client.MemberID())
	snap.Host = client.IsHost()
	snap.P2P = client.P2PStatus()
	snap.RoomCode = client.RoomCode()
	members := client.Peers()
	snap.Members = make([]memberView, 0, len(members))
	for _, p := range members {
		virtual := "—"
		if subnet != nil {
			if ip := win.VirtualMemberIP(subnet, p.ID); ip != nil {
				virtual = ip.String()
			}
		}
		path := tunnel.ViaRelay
		if int(p.ID) < len(snap.P2P.Peers) {
			path = snap.P2P.Peers[p.ID]
		}
		latency := snap.P2P.RelayRTT[p.ID]
		if path == tunnel.Direct {
			latency = snap.P2P.PeerRTT[p.ID]
		}
		snap.Members = append(snap.Members, memberView{
			ID:      int(p.ID),
			Name:    p.Name,
			Host:    p.Host,
			Online:  p.Online,
			Virtual: virtual,
			Path:    path,
			Latency: latency,
		})
	}
	return snap
}

func (s *session) logEntries(knownSeq int64) ([]logEntry, int64, bool) {
	s.mu.Lock()
	if s.logSeq == knownSeq {
		s.mu.Unlock()
		return nil, knownSeq, false
	}
	entries, seq := append([]logEntry(nil), s.logs...), s.logSeq
	s.mu.Unlock()
	return entries, seq, true
}

func (s *session) logSnapshot(knownSeq int64) (string, int64) {
	s.mu.Lock()
	if s.logSeq == knownSeq {
		s.mu.Unlock()
		return "", knownSeq
	}
	entries, seq := append([]logEntry(nil), s.logs...), s.logSeq
	s.mu.Unlock()
	var out strings.Builder
	for _, entry := range entries {
		message := strings.ReplaceAll(strings.ReplaceAll(entry.Message, "\r\n", "\n"), "\n", "\r\n    ")
		fmt.Fprintf(&out, "%s [%s] %s\r\n", entry.Time, entry.Level, message)
	}
	return out.String(), seq
}

func describeTunnelError(err error) string {
	var te *tunnel.Error
	if errors.As(err, &te) {
		switch te.Code {
		case "room_not_found":
			return "房间不存在或已经关闭"
		case "room_full":
			return "房间已满（最多 8 人）"
		case "host_offline":
			return "房主暂时离线"
		case "timeout":
			return "中转服务器没有响应。请检查地址、端口和服务端的 UDP 放行规则。"
		case "roster_too_large":
			return "房间的昵称总长度超出协议限制。请缩短昵称后重试。"
		case "resolve_failed":
			return te.Message
		case "invalid_request":
			return "参数不正确: " + te.Message
		case "closed":
			return "客户端已关闭"
		}
		return te.Message
	}
	return err.Error()
}
