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
	"sync/atomic"
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
	MaxMembers       int
	P2P              tunnel.P2PStatus
	P2PFirewallError string
	Subnet           string
	Tunnel           tunnel.Metrics
	Bridge           win.BridgeStats
	BridgeRunning    bool
	Capture          bool
	HostPCAP         bool
	HostP2P          bool
	PCAPActive       bool
	PCAPPath         string
	PCAPPackets      uint64
	PCAPDropped      uint64
	PCAPError        string
	Members          []memberView
	CivRunning       bool
}

type session struct {
	mu             sync.Mutex
	capture        bool
	hostPCAP       bool
	hostP2P        bool
	maxMembers     int
	p2pAutoApplied *tunnel.Client
	activeRecorder atomic.Pointer[pcap.Recorder]

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
	peerPaths        [protocol.LastMemberID + 1]tunnel.PeerPath
	peerPathsKnown   [protocol.LastMemberID + 1]bool
}

func newSession(capture, hostPCAP, hostP2P bool, maxMembers int) *session {
	s := &session{
		capture:          capture,
		hostPCAP:         hostPCAP,
		hostP2P:          hostP2P,
		maxMembers:       maxMembers,
		stop:             make(chan struct{}),
		p2pFirewallReady: make(chan struct{}),
	}
	s.openLog()
	if s.logPath == "" {
		s.logAt("WARN", "无法创建日志文件")
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

// logEnvironment writes a compact startup line for troubleshooting.
func (s *session) logEnvironment() {
	s.logInfo("启动 %s (%s, 管理员=%v)", version, win.OSVersion(), win.IsAdmin())
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
func (s *session) Connect(join bool, relayText, nameText, codeText, maxMembersText string) error {
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
	maxMembers := s.maxMembers
	if !join {
		parsed, err := parseMaxMembers(maxMembersText)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		maxMembers = parsed
		s.maxMembers = maxMembers
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.state = stateConnecting
	s.detail = ""
	s.pcapPath, s.pcapError = "", ""
	s.pcapPackets, s.pcapDropped = 0, 0
	hostPCAP := s.hostPCAP
	hostP2P := s.hostP2P
	s.work.Add(1)
	s.mu.Unlock()
	stored := loadConfig()
	cfg := Config{RelayHost: host, RelayPort: port, DisplayName: name, RelayHistory: stored.RelayHistory, HostPCAP: hostPCAP, HostP2P: hostP2P, MaxMembers: maxMembers}
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
	s.logInfo("连接 %s", win.FormatRelay(cfg.RelayHost, cfg.RelayPort))
	if err := s.openRoom(ctx, cfg, code); err != nil {
		s.mu.Lock()
		s.cancel = nil
		message := describeTunnelError(err)
		if ctx.Err() != nil {
			s.state, s.detail = stateIdle, ""
		} else {
			s.state, s.detail = stateFailed, message
		}
		cancelled := ctx.Err() != nil
		s.mu.Unlock()
		if !cancelled {
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
			s.activeRecorder.Store(nil)
			s.finishCapture(recorder, stopConnection(client, bridge, recorder))
		}
	}()
	client.SetEventHandler(func(ev tunnel.Event) { s.onTunnelEvent(client, ev) })
	var welcome *protocol.WelcomeMessage
	if code == "" {
		welcome, err = client.CreateRoomContextMaxMembers(handshakeCtx, cfg.DisplayName, cfg.MaxMembers)
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
			s.detail = "启动转发…"
		}
		s.mu.Unlock()
		s.notify()
		var route win.RouteInfo
		var avoidedTUN bool
		subnet, err = win.SelectVirtualSubnet(client.RelayAddr().IP)
		if err != nil {
			return err
		}
		route, avoidedTUN, err = win.SelectLocalRoute(win.VirtualMemberIP(subnet, 2), subnet)
		if err != nil {
			return err
		}
		if avoidedTUN {
			s.logInfo("绕过 TUN，出口 %s", route.LocalIP)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		driverDir, err := win.EnsureDriver()
		if err != nil {
			return fmt.Errorf("释放驱动失败: %w (%s)", err, win.DriverDir())
		}
		status := win.InspectDriver(driverDir)
		if !status.Complete {
			return errors.New(s.driverIncompleteText(status))
		}
		if err := win.EnsureFirewallRule(ctx, subnet); err != nil {
			s.logWarn("%v", err)
			detail = "防火墙规则配置失败"
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		hostPCAP := s.hostPCAP
		s.mu.Unlock()
		if welcome.Host && hostPCAP {
			recorder, err = pcap.NewRecorder(captureDirectory(), welcome.RoomCode)
			if err != nil {
				captureError = err.Error()
				s.logWarn("PCAP 启动失败: %v", err)
				recorder = nil
			} else {
				s.logOK("PCAP: %s", recorder.Path())
			}
		}
		s.activeRecorder.Store(recorder)
		bridge, err = win.StartBridge(client, subnet, route, driverDir, func(level, message string) {
			s.logAt(upperLevel(level), message)
		}, s.recordPacket)
		if err != nil {
			return errors.New(s.driverFailureText(err, driverDir))
		}
		s.logOK("已入房 %s (#%d, 网段 %s, 出口 %s)", welcome.RoomCode, welcome.MemberID, subnet, route.LocalIP)
	}
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		return ctx.Err()
	}
	if !client.Connected() {
		s.mu.Unlock()
		return errors.New("房间已关闭")
	}
	s.client, s.bridge, s.recorder, s.subnet = client, bridge, recorder, subnet
	s.pcapError = captureError
	if recorder != nil {
		s.pcapPath = recorder.Path()
	}
	s.state, s.detail, s.cancel = stateJoined, detail, nil
	s.p2pAutoApplied = nil
	s.peerPaths = [protocol.LastMemberID + 1]tunnel.PeerPath{}
	s.peerPathsKnown = [protocol.LastMemberID + 1]bool{}
	transferred = true
	s.mu.Unlock()
	if !s.capture {
		s.logOK("已入房 %s (#%d)", welcome.RoomCode, welcome.MemberID)
	}
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

func (s *session) updateConfig(apply func(*Config) bool) {
	cfg := loadConfig()
	if !apply(&cfg) {
		return
	}
	if err := saveConfig(cfg); err != nil {
		s.logWarn("保存配置失败: %v", err)
	}
}

// SetP2PEnabled updates the host P2P preference and, when currently hosting an
// active room, applies it to the room immediately.
func (s *session) SetP2PEnabled(enabled bool) error {
	s.mu.Lock()
	if s.quitting {
		s.mu.Unlock()
		return errors.New("程序正在退出。")
	}
	if s.state == stateConnecting || s.state == stateLeaving {
		s.mu.Unlock()
		return errors.New("当前正在切换连接状态，请稍后再试。")
	}
	state := s.state
	client := s.client
	if state == stateJoined && client != nil {
		s.mu.Unlock()
		if err := client.SetP2PEnabled(enabled); err != nil {
			return err
		}
		s.mu.Lock()
		if s.client == client {
			s.hostP2P = enabled
			s.p2pAutoApplied = client
		}
		s.mu.Unlock()
		s.updateConfig(func(cfg *Config) bool {
			if cfg.HostP2P == enabled {
				return false
			}
			cfg.HostP2P = enabled
			return true
		})
		s.notify()
		return nil
	}
	s.hostP2P = enabled
	s.mu.Unlock()

	s.updateConfig(func(cfg *Config) bool {
		if cfg.HostP2P == enabled {
			return false
		}
		cfg.HostP2P = enabled
		return true
	})
	s.notify()
	return nil
}

func (s *session) maybeAutoEnableP2P(client *tunnel.Client, status tunnel.P2PStatus) tunnel.P2PStatus {
	if client == nil || !client.IsHost() || !status.Supported {
		return status
	}
	s.mu.Lock()
	shouldApply := !s.quitting && s.state == stateJoined && s.client == client && s.hostP2P && s.p2pAutoApplied != client
	if shouldApply {
		s.p2pAutoApplied = client
	}
	s.mu.Unlock()
	if !shouldApply || status.Enabled || status.Pending {
		return status
	}
	if err := client.SetP2PEnabled(true); err != nil {
		s.logWarn("自动开启 P2P 失败: %v", err)
		return status
	}
	return client.P2PStatus()
}

func (s *session) recordPacket(raw []byte) {
	if recorder := s.activeRecorder.Load(); recorder != nil {
		recorder.Record(raw)
	}
}

func (s *session) saveHostPCAPConfig(enabled bool) {
	s.updateConfig(func(cfg *Config) bool {
		if cfg.HostPCAP == enabled {
			return false
		}
		cfg.HostPCAP = enabled
		return true
	})
}

// SetHostPCAP updates the host auto-capture preference and starts or stops
// local PCAP recording immediately when currently hosting an active room.
func (s *session) SetHostPCAP(enabled bool) error {
	s.mu.Lock()
	if s.quitting {
		s.mu.Unlock()
		return errors.New("程序正在退出。")
	}
	if s.state == stateConnecting || s.state == stateLeaving {
		s.mu.Unlock()
		return errors.New("当前正在切换连接状态，请稍后再试。")
	}
	if s.state == stateJoined && s.client != nil && !s.client.IsHost() {
		s.mu.Unlock()
		return errors.New("仅房主可以开关房间抓包。")
	}
	s.hostPCAP = enabled
	state := s.state
	client := s.client
	capture := s.capture
	oldRecorder := s.recorder
	if state != stateJoined || client == nil || !capture {
		s.mu.Unlock()
		s.saveHostPCAPConfig(enabled)
		s.notify()
		return nil
	}
	if !enabled {
		s.activeRecorder.Store(nil)
		s.recorder = nil
		if oldRecorder != nil {
			s.pcapPath = oldRecorder.Path()
			s.work.Add(1)
		}
		s.mu.Unlock()
		s.saveHostPCAPConfig(false)
		if oldRecorder != nil {
			go func() {
				defer s.work.Done()
				s.finishCapture(oldRecorder, oldRecorder.Close())
				s.logInfo("停止 PCAP: %s", oldRecorder.Path())
				s.notify()
			}()
		} else {
			s.notify()
		}
		return nil
	}
	if oldRecorder != nil {
		s.activeRecorder.Store(oldRecorder)
		s.mu.Unlock()
		s.saveHostPCAPConfig(true)
		s.notify()
		return nil
	}
	s.pcapError = ""
	roomCode := client.RoomCode()
	s.mu.Unlock()

	s.saveHostPCAPConfig(true)
	recorder, err := pcap.NewRecorder(captureDirectory(), roomCode)

	s.mu.Lock()
	if err != nil {
		if s.state == stateJoined && s.client == client {
			s.pcapError = err.Error()
		}
		s.mu.Unlock()
		s.logWarn("PCAP 启动失败: %v", err)
		s.notify()
		return err
	}
	if s.quitting || s.state != stateJoined || s.client != client || !s.hostPCAP || s.recorder != nil {
		s.mu.Unlock()
		_ = recorder.Close()
		s.notify()
		return nil
	}
	s.recorder = recorder
	s.pcapPath = recorder.Path()
	s.pcapPackets, s.pcapDropped, s.pcapError = 0, 0, ""
	s.activeRecorder.Store(recorder)
	s.mu.Unlock()
	s.logOK("PCAP: %s", recorder.Path())
	s.notify()
	return nil
}

func (s *session) RetryP2PFirewall(status tunnel.P2PStatus) error {
	if !status.Enabled || len(status.Endpoints) == 0 {
		return errors.New("P2P 未就绪")
	}
	s.mu.Lock()
	if s.quitting {
		s.mu.Unlock()
		return errors.New("程序正在退出")
	}
	if s.p2pFirewallBusy {
		s.mu.Unlock()
		return errors.New("正在同步防火墙规则")
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
			s.logWarn("P2P 防火墙同步失败: %v", err)
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
		s.state, s.detail = stateLeaving, ""
		s.cancel()
	case stateJoined:
		s.teardownLocked("")
	}
	s.mu.Unlock()
	s.notify()
}

func (s *session) teardownLocked(detail string) {
	s.activeRecorder.Store(nil)
	client, bridge, recorder := s.client, s.bridge, s.recorder
	s.client, s.bridge, s.recorder, s.subnet = nil, nil, nil, nil
	if recorder != nil {
		s.pcapPath = recorder.Path()
	}
	s.state, s.detail = stateLeaving, ""
	s.peerPaths = [protocol.LastMemberID + 1]tunnel.PeerPath{}
	s.peerPathsKnown = [protocol.LastMemberID + 1]bool{}
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		s.finishCapture(recorder, stopConnection(client, bridge, recorder))
		if detail != "" {
			s.logInfo("%s", detail)
		} else {
			s.logInfo("已离开房间")
		}
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
	if s.recorder == nil || s.recorder == recorder {
		s.pcapPath = recorder.Path()
		s.pcapPackets, s.pcapDropped = written, dropped
		if closeErr != nil {
			s.pcapError = closeErr.Error()
		}
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
		reason := "房间已关闭"
		switch ev.Reason {
		case "host_left":
			reason = "房主已离开"
		case "host_timeout":
			reason = "房主离线超时"
		}
		s.teardownLocked(reason)
	}
	s.mu.Unlock()
	if ev.Type == tunnel.EventTransportError && ev.Err != nil {
		s.logError("网络错误: %v", ev.Err)
	}
	if ev.Type == tunnel.EventP2P && client.IsHost() {
		s.maybeAutoEnableP2P(client, client.P2PStatus())
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
	s.activeRecorder.Store(nil)
	client, bridge, recorder := s.client, s.bridge, s.recorder
	s.client, s.bridge, s.recorder, s.subnet = nil, nil, nil, nil
	if recorder != nil {
		s.pcapPath = recorder.Path()
	}
	s.mu.Unlock()
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
		State: s.state, Detail: s.detail, Capture: s.capture, HostPCAP: s.hostPCAP, HostP2P: s.hostP2P, MaxMembers: s.maxMembers, CivRunning: s.civRunning,
		P2PFirewallError: s.p2pFirewallError,
		PCAPActive:       s.recorder != nil,
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
	snap.MaxMembers = client.MaxMembers()
	snap.P2P = s.maybeAutoEnableP2P(client, client.P2PStatus())
	snap.RoomCode = client.RoomCode()
	members := client.Peers()
	snap.Members = make([]memberView, 0, len(members))
	var pathChanges []string
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
		if int(p.ID) < len(s.peerPaths) && p.ID != client.MemberID() {
			s.mu.Lock()
			known := s.peerPathsKnown[p.ID]
			prev := s.peerPaths[p.ID]
			if !known || prev != path {
				s.peerPathsKnown[p.ID] = true
				s.peerPaths[p.ID] = path
				if path != tunnel.Punching && (known || path != tunnel.ViaRelay) {
					pathChanges = append(pathChanges, formatPeerPathChange(p.ID, p.Name, path, snap.P2P.PeerRTT[p.ID], snap.P2P.RelayRTT[p.ID]))
				}
			}
			s.mu.Unlock()
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
	for _, msg := range pathChanges {
		s.logInfo("%s", msg)
	}
	return snap
}

func formatPeerPathChange(id uint8, name string, path tunnel.PeerPath, directRTT, relayRTT time.Duration) string {
	label := "中转"
	switch path {
	case tunnel.Punching:
		label = "打洞中"
	case tunnel.Direct:
		label = "直连"
	}
	return fmt.Sprintf("#%d %s → %s (直连 %s / 中转 %s)",
		id, name, label, formatOptionalRTT(directRTT), formatOptionalRTT(relayRTT))
}

func formatOptionalRTT(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	return d.Round(time.Millisecond).String()
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
			return "房间不存在或已关闭"
		case "room_full":
			return "房间已满"
		case "host_offline":
			return "房主离线"
		case "timeout":
			return "中转无响应"
		case "roster_too_large":
			return "昵称总长度超限"
		case "resolve_failed":
			return te.Message
		case "invalid_request":
			return "参数错误: " + te.Message
		case "closed":
			return "已关闭"
		}
		return te.Message
	}
	return err.Error()
}
