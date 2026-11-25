package relay

import (
	"net/netip"
	"sync"

	"civ6relay/internal/protocol"
)

const (
	maxRoomMembers   = 8
	firstMemberID    = 2
	broadcastMember  = 255
	offlineAfterSec  = 30.0
	retentionSec     = 60.0
	handshakeTTLSec  = 10.0
	handshakeIdleSec = 300.0
)

// Outbound is one datagram to send to one endpoint.
type Outbound struct {
	Addr netip.AddrPort
	Data []byte
}

// Batch holds the results of one HandleInto call until it is used again.
type Batch struct {
	Datagrams []Outbound
	buffer    []byte
}

func (b *Batch) reset() {
	clear(b.Datagrams)
	b.Datagrams = b.Datagrams[:0]
	b.buffer = b.buffer[:0]
}

func (b *Batch) appendFrame(addr netip.AddrPort, frame protocol.Frame, auth *protocol.Authenticator) bool {
	offset := len(b.buffer)
	buffer, err := frame.AppendEncode(b.buffer, auth)
	if err != nil {
		return false
	}
	b.buffer = buffer
	b.Datagrams = append(b.Datagrams, Outbound{Addr: addr, Data: buffer[offset:len(buffer):len(buffer)]})
	return true
}

func (b *Batch) appendSigned(m *member, frame protocol.Frame) {
	b.appendSignedTo(m, m.addr, frame)
}

func (b *Batch) appendSignedTo(m *member, addr netip.AddrPort, frame protocol.Frame) {
	frame.TargetMember = m.id
	frame.Sequence = m.outSeq.Next()
	b.appendFrame(addr, frame, m.auth)
}

type member struct {
	id        uint8
	name      string
	token     [32]byte
	auth      *protocol.Authenticator
	addr      netip.AddrPort
	isHost    bool
	lastSeen  float64
	online    bool
	replay    protocol.ReplayWindow
	outSeq    *protocol.Counter
	limit     tokenBucket
	pathLimit tokenBucket
	p2p       p2pMember
}

func newMember(id uint8, name string, token [32]byte, addr netip.AddrPort, host bool, now float64, limit tokenBucket) *member {
	return &member{
		id:        id,
		name:      name,
		token:     token,
		auth:      protocol.NewAuthenticator(token[:]),
		addr:      addr,
		isHost:    host,
		lastSeen:  now,
		online:    true,
		outSeq:    protocol.NewCounter(),
		limit:     limit,
		pathLimit: newTokenBucket(20, 4096, 8),
		p2p:       newP2PMember(),
	}
}

type room struct {
	mu      sync.Mutex
	id      uint64
	code    string
	members [maxRoomMembers]*member
	closed  bool
	p2pSeed [protocol.P2PKeySize]byte
}

func (r *room) member(id uint8) *member {
	slot := int(id) - firstMemberID
	if slot < 0 || slot >= len(r.members) {
		return nil
	}
	return r.members[slot]
}

func (r *room) nextSlot() int {
	for slot, m := range r.members {
		if m == nil {
			return slot
		}
	}
	return -1
}

func (r *room) peerList() []protocol.Peer {
	peers := make([]protocol.Peer, 0, len(r.members))
	for _, m := range r.members {
		if m != nil {
			peers = append(peers, protocol.Peer{ID: m.id, Name: m.name, Host: m.isHost, Online: m.online})
		}
	}
	return peers
}
