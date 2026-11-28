package logsync

import (
	"sync"
	"time"
)

const (
	defaultPollInterval    = 500 * time.Millisecond
	defaultHelloInterval   = 1 * time.Second
	defaultRTOTick         = 100 * time.Millisecond
	defaultHostIdleTimeout = 45 * time.Second
	defaultSendWindow      = 16
	maxSendWindow          = 32
	maxPendingChunks       = 64
	defaultRateBytesPerSec = 160 * 1024 // ~160 KiB/s raw (~20-35 KiB/s wire after deflate)
	defaultRateChunksSec   = 64
	minRTO                 = 250 * time.Millisecond
	maxRTO                 = 2500 * time.Millisecond
	fastRetransmitCooldown = 150 * time.Millisecond
)

// SendFunc transmits one aux payload to targetMember over the tunnel.
type SendFunc func(targetMember uint8, payload []byte) bool

// GuestStatus is a thread-safe snapshot of a guest's upload state for the UI.
type GuestStatus struct {
	Active        bool
	SessionID     string
	FilesSynced   int
	BytesSent     uint64
	BytesAcked    uint64
	PendingChunks int
	LastActivity  time.Time
}

// GuestConfig configures a GuestController.
type GuestConfig struct {
	Send             SendFunc
	HostID           func() uint8
	Identity         func() (computer, user, memberName string)
	WatcherCfg       WatcherConfig
	OnChange         func()
	Logf             func(level, msg string)
	PollInterval     time.Duration
	HelloInterval    time.Duration
	RTOTick          time.Duration
	HostIdleTimeout  time.Duration
	RateBytesPerSec  float64
	RateChunksPerSec float64
}

type incomingAux struct {
	source  uint8
	payload []byte
}

type sentChunk struct {
	seq        uint32
	raw        []byte
	dataLen    uint64
	relPath    string
	firstSent  time.Time
	lastSent   time.Time
	retries    int
	sacked     bool
	sackedSkip int
}

type paceBucket struct {
	byteRate   float64
	chunkRate  float64
	byteBurst  float64
	chunkBurst float64
	bytes      float64
	chunks     float64
	updated    time.Time
}

func newPaceBucket(byteRate, chunkRate float64, now time.Time) paceBucket {
	byteBurst := max(byteRate*0.25, float64(TextChunkSize*4))
	chunkBurst := max(chunkRate*0.25, 8.0)
	return paceBucket{
		byteRate:   byteRate,
		chunkRate:  chunkRate,
		byteBurst:  byteBurst,
		chunkBurst: chunkBurst,
		bytes:      byteBurst,
		chunks:     chunkBurst,
		updated:    now,
	}
}

func (b *paceBucket) allow(size int, now time.Time) bool {
	if dt := now.Sub(b.updated).Seconds(); dt > 0 {
		b.bytes = min(b.byteBurst, b.bytes+dt*b.byteRate)
		b.chunks = min(b.chunkBurst, b.chunks+dt*b.chunkRate)
		b.updated = now
	}
	cost := float64(max(size, 64))
	if b.chunks < 1 || b.bytes < cost {
		return false
	}
	b.chunks--
	b.bytes -= cost
	return true
}

// GuestController runs on non-host room members. It stays completely idle
// until the room host sends MsgCtrl(enabled=true), then streams local logs
// reliably to the host using selective-repeat ARQ and gentle rate pacing.
type GuestController struct {
	cfg GuestConfig

	inCh   chan incomingAux
	stopCh chan struct{}
	stop   sync.Once
	wg     sync.WaitGroup

	statusMu sync.RWMutex
	status   GuestStatus
}

// NewGuestController starts a background GuestController goroutine.
func NewGuestController(cfg GuestConfig) *GuestController {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.HelloInterval <= 0 {
		cfg.HelloInterval = defaultHelloInterval
	}
	if cfg.RTOTick <= 0 {
		cfg.RTOTick = defaultRTOTick
	}
	if cfg.HostIdleTimeout <= 0 {
		cfg.HostIdleTimeout = defaultHostIdleTimeout
	}
	if cfg.RateBytesPerSec <= 0 {
		cfg.RateBytesPerSec = defaultRateBytesPerSec
	}
	if cfg.RateChunksPerSec <= 0 {
		cfg.RateChunksPerSec = defaultRateChunksSec
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, string) {}
	}
	g := &GuestController{
		cfg:    cfg,
		inCh:   make(chan incomingAux, 256),
		stopCh: make(chan struct{}),
	}
	g.wg.Add(1)
	go g.run()
	return g
}

// HandleAux dispatches an inbound sideband payload without blocking readLoop.
func (g *GuestController) HandleAux(sourceMember uint8, payload []byte) {
	mt, ok := PeekMessageType(payload)
	if !ok {
		return
	}
	switch mt {
	case MsgCtrl, MsgHelloAck, MsgAck:
	default:
		return
	}
	if g.cfg.HostID != nil {
		if hostID := g.cfg.HostID(); hostID != 0 && sourceMember != hostID {
			return
		}
	}
	select {
	case g.inCh <- incomingAux{source: sourceMember, payload: payload}:
	default:
	}
}

// Status returns a snapshot of the guest's current log streaming status.
func (g *GuestController) Status() GuestStatus {
	g.statusMu.RLock()
	defer g.statusMu.RUnlock()
	return g.status
}

// Close shuts down the GuestController and waits for its goroutine to exit.
func (g *GuestController) Close() {
	g.stop.Do(func() {
		close(g.stopCh)
	})
	g.wg.Wait()
}

type guestState struct {
	active        bool
	hostMember    uint8
	sessionID     string
	nonce         uint64
	senderID      uint64
	helloAcked    bool
	lastHostSeen  time.Time
	lastHelloSent time.Time

	watcher    *Watcher
	nextSeq    uint32
	cumAck     uint32
	peerWindow uint32
	srtt       time.Duration
	inFlight   map[uint32]*sentChunk
	pending    []FileChunk
	filesSeen  map[string]struct{}
	bytesSent  uint64
	bytesAcked uint64
	lastActive time.Time
	pacer      paceBucket
}

func (g *GuestController) run() {
	defer g.wg.Done()
	pollTicker := time.NewTicker(g.cfg.PollInterval)
	rtoTicker := time.NewTicker(g.cfg.RTOTick)
	defer pollTicker.Stop()
	defer rtoTicker.Stop()

	var st guestState
	for {
		select {
		case <-g.stopCh:
			return
		case msg := <-g.inCh:
			g.handleMessage(&st, msg)
		case <-pollTicker.C:
			if st.active && st.helloAcked {
				g.pollAndPump(&st)
			}
		case <-rtoTicker.C:
			if st.active {
				g.stepTimers(&st)
			}
		}
	}
}

func (g *GuestController) handleMessage(st *guestState, msg incomingAux) {
	mt, ok := PeekMessageType(msg.payload)
	if !ok {
		return
	}
	now := time.Now()
	switch mt {
	case MsgCtrl:
		ctrl, err := DecodeCtrl(msg.payload)
		if err != nil {
			return
		}
		if !ctrl.Enabled {
			if st.active && (ctrl.Nonce == 0 || ctrl.Nonce == st.nonce) {
				g.deactivate(st, "房主已停止日志收集")
			}
			return
		}
		if ctrl.Nonce == 0 {
			return
		}
		if st.active && st.nonce == ctrl.Nonce {
			st.hostMember = msg.source
			st.lastHostSeen = now
			return
		}
		// Start a brand-new collection session.
		*st = guestState{
			active:       true,
			hostMember:   msg.source,
			sessionID:    ctrl.SessionID,
			nonce:        ctrl.Nonce,
			senderID:     RandomUint64(),
			lastHostSeen: now,
			watcher:      NewWatcher(g.cfg.WatcherCfg),
			nextSeq:      1,
			peerWindow:   defaultSendWindow,
			srtt:         120 * time.Millisecond,
			inFlight:     make(map[uint32]*sentChunk),
			filesSeen:    make(map[string]struct{}),
			lastActive:   now,
			pacer:        newPaceBucket(g.cfg.RateBytesPerSec, g.cfg.RateChunksPerSec, now),
		}
		g.cfg.Logf("INFO", "房主已开启日志收集，开始实时同步日志")
		g.sendHello(st, now)
		g.publishStatus(st)

	case MsgHelloAck:
		ack, err := DecodeHelloAck(msg.payload)
		if err != nil || !st.active || ack.Nonce != st.nonce || ack.SenderID != st.senderID {
			return
		}
		st.lastHostSeen = now
		if !st.helloAcked {
			st.helloAcked = true
			g.pollAndPump(st)
			g.publishStatus(st)
		}

	case MsgAck:
		ack, err := DecodeAck(msg.payload)
		if err != nil || !st.active || ack.Nonce != st.nonce || ack.SenderID != st.senderID {
			return
		}
		st.lastHostSeen = now
		g.processAck(st, ack, now)
	}
}

func (g *GuestController) deactivate(st *guestState, reason string) {
	wasActive := st.active
	*st = guestState{}
	g.publishStatus(st)
	if wasActive && reason != "" {
		g.cfg.Logf("INFO", reason)
	}
}

func (g *GuestController) sendHello(st *guestState, now time.Time) {
	var comp, user, memberName string
	if g.cfg.Identity != nil {
		comp, user, memberName = g.cfg.Identity()
	}
	raw, err := EncodeHello(HelloMessage{
		Nonce:      st.nonce,
		SenderID:   st.senderID,
		Computer:   comp,
		User:       user,
		MemberName: memberName,
		Protocol:   ProtoVersion,
	})
	if err != nil || g.cfg.Send == nil {
		return
	}
	if g.cfg.Send(st.hostMember, raw) {
		st.lastHelloSent = now
	}
}

func (g *GuestController) pollAndPump(st *guestState) {
	if st.watcher == nil {
		return
	}
	if len(st.pending) < maxPendingChunks {
		st.watcher.Poll(func(ch FileChunk) bool {
			if ch.DropPending {
				st.pending = dropPendingForPath(st.pending, ch.RelPath)
			}
			if len(st.pending) >= maxPendingChunks {
				return false
			}
			st.pending = append(st.pending, ch)
			return true
		})
	}
	g.pumpWindow(st, time.Now())
	g.publishStatus(st)
}

func dropPendingForPath(pending []FileChunk, relPath string) []FileChunk {
	if len(pending) == 0 {
		return pending
	}
	out := pending[:0]
	for _, item := range pending {
		if item.RelPath == relPath {
			continue
		}
		out = append(out, item)
	}
	return out
}

func (g *GuestController) stepTimers(st *guestState) {
	now := time.Now()
	if now.Sub(st.lastHostSeen) >= g.cfg.HostIdleTimeout {
		g.deactivate(st, "房主日志收集心跳超时，已停止上传")
		return
	}
	if !st.helloAcked {
		if now.Sub(st.lastHelloSent) >= g.cfg.HelloInterval {
			g.sendHello(st, now)
		}
		return
	}
	// Retransmit timed-out unacked packets in sequence order.
	baseRTO := clampDuration(st.srtt*3, minRTO, maxRTO)
	for seq := st.cumAck + 1; seq < st.nextSeq; seq++ {
		sc, ok := st.inFlight[seq]
		if !ok || sc.sacked {
			continue
		}
		rto := clampDuration(baseRTO<<min(sc.retries, 3), minRTO, maxRTO)
		if now.Sub(sc.lastSent) < rto {
			continue
		}
		if !st.pacer.allow(len(sc.raw), now) {
			break
		}
		if g.cfg.Send != nil && g.cfg.Send(st.hostMember, sc.raw) {
			sc.lastSent = now
			sc.retries++
		}
	}
	g.pumpWindow(st, now)
}

func (g *GuestController) processAck(st *guestState, ack AckMessage, now time.Time) {
	changed := false
	if ack.Window > 0 {
		st.peerWindow = min(max(ack.Window, 4), maxSendWindow)
	}
	if ack.CumAck > st.cumAck {
		for seq := st.cumAck + 1; seq <= ack.CumAck; seq++ {
			if sc, ok := st.inFlight[seq]; ok {
				if sc.retries == 0 {
					sample := now.Sub(sc.firstSent)
					if sample > 0 && sample < 5*time.Second {
						st.srtt = (st.srtt*7 + sample) / 8
					}
				}
				if !sc.sacked {
					st.bytesAcked += sc.dataLen
				}
				delete(st.inFlight, seq)
			}
		}
		st.cumAck = ack.CumAck
		st.lastActive = now
		changed = true
	}
	highestSacked := ack.CumAck
	for i := uint32(0); i < 64; i++ {
		if ack.SackMask&(uint64(1)<<i) == 0 {
			continue
		}
		seq := ack.CumAck + 1 + i
		if seq > highestSacked {
			highestSacked = seq
		}
		if sc, ok := st.inFlight[seq]; ok && !sc.sacked {
			sc.sacked = true
			st.bytesAcked += sc.dataLen
			st.lastActive = now
			changed = true
		}
	}
	// Fast retransmit holes before highestSacked.
	if highestSacked > st.cumAck+1 {
		for seq := st.cumAck + 1; seq < highestSacked; seq++ {
			sc, ok := st.inFlight[seq]
			if !ok || sc.sacked {
				continue
			}
			sc.sackedSkip++
			if sc.sackedSkip >= 2 && now.Sub(sc.lastSent) >= fastRetransmitCooldown {
				if !st.pacer.allow(len(sc.raw), now) {
					break
				}
				if g.cfg.Send != nil && g.cfg.Send(st.hostMember, sc.raw) {
					sc.lastSent = now
					sc.retries++
					sc.sackedSkip = 0
				}
			}
		}
	}
	g.pumpWindow(st, now)
	if changed {
		g.publishStatus(st)
	}
}

func (g *GuestController) pumpWindow(st *guestState, now time.Time) {
	if !st.active || !st.helloAcked || g.cfg.Send == nil {
		return
	}
	win := st.peerWindow
	if win == 0 {
		win = defaultSendWindow
	}
	for len(st.pending) > 0 && uint32(len(st.inFlight)) < win && (st.nextSeq-st.cumAck) <= 64 {
		ch := st.pending[0]
		seq := st.nextSeq
		raw, err := EncodeChunk(ChunkMessage{
			Nonce:    st.nonce,
			SenderID: st.senderID,
			Seq:      seq,
			Flags:    ch.Flags,
			Offset:   ch.Offset,
			RelPath:  ch.RelPath,
			Data:     ch.Data,
		})
		if err != nil {
			// Drop malformed chunk rather than wedging the queue.
			st.pending = st.pending[1:]
			continue
		}
		if !st.pacer.allow(len(raw), now) {
			break
		}
		if !g.cfg.Send(st.hostMember, raw) {
			break
		}
		st.pending = st.pending[1:]
		st.nextSeq++
		st.filesSeen[ch.RelPath] = struct{}{}
		st.bytesSent += uint64(len(ch.Data))
		st.lastActive = now
		st.inFlight[seq] = &sentChunk{
			seq:       seq,
			raw:       raw,
			dataLen:   uint64(len(ch.Data)),
			relPath:   ch.RelPath,
			firstSent: now,
			lastSent:  now,
		}
	}
	if len(st.pending) == 0 && cap(st.pending) > maxPendingChunks*2 {
		st.pending = nil
	}
}

func (g *GuestController) publishStatus(st *guestState) {
	next := GuestStatus{
		Active:        st.active,
		SessionID:     st.sessionID,
		FilesSynced:   len(st.filesSeen),
		BytesSent:     st.bytesSent,
		BytesAcked:    st.bytesAcked,
		PendingChunks: len(st.pending) + len(st.inFlight),
		LastActivity:  st.lastActive,
	}
	g.statusMu.Lock()
	g.status = next
	g.statusMu.Unlock()
	if g.cfg.OnChange != nil {
		g.cfg.OnChange()
	}
}

func clampDuration(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}
