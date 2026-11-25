//go:build windows

package main

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"civ6relay/internal/protocol"
	"civ6relay/internal/tunnel"
	"civ6relay/internal/win"
)

type logEntry struct {
	Seq     int64
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
}

type civProcessView struct {
	PID  uint32
	Name string
}

type sessionSnapshot struct {
	Status        string
	Connected     bool
	Busy          bool
	Relay         string
	Name          string
	RoomCode      string
	MemberID      int
	IsHost        bool
	Subnet        string
	RTTms         int64
	HasRTT        bool
	Rx            uint64
	Tx            uint64
	BridgeRunning bool
	Capture       bool
	Members       []memberView
	Civ           []civProcessView
	Logs          []logEntry
	LogSeq        int64
	Error         string
	ErrorSeq      int64
}

type session struct {
	mu      sync.Mutex
	capture bool

	client *tunnel.Client
	bridge *win.Bridge
	subnet *net.IPNet

	relayHost string
	relayPort int
	name      string
	roomCode  string
	status    string
	busy      bool

	lastError string
	errSeq    int64
	logSeq    int64
	logs      []logEntry
	members   []protocol.Peer

	quitting    bool
	procCache   []civProcessView
	procChecked time.Time
}

func newSession(capture bool) *session {
	return &session{
		capture:   capture,
		status:    "未连接",
		relayHost: "127.0.0.1",
		relayPort: defaultRelayPort,
	}
}

func (s *session) setConfig(cfg Config) {
	s.mu.Lock()
	s.relayHost = cfg.RelayHost
	s.relayPort = cfg.RelayPort
	s.name = cfg.DisplayName
	s.mu.Unlock()
}

func (s *session) logAt(level, message string) {
	s.mu.Lock()
	s.logSeq++
	entry := logEntry{Seq: s.logSeq, Time: time.Now().Format("15:04:05"), Level: level, Message: message}
	s.logs = append(s.logs, entry)
	if len(s.logs) > 500 {
		s.logs = s.logs[len(s.logs)-500:]
	}
	s.mu.Unlock()
}

func (s *session) logInfo(format string, args ...any) { s.logAt("INFO", fmt.Sprintf(format, args...)) }
func (s *session) logOK(format string, args ...any)   { s.logAt("OK", fmt.Sprintf(format, args...)) }
func (s *session) logWarn(format string, args ...any) { s.logAt("WARN", fmt.Sprintf(format, args...)) }
func (s *session) logError(format string, args ...any) {
	s.logAt("ERROR", fmt.Sprintf(format, args...))
}

func (s *session) setError(message string) {
	s.mu.Lock()
	s.lastError = message
	s.errSeq++
	s.mu.Unlock()
	s.logAt("ERROR", message)
}

func (s *session) isQuitting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quitting
}

// Connect validates the inputs and starts an asynchronous connection.
func (s *session) Connect(mode, relayText, nameText, codeText string) error {
	host, port, err := parseRelayAddress(relayText)
	if err != nil {
		return err
	}
	name, err := protocol.NormalizeName(nameText)
	if err != nil {
		return errors.New("昵称长度必须为 1 到 24 个可打印字符")
	}
	code := ""
	if mode == "join" {
		code, err = protocol.NormalizeRoomCode(codeText)
		if err != nil {
			return errors.New("请输入 10 位房间码")
		}
	} else if mode != "create" {
		return errors.New("未知操作")
	}
	if err := saveConfig(Config{RelayHost: host, RelayPort: port, DisplayName: name}); err != nil {
		s.logWarn("保存配置失败: %v", err)
	}
	s.mu.Lock()
	if s.busy || s.client != nil {
		s.mu.Unlock()
		return errors.New("当前已在连接或房间中")
	}
	s.busy = true
	s.status = "正在连接…"
	s.lastError = ""
	s.relayHost, s.relayPort, s.name = host, port, name
	if mode == "join" {
		s.roomCode = code
	}
	s.mu.Unlock()
	s.logInfo("正在连接中转 %s", win.FormatRelay(host, port))
	go s.connectWorker(mode, host, port, name, code)
	return nil
}

func (s *session) connectWorker(mode, host string, port int, name, code string) {
	client, err := tunnel.New(host, port)
	if err != nil {
		s.failConnect(err)
		return
	}
	client.SetEventHandler(func(ev tunnel.Event) { s.onTunnelEvent(client, ev) })
	var welcome *protocol.WelcomeMessage
	if mode == "create" {
		welcome, err = client.CreateRoom(name, 5*time.Second)
	} else {
		welcome, err = client.JoinRoom(code, name, 5*time.Second)
	}
	if err != nil {
		client.Disconnect()
		s.failConnect(err)
		return
	}
	if s.isQuitting() {
		client.Disconnect()
		return
	}
	s.logOK("已加入房间 %s，成员 ID %d", welcome.RoomCode, welcome.MemberID)

	var route win.RouteInfo
	var subnet *net.IPNet
	var bridge *win.Bridge
	if s.capture {
		route, err = win.RouteTo(client.RelayAddr().IP)
		if err == nil {
			subnet, err = win.SelectVirtualSubnet()
		}
		if err != nil {
			client.Disconnect()
			s.failConnect(err)
			return
		}
		if err := win.EnsureFirewallRule(subnet); err != nil {
			s.logWarn("%v（继续连接）", err)
		} else {
			s.logInfo("已配置 Windows 防火墙入站规则，远程网段 %s", subnet)
		}
		driverDir, derr := win.EnsureDriver()
		if derr != nil {
			client.Disconnect()
			s.failConnect(fmt.Errorf("释放 WinDivert 驱动失败: %w", derr))
			return
		}
		bridge, err = win.StartBridge(client, subnet, route, driverDir, func(level, message string) {
			s.logAt(upperLevel(level), message)
		})
		if err != nil {
			client.Disconnect()
			s.failConnect(fmt.Errorf("启动 WinDivert 失败: %w\n驱动目录: %s\n如果杀毒软件拦截了 WinDivert64.sys，请把该目录加入信任后重试。", err, driverDir))
			return
		}
	}
	if s.isQuitting() {
		if bridge != nil {
			bridge.Stop()
		}
		client.Disconnect()
		return
	}
	s.completeConnect(client, bridge, subnet, route, welcome)
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

func (s *session) completeConnect(client *tunnel.Client, bridge *win.Bridge, subnet *net.IPNet, route win.RouteInfo, welcome *protocol.WelcomeMessage) {
	s.mu.Lock()
	if s.quitting {
		s.mu.Unlock()
		if bridge != nil {
			bridge.Stop()
		}
		client.Disconnect()
		return
	}
	s.client = client
	s.bridge = bridge
	s.subnet = subnet
	s.roomCode = welcome.RoomCode
	s.members = append([]protocol.Peer(nil), welcome.Members...)
	s.busy = false
	if s.capture {
		s.status = "中继运行中"
	} else {
		s.status = "隧道运行中（未抓包）"
	}
	s.mu.Unlock()
	if s.capture {
		s.logOK("本地出口 %s，虚拟网段 %s", route.LocalIP, subnet)
	}
}

func (s *session) failConnect(err error) {
	message := describeTunnelError(err)
	s.mu.Lock()
	s.busy = false
	s.status = "连接失败"
	s.lastError = message
	s.errSeq++
	s.mu.Unlock()
	s.logAt("ERROR", message)
}

// Disconnect tears the current room down asynchronously.
func (s *session) Disconnect() {
	s.mu.Lock()
	if s.busy || s.client == nil {
		s.mu.Unlock()
		return
	}
	s.busy = true
	s.status = "正在断开…"
	client, bridge := s.client, s.bridge
	s.client, s.bridge = nil, nil
	s.mu.Unlock()
	go func() {
		if bridge != nil {
			bridge.Stop()
		}
		if client != nil {
			client.Disconnect()
		}
		s.finishTeardown("未连接")
	}()
}

func (s *session) finishTeardown(status string) {
	s.mu.Lock()
	s.busy = false
	s.status = status
	s.members = nil
	s.subnet = nil
	s.mu.Unlock()
	s.logInfo("已断开中转")
}

func (s *session) onTunnelEvent(client *tunnel.Client, ev tunnel.Event) {
	s.mu.Lock()
	current := s.client
	s.mu.Unlock()
	if current == nil || current != client {
		return
	}
	switch ev.Type {
	case tunnel.EventPeers:
		s.mu.Lock()
		s.members = append([]protocol.Peer(nil), ev.Peers...)
		s.mu.Unlock()
	case tunnel.EventRoomClosed:
		reason := ev.Reason
		switch reason {
		case "host_left":
			reason = "房主已离开"
		case "host_timeout":
			reason = "房主长时间离线"
		}
		s.mu.Lock()
		s.status = "房间已关闭"
		s.mu.Unlock()
		s.logWarn("房间已关闭: %s", reason)
		go s.teardownFromEvent()
	case tunnel.EventSilent:
		if ev.SilentSeconds > 0 {
			s.mu.Lock()
			already := s.status == "中转响应超时"
			if !already {
				s.status = "中转响应超时"
			}
			s.mu.Unlock()
			if !already {
				s.logWarn("中转已 %.0f 秒无响应", ev.SilentSeconds)
			}
		}
	case tunnel.EventTransportError:
		if ev.Err != nil {
			s.logError("网络错误: %v", ev.Err)
		}
	}
}

func (s *session) teardownFromEvent() {
	s.mu.Lock()
	client, bridge := s.client, s.bridge
	if client == nil {
		s.mu.Unlock()
		return
	}
	s.client, s.bridge = nil, nil
	s.busy = true
	s.mu.Unlock()
	if bridge != nil {
		bridge.Stop()
	}
	client.Disconnect()
	s.mu.Lock()
	s.busy = false
	s.status = "房间已关闭"
	s.members = nil
	s.subnet = nil
	s.mu.Unlock()
	s.logInfo("房间会话已结束")
}

// Shutdown stops everything; used when the window closes.
func (s *session) Shutdown() {
	s.mu.Lock()
	if s.quitting {
		s.mu.Unlock()
		return
	}
	s.quitting = true
	client, bridge := s.client, s.bridge
	s.client, s.bridge = nil, nil
	s.mu.Unlock()
	if bridge != nil {
		bridge.Stop()
	}
	if client != nil {
		client.Disconnect()
	}
}

// Snapshot returns a consistent copy of the current state for the UI.
func (s *session) Snapshot() sessionSnapshot {
	s.mu.Lock()
	snap := sessionSnapshot{
		Status:    s.status,
		Busy:      s.busy,
		Relay:     win.FormatRelay(s.relayHost, s.relayPort),
		Name:      s.name,
		RoomCode:  s.roomCode,
		Subnet:    "",
		Capture:   s.capture,
		Logs:      append([]logEntry(nil), s.logs...),
		LogSeq:    s.logSeq,
		Error:     s.lastError,
		ErrorSeq:  s.errSeq,
		MemberID:  0,
		IsHost:    false,
		Members:   nil,
		Civ:       nil,
		Connected: s.client != nil,
	}
	members := append([]protocol.Peer(nil), s.members...)
	subnet := s.subnet
	client := s.client
	bridge := s.bridge
	s.mu.Unlock()

	if subnet != nil {
		snap.Subnet = subnet.String()
	}
	if client != nil {
		m := client.Metrics()
		snap.RTTms = m.RTT.Milliseconds()
		snap.HasRTT = m.HasRTT
		snap.Rx = m.GameRxDatagrams
		snap.Tx = m.GameTxDatagrams
		snap.MemberID = int(client.MemberID())
		snap.IsHost = client.IsHost()
		if code := client.RoomCode(); code != "" {
			snap.RoomCode = code
		}
	}
	if bridge != nil {
		snap.BridgeRunning = bridge.Running()
	}
	for _, p := range members {
		virtual := "-"
		if subnet != nil {
			if ip := win.VirtualMemberIP(subnet, p.ID); ip != nil {
				virtual = ip.String()
			}
		}
		snap.Members = append(snap.Members, memberView{
			ID:      int(p.ID),
			Name:    p.Name,
			Host:    p.Host,
			Online:  p.Online,
			Virtual: virtual,
		})
	}
	snap.Civ = s.civProcesses()
	return snap
}

func (s *session) civProcesses() []civProcessView {
	s.mu.Lock()
	if time.Since(s.procChecked) < 2*time.Second {
		out := append([]civProcessView(nil), s.procCache...)
		s.mu.Unlock()
		return out
	}
	s.mu.Unlock()
	procs := win.FindCiv6Processes()
	views := make([]civProcessView, 0, len(procs))
	for _, p := range procs {
		views = append(views, civProcessView{PID: p.PID, Name: p.Name})
	}
	s.mu.Lock()
	s.procCache = views
	s.procChecked = time.Now()
	s.mu.Unlock()
	return views
}

func (s *session) logText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	for _, entry := range s.logs {
		out = append(out, entry.Time...)
		out = append(out, " ["...)
		out = append(out, entry.Level...)
		out = append(out, "] "...)
		out = append(out, entry.Message...)
		out = append(out, "\r\n"...)
	}
	return string(out)
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
			return "中转服务器没有响应，请检查地址、端口和阿里云安全组 UDP 放行"
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
