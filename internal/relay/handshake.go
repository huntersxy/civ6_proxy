package relay

import (
	"bytes"
	"crypto/hmac"
	"encoding/base64"
	"fmt"
	"net/netip"

	"civ6relay/internal/protocol"
)

func (s *Server) handleHandshake(frame *protocol.Frame, addr netip.AddrPort, size int, batch *Batch) {
	s.handshakeMu.Lock()
	defer s.handshakeMu.Unlock()
	now := s.clock()
	if !s.allowHandshakeLocked(addr.Addr(), size, now) {
		return
	}
	var req protocol.HandshakeRequest
	if err := protocol.DecodeControl(frame.Payload, &req); err != nil {
		return
	}
	requestID, err := protocol.NormalizeRequestID(req.RequestID)
	if err != nil {
		return
	}
	key := handshakeKey{endpoint: addr, kind: frame.Kind, requestID: requestID}
	if cached, ok := s.handshakeCache[key]; ok && cached.expires > now {
		for _, dg := range cached.datagrams {
			offset := len(batch.buffer)
			batch.buffer = append(batch.buffer, dg...)
			batch.Datagrams = append(batch.Datagrams, Outbound{Addr: addr, Data: batch.buffer[offset:len(batch.buffer):len(batch.buffer)]})
		}
		return
	}
	if frame.Kind == protocol.KindCreate {
		s.createRoom(&req, requestID, addr, batch)
	} else {
		s.joinRoom(&req, requestID, addr, batch)
	}
	var cachedDatagrams [][]byte
	for _, out := range batch.Datagrams {
		if out.Addr == addr && len(out.Data) > 5 &&
			(out.Data[5] == byte(protocol.KindWelcome) || out.Data[5] == byte(protocol.KindError)) {
			cachedDatagrams = append(cachedDatagrams, bytes.Clone(out.Data))
		}
	}
	if len(cachedDatagrams) > 0 {
		s.handshakeCache[key] = cachedHandshake{expires: now + handshakeTTLSec, datagrams: cachedDatagrams}
	}
}

func (s *Server) createRoom(req *protocol.HandshakeRequest, requestID string, addr netip.AddrPort, batch *Batch) {
	name, err := protocol.NormalizeName(req.Name)
	if err != nil {
		batch.appendError(addr, requestID, "invalid_request", err.Error())
		return
	}
	if req.MaxMembers < 0 || req.MaxMembers > protocol.MaxRoomMembers {
		batch.appendError(addr, requestID, "invalid_request", fmt.Sprintf("max_members must be 0 (unlimited) or 1..%d", protocol.MaxRoomMembers))
		return
	}
	s.roomsMu.Lock()
	if len(s.roomsByID) >= s.maxRooms {
		s.roomsMu.Unlock()
		batch.appendError(addr, requestID, "server_full", "relay has reached its room limit")
		return
	}
	now := s.clock()
	roomID := s.newRoomIDLocked()
	code := s.newRoomCodeLocked()
	var token [32]byte
	copy(token[:], protocol.RandomBytes(32))
	host := newMember(firstMemberID, name, token, addr, true, now, s.limit)
	r := &room{id: roomID, code: code, maxMembers: req.MaxMembers, members: []*member{host}}
	r.appendWelcome(host, requestID, batch)
	s.roomsByID[roomID] = r
	s.roomsByCode[code] = r
	s.memberCount.Add(1)
	s.roomsMu.Unlock()
	s.log.Printf("room created room=%s member=%d max=%d endpoint=%s", code, host.id, req.MaxMembers, addr.Addr())
}

func (s *Server) joinRoom(req *protocol.HandshakeRequest, requestID string, addr netip.AddrPort, batch *Batch) {
	code, err := protocol.NormalizeRoomCode(req.RoomCode)
	if err != nil {
		batch.appendError(addr, requestID, "invalid_request", err.Error())
		return
	}
	s.roomsMu.RLock()
	r := s.roomsByCode[code]
	s.roomsMu.RUnlock()
	if r == nil {
		batch.appendError(addr, requestID, "room_not_found", "room does not exist")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		batch.appendError(addr, requestID, "room_not_found", "room does not exist")
		return
	}
	if resumed := s.tryResumeLocked(r, req, addr); resumed != nil {
		s.log.Printf("member resumed room=%s member=%d endpoint=%s", code, resumed.id, addr.Addr())
		r.appendWelcome(resumed, requestID, batch)
		s.peerUpdatesLocked(r, batch)
		return
	}
	if !r.members[0].online {
		batch.appendError(addr, requestID, "host_offline", "room host is temporarily offline")
		return
	}
	slot := r.nextSlot()
	if slot < 0 {
		batch.appendError(addr, requestID, "room_full", fmt.Sprintf("room is full (limit %d)", r.effectiveLimit()))
		return
	}
	name, err := protocol.NormalizeName(req.Name)
	if err != nil {
		batch.appendError(addr, requestID, "invalid_request", err.Error())
		return
	}
	var token [32]byte
	copy(token[:], protocol.RandomBytes(32))
	m := newMember(uint8(slot+int(firstMemberID)), name, token, addr, false, s.clock(), s.limit)
	r.members[slot] = m
	if !r.appendWelcome(m, requestID, batch) {
		r.members[slot] = nil
		batch.appendError(addr, requestID, "roster_too_large", "member names exceed the room roster size limit")
		return
	}
	s.memberCount.Add(1)
	s.log.Printf("member joined room=%s member=%d endpoint=%s", code, m.id, addr.Addr())
	s.peerUpdatesLocked(r, batch)
}

func (s *Server) tryResumeLocked(r *room, req *protocol.HandshakeRequest, addr netip.AddrPort) *member {
	m := r.member(req.ResumeMember)
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
	m.auth = protocol.NewAuthenticator(m.token[:])
	m.addr = addr
	m.lastSeen = s.clock()
	m.online = true
	m.replay.Reset()
	m.outSeq = protocol.NewCounter()
	m.p2p = newP2PMember()
	return m
}

func decodeToken(encoded string) ([]byte, error) {
	if b, err := base64.URLEncoding.DecodeString(encoded); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(encoded)
}

func (r *room) appendWelcome(m *member, requestID string, batch *Batch) bool {
	payload, err := protocol.EncodeControl(protocol.WelcomeMessage{
		RequestID:  requestID,
		RoomCode:   r.code,
		RoomID:     fmt.Sprintf("%016x", r.id),
		MemberID:   m.id,
		Host:       m.isHost,
		MaxMembers: r.maxMembers,
		Token:      base64.URLEncoding.EncodeToString(m.token[:]),
		Members:    r.peerList(),
	})
	if err != nil {
		return false
	}
	return r.appendControl(m, m.addr, protocol.KindWelcome, payload, false, batch)
}

func (b *Batch) appendError(addr netip.AddrPort, requestID, code, message string) {
	payload, err := protocol.EncodeControl(protocol.ErrorMessage{
		RequestID: requestID,
		Code:      code,
		Message:   message,
	})
	if err != nil {
		return
	}
	frame := protocol.Frame{Kind: protocol.KindError, Payload: payload}
	b.appendFrame(addr, frame, nil)
}
