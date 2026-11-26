// Package relay implements the in-memory room server for Civ6Relay v3.
package relay

import (
	"crypto/rand"
	"encoding/binary"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"civ6relay/internal/protocol"
)

const (
	defaultMaxRooms     = 1024
	maxRoomsCeiling     = 65536
	handshakePacketRate = 5.0
	handshakeByteRate   = 12000.0
	handshakeBurst      = 2.0

	DefaultMemberPacketRate = 5000
	DefaultMemberByteRate   = 3_750_000
)

// DropStats reports cumulative server-side packet drops by reason.
type DropStats struct {
	DecodeErrors uint64
	UnknownRoom  uint64
	AuthFailures uint64
	ReplayDrops  uint64
	RateDrops    uint64
}

type dropCounters struct {
	decodeErrors atomic.Uint64
	unknownRoom  atomic.Uint64
	authFailures atomic.Uint64
	replayDrops  atomic.Uint64
	rateDrops    atomic.Uint64
}

// Config configures a Server. Zero values are replaced with defaults.
type Config struct {
	MaxRooms         int
	MemberPacketRate int
	MemberByteRate   int
	Logger           *log.Logger
	Now              func() time.Time
}

// Server is a concurrency-safe relay room manager.
type Server struct {
	maxRooms int
	limit    tokenBucket
	log      *log.Logger
	now      func() time.Time
	start    time.Time

	roomsMu         sync.RWMutex
	roomsByID       map[uint64]*room
	roomsByCode     map[string]*room
	memberCount     atomic.Int64
	drops           dropCounters
	handshakeMu     sync.Mutex
	handshakeCache  map[handshakeKey]cachedHandshake
	handshakeLimits map[netip.Addr]*handshakeLimit
}

type handshakeKey struct {
	endpoint  netip.AddrPort
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
	if cfg.MemberPacketRate <= 0 {
		cfg.MemberPacketRate = DefaultMemberPacketRate
	}
	if cfg.MemberByteRate <= 0 {
		cfg.MemberByteRate = DefaultMemberByteRate
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{
		maxRooms:        cfg.MaxRooms,
		limit:           newTokenBucket(float64(cfg.MemberPacketRate), float64(cfg.MemberByteRate), 2),
		log:             cfg.Logger,
		now:             cfg.Now,
		start:           cfg.Now(),
		roomsByID:       make(map[uint64]*room),
		roomsByCode:     make(map[string]*room),
		handshakeCache:  make(map[handshakeKey]cachedHandshake),
		handshakeLimits: make(map[netip.Addr]*handshakeLimit),
	}
}

func (s *Server) clock() float64 {
	return s.now().Sub(s.start).Seconds()
}

// Handle processes one received datagram and returns the datagrams to send.
func (s *Server) Handle(datagram []byte, addr *net.UDPAddr) []Outbound {
	if addr == nil {
		return nil
	}
	var batch Batch
	s.HandleInto(datagram, addr.AddrPort(), &batch)
	return batch.Datagrams
}

// HandleInto replaces batch with the datagrams to send for one received frame.
// The caller owns batch and can reuse it after sending its contents.
func (s *Server) HandleInto(datagram []byte, addr netip.AddrPort, batch *Batch) {
	batch.reset()
	frame, err := protocol.DecodeView(datagram)
	if err != nil || !addr.IsValid() {
		s.drops.decodeErrors.Add(1)
		return
	}
	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	if frame.Kind == protocol.KindCreate || frame.Kind == protocol.KindJoin {
		if frame.Flags&protocol.FlagAuthenticated == 0 {
			s.handleHandshake(&frame, addr, len(datagram), batch)
		} else {
			s.drops.decodeErrors.Add(1)
		}
		return
	}

	s.roomsMu.RLock()
	r := s.roomsByID[frame.RoomID]
	s.roomsMu.RUnlock()
	if r == nil {
		s.drops.unknownRoom.Add(1)
		return
	}
	s.handleAuthenticated(r, &frame, addr, datagram, batch)
}

func (s *Server) allowHandshakeLocked(ip netip.Addr, size int, now float64) bool {
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
	s.handshakeMu.Lock()
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
	s.handshakeMu.Unlock()

	s.roomsMu.RLock()
	rooms := make([]*room, 0, len(s.roomsByID))
	for _, r := range s.roomsByID {
		rooms = append(rooms, r)
	}
	s.roomsMu.RUnlock()
	var batch Batch
	for _, r := range rooms {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			continue
		}
		changed := false
		hostTimeout := false
		for slot, m := range r.members {
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
				r.members[slot] = nil
				s.memberCount.Add(-1)
				changed = true
				s.log.Printf("member expired room=%s member=%d", r.code, m.id)
			}
		}
		if hostTimeout {
			s.closeRoomLocked(r, "host_timeout", &batch)
		} else if changed {
			s.peerUpdatesLocked(r, &batch)
		}
		r.mu.Unlock()
	}
	return batch.Datagrams
}

// Stats reports current room and member counts.
func (s *Server) Stats() (rooms, members int) {
	s.roomsMu.RLock()
	rooms, members = len(s.roomsByID), int(s.memberCount.Load())
	s.roomsMu.RUnlock()
	return
}

// DropStats returns cumulative drop counts by reason.
func (s *Server) DropStats() DropStats {
	return DropStats{
		DecodeErrors: s.drops.decodeErrors.Load(),
		UnknownRoom:  s.drops.unknownRoom.Load(),
		AuthFailures: s.drops.authFailures.Load(),
		ReplayDrops:  s.drops.replayDrops.Load(),
		RateDrops:    s.drops.rateDrops.Load(),
	}
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
		var code [10]byte
		for i := range b {
			code[i] = protocol.RoomCodeAlphabet[int(b[i])&31]
		}
		text := string(code[:])
		if _, exists := s.roomsByCode[text]; !exists {
			return text
		}
	}
}
