package logsync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"civ6relay/internal/protocol"
)

const (
	defaultProbeInterval     = 2 * time.Second
	defaultKeepaliveInterval = 5 * time.Second
	defaultAckDelay          = 50 * time.Millisecond
	maxProbeAttempts         = 3
	hostRecvWindow           = 32
	idleHandleTTL            = 30 * time.Second
	maxPrevRotationsPerFile  = 2
)

// PeerSyncStatus summarizes one teammate's log collection state.
type PeerSyncStatus struct {
	MemberID    uint8
	Name        string
	Computer    string
	DirName     string
	State       string // "syncing", "probing", "unsupported", "offline"
	Files       int
	Bytes       uint64
	LastUpdated time.Time
}

// HostStatus is a thread-safe snapshot of the host's active collection session.
type HostStatus struct {
	Enabled      bool
	SessionID    string
	SessionDir   string
	ActivePeers  int
	OnlineGuests int
	TotalFiles   int
	TotalBytes   uint64
	HostFiles    int
	HostBytes    uint64
	GuestBytes   uint64
	LastError    string
	Peers        []PeerSyncStatus
}

// HostConfig configures a HostController.
type HostConfig struct {
	BaseDir           string
	RoomCode          string
	RelayAddr         string
	HostMemberID      uint8
	HostName          string
	HostComputer      string
	PCAPPath          func() string
	Send              SendFunc
	Peers             func() []protocol.Peer
	HostWatcherCfg    WatcherConfig
	OnChange          func()
	Logf              func(level, msg string)
	Retention         time.Duration
	MaxSessions       int
	PollInterval      time.Duration
	ProbeInterval     time.Duration
	KeepaliveInterval time.Duration
	AckDelay          time.Duration
}

type streamHandle struct {
	file             *os.File
	lastUsed         time.Time
	writtenInSession bool
	size             uint64
}

type prevRotation struct {
	path string
	size uint64
}

type memberWriter struct {
	dir          string
	streams      map[string]*streamHandle
	temps        map[string]*os.File
	files        map[string]uint64
	prevFiles    map[string][]prevRotation
	totalWritten uint64
	lastUpdated  time.Time
	lastErr      error
}

func newMemberWriter(dir string) (*memberWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &memberWriter{
		dir:       dir,
		streams:   make(map[string]*streamHandle),
		temps:     make(map[string]*os.File),
		files:     make(map[string]uint64),
		prevFiles: make(map[string][]prevRotation),
	}, nil
}

func (mw *memberWriter) liveDiskBytes() uint64 {
	var total uint64
	for _, sz := range mw.files {
		total += sz
	}
	for _, list := range mw.prevFiles {
		for _, prev := range list {
			total += prev.size
		}
	}
	return total
}

func (mw *memberWriter) WriteChunk(ch FileChunk, now time.Time) error {
	rel, err := ValidateRelPath(ch.RelPath)
	if err != nil {
		return err
	}
	endOff := ch.Offset + uint64(len(ch.Data))
	if endOff > MaxFileSizeBytes {
		return fmt.Errorf("%s exceeds max file size", rel)
	}
	targetPath := filepath.Join(mw.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		mw.lastErr = err
		return err
	}

	if ch.Flags&ChunkFlagSnapshot != 0 {
		return mw.writeSnapshotChunk(rel, targetPath, ch, now)
	}
	return mw.writeStreamChunk(rel, targetPath, ch, now)
}

func (mw *memberWriter) writeSnapshotChunk(rel, targetPath string, ch FileChunk, now time.Time) error {
	endOff := ch.Offset + uint64(len(ch.Data))
	curSize := mw.files[rel]
	if endOff > curSize && mw.liveDiskBytes()+(endOff-curSize) > MaxMemberBytes {
		err := fmt.Errorf("member log quota exceeded (%s)", FormatByteSize(MaxMemberBytes))
		mw.lastErr = err
		return err
	}
	tmpPath := targetPath + ".tmp"
	if ch.Flags&ChunkFlagReset != 0 {
		if old := mw.temps[rel]; old != nil {
			_ = old.Close()
			delete(mw.temps, rel)
		}
		f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			mw.lastErr = err
			return err
		}
		mw.temps[rel] = f
	}
	f := mw.temps[rel]
	if f == nil {
		var err error
		f, err = os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			mw.lastErr = err
			return err
		}
		mw.temps[rel] = f
	}
	if len(ch.Data) > 0 {
		if _, err := f.WriteAt(ch.Data, int64(ch.Offset)); err != nil {
			mw.lastErr = err
			return err
		}
		mw.totalWritten += uint64(len(ch.Data))
	}
	if ch.Flags&ChunkFlagEOF != 0 {
		closeErr := f.Close()
		delete(mw.temps, rel)
		if closeErr != nil {
			mw.lastErr = closeErr
			return closeErr
		}
		// Ensure any open streaming handle on the same path is closed before swap.
		if sh := mw.streams[rel]; sh != nil && sh.file != nil {
			_ = sh.file.Close()
			sh.file = nil
		}
		if err := os.Rename(tmpPath, targetPath); err != nil {
			mw.lastErr = err
			return err
		}
		mw.files[rel] = endOff
		mw.lastUpdated = now
	}
	return nil
}

func (mw *memberWriter) writeStreamChunk(rel, targetPath string, ch FileChunk, now time.Time) error {
	sh := mw.streams[rel]
	if sh == nil {
		sh = &streamHandle{}
		mw.streams[rel] = sh
	}
	if ch.Flags&ChunkFlagReset != 0 {
		if sh.file != nil {
			_ = sh.file.Close()
			sh.file = nil
		}
		if sh.writtenInSession {
			if info, err := os.Stat(targetPath); err == nil && info.Size() > 0 {
				if dst := rotatePrevFile(targetPath, now); dst != "" {
					mw.prevFiles[rel] = append(mw.prevFiles[rel], prevRotation{
						path: dst,
						size: uint64(info.Size()),
					})
					for len(mw.prevFiles[rel]) > maxPrevRotationsPerFile {
						oldest := mw.prevFiles[rel][0]
						_ = os.Remove(oldest.path)
						mw.prevFiles[rel] = mw.prevFiles[rel][1:]
					}
				}
			}
		}
		f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			mw.lastErr = err
			return err
		}
		sh.file = f
		sh.size = 0
		sh.writtenInSession = false
		mw.files[rel] = 0
	} else if sh.file == nil {
		f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			mw.lastErr = err
			return err
		}
		sh.file = f
	}
	endOff := ch.Offset + uint64(len(ch.Data))
	if endOff > sh.size {
		delta := endOff - sh.size
		if mw.liveDiskBytes()+delta > MaxMemberBytes {
			err := fmt.Errorf("member log quota exceeded (%s)", FormatByteSize(MaxMemberBytes))
			mw.lastErr = err
			return err
		}
	}
	if len(ch.Data) > 0 {
		if _, err := sh.file.WriteAt(ch.Data, int64(ch.Offset)); err != nil {
			mw.lastErr = err
			return err
		}
		sh.writtenInSession = true
		mw.totalWritten += uint64(len(ch.Data))
	}
	if endOff > sh.size || ch.Flags&ChunkFlagReset != 0 {
		sh.size = endOff
	}
	sh.lastUsed = now
	mw.files[rel] = sh.size
	mw.lastUpdated = now
	return nil
}

func rotatePrevFile(targetPath string, now time.Time) string {
	stamp := now.Format("150405")
	for suffix := 0; suffix < 100; suffix++ {
		dst := fmt.Sprintf("%s.prev-%s", targetPath, stamp)
		if suffix > 0 {
			dst = fmt.Sprintf("%s.prev-%s-%02d", targetPath, stamp, suffix)
		}
		if _, err := os.Stat(dst); errors.Is(err, fs.ErrNotExist) {
			if err := os.Rename(targetPath, dst); err == nil {
				return dst
			}
			return ""
		}
	}
	return ""
}

func (mw *memberWriter) closeIdle(now time.Time) {
	for _, sh := range mw.streams {
		if sh.file != nil && now.Sub(sh.lastUsed) >= idleHandleTTL {
			_ = sh.file.Close()
			sh.file = nil
		}
	}
}

func (mw *memberWriter) Close() {
	for _, sh := range mw.streams {
		if sh.file != nil {
			_ = sh.file.Close()
			sh.file = nil
		}
	}
	for rel, f := range mw.temps {
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
		delete(mw.temps, rel)
	}
}

type peerReceiver struct {
	memberID     uint8
	name         string
	computer     string
	dirName      string
	online       bool
	state        string // "probing", "syncing", "unsupported", "offline"
	probesSent   int
	lastProbeAt  time.Time
	lastCtrlAt   time.Time
	senderID     uint64
	nextSeq      uint32
	reorder      map[uint32]FileChunk
	ackPending   bool
	ackChunks    int
	writer       *memberWriter
	lastErr      string
}

// HostController coordinates real-time log collection for the room host:
// it records the host's own logs locally and receives ordered streaming logs
// from every teammate into one structured session directory.
type HostController struct {
	cfg        HostConfig
	sessionID  string
	sessionDir string
	nonce      uint64
	startedAt  time.Time

	inCh    chan incomingAux
	peersCh chan []protocol.Peer
	stopCh  chan struct{}
	stop    sync.Once
	wg      sync.WaitGroup

	hostDirName string
	hostWriter  *memberWriter
	hostWatcher *Watcher

	statusMu sync.RWMutex
	status   HostStatus
}

// StartHostController creates a fresh session directory, prunes expired
// historical sessions, starts local host collection, and begins coordinating
// teammates in the background.
func StartHostController(cfg HostConfig) (*HostController, error) {
	if cfg.BaseDir == "" {
		return nil, errors.New("logsync base directory is required")
	}
	if cfg.Retention <= 0 {
		cfg.Retention = DefaultRetention
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = DefaultMaxSessions
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.ProbeInterval <= 0 {
		cfg.ProbeInterval = defaultProbeInterval
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = defaultKeepaliveInterval
	}
	if cfg.AckDelay <= 0 {
		cfg.AckDelay = defaultAckDelay
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, string) {}
	}

	now := time.Now()
	if err := os.MkdirAll(cfg.BaseDir, 0o755); err != nil {
		return nil, err
	}
	sessionID, sessionDir, err := createUniqueSessionDir(cfg.BaseDir, cfg.RoomCode, now)
	if err != nil {
		return nil, err
	}
	_ = PruneOldSessions(cfg.BaseDir, now, cfg.Retention, cfg.MaxSessions, sessionDir)

	hostDirName := formatMemberDir(1, true, cfg.HostMemberID, cfg.HostName, cfg.HostComputer)
	hostWriter, err := newMemberWriter(filepath.Join(sessionDir, hostDirName))
	if err != nil {
		return nil, err
	}

	h := &HostController{
		cfg:         cfg,
		sessionID:   sessionID,
		sessionDir:  sessionDir,
		nonce:       RandomUint64(),
		startedAt:   now,
		inCh:        make(chan incomingAux, 512),
		peersCh:     make(chan []protocol.Peer, 8),
		stopCh:      make(chan struct{}),
		hostDirName: hostDirName,
		hostWriter:  hostWriter,
		hostWatcher: NewWatcher(cfg.HostWatcherCfg),
	}
	h.status = HostStatus{
		Enabled:    true,
		SessionID:  sessionID,
		SessionDir: sessionDir,
	}
	h.wg.Add(1)
	go h.run()
	return h, nil
}

// SessionDir returns the active session directory path.
func (h *HostController) SessionDir() string { return h.sessionDir }

// SessionID returns the active session identifier.
func (h *HostController) SessionID() string { return h.sessionID }

// HandleAux enqueues one incoming sideband packet from a room member.
func (h *HostController) HandleAux(sourceMember uint8, payload []byte) {
	mt, ok := PeekMessageType(payload)
	if !ok {
		return
	}
	switch mt {
	case MsgHello, MsgChunk:
	default:
		return
	}
	select {
	case h.inCh <- incomingAux{source: sourceMember, payload: payload}:
	default:
	}
}

// UpdatePeers notifies the host controller of updated room membership.
func (h *HostController) UpdatePeers(peers []protocol.Peer) {
	copied := append([]protocol.Peer(nil), peers...)
	select {
	case h.peersCh <- copied:
	default:
		select {
		case <-h.peersCh:
		default:
		}
		select {
		case h.peersCh <- copied:
		default:
		}
	}
}

// Status returns a snapshot of the host collection session.
func (h *HostController) Status() HostStatus {
	h.statusMu.RLock()
	defer h.statusMu.RUnlock()
	out := h.status
	out.Peers = append([]PeerSyncStatus(nil), h.status.Peers...)
	return out
}

// Stop stops log collection, tells teammates to stop streaming, flushes all
// files, and writes the final session_info.txt.
func (h *HostController) Stop() {
	h.stop.Do(func() {
		close(h.stopCh)
	})
	h.wg.Wait()
}

func (h *HostController) run() {
	defer h.wg.Done()
	peers := make(map[uint8]*peerReceiver)
	nextMemberIdx := 2 // 01 is reserved for the host

	pollTicker := time.NewTicker(h.cfg.PollInterval)
	ackTicker := time.NewTicker(h.cfg.AckDelay)
	defer pollTicker.Stop()
	defer ackTicker.Stop()

	lastMetaWrite := time.Time{}
	metaDirty := true

	// Initial local host poll + initial room peers sync.
	h.pollHostLocal()
	if h.cfg.Peers != nil {
		h.syncPeers(peers, h.cfg.Peers(), time.Now())
	}
	h.writeSessionInfo(peers, time.Time{})
	h.publishStatus(peers)
	lastMetaWrite = time.Now()
	metaDirty = false

	for {
		select {
		case <-h.stopCh:
			h.shutdown(peers)
			return

		case list := <-h.peersCh:
			h.syncPeers(peers, list, time.Now())
			metaDirty = true
			h.publishStatus(peers)

		case msg := <-h.inCh:
			if h.handleIncoming(peers, &nextMemberIdx, msg, time.Now()) {
				metaDirty = true
			}

		case <-ackTicker.C:
			h.flushPendingAcks(peers)

		case <-pollTicker.C:
			now := time.Now()
			h.pollHostLocal()
			h.hostWriter.closeIdle(now)
			if h.cfg.Peers != nil {
				h.syncPeers(peers, h.cfg.Peers(), now)
			}
			for _, pr := range peers {
				if pr.writer != nil {
					pr.writer.closeIdle(now)
				}
				h.stepPeerControl(pr, now)
			}
			h.flushPendingAcks(peers)
			if metaDirty || now.Sub(lastMetaWrite) >= 3*time.Second {
				h.writeSessionInfo(peers, time.Time{})
				lastMetaWrite = now
				metaDirty = false
			}
			h.publishStatus(peers)
		}
	}
}

func (h *HostController) pollHostLocal() {
	if h.hostWatcher == nil || h.hostWriter == nil {
		return
	}
	now := time.Now()
	h.hostWatcher.Poll(func(ch FileChunk) bool {
		_ = h.hostWriter.WriteChunk(ch, now)
		return true
	})
}

func (h *HostController) syncPeers(peers map[uint8]*peerReceiver, list []protocol.Peer, now time.Time) {
	present := make(map[uint8]protocol.Peer, len(list))
	for _, p := range list {
		if p.ID == h.cfg.HostMemberID || p.Host {
			continue
		}
		present[p.ID] = p
		pr, exists := peers[p.ID]
		if !exists {
			pr = &peerReceiver{
				memberID: p.ID,
				name:     p.Name,
				online:   p.Online,
				state:    "probing",
				reorder:  make(map[uint32]FileChunk),
			}
			if !p.Online {
				pr.state = "offline"
			}
			peers[p.ID] = pr
			if p.Online {
				h.sendCtrl(pr, true, now)
				pr.probesSent = 1
				pr.lastProbeAt = now
			}
			continue
		}
		pr.name = p.Name
		wasOnline := pr.online
		pr.online = p.Online
		if !p.Online {
			if pr.state != "offline" {
				pr.state = "offline"
			}
		} else if !wasOnline {
			// Member came back online: probe again.
			if pr.state == "offline" || pr.state == "unsupported" {
				pr.state = "probing"
				pr.probesSent = 0
			}
			h.sendCtrl(pr, true, now)
			pr.probesSent++
			pr.lastProbeAt = now
		}
	}
	for id, pr := range peers {
		if _, ok := present[id]; !ok {
			pr.online = false
			pr.state = "offline"
		}
	}
}

func (h *HostController) stepPeerControl(pr *peerReceiver, now time.Time) {
	if !pr.online {
		return
	}
	switch pr.state {
	case "probing":
		if pr.probesSent >= maxProbeAttempts {
			if now.Sub(pr.lastProbeAt) >= h.cfg.ProbeInterval {
				pr.state = "unsupported"
				h.cfg.Logf("WARN", fmt.Sprintf("队友 #%d %s 未响应日志收集（可能为旧版客户端）", pr.memberID, pr.name))
			}
			return
		}
		if now.Sub(pr.lastProbeAt) >= h.cfg.ProbeInterval {
			h.sendCtrl(pr, true, now)
			pr.probesSent++
			pr.lastProbeAt = now
		}
	case "syncing":
		if now.Sub(pr.lastCtrlAt) >= h.cfg.KeepaliveInterval {
			h.sendCtrl(pr, true, now)
		}
	}
}

func (h *HostController) sendCtrl(pr *peerReceiver, enabled bool, now time.Time) {
	if h.cfg.Send == nil {
		return
	}
	raw, err := EncodeCtrl(CtrlMessage{
		Enabled:   enabled,
		SessionID: h.sessionID,
		Nonce:     h.nonce,
	})
	if err != nil {
		return
	}
	if h.cfg.Send(pr.memberID, raw) {
		pr.lastCtrlAt = now
	}
}

func (h *HostController) handleIncoming(peers map[uint8]*peerReceiver, nextMemberIdx *int, msg incomingAux, now time.Time) bool {
	mt, ok := PeekMessageType(msg.payload)
	if !ok || msg.source == h.cfg.HostMemberID {
		return false
	}
	switch mt {
	case MsgHello:
		hello, err := DecodeHello(msg.payload)
		if err != nil || hello.Nonce != h.nonce || hello.SenderID == 0 {
			return false
		}
		pr := peers[msg.source]
		if pr == nil {
			pr = &peerReceiver{
				memberID: msg.source,
				name:     hello.MemberName,
				online:   true,
				reorder:  make(map[uint32]FileChunk),
			}
			peers[msg.source] = pr
		}
		if hello.MemberName != "" {
			pr.name = hello.MemberName
		}
		if hello.Computer != "" {
			pr.computer = hello.Computer
		}
		pr.online = true
		if pr.writer == nil {
			dirName := formatMemberDir(*nextMemberIdx, false, pr.memberID, pr.name, pr.computer)
			*nextMemberIdx = *nextMemberIdx + 1
			mw, err := newMemberWriter(filepath.Join(h.sessionDir, dirName))
			if err != nil {
				pr.lastErr = err.Error()
				h.cfg.Logf("WARN", fmt.Sprintf("创建队友日志目录失败 (#%d %s): %v", pr.memberID, pr.name, err))
				return false
			}
			pr.dirName = dirName
			pr.writer = mw
		}
		if pr.senderID != hello.SenderID {
			pr.senderID = hello.SenderID
			pr.nextSeq = 1
			clear(pr.reorder)
			pr.ackPending = false
			pr.ackChunks = 0
		}
		wasSyncing := pr.state == "syncing"
		pr.state = "syncing"
		pr.lastCtrlAt = now
		if ackRaw, err := EncodeHelloAck(HelloAckMessage{Nonce: h.nonce, SenderID: hello.SenderID}); err == nil && h.cfg.Send != nil {
			h.cfg.Send(pr.memberID, ackRaw)
		}
		if !wasSyncing {
			h.cfg.Logf("OK", fmt.Sprintf("队友 #%d %s (%s) 已连接日志同步 -> %s", pr.memberID, pr.name, pr.computer, pr.dirName))
		}
		h.publishStatus(peers)
		return true

	case MsgChunk:
		ch, err := DecodeChunk(msg.payload)
		if err != nil || ch.Nonce != h.nonce || ch.Seq == 0 {
			return false
		}
		pr := peers[msg.source]
		if pr == nil || pr.writer == nil || pr.senderID != ch.SenderID {
			return false
		}
		pr.state = "syncing"
		pr.online = true
		if ch.Seq < pr.nextSeq {
			// Duplicate already delivered -> acknowledge promptly.
			h.sendAck(pr)
			return false
		}
		if ch.Seq >= pr.nextSeq+64 {
			// Outside receive window -> advertise current CumAck/SackMask.
			h.sendAck(pr)
			return false
		}
		if _, dup := pr.reorder[ch.Seq]; !dup {
			pr.reorder[ch.Seq] = FileChunk{
				RelPath: ch.RelPath,
				Offset:  ch.Offset,
				Flags:   ch.Flags,
				Data:    ch.Data,
			}
		}
		wrote := false
		for {
			item, ok := pr.reorder[pr.nextSeq]
			if !ok {
				break
			}
			delete(pr.reorder, pr.nextSeq)
			if err := pr.writer.WriteChunk(item, now); err != nil {
				pr.lastErr = err.Error()
			}
			pr.nextSeq++
			wrote = true
		}
		pr.ackPending = true
		pr.ackChunks++
		// Immediate ACK on gap (triggers fast retransmit) or every 4 chunks.
		if len(pr.reorder) > 0 || pr.ackChunks >= 4 {
			h.sendAck(pr)
		}
		return wrote
	}
	return false
}

func (h *HostController) flushPendingAcks(peers map[uint8]*peerReceiver) {
	for _, pr := range peers {
		if pr.ackPending && pr.senderID != 0 {
			h.sendAck(pr)
		}
	}
}

func (h *HostController) sendAck(pr *peerReceiver) {
	if h.cfg.Send == nil || pr.senderID == 0 {
		return
	}
	cumAck := uint32(0)
	if pr.nextSeq > 1 {
		cumAck = pr.nextSeq - 1
	}
	var mask uint64
	for seq := range pr.reorder {
		if seq > cumAck && seq <= cumAck+64 {
			mask |= uint64(1) << (seq - cumAck - 1)
		}
	}
	raw := EncodeAck(AckMessage{
		Nonce:    h.nonce,
		SenderID: pr.senderID,
		CumAck:   cumAck,
		SackMask: mask,
		Window:   hostRecvWindow,
	})
	h.cfg.Send(pr.memberID, raw)
	pr.ackPending = false
	pr.ackChunks = 0
}

func (h *HostController) shutdown(peers map[uint8]*peerReceiver) {
	now := time.Now()
	// Send stop signal twice for UDP reliability.
	for attempt := 0; attempt < 2; attempt++ {
		for _, pr := range peers {
			if pr.online && (pr.state == "syncing" || pr.state == "probing") {
				h.sendCtrl(pr, false, now)
			}
		}
		if attempt == 0 {
			time.Sleep(40 * time.Millisecond)
		}
	}
	// Capture any final local host log lines before closing.
	h.pollHostLocal()
	h.hostWriter.Close()
	for _, pr := range peers {
		if pr.writer != nil {
			pr.writer.Close()
		}
	}
	h.writeSessionInfo(peers, time.Now())
	h.statusMu.Lock()
	h.status.Enabled = false
	h.statusMu.Unlock()
	if h.cfg.OnChange != nil {
		h.cfg.OnChange()
	}
}

func (h *HostController) publishStatus(peers map[uint8]*peerReceiver) {
	st := HostStatus{
		Enabled:    true,
		SessionID:  h.sessionID,
		SessionDir: h.sessionDir,
		HostFiles:  len(h.hostWriter.files),
		HostBytes:  h.hostWriter.totalWritten,
	}
	if h.hostWriter.lastErr != nil {
		st.LastError = h.hostWriter.lastErr.Error()
	}
	st.TotalFiles = st.HostFiles
	st.TotalBytes = st.HostBytes

	ids := make([]int, 0, len(peers))
	for id := range peers {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, id := range ids {
		pr := peers[uint8(id)]
		if pr.online {
			st.OnlineGuests++
		}
		if pr.state == "syncing" && pr.online {
			st.ActivePeers++
		}
		var files int
		var bytes uint64
		var updated time.Time
		if pr.writer != nil {
			files = len(pr.writer.files)
			bytes = pr.writer.totalWritten
			updated = pr.writer.lastUpdated
		}
		st.TotalFiles += files
		st.TotalBytes += bytes
		st.GuestBytes += bytes
		if st.LastError == "" && pr.lastErr != "" {
			st.LastError = pr.lastErr
		}
		st.Peers = append(st.Peers, PeerSyncStatus{
			MemberID:    pr.memberID,
			Name:        pr.name,
			Computer:    pr.computer,
			DirName:     pr.dirName,
			State:       pr.state,
			Files:       files,
			Bytes:       bytes,
			LastUpdated: updated,
		})
	}
	h.statusMu.Lock()
	h.status = st
	h.statusMu.Unlock()
	if h.cfg.OnChange != nil {
		h.cfg.OnChange()
	}
}

func (h *HostController) writeSessionInfo(peers map[uint8]*peerReceiver, endedAt time.Time) {
	var b strings.Builder
	b.WriteString("\uFEFF")
	fmt.Fprintf(&b, "Civ6Relay 日志收集会话清单\r\n")
	fmt.Fprintf(&b, "SessionID: %s\r\n", h.sessionID)
	fmt.Fprintf(&b, "RoomCode:  %s\r\n", h.cfg.RoomCode)
	if h.cfg.RelayAddr != "" {
		fmt.Fprintf(&b, "Relay:     %s\r\n", h.cfg.RelayAddr)
	}
	fmt.Fprintf(&b, "StartedAt: %s\r\n", h.startedAt.Format("2006-01-02 15:04:05 -0700"))
	if !endedAt.IsZero() {
		fmt.Fprintf(&b, "EndedAt:   %s\r\n", endedAt.Format("2006-01-02 15:04:05 -0700"))
	} else {
		fmt.Fprintf(&b, "UpdatedAt: %s (收集中)\r\n", time.Now().Format("2006-01-02 15:04:05 -0700"))
	}
	if h.cfg.PCAPPath != nil {
		if p := h.cfg.PCAPPath(); p != "" {
			fmt.Fprintf(&b, "HostPCAP:  %s\r\n", p)
		}
	}
	b.WriteString("\r\n=== 成员目录 ===\r\n")
	fmt.Fprintf(&b, "- [房主] #%02d %s (%s) -> %s/ (%d 文件, %s)\r\n",
		h.cfg.HostMemberID, h.cfg.HostName, h.cfg.HostComputer,
		h.hostDirName, len(h.hostWriter.files), FormatByteSize(h.hostWriter.totalWritten))

	ids := make([]int, 0, len(peers))
	for id := range peers {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, id := range ids {
		pr := peers[uint8(id)]
		dir := pr.dirName
		if dir == "" {
			dir = "(尚未建立)"
		} else {
			dir += "/"
		}
		var files int
		var bytes uint64
		if pr.writer != nil {
			files = len(pr.writer.files)
			bytes = pr.writer.totalWritten
		}
		comp := pr.computer
		if comp == "" {
			comp = "-"
		}
		fmt.Fprintf(&b, "- [队友] #%02d %s (%s) [%s] -> %s (%d 文件, %s)\r\n",
			pr.memberID, pr.name, comp, pr.state, dir, files, FormatByteSize(bytes))
		if pr.lastErr != "" {
			fmt.Fprintf(&b, "    错误: %s\r\n", pr.lastErr)
		}
	}

	dst := filepath.Join(h.sessionDir, "session_info.txt")
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err == nil {
		_ = os.Rename(tmp, dst)
	}
}

func formatMemberDir(orderIdx int, isHost bool, memberID uint8, name, computer string) string {
	role := "队员"
	if isHost {
		role = "房主"
	}
	parts := []string{
		fmt.Sprintf("%02d", orderIdx),
		role,
		fmt.Sprintf("#%02d", memberID),
		SanitizeDirComponent(name, "Player"),
	}
	if comp := SanitizeDirComponent(computer, ""); comp != "" && !strings.EqualFold(comp, parts[len(parts)-1]) {
		parts = append(parts, comp)
	}
	return strings.Join(parts, "_")
}

func createUniqueSessionDir(baseDir, roomCode string, now time.Time) (string, string, error) {
	safeRoom := SanitizeDirComponent(roomCode, "ROOM")
	baseName := fmt.Sprintf("%s_%s", now.Local().Format("20060102_150405"), safeRoom)
	for suffix := 0; suffix < 100; suffix++ {
		name := baseName
		if suffix > 0 {
			name = fmt.Sprintf("%s-%02d", baseName, suffix)
		}
		full := filepath.Join(baseDir, name)
		err := os.Mkdir(full, 0o755)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		return name, full, nil
	}
	return "", "", errors.New("too many log collection sessions share the same timestamp")
}

// PruneOldSessions removes session directories inside baseDir that are older
// than retention or exceed maxSessions, never removing activeDir.
func PruneOldSessions(baseDir string, now time.Time, retention time.Duration, maxSessions int, activeDir string) error {
	if retention <= 0 {
		retention = DefaultRetention
	}
	if maxSessions <= 0 {
		maxSessions = DefaultMaxSessions
	}
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	cleanActive := ""
	if activeDir != "" {
		cleanActive = filepath.Clean(activeDir)
	}
	type sessionEntry struct {
		path    string
		modTime time.Time
		name    string
	}
	var sessions []sessionEntry
	for _, entry := range entries {
		if !entry.IsDir() || !looksLikeSessionDir(entry.Name()) {
			continue
		}
		full := filepath.Join(baseDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		sessions = append(sessions, sessionEntry{path: full, modTime: info.ModTime(), name: entry.Name()})
	}
	// Sort newest first by folder timestamp name (and mtime tie-breaker).
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].name != sessions[j].name {
			return sessions[i].name > sessions[j].name
		}
		return sessions[i].modTime.After(sessions[j].modTime)
	})
	cutoff := now.Add(-retention)
	var errs []error
	for i, s := range sessions {
		if cleanActive != "" && filepath.Clean(s.path) == cleanActive {
			continue
		}
		if i >= maxSessions || s.modTime.Before(cutoff) {
			if err := os.RemoveAll(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func looksLikeSessionDir(name string) bool {
	// Matches YYYYMMDD_HHMMSS_*
	if len(name) < 17 || name[8] != '_' || name[15] != '_' {
		return false
	}
	for _, idx := range []int{0, 1, 2, 3, 4, 5, 6, 7, 9, 10, 11, 12, 13, 14} {
		if name[idx] < '0' || name[idx] > '9' {
			return false
		}
	}
	return true
}
