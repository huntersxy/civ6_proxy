// Package tunnel implements the authenticated client side of the relay
// protocol, independent of any Windows packet-capture code.
package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"civ6relay/internal/protocol"
)

const (
	keepaliveInterval = 5 * time.Second
	silenceThreshold  = 15 * time.Second
	handshakeRetry    = 400 * time.Millisecond
	leaveTimeout      = 200 * time.Millisecond
)

// EventType identifies asynchronous client state changes.
type EventType string

const (
	EventPeers          EventType = "peers"
	EventRoomClosed     EventType = "room_closed"
	EventRTT            EventType = "rtt"
	EventTransportError EventType = "transport_error"
	EventDisconnected   EventType = "disconnected"
)

// Event is delivered to the event handler.
type Event struct {
	Type   EventType
	Peers  []protocol.Peer
	Reason string
	Err    error
	RTT    time.Duration
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
	DirectTxFrames  uint64
	DirectRxFrames  uint64
	Dropped         uint64
	AuthFailures    uint64
	ReplayDrops     uint64
	RTT             time.Duration
	HasRTT          bool
	Silent          bool
}

type counters struct {
	tunnelTxPackets atomic.Uint64
	tunnelRxPackets atomic.Uint64
	tunnelTxBytes   atomic.Uint64
	tunnelRxBytes   atomic.Uint64
	gameTxDatagrams atomic.Uint64
	gameRxDatagrams atomic.Uint64
	directTxFrames  atomic.Uint64
	directRxFrames  atomic.Uint64
	dropped         atomic.Uint64
	authFailures    atomic.Uint64
	replayDrops     atomic.Uint64
}

// DataHandler receives complete game datagrams and owns the payload.
type DataHandler func(sourceMember uint8, sourcePort, targetPort uint16, payload []byte)

// Client is a relay tunnel client.
type Client struct {
	conn  *net.UDPConn
	relay netip.AddrPort
	start time.Time

	room       atomic.Pointer[clientRoom]
	sendMu     sync.Mutex
	sendBuffer [protocol.MaxOuterDatagram]byte
	packetID   uint32

	pendingMu sync.Mutex
	pending   *pendingHandshake

	handlerMu    sync.RWMutex
	dataHandler  DataHandler
	eventHandler func(Event)

	counter counters
	closed  atomic.Bool
	probe   chan struct{}
	p2pWake chan struct{}
	stop    chan struct{}
	wg      sync.WaitGroup
}

type clientRoom struct {
	token    []byte
	roomID   uint64
	memberID uint8
	roomCode string
	isHost   bool

	txAuth      *protocol.Authenticator
	rxAuth      *protocol.Authenticator
	seq         *protocol.Counter
	replay      protocol.ReplayWindow
	peerSeq     uint64
	reassembler *protocol.Reassembler
	peers       atomic.Pointer[[]protocol.Peer]
	lastRx      atomic.Int64
	rttNanos    atomic.Int64
	p2p         p2pRoom
}

type pendingHandshake struct {
	id     string
	ctx    context.Context
	result chan handshakeResult
}

type handshakeResult struct {
	welcome *protocol.WelcomeMessage
	err     error
}

// New resolves the relay address, opens a UDP socket, and starts
// background loops.
func New(host string, port int) (*Client, error) {
	return NewContext(context.Background(), host, port)
}

// NewContext uses ctx for DNS and socket setup.
func NewContext(ctx context.Context, host string, port int) (*Client, error) {
	if host == "" {
		return nil, &Error{Code: "resolve_failed", Message: "中转地址不能为空"}
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		if ctx.Err() != nil {
			return nil, handshakeContextError(ctx.Err())
		}
		return nil, &Error{Code: "resolve_failed", Message: fmt.Sprintf("无法连接中转服务器: %v", err)}
	}
	if len(addresses) == 0 || !addresses[0].Unmap().Is4() || port < 1 || port > 65535 {
		return nil, &Error{Code: "resolve_failed", Message: "中转服务器需要有效的 IPv4 地址和端口"}
	}
	socket, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp4", ":0")
	if err != nil {
		return nil, &Error{Code: "socket_failed", Message: fmt.Sprintf("无法打开 UDP 套接字: %v", err)}
	}
	conn := socket.(*net.UDPConn)
	_ = conn.SetReadBuffer(4 << 20)
	_ = conn.SetWriteBuffer(1 << 20)
	c := &Client{
		conn:     conn,
		relay:    netip.AddrPortFrom(addresses[0].Unmap(), uint16(port)),
		start:    time.Now(),
		packetID: randomUint32(),
		probe:    make(chan struct{}, 1),
		p2pWake:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
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
func (c *Client) RelayAddr() *net.UDPAddr { return net.UDPAddrFromAddrPort(c.relay) }

// Connected reports whether the client holds room credentials.
func (c *Client) Connected() bool {
	return c.room.Load() != nil && !c.closed.Load()
}

// MemberID returns the current member ID (0 when disconnected).
func (c *Client) MemberID() uint8 {
	if room := c.room.Load(); room != nil {
		return room.memberID
	}
	return 0
}

// RoomCode returns the current room code.
func (c *Client) RoomCode() string {
	if room := c.room.Load(); room != nil {
		return room.roomCode
	}
	return ""
}

// IsHost reports whether this client is the room host.
func (c *Client) IsHost() bool {
	room := c.room.Load()
	return room != nil && room.isHost
}

// Peers returns a copy of the current member list.
func (c *Client) Peers() []protocol.Peer {
	if room := c.room.Load(); room != nil {
		return append([]protocol.Peer(nil), (*room.peers.Load())...)
	}
	return nil
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
		DirectTxFrames:  c.counter.directTxFrames.Load(),
		DirectRxFrames:  c.counter.directRxFrames.Load(),
		Dropped:         c.counter.dropped.Load(),
		AuthFailures:    c.counter.authFailures.Load(),
		ReplayDrops:     c.counter.replayDrops.Load(),
	}
	if room := c.room.Load(); room != nil {
		m.RTT = time.Duration(room.rttNanos.Load())
		m.Silent = time.Since(c.start)-time.Duration(room.lastRx.Load()) >= silenceThreshold
		m.HasRTT = m.RTT > 0 && !m.Silent
	}
	return m
}

// CreateRoom creates a new room and returns the welcome message.
func (c *Client) CreateRoom(name string, timeout time.Duration) (*protocol.WelcomeMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.CreateRoomContext(ctx, name)
}

func (c *Client) CreateRoomContext(ctx context.Context, name string) (*protocol.WelcomeMessage, error) {
	return c.handshake(ctx, protocol.KindCreate, protocol.HandshakeRequest{Name: name})
}

// JoinRoom joins a room by code and returns the welcome message.
func (c *Client) JoinRoom(code, name string, timeout time.Duration) (*protocol.WelcomeMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.JoinRoomContext(ctx, code, name)
}

func (c *Client) JoinRoomContext(ctx context.Context, code, name string) (*protocol.WelcomeMessage, error) {
	normalized, err := protocol.NormalizeRoomCode(code)
	if err != nil {
		return nil, &Error{Code: "invalid_request", Message: "房间码格式不正确"}
	}
	req := protocol.HandshakeRequest{Name: name, RoomCode: normalized}
	if room := c.room.Load(); room != nil && room.roomCode == normalized {
		req.ResumeMember = room.memberID
		req.ResumeToken = base64.URLEncoding.EncodeToString(room.token)
	}
	return c.handshake(ctx, protocol.KindJoin, req)
}

func (c *Client) handshake(ctx context.Context, kind protocol.Kind, req protocol.HandshakeRequest) (*protocol.WelcomeMessage, error) {
	if ctx.Err() != nil {
		return nil, handshakeContextError(ctx.Err())
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
	attempt := &pendingHandshake{id: req.RequestID, ctx: ctx, result: make(chan handshakeResult, 1)}
	c.pendingMu.Lock()
	if c.closed.Load() {
		c.pendingMu.Unlock()
		return nil, &Error{Code: "closed", Message: "客户端已关闭"}
	}
	if c.pending != nil {
		c.pendingMu.Unlock()
		return nil, &Error{Code: "busy", Message: "已有房间请求正在处理"}
	}
	c.pending = attempt
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		if c.pending == attempt {
			c.pending = nil
		}
		c.pendingMu.Unlock()
	}()

	ticker := time.NewTicker(handshakeRetry)
	defer ticker.Stop()
	c.write(raw)
	for {
		select {
		case result := <-attempt.result:
			return result.welcome, result.err
		case <-c.stop:
			return nil, &Error{Code: "closed", Message: "客户端已关闭"}
		case <-ctx.Done():
			c.pendingMu.Lock()
			select {
			case result := <-attempt.result:
				c.pendingMu.Unlock()
				return result.welcome, result.err
			default:
				c.pendingMu.Unlock()
				return nil, handshakeContextError(ctx.Err())
			}
		case <-ticker.C:
			if ctx.Err() == nil {
				c.write(raw)
			}
		}
	}
}

func handshakeContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "timeout", Message: "中转服务器没有响应"}
	}
	return err
}

// SendGameData forwards one game datagram to a member (or the whole room).
func (c *Client) SendGameData(targetMember uint8, sourcePort, targetPort uint16, payload []byte, broadcast bool) bool {
	if len(payload) > protocol.MaxUDPPayload {
		c.counter.dropped.Add(1)
		return false
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	room := c.room.Load()
	if room == nil || c.closed.Load() {
		return false
	}
	count := max(1, (len(payload)+protocol.MaxFragmentPayload-1)/protocol.MaxFragmentPayload)
	c.packetID++
	frame := protocol.Frame{
		Kind:          protocol.KindData,
		TargetMember:  targetMember,
		FragmentCount: uint16(count),
		SourcePort:    sourcePort,
		TargetPort:    targetPort,
		PacketID:      c.packetID,
	}
	if broadcast {
		frame.TargetMember = 255
		frame.Flags |= protocol.FlagBroadcast
	}
	if count > 1 {
		frame.Flags |= protocol.FlagFragmented
	}
	var direct *p2pPath
	if !broadcast && room.p2p.enabled && validMember(targetMember) {
		now := time.Now()
		if path := room.p2p.paths[targetMember]; path != nil && room.p2p.usable(now) && path.ready(now) {
			direct = path
		}
	}
	for index := 0; index < count; index++ {
		offset := index * protocol.MaxFragmentPayload
		frame.FragmentIndex = uint16(index)
		frame.Payload = payload[offset:min(len(payload), offset+protocol.MaxFragmentPayload)]
		if direct != nil {
			if c.writePeerFrame(room, direct, &frame) {
				c.counter.directTxFrames.Add(1)
				continue
			}
			direct.verifiedUntil = time.Time{}
			direct.nextProbe = time.Time{}
			direct = nil
		}
		if !c.writeFrame(room, &frame) {
			c.counter.dropped.Add(1)
			return false
		}
	}
	c.counter.gameTxDatagrams.Add(1)
	return true
}

// Disconnect sends LEAVE and shuts the client down.
func (c *Client) Disconnect() {
	c.pendingMu.Lock()
	if !c.closed.CompareAndSwap(false, true) {
		c.pendingMu.Unlock()
		return
	}
	close(c.stop)
	c.pendingMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(leaveTimeout))
	c.sendMu.Lock()
	if room := c.room.Load(); room != nil {
		c.writeFrame(room, &protocol.Frame{Kind: protocol.KindLeave})
	}
	c.sendMu.Unlock()
	_ = c.conn.Close()
	c.wg.Wait()
	c.room.Store(nil)
	c.emit(Event{Type: EventDisconnected})
}

func (c *Client) write(raw []byte) bool {
	return c.writeTo(c.relay, raw)
}

func (c *Client) writeTo(address netip.AddrPort, raw []byte) bool {
	n, err := c.conn.WriteToUDPAddrPort(raw, address)
	if err != nil {
		return false
	}
	c.counter.tunnelTxPackets.Add(1)
	c.counter.tunnelTxBytes.Add(uint64(n))
	return n == len(raw)
}

func (c *Client) writeFrame(room *clientRoom, frame *protocol.Frame) bool {
	frame.RoomID = room.roomID
	frame.SourceMember = room.memberID
	frame.Sequence = room.seq.Next()
	raw, err := frame.AppendEncode(c.sendBuffer[:0], room.txAuth)
	return err == nil && c.write(raw)
}

func (c *Client) readLoop() {
	defer c.wg.Done()
	buf := make([]byte, protocol.MaxUDPPayload)
	var lastError time.Time
	for {
		n, address, err := c.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if c.closed.Load() {
				return
			}
			if time.Since(lastError) >= keepaliveInterval {
				c.emit(Event{Type: EventTransportError, Err: err})
				lastError = time.Now()
			}
			select {
			case <-c.stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if address != c.relay {
			c.handlePeerDatagram(address, buf[:n])
			continue
		}
		c.counter.tunnelRxPackets.Add(1)
		c.counter.tunnelRxBytes.Add(uint64(n))
		frame, err := protocol.DecodeView(buf[:n])
		if err != nil {
			c.counter.dropped.Add(1)
			continue
		}
		if frame.Kind == protocol.KindWelcome || frame.Kind == protocol.KindError {
			c.handleHandshakeResponse(&frame)
			continue
		}
		c.handleAuthenticated(&frame, buf[:n])
	}
}

func (c *Client) handleHandshakeResponse(frame *protocol.Frame) {
	if frame.Flags&protocol.FlagAuthenticated != 0 {
		return
	}
	if frame.Kind == protocol.KindError {
		var msg protocol.ErrorMessage
		if err := protocol.DecodeControl(frame.Payload, &msg); err != nil {
			c.counter.dropped.Add(1)
			return
		}
		c.finishHandshake(msg.RequestID, handshakeResult{err: &Error{Code: msg.Code, Message: msg.Message}}, nil)
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
	room := &clientRoom{
		token: token, roomID: roomID, memberID: welcome.MemberID, roomCode: welcome.RoomCode, isHost: welcome.Host,
		txAuth: protocol.NewAuthenticator(token), rxAuth: protocol.NewAuthenticator(token),
		seq: protocol.NewCounter(), reassembler: protocol.NewReassembler(),
	}
	peers := append([]protocol.Peer(nil), welcome.Members...)
	room.peers.Store(&peers)
	room.lastRx.Store(time.Since(c.start).Nanoseconds())
	c.finishHandshake(welcome.RequestID, handshakeResult{welcome: &welcome}, room)
}

func (c *Client) finishHandshake(id string, result handshakeResult, room *clientRoom) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	attempt := c.pending
	if c.closed.Load() || attempt == nil || id != attempt.id || attempt.ctx.Err() != nil {
		return
	}
	if room != nil {
		c.room.Store(room)
		select {
		case c.probe <- struct{}{}:
		default:
		}
	}
	c.pending = nil
	attempt.result <- result
}

func (c *Client) handleAuthenticated(frame *protocol.Frame, raw []byte) {
	room := c.room.Load()
	if room == nil || frame.RoomID != room.roomID || frame.TargetMember != room.memberID {
		c.counter.dropped.Add(1)
		return
	}
	if !room.rxAuth.Verify(raw) {
		c.counter.authFailures.Add(1)
		return
	}
	if !room.replay.Accept(frame.Sequence) {
		c.counter.replayDrops.Add(1)
		return
	}
	room.lastRx.Store(time.Since(c.start).Nanoseconds())
	switch frame.Kind {
	case protocol.KindPeers:
		if frame.Sequence < room.peerSeq {
			return
		}
		var msg protocol.PeerListMessage
		if err := protocol.DecodeControl(frame.Payload, &msg); err != nil {
			c.counter.dropped.Add(1)
			return
		}
		room.peerSeq = frame.Sequence
		peers := append([]protocol.Peer(nil), msg.Members...)
		c.sendMu.Lock()
		state := &room.p2p
		resetProbe := func(memberID uint8) {
			state.relayNextProbe[memberID] = time.Time{}
			state.relayPending[memberID] = 0
			state.relaySentAt[memberID] = time.Time{}
			state.relayRTT[memberID] = 0
		}
		for _, previous := range *room.peers.Load() {
			unchanged := false
			for _, peer := range peers {
				if peer.ID == previous.ID && peer.Name == previous.Name && peer.Online == previous.Online {
					unchanged = true
					break
				}
			}
			if !unchanged {
				resetProbe(previous.ID)
			}
		}
		room.peers.Store(&peers)
		c.sendMu.Unlock()
		c.emit(Event{Type: EventPeers, Peers: msg.Members})
	case protocol.KindPong:
		var msg protocol.PingMessage
		if err := protocol.DecodeControl(frame.Payload, &msg); err != nil {
			return
		}
		rtt := time.Since(c.start) - time.Duration(msg.SentNS)
		if rtt < 0 || msg.SentNS < 0 {
			return
		}
		rtt = max(rtt, time.Nanosecond)
		room.rttNanos.Store(int64(rtt))
		c.emit(Event{Type: EventRTT, RTT: rtt})
	case protocol.KindPathPing:
		if !validMember(frame.SourceMember) || frame.SourceMember == room.memberID || frame.FragmentCount != 1 || len(frame.Payload) != 8 {
			return
		}
		c.sendMu.Lock()
		if !c.closed.Load() && c.room.Load() == room {
			c.writeFrame(room, &protocol.Frame{
				Kind: protocol.KindPathPong, TargetMember: frame.SourceMember, Payload: frame.Payload,
			})
		}
		c.sendMu.Unlock()
	case protocol.KindPathPong:
		if !validMember(frame.SourceMember) || frame.SourceMember == room.memberID || frame.FragmentCount != 1 || len(frame.Payload) != 8 {
			return
		}
		probe := binary.BigEndian.Uint64(frame.Payload)
		c.sendMu.Lock()
		state := &room.p2p
		if c.room.Load() == room && state.relayPending[frame.SourceMember] == probe && !state.relaySentAt[frame.SourceMember].IsZero() {
			state.relayRTT[frame.SourceMember] = max(time.Since(state.relaySentAt[frame.SourceMember]), time.Nanosecond)
			state.relayPending[frame.SourceMember] = 0
			state.relaySentAt[frame.SourceMember] = time.Time{}
		}
		c.sendMu.Unlock()
	case protocol.KindRoomClosed:
		var msg protocol.RoomClosedMessage
		_ = protocol.DecodeControl(frame.Payload, &msg)
		c.room.Store(nil)
		c.emit(Event{Type: EventRoomClosed, Reason: msg.Reason})
	case protocol.KindP2PState:
		c.handleP2PState(room, frame)
	case protocol.KindData:
		c.deliverGameData(room, frame)
	}
}

func (c *Client) deliverGameData(room *clientRoom, frame *protocol.Frame) {
	payload, err := room.reassembler.Add(frame)
	if err != nil {
		c.counter.dropped.Add(1)
		return
	}
	if payload == nil {
		return
	}
	c.counter.gameRxDatagrams.Add(1)
	c.handlerMu.RLock()
	h := c.dataHandler
	c.handlerMu.RUnlock()
	if h != nil {
		if frame.FragmentCount == 1 {
			payload = bytes.Clone(payload)
		}
		h(frame.SourceMember, frame.SourcePort, frame.TargetPort, payload)
	}
}

func (c *Client) heartbeatLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	punch := time.NewTicker(p2pTick)
	defer punch.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-c.probe:
			ticker.Reset(keepaliveInterval)
			c.sendProbe()
			c.stepP2P(true)
		case <-ticker.C:
			c.sendProbe()
		case <-c.p2pWake:
			c.stepP2P(true)
		case <-punch.C:
			c.stepP2P(false)
		}
	}
}

func (c *Client) sendProbe() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	room := c.room.Load()
	if room == nil || c.closed.Load() {
		return
	}
	if expired := room.reassembler.Expire(); expired != 0 {
		c.counter.dropped.Add(uint64(expired))
	}
	payload, _ := protocol.EncodeControl(protocol.PingMessage{SentNS: time.Since(c.start).Nanoseconds()})
	c.writeFrame(room, &protocol.Frame{Kind: protocol.KindPing, Payload: payload})
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
