// Package tunnel implements the authenticated client side of the relay
// protocol, independent of any Windows packet-capture code.
package tunnel

import (
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"civ6relay/internal/protocol"
)

const (
	keepaliveInterval = 5 * time.Second
	silenceThreshold  = 15 * time.Second
)

// EventType identifies asynchronous client state changes.
type EventType string

const (
	EventPeers          EventType = "peers"
	EventRoomClosed     EventType = "room_closed"
	EventRTT            EventType = "rtt"
	EventSilent         EventType = "silent"
	EventTransportError EventType = "transport_error"
	EventDisconnected   EventType = "disconnected"
)

// Event is delivered to the event handler.
type Event struct {
	Type          EventType
	Peers         []protocol.Peer
	Reason        string
	SilentSeconds float64
	Err           error
	RTT           time.Duration
}

// Error is a relay-side or transport error with a stable code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Metrics is a point-in-time snapshot of client counters.
type Metrics struct {
	TunnelTxPackets uint64
	TunnelRxPackets uint64
	TunnelTxBytes   uint64
	TunnelRxBytes   uint64
	GameTxDatagrams uint64
	GameRxDatagrams uint64
	Dropped         uint64
	AuthFailures    uint64
	ReplayDrops     uint64
	RTT             time.Duration
	HasRTT          bool
}

type counters struct {
	tunnelTxPackets atomic.Uint64
	tunnelRxPackets atomic.Uint64
	tunnelTxBytes   atomic.Uint64
	tunnelRxBytes   atomic.Uint64
	gameTxDatagrams atomic.Uint64
	gameRxDatagrams atomic.Uint64
	dropped         atomic.Uint64
	authFailures    atomic.Uint64
	replayDrops     atomic.Uint64
	rttNanos        atomic.Int64
}

// DataHandler receives complete game datagrams from other members.
type DataHandler func(sourceMember uint8, sourcePort, targetPort uint16, payload []byte)

// Client is a relay tunnel client.
type Client struct {
	conn      *net.UDPConn
	relayAddr *net.UDPAddr
	start     time.Time

	mu       sync.RWMutex
	token    []byte
	roomID   uint64
	memberID uint8
	roomCode string
	isHost   bool
	peers    []protocol.Peer
	replay   protocol.ReplayWindow

	seq      atomic.Uint64
	packetID atomic.Uint32
	lastRx   atomic.Int64 // UnixNano

	pendingMu sync.Mutex
	pending   map[string]chan handshakeResult

	reassembler *protocol.Reassembler

	handlerMu    sync.RWMutex
	dataHandler  DataHandler
	eventHandler func(Event)

	counter counters
	closed  atomic.Bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

type handshakeResult struct {
	welcome *protocol.WelcomeMessage
	err     error
}

// New resolves the relay address, opens a connected UDP socket, and starts
// background loops.
func New(host string, port int) (*Client, error) {
	if host == "" {
		return nil, &Error{Code: "resolve_failed", Message: "中转地址不能为空"}
	}
	addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, &Error{Code: "resolve_failed", Message: fmt.Sprintf("无法解析中转服务器地址: %v", err)}
	}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil, &Error{Code: "resolve_failed", Message: fmt.Sprintf("无法连接中转服务器: %v", err)}
	}
	_ = conn.SetReadBuffer(4 << 20)
	_ = conn.SetWriteBuffer(1 << 20)
	c := &Client{
		conn:        conn,
		relayAddr:   addr,
		start:       time.Now(),
		pending:     make(map[string]chan handshakeResult),
		reassembler: protocol.NewReassembler(),
		stop:        make(chan struct{}),
	}
	c.seq.Store(protocol.NewCounter().Next())
	c.packetID.Store(randomUint32())
	c.wg.Add(2)
	go c.readLoop()
	go c.heartbeatLoop()
	return c, nil
}

// SetDataHandler installs the game datagram callback.
func (c *Client) SetDataHandler(h DataHandler) {
	c.handlerMu.Lock()
	c.dataHandler = h
	c.handlerMu.Unlock()
}

// SetEventHandler installs the asynchronous event callback.
func (c *Client) SetEventHandler(h func(Event)) {
	c.handlerMu.Lock()
	c.eventHandler = h
	c.handlerMu.Unlock()
}

// RelayAddr returns the resolved relay endpoint.
func (c *Client) RelayAddr() *net.UDPAddr { return c.relayAddr }

// Connected reports whether the client holds room credentials.
func (c *Client) Connected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token != nil && c.roomID != 0 && c.memberID != 0
}

// MemberID returns the current member ID (0 when disconnected).
func (c *Client) MemberID() uint8 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.memberID
}

// RoomCode returns the current room code.
func (c *Client) RoomCode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.roomCode
}

// IsHost reports whether this client is the room host.
func (c *Client) IsHost() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isHost
}

// Peers returns a copy of the current member list.
func (c *Client) Peers() []protocol.Peer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]protocol.Peer, len(c.peers))
	copy(out, c.peers)
	return out
}

// Metrics returns a snapshot of the counters.
func (c *Client) Metrics() Metrics {
	m := Metrics{
		TunnelTxPackets: c.counter.tunnelTxPackets.Load(),
		TunnelRxPackets: c.counter.tunnelRxPackets.Load(),
		TunnelTxBytes:   c.counter.tunnelTxBytes.Load(),
		TunnelRxBytes:   c.counter.tunnelRxBytes.Load(),
		GameTxDatagrams: c.counter.gameTxDatagrams.Load(),
		GameRxDatagrams: c.counter.gameRxDatagrams.Load(),
		Dropped:         c.counter.dropped.Load(),
		AuthFailures:    c.counter.authFailures.Load(),
		ReplayDrops:     c.counter.replayDrops.Load(),
	}
	if ns := c.counter.rttNanos.Load(); ns > 0 {
		m.RTT = time.Duration(ns)
		m.HasRTT = true
	}
	return m
}

// CreateRoom creates a new room and returns the welcome message.
func (c *Client) CreateRoom(name string, timeout time.Duration) (*protocol.WelcomeMessage, error) {
	return c.handshake(protocol.KindCreate, protocol.HandshakeRequest{Name: name}, timeout)
}

// JoinRoom joins a room by code and returns the welcome message.
func (c *Client) JoinRoom(code, name string, timeout time.Duration) (*protocol.WelcomeMessage, error) {
	normalized, err := protocol.NormalizeRoomCode(code)
	if err != nil {
		return nil, &Error{Code: "invalid_request", Message: "房间码格式不正确"}
	}
	req := protocol.HandshakeRequest{Name: name, RoomCode: normalized}
	c.mu.RLock()
	if c.token != nil && c.roomCode == normalized && c.memberID != 0 {
		req.ResumeMember = c.memberID
		req.ResumeToken = base64.URLEncoding.EncodeToString(c.token)
	}
	c.mu.RUnlock()
	return c.handshake(protocol.KindJoin, req, timeout)
}

func (c *Client) handshake(kind protocol.Kind, req protocol.HandshakeRequest, timeout time.Duration) (*protocol.WelcomeMessage, error) {
	if c.closed.Load() {
		return nil, &Error{Code: "closed", Message: "客户端已关闭"}
	}
	req.RequestID = protocol.RandomHex(12)
	payload, err := protocol.EncodeControl(req)
	if err != nil {
		return nil, err
	}
	raw, err := (&protocol.Frame{Kind: kind, Payload: payload, PacketID: randomUint32()}).Encode(nil)
	if err != nil {
		return nil, err
	}
	ch := make(chan handshakeResult, 1)
	c.pendingMu.Lock()
	c.pending[req.RequestID] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, req.RequestID)
		c.pendingMu.Unlock()
	}()

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		c.write(raw)
		select {
		case result := <-ch:
			if result.err != nil {
				return nil, result.err
			}
			return result.welcome, nil
		case <-c.stop:
			return nil, &Error{Code: "closed", Message: "客户端已关闭"}
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return nil, &Error{Code: "timeout", Message: "中转服务器没有响应"}
		}
	}
}

// SendGameData forwards one game datagram to a member (or the whole room).
func (c *Client) SendGameData(targetMember uint8, sourcePort, targetPort uint16, payload []byte, broadcast bool) bool {
	c.mu.RLock()
	token := c.token
	roomID := c.roomID
	memberID := c.memberID
	c.mu.RUnlock()
	if token == nil || roomID == 0 || memberID == 0 {
		return false
	}
	chunks, err := protocol.SplitPayload(payload)
	if err != nil {
		c.counter.dropped.Add(1)
		return false
	}
	target := targetMember
	flags := protocol.Flags(0)
	if broadcast {
		target = 255
		flags |= protocol.FlagBroadcast
	}
	if len(chunks) > 1 {
		flags |= protocol.FlagFragmented
	}
	packetID := c.packetID.Add(1)
	for i, chunk := range chunks {
		frame := protocol.Frame{
			Kind:          protocol.KindData,
			Flags:         flags,
			RoomID:        roomID,
			SourceMember:  memberID,
			TargetMember:  target,
			FragmentIndex: uint16(i),
			FragmentCount: uint16(len(chunks)),
			SourcePort:    sourcePort,
			TargetPort:    targetPort,
			Sequence:      c.nextSeq(),
			PacketID:      packetID,
			Payload:       chunk,
		}
		raw, err := frame.Encode(token)
		if err != nil {
			c.counter.dropped.Add(1)
			return false
		}
		if !c.write(raw) {
			return false
		}
	}
	c.counter.gameTxDatagrams.Add(1)
	return true
}

// Disconnect sends LEAVE and shuts the client down.
func (c *Client) Disconnect() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	if c.Connected() {
		c.sendAuthenticated(protocol.KindLeave, []byte{})
	}
	close(c.stop)
	_ = c.conn.Close()
	c.wg.Wait()
	c.mu.Lock()
	c.token = nil
	c.roomID = 0
	c.memberID = 0
	c.roomCode = ""
	c.isHost = false
	c.peers = nil
	c.replay.Reset()
	c.mu.Unlock()
	c.emit(Event{Type: EventDisconnected})
}

func (c *Client) nextSeq() uint64 {
	return c.seq.Add(1) - 1
}

func (c *Client) write(raw []byte) bool {
	n, err := c.conn.Write(raw)
	if err != nil {
		return false
	}
	c.counter.tunnelTxPackets.Add(1)
	c.counter.tunnelTxBytes.Add(uint64(n))
	return n == len(raw)
}

func (c *Client) sendAuthenticated(kind protocol.Kind, payload []byte) bool {
	c.mu.RLock()
	token := c.token
	roomID := c.roomID
	memberID := c.memberID
	c.mu.RUnlock()
	if token == nil {
		return false
	}
	frame := protocol.Frame{
		Kind:         kind,
		RoomID:       roomID,
		SourceMember: memberID,
		Sequence:     c.nextSeq(),
		Payload:      payload,
	}
	raw, err := frame.Encode(token)
	if err != nil {
		return false
	}
	return c.write(raw)
}

func (c *Client) readLoop() {
	defer c.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			if c.closed.Load() {
				return
			}
			c.emit(Event{Type: EventTransportError, Err: err})
			select {
			case <-c.stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		c.counter.tunnelRxPackets.Add(1)
		c.counter.tunnelRxBytes.Add(uint64(n))
		frame, err := protocol.Decode(buf[:n])
		if err != nil {
			c.counter.dropped.Add(1)
			continue
		}
		if frame.Kind == protocol.KindWelcome || frame.Kind == protocol.KindError {
			c.handleHandshakeResponse(&frame)
			continue
		}
		c.handleAuthenticated(&frame)
	}
}

func (c *Client) handleHandshakeResponse(frame *protocol.Frame) {
	var head struct {
		RequestID string `json:"request_id"`
	}
	if err := protocol.DecodeControl(frame.Payload, &head); err != nil {
		c.counter.dropped.Add(1)
		return
	}
	c.pendingMu.Lock()
	ch := c.pending[head.RequestID]
	c.pendingMu.Unlock()
	if ch == nil {
		return
	}
	if frame.Kind == protocol.KindError {
		var msg protocol.ErrorMessage
		if err := protocol.DecodeControl(frame.Payload, &msg); err != nil {
			c.counter.dropped.Add(1)
			return
		}
		select {
		case ch <- handshakeResult{err: &Error{Code: msg.Code, Message: msg.Message}}:
		default:
		}
		return
	}
	var welcome protocol.WelcomeMessage
	if err := protocol.DecodeControl(frame.Payload, &welcome); err != nil {
		c.counter.dropped.Add(1)
		return
	}
	token, err := base64.URLEncoding.DecodeString(welcome.Token)
	if err != nil || len(token) != 32 || welcome.MemberID < 2 || welcome.MemberID > 9 {
		c.counter.dropped.Add(1)
		return
	}
	roomID, err := strconv.ParseUint(welcome.RoomID, 16, 64)
	if err != nil || roomID == 0 {
		c.counter.dropped.Add(1)
		return
	}
	c.mu.Lock()
	c.token = token
	c.roomID = roomID
	c.memberID = welcome.MemberID
	c.roomCode = welcome.RoomCode
	c.isHost = welcome.Host
	c.peers = append([]protocol.Peer(nil), welcome.Members...)
	c.replay.Reset()
	c.mu.Unlock()
	c.seq.Store(protocol.NewCounter().Next())
	c.lastRx.Store(time.Now().UnixNano())
	select {
	case ch <- handshakeResult{welcome: &welcome}:
	default:
	}
}

func (c *Client) handleAuthenticated(frame *protocol.Frame) {
	c.mu.RLock()
	token := c.token
	roomID := c.roomID
	memberID := c.memberID
	c.mu.RUnlock()
	if token == nil || frame.RoomID != roomID || frame.TargetMember != memberID {
		c.counter.dropped.Add(1)
		return
	}
	if !frame.Verified(token) {
		c.counter.authFailures.Add(1)
		return
	}
	c.mu.Lock()
	ok := c.replay.Accept(frame.Sequence)
	c.mu.Unlock()
	if !ok {
		c.counter.replayDrops.Add(1)
		return
	}
	c.lastRx.Store(time.Now().UnixNano())
	switch frame.Kind {
	case protocol.KindPeers:
		var msg protocol.PeerListMessage
		if err := protocol.DecodeControl(frame.Payload, &msg); err != nil {
			c.counter.dropped.Add(1)
			return
		}
		c.mu.Lock()
		c.peers = append([]protocol.Peer(nil), msg.Members...)
		c.mu.Unlock()
		c.emit(Event{Type: EventPeers, Peers: msg.Members})
	case protocol.KindPong:
		var msg protocol.PingMessage
		if err := protocol.DecodeControl(frame.Payload, &msg); err != nil {
			return
		}
		rtt := time.Since(c.start) - time.Duration(msg.SentNS)
		if rtt < 0 {
			rtt = 0
		}
		c.counter.rttNanos.Store(int64(rtt))
		c.emit(Event{Type: EventRTT, RTT: rtt})
	case protocol.KindRoomClosed:
		var msg protocol.RoomClosedMessage
		_ = protocol.DecodeControl(frame.Payload, &msg)
		c.mu.Lock()
		c.token = nil
		c.roomID = 0
		c.memberID = 0
		c.roomCode = ""
		c.isHost = false
		c.peers = nil
		c.replay.Reset()
		c.mu.Unlock()
		c.emit(Event{Type: EventRoomClosed, Reason: msg.Reason})
	case protocol.KindData:
		payload, err := c.reassembler.Add(frame)
		if err != nil || payload == nil {
			return
		}
		c.counter.gameRxDatagrams.Add(1)
		c.handlerMu.RLock()
		h := c.dataHandler
		c.handlerMu.RUnlock()
		if h != nil {
			h(frame.SourceMember, frame.SourcePort, frame.TargetPort, payload)
		}
	}
}

func (c *Client) heartbeatLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.reassembler.Expire()
			if !c.Connected() {
				continue
			}
			payload, _ := protocol.EncodeControl(protocol.PingMessage{SentNS: time.Since(c.start).Nanoseconds()})
			c.sendAuthenticated(protocol.KindPing, payload)
			if last := c.lastRx.Load(); last != 0 {
				silent := time.Since(time.Unix(0, last))
				if silent > silenceThreshold {
					c.emit(Event{Type: EventSilent, SilentSeconds: silent.Seconds()})
				}
			}
		}
	}
}

func (c *Client) emit(ev Event) {
	c.handlerMu.RLock()
	h := c.eventHandler
	c.handlerMu.RUnlock()
	if h != nil {
		h(ev)
	}
}

func randomUint32() uint32 {
	b := protocol.RandomBytes(4)
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
