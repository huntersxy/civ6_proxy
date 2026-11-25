package speedtest

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const cookiePeriod = 5 * time.Second

type Server struct {
	maxMbps int
	write   func([]byte, netip.AddrPort) error
	key     [32]byte
	start   time.Time

	mu      sync.Mutex
	active  *testSession
	next    time.Time
	sending bool
	closed  bool
	writers sync.WaitGroup
}

type testSession struct {
	addr           netip.AddrPort
	request, id    token
	phase          Phase
	duration       time.Duration
	mbps           int
	end            time.Time
	lease          atomic.Int64
	cancelDownload context.CancelFunc
	download       sendProgress
	downloadResult *sendResult
	upload         packetTracker
	uploadStart    time.Time
	uploadFinish   time.Time
	uploadSent     uint32
}

func NewServer(mbps int, write func([]byte, netip.AddrPort) error) (*Server, error) {
	server := &Server{maxMbps: mbps, write: write, start: time.Now()}
	if mbps > 0 {
		if _, err := rand.Read(server.key[:]); err != nil {
			return nil, err
		}
	}
	return server, nil
}

func (server *Server) Handle(packet []byte, addr netip.AddrPort) {
	msg, ok := decode(packet)
	if !ok || msg.id == (token{}) || addr.Port() == 0 || !addr.Addr().Unmap().Is4() {
		return
	}
	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	if msg.kind == hello {
		reply := message{kind: unavailable, id: msg.id}
		if server.maxMbps > 0 {
			reply.kind = challenge
			reply.proof = server.cookie(addr, msg, time.Since(server.start)/cookiePeriod)
		}
		server.reply(reply, addr)
		return
	}
	if server.maxMbps == 0 {
		return
	}
	if msg.kind == open {
		bucket := time.Since(server.start) / cookiePeriod
		current, previous := server.cookie(addr, msg, bucket), server.cookie(addr, msg, bucket-1)
		if !hmac.Equal(msg.proof[:], current[:]) && !hmac.Equal(msg.proof[:], previous[:]) {
			return
		}
		if duration := phaseDuration(msg.millis); duration < time.Second || duration > maxDuration || msg.sent > MaxMbps {
			return
		}
		server.reply(server.admit(msg, addr), addr)
		return
	}
	if msg.kind != data && msg.kind != probe && msg.kind != stop {
		return
	}

	server.mu.Lock()
	session := server.active
	if session == nil || session.addr != addr || session.id != msg.id {
		server.mu.Unlock()
		return
	}
	now := time.Now()
	if server.expireLocked(now) {
		server.mu.Unlock()
		return
	}
	switch msg.kind {
	case data:
		if session.phase == Upload && msg.phase == Upload &&
			(session.uploadFinish.IsZero() || (now.Before(session.uploadFinish) && msg.serial < session.uploadSent)) {
			if session.uploadStart.IsZero() {
				session.uploadStart = now
			}
			session.upload.add(msg.serial)
		}
	case stop:
		server.releaseLocked(now)
	case probe:
		reply, ok := server.pollLocked(session, msg, now)
		server.mu.Unlock()
		if ok {
			server.reply(reply, addr)
		}
		return
	}
	server.mu.Unlock()
}

func (server *Server) cookie(addr netip.AddrPort, msg message, bucket time.Duration) token {
	var seed [38]byte
	ip := addr.Addr().As4()
	copy(seed[:4], ip[:])
	binary.BigEndian.PutUint16(seed[4:6], addr.Port())
	copy(seed[6:22], msg.id[:])
	binary.BigEndian.PutUint32(seed[22:26], msg.millis)
	binary.BigEndian.PutUint32(seed[26:30], msg.sent)
	binary.BigEndian.PutUint64(seed[30:], uint64(bucket))
	mac := hmac.New(sha256.New, server.key[:])
	_, _ = mac.Write(seed[:])
	var sum [sha256.Size]byte
	var proof token
	copy(proof[:], mac.Sum(sum[:0]))
	return proof
}

func (server *Server) reply(msg message, addr netip.AddrPort) {
	var packet [controlSize]byte
	msg.encode(packet[:])
	_ = server.write(packet[:], addr)
}

func (server *Server) admit(msg message, addr netip.AddrPort) message {
	server.mu.Lock()
	defer server.mu.Unlock()
	now := time.Now()
	server.expireLocked(now)
	if server.closed {
		return message{kind: unavailable, id: msg.id}
	}
	session := server.active
	if session == nil && !server.sending && !now.Before(server.next) {
		duration := phaseDuration(msg.millis)
		mbps := server.maxMbps
		if msg.sent != 0 {
			mbps = min(mbps, int(msg.sent))
		}
		session = &testSession{addr: addr, request: msg.id, mbps: mbps,
			duration: duration, end: now.Add(2*duration + 8*time.Second)}
		if _, err := rand.Read(session.id[:]); err != nil {
			return message{kind: unavailable, id: msg.id}
		}
		session.lease.Store(now.Sub(server.start).Nanoseconds() + leaseTime.Nanoseconds())
		server.active = session
	}
	if session != nil && session.addr == addr && session.request == msg.id {
		return message{kind: ready, id: msg.id, sent: uint32(session.mbps),
			millis: uint32(session.duration.Milliseconds()), proof: session.id}
	}
	return message{kind: busy, id: msg.id}
}

func (server *Server) pollLocked(session *testSession, msg message, now time.Time) (message, bool) {
	if msg.phase == Download && session.phase == Baseline {
		server.startDownloadLocked(session)
	}
	if msg.phase == Upload && session.phase == Download && session.downloadResult != nil {
		session.phase = Upload
		session.upload = trackPackets(session.duration, session.mbps)
	}
	if msg.phase != session.phase {
		return message{}, false
	}
	session.lease.Store(now.Sub(server.start).Nanoseconds() + leaseTime.Nanoseconds())
	reply := message{kind: status, phase: msg.phase, id: session.id, serial: msg.serial}
	switch session.phase {
	case Download:
		session.download.setFeedback(msg.sent, msg.received)
		if result := session.downloadResult; result != nil {
			reply.final, reply.sent, reply.millis = true, result.sent, uint32(result.elapsed.Milliseconds())
		}
	case Upload:
		if msg.final && session.uploadFinish.IsZero() {
			session.uploadSent = min(session.upload.limit, max(msg.sent, session.upload.seen))
			session.uploadFinish = now.Add(settleTime)
		}
		reply.sent, reply.received = session.upload.seen, session.upload.kept
		if !session.uploadStart.IsZero() {
			elapsed := now.Sub(session.uploadStart)
			if !session.uploadFinish.IsZero() {
				elapsed = session.uploadFinish.Add(-settleTime).Sub(session.uploadStart)
			}
			reply.millis = uint32(max(0, elapsed.Milliseconds()))
		}
		if !session.uploadFinish.IsZero() && !now.Before(session.uploadFinish) {
			reply.final, reply.sent = true, session.uploadSent
		}
	}
	return reply, true
}

func (server *Server) startDownloadLocked(session *testSession) {
	session.phase = Download
	ctx, cancel := context.WithDeadline(context.Background(), session.end)
	session.cancelDownload = cancel
	server.sending = true
	server.writers.Add(1)
	go func() {
		defer server.writers.Done()
		defer cancel()
		result := sendTrain(ctx, session.id, Download, session.duration, session.mbps, &session.download,
			func() bool { return time.Since(server.start).Nanoseconds() < session.lease.Load() },
			func(packet []byte) error { return server.write(packet, session.addr) })
		server.mu.Lock()
		session.downloadResult = &result
		server.sending = false
		server.mu.Unlock()
	}()
}

func (server *Server) releaseLocked(now time.Time) {
	if server.active != nil {
		if cancel := server.active.cancelDownload; cancel != nil {
			cancel()
		}
		server.active = nil
		server.next = now.Add(cooldown)
	}
}

func (server *Server) expireLocked(now time.Time) bool {
	if server.active != nil &&
		(!now.Before(server.active.end) || now.Sub(server.start).Nanoseconds() >= server.active.lease.Load()) {
		server.releaseLocked(now)
		return true
	}
	return false
}

func (server *Server) Sweep() {
	server.mu.Lock()
	server.expireLocked(time.Now())
	server.mu.Unlock()
}

func (server *Server) Close() {
	server.mu.Lock()
	server.closed = true
	server.releaseLocked(time.Now())
	server.mu.Unlock()
	server.writers.Wait()
}
