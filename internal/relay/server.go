// Package relay implements the in-memory room server for Civ6Relay v1.
package relay

import (
	"crypto/rand"
	"encoding/binary"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"civ6relay/internal/protocol"
)

const (
	defaultMaxRooms     = 1024
	maxRoomsCeiling     = 65536
	handshakePacketRate = 5.0
	handshakeByteRate   = 12000.0
	handshakeBurst      = 2.0
)

// Config configures a Server. Zero values are replaced with defaults.
type Config struct {
	MaxRooms int
	Logger   *log.Logger
	Now      func() time.Time
}

// Server is a concurrency-safe relay room manager.
type Server struct {
	maxRooms int
	log      *log.Logger
	now      func() time.Time
	start    time.Time

	mu              sync.Mutex
	roomsByID       map[uint64]*room
	roomsByCode     map[string]*room
	handshakeCache  map[handshakeKey]cachedHandshake
	handshakeLimits map[string]*handshakeLimit
}

type handshakeKey struct {
	endpoint  string
	kind      protocol.Kind
	requestID string
}

type cachedHandshake struct {
	expires float64
	data    []byte
}

type handshakeLimit struct {
	bucket   tokenBucket
	lastSeen float64
}

// New returns a relay server.
func New(cfg Config) *Server {
	if cfg.MaxRooms <= 0 {
		cfg.MaxRooms = defaultMaxRooms
	}
	if cfg.MaxRooms > maxRoomsCeiling {
		cfg.MaxRooms = maxRoomsCeiling
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{
		maxRooms:        cfg.MaxRooms,
		log:             cfg.Logger,
		now:             cfg.Now,
		start:           cfg.Now(),
		roomsByID:       make(map[uint64]*room),
		roomsByCode:     make(map[string]*room),
		handshakeCache:  make(map[handshakeKey]cachedHandshake),
		handshakeLimits: make(map[string]*handshakeLimit),
	}
}

func (s *Server) clock() float64 {
	return s.now().Sub(s.start).Seconds()
}

// Handle processes one received datagram and returns the datagrams to send.
func (s *Server) Handle(datagram []byte, addr *net.UDPAddr) []Outbound {
	frame, err := protocol.Decode(datagram)
	if err != nil || addr == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if frame.Kind == protocol.KindCreate || frame.Kind == protocol.KindJoin {
		if frame.Flags&protocol.FlagAuthenticated != 0 {
			return nil
		}
		if !s.allowHandshakeLocked(addr.IP.String(), len(datagram)) {
			return nil
		}
		return s.handleHandshakeLocked(&frame, addr)
	}
	return s.handleAuthenticatedLocked(&frame, addr, len(datagram))
}

func (s *Server) allowHandshakeLocked(ip string, size int) bool {
	now := s.clock()
	state := s.handshakeLimits[ip]
	if state == nil {
		state = &handshakeLimit{bucket: newTokenBucket(handshakePacketRate, handshakeByteRate, handshakeBurst)}
		s.handshakeLimits[ip] = state
	}
	state.lastSeen = now
	return state.bucket.allow(size, now)
}

// Cleanup advances timers: it expires cached handshakes, marks idle members
// offline, drops expired members, and closes rooms whose host timed out.
func (s *Server) Cleanup() []Outbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	for key, cached := range s.handshakeCache {
		if cached.expires <= now {
			delete(s.handshakeCache, key)
		}
	}
	for ip, state := range s.handshakeLimits {
		if now-state.lastSeen >= handshakeIdleSec {
			delete(s.handshakeLimits, ip)
		}
	}
	var out []Outbound
	for _, id := range s.sortedRoomIDsLocked() {
		r := s.roomsByID[id]
		if r == nil {
			continue
		}
		changed := false
		hostTimeout := false
		for _, memberID := range r.sortedIDs() {
			m := r.members[memberID]
			if m == nil {
				continue
			}
			idle := now - m.lastSeen
			if idle >= offlineAfterSec && m.online {
				m.online = false
				changed = true
				s.log.Printf("member offline room=%s member=%d", r.code, m.id)
			}
			if idle >= retentionSec {
				if m.isHost {
					hostTimeout = true
					break
				}
				delete(r.members, m.id)
				changed = true
				s.log.Printf("member expired room=%s member=%d", r.code, m.id)
			}
		}
		if hostTimeout {
			out = append(out, s.closeRoomLocked(r, "host_timeout")...)
		} else if changed {
			out = append(out, s.peerUpdatesLocked(r)...)
		}
	}
	return out
}

// Stats reports current room and member counts.
func (s *Server) Stats() (rooms, members int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rooms = len(s.roomsByID)
	for _, r := range s.roomsByID {
		members += len(r.members)
	}
	return rooms, members
}

func (s *Server) sortedRoomIDsLocked() []uint64 {
	ids := make([]uint64, 0, len(s.roomsByID))
	for id := range s.roomsByID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *Server) newRoomIDLocked() uint64 {
	for {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic("crypto/rand failure: " + err.Error())
		}
		id := binary.BigEndian.Uint64(b[:])
		if id != 0 {
			if _, exists := s.roomsByID[id]; !exists {
				return id
			}
		}
	}
}

func (s *Server) newRoomCodeLocked() string {
	for {
		var b [10]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic("crypto/rand failure: " + err.Error())
		}
		code := make([]byte, 10)
		for i := range b {
			code[i] = protocol.RoomCodeAlphabet[int(b[i])&31]
		}
		text := string(code)
		if _, exists := s.roomsByCode[text]; !exists {
			return text
		}
	}
}

func (s *Server) appendSignedLocked(dst []Outbound, m *member, frame protocol.Frame) []Outbound {
	frame.TargetMember = m.id
	frame.Sequence = m.outSeq.Next()
	data, err := frame.Encode(m.token[:])
	if err != nil {
		return dst
	}
	return append(dst, Outbound{Addr: m.addr, Data: data})
}

func (s *Server) appendUnsignedLocked(dst []Outbound, addr *net.UDPAddr, frame protocol.Frame) []Outbound {
	data, err := frame.Encode(nil)
	if err != nil {
		return dst
	}
	return append(dst, Outbound{Addr: addr, Data: data})
}
