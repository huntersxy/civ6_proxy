package relay

import (
	"net"
	"sort"

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
	Addr *net.UDPAddr
	Data []byte
}

type member struct {
	id       uint8
	name     string
	token    [32]byte
	addr     *net.UDPAddr
	isHost   bool
	lastSeen float64
	online   bool
	replay   protocol.ReplayWindow
	outSeq   *protocol.Counter
	limit    tokenBucket
}

func newMember(id uint8, name string, token [32]byte, addr *net.UDPAddr, host bool, now float64) *member {
	return &member{
		id:       id,
		name:     name,
		token:    token,
		addr:     cloneAddr(addr),
		isHost:   host,
		lastSeen: now,
		online:   true,
		outSeq:   protocol.NewCounter(),
		limit:    newTokenBucket(5000, 1_250_000, 2),
	}
}

type room struct {
	id        uint64
	code      string
	members   map[uint8]*member
	createdAt float64
}

func (r *room) sortedIDs() []uint8 {
	ids := make([]uint8, 0, len(r.members))
	for id := range r.members {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (r *room) nextMemberID() (uint8, bool) {
	for id := firstMemberID; id < firstMemberID+maxRoomMembers; id++ {
		if _, exists := r.members[uint8(id)]; !exists {
			return uint8(id), true
		}
	}
	return 0, false
}

func (r *room) host() *member {
	for _, m := range r.members {
		if m.isHost {
			return m
		}
	}
	return nil
}

func (r *room) peerList() []protocol.Peer {
	peers := make([]protocol.Peer, 0, len(r.members))
	for _, id := range r.sortedIDs() {
		m := r.members[id]
		peers = append(peers, protocol.Peer{ID: m.id, Name: m.name, Host: m.isHost, Online: m.online})
	}
	return peers
}

func cloneAddr(a *net.UDPAddr) *net.UDPAddr {
	if a == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), a.IP...), Port: a.Port, Zone: a.Zone}
}
