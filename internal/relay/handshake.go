package relay

import (
	"crypto/hmac"
	"encoding/base64"
	"fmt"
	"net"

	"civ6relay/internal/protocol"
)

func (s *Server) handleHandshakeLocked(frame *protocol.Frame, addr *net.UDPAddr) []Outbound {
	var req protocol.HandshakeRequest
	if err := protocol.DecodeControl(frame.Payload, &req); err != nil {
		return nil
	}
	requestID, err := protocol.NormalizeRequestID(req.RequestID)
	if err != nil {
		return nil
	}
	now := s.clock()
	key := handshakeKey{endpoint: addr.String(), kind: frame.Kind, requestID: requestID}
	if cached, ok := s.handshakeCache[key]; ok && cached.expires > now {
		return []Outbound{{Addr: addr, Data: cached.data}}
	}
	var out []Outbound
	if frame.Kind == protocol.KindCreate {
		out = s.createRoomLocked(&req, requestID, addr)
	} else {
		out = s.joinRoomLocked(&req, requestID, addr)
	}
	if len(out) > 0 {
		s.handshakeCache[key] = cachedHandshake{expires: now + handshakeTTLSec, data: out[0].Data}
	}
	return out
}

func (s *Server) createRoomLocked(req *protocol.HandshakeRequest, requestID string, addr *net.UDPAddr) []Outbound {
	name, err := protocol.NormalizeName(req.Name)
	if err != nil {
		return s.errorLocked(addr, requestID, "invalid_request", err.Error())
	}
	if len(s.roomsByID) >= s.maxRooms {
		return s.errorLocked(addr, requestID, "server_full", "relay has reached its room limit")
	}
	now := s.clock()
	roomID := s.newRoomIDLocked()
	code := s.newRoomCodeLocked()
	var token [32]byte
	copy(token[:], protocol.RandomBytes(32))
	host := newMember(firstMemberID, name, token, addr, true, now)
	r := &room{id: roomID, code: code, members: map[uint8]*member{host.id: host}, createdAt: now}
	s.roomsByID[roomID] = r
	s.roomsByCode[code] = r
	s.log.Printf("room created room=%s member=%d endpoint=%s", code, host.id, addr.IP)
	return s.welcomeLocked(r, host, requestID)
}

func (s *Server) joinRoomLocked(req *protocol.HandshakeRequest, requestID string, addr *net.UDPAddr) []Outbound {
	code, err := protocol.NormalizeRoomCode(req.RoomCode)
	if err != nil {
		return s.errorLocked(addr, requestID, "invalid_request", err.Error())
	}
	r := s.roomsByCode[code]
	if r == nil {
		return s.errorLocked(addr, requestID, "room_not_found", "room does not exist")
	}
	if resumed := s.tryResumeLocked(r, req, addr); resumed != nil {
		s.log.Printf("member resumed room=%s member=%d endpoint=%s", code, resumed.id, addr.IP)
		out := s.welcomeLocked(r, resumed, requestID)
		return append(out, s.peerUpdatesLocked(r)...)
	}
	host := r.host()
	if host == nil || !host.online {
		return s.errorLocked(addr, requestID, "host_offline", "room host is temporarily offline")
	}
	memberID, ok := r.nextMemberID()
	if !ok {
		return s.errorLocked(addr, requestID, "room_full", "room already has eight members")
	}
	name, err := protocol.NormalizeName(req.Name)
	if err != nil {
		return s.errorLocked(addr, requestID, "invalid_request", err.Error())
	}
	var token [32]byte
	copy(token[:], protocol.RandomBytes(32))
	m := newMember(memberID, name, token, addr, false, s.clock())
	r.members[memberID] = m
	s.log.Printf("member joined room=%s member=%d endpoint=%s", code, memberID, addr.IP)
	out := s.welcomeLocked(r, m, requestID)
	return append(out, s.peerUpdatesLocked(r)...)
}

func (s *Server) tryResumeLocked(r *room, req *protocol.HandshakeRequest, addr *net.UDPAddr) *member {
	if req.ResumeMember < firstMemberID || req.ResumeMember >= firstMemberID+maxRoomMembers {
		return nil
	}
	m := r.members[req.ResumeMember]
	if m == nil || req.ResumeToken == "" {
		return nil
	}
	token, err := decodeToken(req.ResumeToken)
	if err != nil || len(token) != len(m.token) || !hmac.Equal(token, m.token[:]) {
		return nil
	}
	var fresh [32]byte
	copy(fresh[:], protocol.RandomBytes(32))
	m.token = fresh
	m.addr = cloneAddr(addr)
	m.lastSeen = s.clock()
	m.online = true
	m.replay.Reset()
	m.outSeq = protocol.NewCounter()
	return m
}

func decodeToken(encoded string) ([]byte, error) {
	if b, err := base64.URLEncoding.DecodeString(encoded); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(encoded)
}

func (s *Server) welcomeLocked(r *room, m *member, requestID string) []Outbound {
	payload, err := protocol.EncodeControl(protocol.WelcomeMessage{
		RequestID: requestID,
		RoomCode:  r.code,
		RoomID:    fmt.Sprintf("%016x", r.id),
		MemberID:  m.id,
		Host:      m.isHost,
		Token:     base64.URLEncoding.EncodeToString(m.token[:]),
		Members:   r.peerList(),
	})
	if err != nil {
		return nil
	}
	frame := protocol.Frame{
		Kind:         protocol.KindWelcome,
		RoomID:       r.id,
		SourceMember: 0,
		TargetMember: m.id,
		Payload:      payload,
	}
	return s.appendUnsignedLocked(nil, m.addr, frame)
}

func (s *Server) errorLocked(addr *net.UDPAddr, requestID, code, message string) []Outbound {
	payload, err := protocol.EncodeControl(protocol.ErrorMessage{
		RequestID: requestID,
		Code:      code,
		Message:   message,
	})
	if err != nil {
		return nil
	}
	frame := protocol.Frame{Kind: protocol.KindError, Payload: payload}
	return s.appendUnsignedLocked(nil, addr, frame)
}
