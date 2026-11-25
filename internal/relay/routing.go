package relay

import (
	"net"

	"civ6relay/internal/protocol"
)

func (s *Server) handleAuthenticatedLocked(frame *protocol.Frame, addr *net.UDPAddr, datagramSize int) []Outbound {
	r := s.roomsByID[frame.RoomID]
	if r == nil {
		return nil
	}
	m := r.members[frame.SourceMember]
	if m == nil {
		return nil
	}
	if !frame.Verified(m.token[:]) || !m.replay.Accept(frame.Sequence) {
		return nil
	}
	becameOnline := !m.online
	m.addr = cloneAddr(addr)
	m.lastSeen = s.clock()
	m.online = true
	var out []Outbound
	if becameOnline {
		out = s.peerUpdatesLocked(r)
	}
	switch frame.Kind {
	case protocol.KindPing:
		pong := protocol.Frame{
			Kind:         protocol.KindPong,
			RoomID:       r.id,
			SourceMember: 0,
			Payload:      frame.Payload,
		}
		out = s.appendSignedLocked(out, m, pong)
	case protocol.KindLeave:
		if m.isHost {
			out = append(out, s.closeRoomLocked(r, "host_left")...)
		} else {
			delete(r.members, m.id)
			out = append(out, s.peerUpdatesLocked(r)...)
			s.log.Printf("member left room=%s member=%d", r.code, m.id)
		}
	case protocol.KindData:
		if m.limit.allow(datagramSize, s.clock()) {
			out = append(out, s.routeDataLocked(r, m, frame)...)
		}
	}
	return out
}

func (s *Server) routeDataLocked(r *room, sender *member, frame *protocol.Frame) []Outbound {
	var targets []*member
	if frame.TargetMember == broadcastMember {
		for _, id := range r.sortedIDs() {
			m := r.members[id]
			if m.id != sender.id && m.online {
				targets = append(targets, m)
			}
		}
	} else {
		m := r.members[frame.TargetMember]
		if m != nil && m.online && m.id != sender.id {
			targets = append(targets, m)
		}
	}
	var out []Outbound
	for _, target := range targets {
		forwarded := protocol.Frame{
			Kind:          protocol.KindData,
			Flags:         frame.Flags & (protocol.FlagBroadcast | protocol.FlagFragmented),
			RoomID:        r.id,
			SourceMember:  sender.id,
			PacketID:      frame.PacketID,
			FragmentIndex: frame.FragmentIndex,
			FragmentCount: frame.FragmentCount,
			SourcePort:    frame.SourcePort,
			TargetPort:    frame.TargetPort,
			Payload:       frame.Payload,
		}
		out = s.appendSignedLocked(out, target, forwarded)
	}
	return out
}

func (s *Server) peerUpdatesLocked(r *room) []Outbound {
	payload, err := protocol.EncodeControl(protocol.PeerListMessage{Members: r.peerList()})
	if err != nil {
		return nil
	}
	var out []Outbound
	for _, id := range r.sortedIDs() {
		m := r.members[id]
		if !m.online {
			continue
		}
		frame := protocol.Frame{
			Kind:         protocol.KindPeers,
			RoomID:       r.id,
			SourceMember: 0,
			Payload:      payload,
		}
		out = s.appendSignedLocked(out, m, frame)
	}
	return out
}

func (s *Server) closeRoomLocked(r *room, reason string) []Outbound {
	if s.roomsByID[r.id] != r {
		return nil
	}
	payload, err := protocol.EncodeControl(protocol.RoomClosedMessage{Reason: reason})
	if err != nil {
		return nil
	}
	var out []Outbound
	for _, id := range r.sortedIDs() {
		m := r.members[id]
		if !m.online {
			continue
		}
		frame := protocol.Frame{
			Kind:         protocol.KindRoomClosed,
			RoomID:       r.id,
			SourceMember: 0,
			Payload:      payload,
		}
		out = s.appendSignedLocked(out, m, frame)
	}
	delete(s.roomsByID, r.id)
	delete(s.roomsByCode, r.code)
	s.log.Printf("room closed room=%s reason=%s", r.code, reason)
	return out
}
