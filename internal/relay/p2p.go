package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"net/netip"

	"civ6relay/internal/protocol"
)

type p2pMember struct {
	endpoint        netip.AddrPort
	expires         float64
	generation      uint64
	requestSeq      uint64
	challenge       [protocol.P2PProofSize]byte
	challengeTarget netip.AddrPort
	challengeExpiry float64
	limit           tokenBucket
}

func newP2PMember() p2pMember {
	return p2pMember{limit: newTokenBucket(4, 2048, 2)}
}

func (r *room) handleP2PLocked(member *member, frame *protocol.Frame, addr netip.AddrPort, size int, now float64, batch *Batch) {
	state := &member.p2p
	if frame.TargetMember != 0 || frame.FragmentCount != 1 || frame.Sequence < state.requestSeq || !state.limit.allow(size, now) {
		return
	}
	var request protocol.P2PRequest
	if protocol.DecodeControl(frame.Payload, &request) != nil || request.Mode > protocol.P2POn ||
		(len(request.Proof) != 0 && len(request.Proof) != protocol.P2PProofSize) {
		return
	}
	state.requestSeq = frame.Sequence
	if state.endpoint != addr || now >= state.expires {
		if state.challengeTarget != addr || now >= state.challengeExpiry {
			copy(state.challenge[:], protocol.RandomBytes(len(state.challenge)))
			state.challengeTarget = addr
			state.challengeExpiry = now + protocol.P2PLease.Seconds()
		}
		if !hmac.Equal(state.challenge[:], request.Proof) {
			r.sendP2PState(member, addr, protocol.P2PState{Request: frame.Sequence, Challenge: state.challenge[:]}, batch)
			return
		}
		state.endpoint = addr
		state.generation++
		state.challengeTarget = netip.AddrPort{}
	}
	state.expires = now + protocol.P2PLease.Seconds()
	var zero [protocol.P2PKeySize]byte
	if member.isHost {
		switch request.Mode {
		case protocol.P2POff:
			r.p2pSeed = zero
			r.broadcastP2PDisabled(member, batch)
		case protocol.P2POn:
			if r.p2pSeed == zero {
				copy(r.p2pSeed[:], protocol.RandomBytes(len(r.p2pSeed)))
			}
		}
	}
	reply := protocol.P2PState{Request: frame.Sequence, Enabled: r.p2pSeed != zero}
	if reply.Enabled {
		for _, peer := range r.members {
			if peer == nil || peer == member || !peer.online || peer.p2p.expires <= now ||
				peer.p2p.endpoint == addr || !eligibleP2PEndpoint(addr, peer.p2p.endpoint) {
				continue
			}
			reply.Peers = append(reply.Peers, protocol.P2PCandidate{
				MemberID: peer.id, Endpoint: peer.p2p.endpoint, Key: r.p2pKey(member, peer),
			})
		}
	}
	r.sendP2PState(member, addr, reply, batch)
}

func (r *room) broadcastP2PDisabled(host *member, batch *Batch) {
	for _, peer := range r.members {
		if peer == nil || peer == host || !peer.online {
			continue
		}
		payload, err := protocol.EncodeControl(protocol.P2PState{Request: peer.p2p.requestSeq})
		if err == nil {
			frame := protocol.Frame{Kind: protocol.KindP2PState, RoomID: r.id, Payload: payload}
			batch.appendSigned(peer, frame)
		}
	}
}

func eligibleP2PEndpoint(local, remote netip.AddrPort) bool {
	ip := remote.Addr()
	return remote.Port() != 0 && ip.Is4() && (ip.IsGlobalUnicast() || (ip.IsLoopback() && local.Addr().IsLoopback()))
}

func (r *room) p2pKey(first, second *member) []byte {
	if first.id > second.id {
		first, second = second, first
	}
	var material [80]byte
	copy(material[:32], first.token[:])
	copy(material[32:64], second.token[:])
	binary.BigEndian.PutUint64(material[64:72], first.p2p.generation)
	binary.BigEndian.PutUint64(material[72:], second.p2p.generation)
	mac := hmac.New(sha256.New, r.p2pSeed[:])
	_, _ = mac.Write(material[:])
	return mac.Sum(nil)
}

func (r *room) sendP2PState(member *member, addr netip.AddrPort, state protocol.P2PState, batch *Batch) {
	payload, err := protocol.EncodeControl(state)
	if err == nil {
		batch.appendSignedTo(member, addr, protocol.Frame{Kind: protocol.KindP2PState, RoomID: r.id, Payload: payload})
	}
}
