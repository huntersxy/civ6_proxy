package logsync

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	defaultDiscoverInterval   = 2 * time.Second
	defaultEnvInterval        = 60 * time.Second
	defaultEnvMinRefreshDelay = 10 * time.Second
	maxChunksPerFilePerPoll   = 8
	maxSnapshotFileBytes      = 4 << 20
	headFingerprintBytes      = 512
	lineAlignScanBytes        = 4096
)

// SourceSpec describes one local file to stream or snapshot.
type SourceSpec struct {
	RelPath          string    // validated relative path (e.g. "civ6_logs/Civ6SyncFix.log")
	FullPath         string    // absolute local path
	Snapshot         bool      // true -> emit entire file atomically when hash changes
	Binary           bool      // true -> use BinaryChunkSize (1 KiB) chunks; never sliced mid-file
	MaxBytes         int64     // optional byte cap for binary or non-segmented stream (0 defaults to MaxFileSizeBytes)
	SkipIfOlderThan  time.Time // if file exists at initial baseline with ModTime < SkipIfOlderThan, stay dormant until modified/replaced
	InitialMaxBytes  int64     // if > 0 and text file exceeds this when starting sync, tail latest line-aligned bytes
	SegmentMaxBytes  int64     // if > 0 and text stream reaches this remote size, rotate to a fresh segment
	MaxBacklogBytes  int64     // if > 0 and unread text backlog exceeds this during active streaming, fast-forward
	BacklogTailBytes int64     // tail window retained after a backlog fast-forward (defaults to MaxBacklogBytes/4)
}

// FileChunk is one emitted unit of file data ready for local write or ARQ send.
type FileChunk struct {
	RelPath     string
	Offset      uint64
	Flags       uint16
	Data        []byte
	DropPending bool // internal hint: local file was replaced/truncated, drop unsent queued chunks for RelPath
}

// WatcherConfig configures a local file Watcher.
type WatcherConfig struct {
	DiscoverFiles           func() []SourceSpec
	DiscoverFilesForSession func(sessionStart time.Time) []SourceSpec
	GenerateEnv             func() []byte
	DiscoverInterval        time.Duration
	EnvInterval             time.Duration
	Now                     func() time.Time
}

type trackedFile struct {
	spec            SourceSpec
	localOffset     int64
	remoteOffset    int64
	lastSize        int64
	lastMod         time.Time
	fileID          fileIdentity
	headLen         int
	headHash        [32]byte
	snapHash        [32]byte
	started         bool
	dormant         bool
	baselineChecked bool
}

// Watcher tracks a set of local files without holding file handles open
// between ticks, detecting live appends, snapshots, truncations, dormant
// pre-session files, initial large-file tailing, burst fast-forwarding,
// and long-session segment rotation.
type Watcher struct {
	cfg       WatcherConfig
	now       func() time.Time
	startedAt time.Time

	files        map[string]*trackedFile
	order        []string
	pollCursor   int
	lastDiscover time.Time
	lastEnvCheck time.Time
	envDirty     bool
	envStarted   bool
	lastEnvHash  [32]byte
}

// NewWatcher creates a fresh Watcher anchored to the current session start time.
func NewWatcher(cfg WatcherConfig) *Watcher {
	if cfg.DiscoverInterval <= 0 {
		cfg.DiscoverInterval = defaultDiscoverInterval
	}
	if cfg.EnvInterval <= 0 {
		cfg.EnvInterval = defaultEnvInterval
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Watcher{
		cfg:       cfg,
		now:       nowFn,
		startedAt: nowFn(),
		files:     make(map[string]*trackedFile),
		envDirty:  true,
	}
}

// MarkEnvDirty requests an env_check.txt refresh on the next eligible poll.
func (w *Watcher) MarkEnvDirty() {
	w.envDirty = true
}

// Poll discovers newly created files, checks env_check.txt snapshots, and
// streams incremental changes via emit. If emit returns false (backpressure),
// Poll stops immediately without losing its offset position and resumes fairly
// across remaining files on the next poll.
func (w *Watcher) Poll(emit func(FileChunk) bool) {
	now := w.now()
	hasDiscover := w.cfg.DiscoverFilesForSession != nil || w.cfg.DiscoverFiles != nil
	if hasDiscover && (w.lastDiscover.IsZero() || now.Sub(w.lastDiscover) >= w.cfg.DiscoverInterval) {
		w.refreshSources(now)
	}
	if w.cfg.GenerateEnv != nil {
		duePeriodic := !w.envStarted || now.Sub(w.lastEnvCheck) >= w.cfg.EnvInterval
		dueDirty := w.envDirty && (w.lastEnvCheck.IsZero() || now.Sub(w.lastEnvCheck) >= defaultEnvMinRefreshDelay)
		if duePeriodic || dueDirty {
			if !w.pollEnv(now, emit) {
				return
			}
		}
	}
	n := len(w.order)
	if n == 0 {
		return
	}
	start := w.pollCursor % n
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		tf := w.files[w.order[idx]]
		if tf == nil {
			continue
		}
		var ok bool
		if tf.spec.Snapshot {
			ok = w.pollSnapshotFile(tf, emit)
		} else {
			ok = w.pollStreamFile(tf, emit)
		}
		if !ok {
			w.pollCursor = (idx + 1) % n
			return
		}
	}
	w.pollCursor = 0
}

func (w *Watcher) refreshSources(now time.Time) {
	w.lastDiscover = now
	var discovered []SourceSpec
	if w.cfg.DiscoverFilesForSession != nil {
		discovered = w.cfg.DiscoverFilesForSession(w.startedAt)
	} else if w.cfg.DiscoverFiles != nil {
		discovered = w.cfg.DiscoverFiles()
	}
	changed := false
	seen := make(map[string]struct{}, len(discovered))
	for _, spec := range discovered {
		rel, err := ValidateRelPath(spec.RelPath)
		if err != nil || spec.FullPath == "" {
			continue
		}
		spec.RelPath = rel
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		if existing, ok := w.files[rel]; ok {
			if existing.spec.FullPath != spec.FullPath || existing.spec.Snapshot != spec.Snapshot || existing.spec.Binary != spec.Binary {
				existing.spec = spec
				existing.started = false
				existing.dormant = false
				existing.baselineChecked = false
				existing.localOffset = 0
				existing.remoteOffset = 0
				existing.headLen = 0
				existing.headHash = [32]byte{}
				existing.fileID = fileIdentity{}
				changed = true
			} else {
				existing.spec.MaxBytes = spec.MaxBytes
				existing.spec.SkipIfOlderThan = spec.SkipIfOlderThan
				existing.spec.InitialMaxBytes = spec.InitialMaxBytes
				existing.spec.SegmentMaxBytes = spec.SegmentMaxBytes
				existing.spec.MaxBacklogBytes = spec.MaxBacklogBytes
				existing.spec.BacklogTailBytes = spec.BacklogTailBytes
			}
		} else {
			w.files[rel] = &trackedFile{spec: spec}
			changed = true
		}
	}
	for rel := range w.files {
		if _, ok := seen[rel]; !ok {
			delete(w.files, rel)
			changed = true
		}
	}
	if changed || len(w.order) != len(w.files) {
		w.order = w.order[:0]
		for rel := range w.files {
			w.order = append(w.order, rel)
		}
		sort.Slice(w.order, func(i, j int) bool {
			pi, pj := relPathPriority(w.order[i]), relPathPriority(w.order[j])
			if pi != pj {
				return pi < pj
			}
			return w.order[i] < w.order[j]
		})
		if len(w.order) == 0 || w.pollCursor >= len(w.order) {
			w.pollCursor = 0
		}
		if w.envStarted {
			w.envDirty = true
		}
	}
}

func relPathPriority(rel string) int {
	lower := strings.ToLower(rel)
	switch {
	case lower == "env_check.txt":
		return 0
	case lower == "relay_logs/config.json" ||
		lower == "relay_logs/civ6relaydiag.txt" ||
		lower == "civ6_logs/civ6syncfix.log" ||
		lower == "civ6_logs/fastsync.log" ||
		lower == "civ6_logs/modding.log" ||
		lower == "civ6_logs/database.log":
		return 1
	case lower == "civ6_logs/net_message_debug.log" ||
		lower == "civ6_logs/net_connection_debug.log" ||
		lower == "civ6_logs/lua.log" ||
		(strings.HasPrefix(lower, "civ6_logs/") && strings.Contains(lower, "desync")):
		return 2
	case strings.HasPrefix(lower, "civ6_logs/"):
		return 3
	case strings.HasPrefix(lower, "relay_logs/"):
		return 4
	case strings.HasPrefix(lower, "relay_captures/"):
		return 5
	default:
		return 6
	}
}

func (w *Watcher) pollEnv(now time.Time, emit func(FileChunk) bool) bool {
	content := w.cfg.GenerateEnv()
	if len(content) == 0 {
		w.lastEnvCheck = now
		w.envDirty = false
		return true
	}
	normHash := hashIgnoringCollectedAt(content)
	if w.envStarted && normHash == w.lastEnvHash {
		w.lastEnvCheck = now
		w.envDirty = false
		return true
	}
	if !emitSnapshotBytes("env_check.txt", content, TextChunkSize, emit) {
		return false
	}
	w.envStarted = true
	w.lastEnvHash = normHash
	w.lastEnvCheck = now
	w.envDirty = false
	return true
}

func hashIgnoringCollectedAt(data []byte) [32]byte {
	h := sha256.New()
	for _, line := range bytes.Split(data, []byte("\n")) {
		trimmed := bytes.TrimPrefix(line, []byte{0xEF, 0xBB, 0xBF})
		if bytes.HasPrefix(trimmed, []byte("CollectedAt:")) {
			continue
		}
		_, _ = h.Write(line)
		_, _ = h.Write([]byte{'\n'})
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

func (w *Watcher) pollSnapshotFile(tf *trackedFile, emit func(FileChunk) bool) bool {
	info, err := os.Stat(tf.spec.FullPath)
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	if tf.started && info.Size() == tf.lastSize && info.ModTime().Equal(tf.lastMod) {
		return true
	}
	f, err := openSharedRead(tf.spec.FullPath)
	if err != nil {
		return true
	}
	limit := int64(maxSnapshotFileBytes)
	if tf.spec.MaxBytes > 0 && tf.spec.MaxBytes < limit {
		limit = tf.spec.MaxBytes
	}
	data, readErr := io.ReadAll(io.LimitReader(f, limit))
	_ = f.Close()
	if readErr != nil {
		return true
	}
	sum := sha256.Sum256(data)
	if tf.started && sum == tf.snapHash {
		tf.lastSize = info.Size()
		tf.lastMod = info.ModTime()
		return true
	}
	chunkSize := TextChunkSize
	if tf.spec.Binary {
		chunkSize = BinaryChunkSize
	}
	if !emitSnapshotBytes(tf.spec.RelPath, data, chunkSize, emit) {
		return false
	}
	tf.started = true
	tf.snapHash = sum
	tf.lastSize = info.Size()
	tf.lastMod = info.ModTime()
	return true
}

func emitSnapshotBytes(rel string, data []byte, chunkSize int, emit func(FileChunk) bool) bool {
	if len(data) == 0 {
		return emit(FileChunk{
			RelPath: rel,
			Offset:  0,
			Flags:   ChunkFlagReset | ChunkFlagSnapshot | ChunkFlagEOF,
		})
	}
	for off := 0; off < len(data); off += chunkSize {
		end := min(off+chunkSize, len(data))
		var flags uint16 = ChunkFlagSnapshot
		if off == 0 {
			flags |= ChunkFlagReset
		}
		if end == len(data) {
			flags |= ChunkFlagEOF
		}
		chunk := FileChunk{
			RelPath: rel,
			Offset:  uint64(off),
			Flags:   flags,
			Data:    append([]byte(nil), data[off:end]...),
		}
		if !emit(chunk) {
			return false
		}
	}
	return true
}

func readHeadFingerprint(f *os.File, size int64) (int, [32]byte) {
	if size <= 0 {
		return 0, [32]byte{}
	}
	want := min(size, int64(headFingerprintBytes))
	var head [headFingerprintBytes]byte
	n, err := f.ReadAt(head[:want], 0)
	if n <= 0 || (err != nil && err != io.EOF) {
		return 0, [32]byte{}
	}
	return n, sha256.Sum256(head[:n])
}

func headChanged(f *os.File, size int64, headLen int, headHash [32]byte) bool {
	if headLen <= 0 || size < int64(headLen) {
		return false
	}
	var head [headFingerprintBytes]byte
	n, err := f.ReadAt(head[:headLen], 0)
	if err != nil && err != io.EOF {
		return false
	}
	return n < headLen || sha256.Sum256(head[:n]) != headHash
}

// alignToNextLine advances startOff to the start of the next complete line
// within lineAlignScanBytes so tailed text logs do not begin mid-line.
func alignToNextLine(f *os.File, startOff, fileSize int64) int64 {
	if startOff <= 0 || startOff >= fileSize {
		return startOff
	}
	scanLen := min(int64(lineAlignScanBytes), fileSize-startOff)
	buf := make([]byte, scanLen)
	n, err := f.ReadAt(buf, startOff)
	if n <= 0 || (err != nil && err != io.EOF) {
		return startOff
	}
	if idx := bytes.IndexByte(buf[:n], '\n'); idx >= 0 {
		next := startOff + int64(idx) + 1
		if next < fileSize {
			return next
		}
	}
	return startOff
}

func (w *Watcher) pollStreamFile(tf *trackedFile, emit func(FileChunk) bool) bool {
	info, err := os.Stat(tf.spec.FullPath)
	if err != nil || !info.Mode().IsRegular() {
		tf.baselineChecked = true
		if tf.dormant {
			tf.dormant = false
			tf.started = false
			tf.localOffset = 0
			tf.remoteOffset = 0
			tf.headLen = 0
			tf.headHash = [32]byte{}
			tf.fileID = fileIdentity{}
		}
		return true
	}

	hardMax := tf.spec.MaxBytes
	if hardMax <= 0 || hardMax > MaxFileSizeBytes {
		hardMax = MaxFileSizeBytes
	}
	segLimit := int64(0)
	if !tf.spec.Binary && tf.spec.SegmentMaxBytes > 0 {
		segLimit = min(tf.spec.SegmentMaxBytes, int64(MaxFileSizeBytes))
	}

	if tf.dormant && info.Size() == tf.lastSize && info.ModTime().Equal(tf.lastMod) {
		return true
	}
	if tf.started && info.Size() == tf.lastSize && info.ModTime().Equal(tf.lastMod) {
		if segLimit > 0 {
			if tf.localOffset >= info.Size() {
				return true
			}
		} else if tf.localOffset >= min(info.Size(), hardMax) {
			return true
		}
	}

	f, err := openSharedRead(tf.spec.FullPath)
	if err != nil {
		return true
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return true
	}
	size := st.Size()
	id := getFileIdentity(f)

	// 1. First-time baseline check: park pre-session stale files in dormant mode (0 bytes sent).
	if !tf.started && !tf.dormant && !tf.baselineChecked {
		tf.baselineChecked = true
		if !tf.spec.SkipIfOlderThan.IsZero() && st.ModTime().Before(tf.spec.SkipIfOlderThan) {
			tf.dormant = true
			tf.lastSize = size
			tf.lastMod = st.ModTime()
			tf.fileID = id
			tf.headLen, tf.headHash = readHeadFingerprint(f, size)
			return true
		}
	}

	needFreshStart := false
	dropPending := false

	// 2. Wake-up evaluation for dormant files.
	if tf.dormant {
		replaced := size < tf.lastSize ||
			(tf.fileID.valid && id.valid && !tf.fileID.equal(id)) ||
			headChanged(f, size, tf.headLen, tf.headHash)

		if !replaced && size == tf.lastSize {
			tf.lastMod = st.ModTime()
			return true
		}
		if !replaced && size > tf.lastSize && !tf.spec.Binary && tf.lastSize > 0 {
			// Pre-session text file had new lines appended without truncation: resume from baseline.
			startOff := tf.lastSize
			if tf.spec.InitialMaxBytes > 0 && size-startOff > tf.spec.InitialMaxBytes {
				startOff = alignToNextLine(f, size-tf.spec.InitialMaxBytes, size)
			}
			banner := []byte(fmt.Sprintf("[Civ6Relay] skipped pre-session %s; streaming new writes from offset %d\r\n",
				FormatByteSize(uint64(startOff)), startOff))
			if !emit(FileChunk{
				RelPath: tf.spec.RelPath,
				Offset:  0,
				Flags:   ChunkFlagReset,
				Data:    banner,
			}) {
				return false
			}
			tf.dormant = false
			tf.started = true
			tf.localOffset = startOff
			tf.remoteOffset = int64(len(banner))
			tf.fileID = id
			tf.headLen, tf.headHash = readHeadFingerprint(f, size)
		} else {
			// Truncated/replaced (e.g., 300 MiB old log overwritten to 5 KiB on game launch) or empty/binary.
			needFreshStart = true
		}
	} else if tf.started {
		// 3. Active file replacement/truncation check.
		replaced := size < tf.localOffset ||
			(tf.fileID.valid && id.valid && !tf.fileID.equal(id)) ||
			headChanged(f, size, tf.headLen, tf.headHash)
		if replaced {
			needFreshStart = true
			dropPending = true
		}
	} else {
		needFreshStart = true
	}

	// 4. Fresh start (new file, woken truncated/replaced file, or active file replaced mid-session).
	needResetFlag := false
	localOff := tf.localOffset
	remoteOff := tf.remoteOffset
	if needFreshStart {
		if size == 0 {
			if !emit(FileChunk{
				RelPath:     tf.spec.RelPath,
				Offset:      0,
				Flags:       ChunkFlagReset,
				DropPending: dropPending,
			}) {
				return false
			}
			tf.dormant = false
			tf.started = true
			tf.localOffset = 0
			tf.remoteOffset = 0
			tf.fileID = id
			tf.headLen = 0
			tf.headHash = [32]byte{}
			tf.lastSize = 0
			tf.lastMod = st.ModTime()
			return true
		}
		if !tf.spec.Binary && tf.spec.InitialMaxBytes > 0 && size > tf.spec.InitialMaxBytes {
			startOff := alignToNextLine(f, size-tf.spec.InitialMaxBytes, size)
			banner := []byte(fmt.Sprintf("[Civ6Relay] file was %s at sync start; omitted earlier %s, tailing from offset %d\r\n",
				FormatByteSize(uint64(size)), FormatByteSize(uint64(startOff)), startOff))
			if !emit(FileChunk{
				RelPath:     tf.spec.RelPath,
				Offset:      0,
				Flags:       ChunkFlagReset,
				Data:        banner,
				DropPending: dropPending,
			}) {
				return false
			}
			tf.dormant = false
			tf.started = true
			tf.localOffset = startOff
			tf.remoteOffset = int64(len(banner))
			tf.fileID = id
			tf.headLen, tf.headHash = readHeadFingerprint(f, size)
			localOff = tf.localOffset
			remoteOff = tf.remoteOffset
			dropPending = false
		} else {
			needResetFlag = true
			localOff = 0
			remoteOff = 0
		}
	}

	// 5. Active streaming safeguards: burst backlog fast-forward & long-session segment rotation.
	if tf.started && !needResetFlag && !tf.spec.Binary {
		if tf.spec.MaxBacklogBytes > 0 && size-localOff > tf.spec.MaxBacklogBytes {
			tailKeep := tf.spec.BacklogTailBytes
			if tailKeep <= 0 || tailKeep >= tf.spec.MaxBacklogBytes {
				tailKeep = max(tf.spec.MaxBacklogBytes/4, int64(TextChunkSize))
			}
			newLocal := alignToNextLine(f, size-tailKeep, size)
			if skipped := newLocal - localOff; skipped > 0 {
				banner := []byte(fmt.Sprintf("\r\n[Civ6Relay] fast-forwarded %s burst backlog (local offset %d -> %d)\r\n",
					FormatByteSize(uint64(skipped)), localOff, newLocal))
				var flags uint16
				writeOff := remoteOff
				if segLimit > 0 && remoteOff+int64(len(banner)) >= segLimit {
					flags = ChunkFlagReset
					writeOff = 0
				} else if segLimit == 0 && remoteOff+int64(len(banner)) > hardMax {
					return true
				}
				if !emit(FileChunk{
					RelPath: tf.spec.RelPath,
					Offset:  uint64(writeOff),
					Flags:   flags,
					Data:    banner,
				}) {
					return false
				}
				localOff = newLocal
				remoteOff = writeOff + int64(len(banner))
				tf.localOffset = localOff
				tf.remoteOffset = remoteOff
			}
		}

		if segLimit > 0 && remoteOff >= segLimit && localOff < size {
			banner := []byte(fmt.Sprintf("[Civ6Relay] log segment rotated; continuing from local offset %d (%s)\r\n",
				localOff, FormatByteSize(uint64(localOff))))
			if !emit(FileChunk{
				RelPath: tf.spec.RelPath,
				Offset:  0,
				Flags:   ChunkFlagReset,
				Data:    banner,
			}) {
				return false
			}
			remoteOff = int64(len(banner))
			tf.remoteOffset = remoteOff
		}
	}

	// 6. Stream data chunks up to per-poll burst cap.
	chunkSize := TextChunkSize
	if tf.spec.Binary {
		chunkSize = BinaryChunkSize
	}
	buf := make([]byte, chunkSize)
	sent := 0
	for localOff < size && sent < maxChunksPerFilePerPoll {
		var room int64
		if segLimit > 0 {
			room = segLimit - remoteOff
		} else {
			room = min(hardMax-localOff, int64(MaxFileSizeBytes)-remoteOff)
		}
		if room <= 0 {
			break
		}
		want := min(int64(chunkSize), room)
		n, rerr := f.ReadAt(buf[:want], localOff)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			var flags uint16
			chunkDrop := false
			if needResetFlag {
				flags |= ChunkFlagReset
				chunkDrop = dropPending
			}
			if !emit(FileChunk{
				RelPath:     tf.spec.RelPath,
				Offset:      uint64(remoteOff),
				Flags:       flags,
				Data:        data,
				DropPending: chunkDrop,
			}) {
				return false
			}
			if localOff == 0 {
				hlen := min(n, headFingerprintBytes)
				tf.headLen = hlen
				tf.headHash = sha256.Sum256(data[:hlen])
				tf.fileID = id
			}
			if needResetFlag {
				tf.dormant = false
				tf.started = true
				needResetFlag = false
				dropPending = false
			}
			localOff += int64(n)
			remoteOff += int64(n)
			tf.localOffset = localOff
			tf.remoteOffset = remoteOff
			sent++
		}
		if rerr != nil {
			break
		}
	}
	tf.lastSize = size
	tf.lastMod = st.ModTime()
	return true
}
