package relay

import (
	"net/netip"

	"civ6relay/internal/protocol"
)

func (s *Server) handleAuthenticated(r *room, frame *protocol.Frame, addr netip.AddrPort, raw []byte, batch *Batch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.member(frame.SourceMember)
	if r.closed || m == nil {
		s.drops.unknownRoom.Add(1)
		return
	}
	if !m.auth.Verify(raw) {
		s.drops.authFailures.Add(1)
		return
	}
	if !m.replay.Accept(frame.Sequence) {
		s.drops.replayDrops.Add(1)
		return
	}
	now := s.clock()
	switch frame.Kind {
	case protocol.KindPing, protocol.KindLeave, protocol.KindData, protocol.KindP2PRequest,
		protocol.KindPathPing, protocol.KindPathPong:
	default:
		return
	}
	becameOnline := !m.online
	m.addr = addr
	m.lastSeen = now
	m.online = true
	if becameOnline {
		s.peerUpdatesLocked(r, batch)
	}
	if frame.Kind == protocol.KindP2PRequest {
		r.handleP2PLocked(m, frame, addr, len(raw), now, batch)
		return
	}
	if frame.Kind == protocol.KindPathPing || frame.Kind == protocol.KindPathPong {
		r.routePathProbeLocked(m, frame, now, batch)
		return
	}
	switch frame.Kind {
	case protocol.KindPing:
		pong := protocol.Frame{
			Kind:         protocol.KindPong,
			RoomID:       r.id,
			SourceMember: 0,
			Payload:      frame.Payload,
		}
		batch.appendSigned(m, pong)
	case protocol.KindLeave:
		if m.isHost {
			s.closeRoomLocked(r, "host_left", batch)
		} else {
			r.members[int(m.id)-int(firstMemberID)] = nil
			s.memberCount.Add(-1)
			s.peerUpdatesLocked(r, batch)
			s.log.Printf("member left room=%s member=%d", r.code, m.id)
		}
	case protocol.KindData:
		if m.limit.allow(len(raw), now) {
			r.routeDataLocked(m, frame, batch)
		} else {
			s.drops.rateDrops.Add(1)
		}
	}
}

func (r *room) routePathProbeLocked(sender *member, frame *protocol.Frame, now float64, batch *Batch) {
	if frame.FragmentCount != 1 || frame.Flags&protocol.FlagBroadcast != 0 || len(frame.Payload) != 8 ||
		frame.TargetMember < protocol.FirstMemberID || frame.TargetMember > protocol.LastMemberID ||
		frame.TargetMember == sender.id || !sender.pathLimit.allow(len(frame.Payload), now) {
		return
	}
	target := r.member(frame.TargetMember)
	if target == nil || !target.online {
		return
	}
	forwarded := protocol.Frame{
		Kind: frame.Kind, RoomID: r.id, SourceMember: sender.id, Payload: frame.Payload,
	}
	batch.appendSigned(target, forwarded)
}

func (r *room) routeDataLocked(sender *member, frame *protocol.Frame, batch *Batch) {
	forwarded := *frame
	forwarded.Flags &= protocol.FlagBroadcast | protocol.FlagFragmented | protocol.FlagCompressed
	if frame.TargetMember != broadcastMember {
		m := r.member(frame.TargetMember)
		if m != nil && m.online && m != sender {
			batch.appendSigned(m, forwarded)
		}
		return
	}
	for _, m := range r.members {
		if m != nil && m.online && m != sender {
			batch.appendSigned(m, forwarded)
		}
	}
}

func (s *Server) peerUpdatesLocked(r *room, batch *Batch) {
	payload, err := protocol.EncodeControl(protocol.PeerListMessage{Members: r.peerList()})
	if err != nil {
		return
	}
	chunks, err := protocol.SplitPayload(payload)
	if err != nil {
		return
	}
	packetID := r.nextPacketID()
	var flags protocol.Flags
	if len(chunks) > 1 {
		flags |= protocol.FlagFragmented
	}
	for _, m := range r.members {
		if m == nil || !m.online {
			continue
		}
		for index, chunk := range chunks {
			frame := protocol.Frame{
				Kind:          protocol.KindPeers,
				Flags:         flags,
				RoomID:        r.id,
				TargetMember:  m.id,
				FragmentIndex: uint16(index),
				FragmentCount: uint16(len(chunks)),
				PacketID:      packetID,
				Payload:       chunk,
			}
			batch.appendSigned(m, frame)
		}
	}
}

func (s *Server) closeRoomLocked(r *room, reason string, batch *Batch) {
	payload, err := protocol.EncodeControl(protocol.RoomClosedMessage{Reason: reason})
	if err != nil {
		return
	}
	frame := protocol.Frame{Kind: protocol.KindRoomClosed, RoomID: r.id, Payload: payload}
	var members int64
	for _, m := range r.members {
		if m != nil {
			members++
			if m.online {
				batch.appendSigned(m, frame)
			}
		}
	}
	r.closed = true
	s.roomsMu.Lock()
	delete(s.roomsByID, r.id)
	delete(s.roomsByCode, r.code)
	s.memberCount.Add(-members)
	s.roomsMu.Unlock()
	s.log.Printf("room closed room=%s reason=%s", r.code, reason)
}
