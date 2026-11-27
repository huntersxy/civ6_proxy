package tunnel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"net"
	"net/netip"
	"time"

	"civ6relay/internal/protocol"
)

const (
	p2pTick                = 500 * time.Millisecond
	p2pPollEvery           = 2 * time.Second
	p2pProbeEvery          = time.Second
	p2pRetryEvery          = 3 * time.Second
	p2pPathTimeout         = 4 * time.Second
	p2pRelayPreferMargin   = 15 * time.Millisecond
	p2pDirectRestoreMargin = 5 * time.Millisecond
	p2pProbeHistory        = 8
	maxMemberID            = 9
)

type PeerPath uint8

const (
	ViaRelay PeerPath = iota
	Punching
	Direct
)

type P2PStatus struct {
	Supported bool
	Checking  bool
	Enabled   bool
	Pending   bool
	LocalPort uint16
	Endpoints []netip.AddrPort
	Peers     [maxMemberID + 1]PeerPath
	PeerRTT   [maxMemberID + 1]time.Duration
	RelayRTT  [maxMemberID + 1]time.Duration
}

type p2pRoom struct {
	supported      bool
	enabled        bool
	requests       int
	seq            uint64
	proof          []byte
	mode           protocol.P2PMode
	modeSeq        uint64
	pendingMode    bool
	pendingEnabled bool
	nextPoll       time.Time
	leaseUntil     time.Time
	paths          [maxMemberID + 1]*p2pPath
	relayNextProbe [maxMemberID + 1]time.Time
	relayPending   [maxMemberID + 1]uint64
	relaySentAt    [maxMemberID + 1]time.Time
	relayRTT       [maxMemberID + 1]time.Duration
}

type p2pPath struct {
	member        uint8
	address       netip.AddrPort
	key           [protocol.P2PKeySize]byte
	txAuth        *protocol.Authenticator
	rxAuth        *protocol.Authenticator
	seq           *protocol.Counter
	replay        protocol.ReplayWindow
	started       time.Time
	nextProbe     time.Time
	probes        [p2pProbeHistory]peerProbe
	probeCursor   uint8
	rtt           time.Duration
	verifiedUntil time.Time
	preferRelay   bool
}

type peerProbe struct {
	sequence uint64
	sentAt   time.Time
}

func (c *Client) P2PStatus() P2PStatus {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	var status P2PStatus
	room := c.room.Load()
	if room == nil || c.closed.Load() {
		return status
	}
	state := &room.p2p
	status.LocalPort = uint16(c.conn.LocalAddr().(*net.UDPAddr).Port)
	status.Supported = state.supported
	status.Checking = !state.supported && state.requests < 3
	status.Enabled = state.enabled
	status.RelayRTT = state.relayRTT
	if state.mode != protocol.P2PPoll {
		status.Pending = true
		status.Enabled = state.pendingMode && state.pendingEnabled
	}
	now := time.Now()
	if state.usable(now) {
		for memberID, path := range state.paths {
			if path == nil {
				continue
			}
			status.Endpoints = append(status.Endpoints, path.address)
			if path.activeDirect(now) {
				status.Peers[memberID] = Direct
				status.PeerRTT[memberID] = path.rtt
			} else if !path.ready(now) && now.Sub(path.started) < p2pPathTimeout {
				status.Peers[memberID] = Punching
			}
		}
	}
	return status
}

func (c *Client) SetP2PEnabled(enabled bool) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	room := c.room.Load()
	if room == nil || c.closed.Load() || !room.isHost {
		return &Error{Code: "p2p_host_required", Message: "只有房主能更改 P2P 直连设置。"}
	}
	state := &room.p2p
	if !state.supported {
		return &Error{Code: "p2p_unavailable", Message: "当前中转未确认支持 P2P 直连。"}
	}
	state.mode, state.modeSeq = protocol.P2POff, 0
	if enabled {
		state.mode = protocol.P2POn
	}
	state.pendingMode, state.pendingEnabled = true, enabled
	c.wakeP2P()
	return nil
}

func (state *p2pRoom) usable(now time.Time) bool {
	return state.enabled && state.mode != protocol.P2POff && now.Before(state.leaseUntil)
}

func (path *p2pPath) ready(now time.Time) bool { return now.Before(path.verifiedUntil) }

func (path *p2pPath) activeDirect(now time.Time) bool {
	return path.ready(now) && !path.preferRelay
}

func (state *p2pRoom) updatePathPreference(memberID uint8) {
	if !validMember(memberID) {
		return
	}
	path := state.paths[memberID]
	if path == nil {
		return
	}
	relayRTT := state.relayRTT[memberID]
	if relayRTT <= 0 || path.rtt <= 0 {
		path.preferRelay = false
		return
	}
	if !path.preferRelay {
		if path.rtt > relayRTT+p2pRelayPreferMargin && path.rtt*4 > relayRTT*5 {
			path.preferRelay = true
		}
	} else if path.rtt <= relayRTT+p2pDirectRestoreMargin {
		path.preferRelay = false
	}
}

func validMember(id uint8) bool { return id >= 2 && id <= maxMemberID }

func (c *Client) handleP2PState(room *clientRoom, frame *protocol.Frame) {
	if frame.SourceMember != 0 || frame.FragmentCount != 1 {
		return
	}
	var reply protocol.P2PState
	if protocol.DecodeControl(frame.Payload, &reply) != nil || reply.Request == 0 || len(reply.Peers) > maxMemberID-2 ||
		(len(reply.Challenge) != 0 && len(reply.Challenge) != protocol.P2PProofSize) {
		return
	}
	c.sendMu.Lock()
	if c.closed.Load() || c.room.Load() != room || frame.Sequence <= room.p2p.seq {
		c.sendMu.Unlock()
		return
	}
	state := &room.p2p
	state.seq = frame.Sequence
	if len(reply.Challenge) != 0 {
		state.leaseUntil = time.Time{}
		state.proof = reply.Challenge
		state.nextPoll = time.Time{}
		c.wakeP2P()
		c.sendMu.Unlock()
		return
	}
	prevSupported, prevEnabled := state.supported, state.enabled
	state.supported, state.enabled, state.proof = true, reply.Enabled, nil
	if state.mode != protocol.P2PPoll && state.modeSeq != 0 && reply.Request >= state.modeSeq &&
		reply.Enabled == (state.mode == protocol.P2POn) {
		state.mode, state.modeSeq = protocol.P2PPoll, 0
		state.pendingMode = false
	}
	now := time.Now()
	state.leaseUntil = now.Add(protocol.P2PLease)
	var paths [maxMemberID + 1]*p2pPath
	if reply.Enabled {
		for _, peer := range reply.Peers {
			if !validMember(peer.MemberID) || peer.MemberID == room.memberID || len(peer.Key) != protocol.P2PKeySize ||
				!c.validPeerAddress(peer.Endpoint) || paths[peer.MemberID] != nil {
				continue
			}
			key := [protocol.P2PKeySize]byte(peer.Key)
			path := state.paths[peer.MemberID]
			if path == nil || path.address != peer.Endpoint || path.key != key {
				path = &p2pPath{
					member: peer.MemberID, address: peer.Endpoint, key: key, seq: protocol.NewCounter(), started: now,
					txAuth: peerAuthenticator(key, room.memberID), rxAuth: peerAuthenticator(key, peer.MemberID),
				}
			}
			paths[peer.MemberID] = path
		}
	}
	state.paths = paths
	c.sendMu.Unlock()
	if !prevSupported || prevEnabled != reply.Enabled {
		c.emit(Event{Type: EventP2P})
	}
}

func (c *Client) validPeerAddress(addr netip.AddrPort) bool {
	ip := addr.Addr()
	return addr != c.relay && addr.Port() != 0 && ip.Is4() && (ip.IsGlobalUnicast() || (ip.IsLoopback() && c.relay.Addr().IsLoopback()))
}

func peerAuthenticator(key [protocol.P2PKeySize]byte, sender uint8) *protocol.Authenticator {
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte{sender})
	return protocol.NewAuthenticator(mac.Sum(nil))
}

func (c *Client) wakeP2P() {
	select {
	case c.p2pWake <- struct{}{}:
	default:
	}
}

func (c *Client) stepP2P(immediate bool) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	room := c.room.Load()
	if room == nil || c.closed.Load() {
		return
	}
	state, now := &room.p2p, time.Now()
	if immediate || !now.Before(state.nextPoll) {
		payload, _ := protocol.EncodeControl(protocol.P2PRequest{Mode: state.mode, Proof: state.proof})
		frame := protocol.Frame{Kind: protocol.KindP2PRequest, Payload: payload}
		c.writeFrame(room, &frame)
		if state.mode != protocol.P2PPoll && state.modeSeq == 0 {
			state.modeSeq = frame.Sequence
		}
		state.requests++
		state.nextPoll = now.Add(p2pPollEvery)
	}
	c.stepRelayProbes(room, state, now)
	if !state.usable(now) {
		return
	}
	for _, path := range state.paths {
		if path == nil || now.Before(path.nextProbe) {
			continue
		}
		frame := protocol.Frame{Kind: protocol.KindPeerProbe}
		sentAt := time.Now()
		if c.writePeerFrame(room, path, &frame) {
			path.probes[path.probeCursor] = peerProbe{sequence: frame.Sequence, sentAt: sentAt}
			path.probeCursor = (path.probeCursor + 1) % p2pProbeHistory
		}
		interval := p2pProbeEvery
		if !path.ready(now) && now.Sub(path.started) >= p2pPathTimeout {
			interval = p2pRetryEvery
		}
		path.nextProbe = now.Add(interval)
	}
}

func (c *Client) stepRelayProbes(room *clientRoom, state *p2pRoom, now time.Time) {
	peers := room.peers.Load()
	if peers == nil {
		return
	}
	for _, peer := range *peers {
		if !validMember(peer.ID) || peer.ID == room.memberID {
			continue
		}
		if !peer.Online {
			state.relayNextProbe[peer.ID] = time.Time{}
			state.relayPending[peer.ID] = 0
			state.relayRTT[peer.ID] = 0
			continue
		}
		if state.relayNextProbe[peer.ID].IsZero() {
			state.relayNextProbe[peer.ID] = now.Add(time.Duration(peer.ID-firstProbeMemberID) * 250 * time.Millisecond)
			continue
		}
		if now.Before(state.relayNextProbe[peer.ID]) {
			continue
		}
		probe := uint64(time.Since(c.start).Nanoseconds())
		var payload [8]byte
		binary.BigEndian.PutUint64(payload[:], probe)
		frame := protocol.Frame{Kind: protocol.KindPathPing, TargetMember: peer.ID, Payload: payload[:]}
		sentAt := time.Now()
		if c.writeFrame(room, &frame) {
			state.relayPending[peer.ID] = probe
			state.relaySentAt[peer.ID] = sentAt
		}
		state.relayNextProbe[peer.ID] = now.Add(2 * time.Second)
		return
	}
}

const firstProbeMemberID = 2

func (c *Client) writePeerFrame(room *clientRoom, path *p2pPath, frame *protocol.Frame) bool {
	frame.RoomID = room.roomID
	frame.SourceMember = room.memberID
	frame.TargetMember = path.member
	frame.Sequence = path.seq.Next()
	raw, err := frame.AppendEncode(c.sendBuffer[:0], path.txAuth)
	return err == nil && c.writeTo(path.address, raw)
}

func (c *Client) handlePeerDatagram(address netip.AddrPort, raw []byte) {
	frame, err := protocol.DecodeView(raw)
	if err != nil || !validMember(frame.SourceMember) || frame.Flags&protocol.FlagBroadcast != 0 {
		return
	}
	switch frame.Kind {
	case protocol.KindData:
	case protocol.KindPeerProbe:
		if frame.FragmentCount != 1 || len(frame.Payload) != 0 {
			return
		}
	case protocol.KindPeerAck:
		if frame.FragmentCount != 1 || len(frame.Payload) != 8 {
			return
		}
	case protocol.KindPathPing, protocol.KindPathPong:
		if frame.FragmentCount != 1 || len(frame.Payload) != 8 {
			return
		}
	default:
		return
	}
	c.sendMu.Lock()
	room := c.room.Load()
	now := time.Now()
	if c.closed.Load() || room == nil || frame.RoomID != room.roomID || frame.TargetMember != room.memberID || !room.p2p.usable(now) {
		c.sendMu.Unlock()
		return
	}
	path := room.p2p.paths[frame.SourceMember]
	if path == nil || path.address != address || !path.rxAuth.Verify(raw) || !path.replay.Accept(frame.Sequence) {
		c.sendMu.Unlock()
		return
	}
	c.counter.tunnelRxPackets.Add(1)
	c.counter.tunnelRxBytes.Add(uint64(len(raw)))
	switch frame.Kind {
	case protocol.KindPeerProbe:
		var payload [8]byte
		binary.BigEndian.PutUint64(payload[:], frame.Sequence)
		c.writePeerFrame(room, path, &protocol.Frame{Kind: protocol.KindPeerAck, Payload: payload[:]})
	case protocol.KindPeerAck:
		sequence := binary.BigEndian.Uint64(frame.Payload)
		for index, probe := range path.probes {
			if probe.sequence == sequence && !probe.sentAt.IsZero() {
				path.probes[index] = peerProbe{}
				path.rtt = max(now.Sub(probe.sentAt), time.Nanosecond)
				path.verifiedUntil = now.Add(p2pPathTimeout)
				room.p2p.updatePathPreference(frame.SourceMember)
				break
			}
		}
	}
	c.sendMu.Unlock()
	if frame.Kind == protocol.KindData {
		c.counter.directRxFrames.Add(1)
		c.deliverGameData(room, &frame)
	}
}
