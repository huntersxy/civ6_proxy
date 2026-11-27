package logsync

import (
	"bytes"
	"crypto/sha256"
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
	maxChunksPerFilePerPoll   = 16
	maxSnapshotFileBytes      = 4 << 20
	headFingerprintBytes      = 512
)

// SourceSpec describes one local file to stream or snapshot.
type SourceSpec struct {
	RelPath  string // validated relative path (e.g. "civ6_logs/Civ6SyncFix.log")
	FullPath string // absolute local path
	Snapshot bool   // true -> emit entire file atomically when hash changes
	Binary   bool   // true -> use BinaryChunkSize (1 KiB) chunks
	MaxBytes int64  // optional byte cap (0 defaults to MaxFileSizeBytes)
}

// FileChunk is one emitted unit of file data ready for local write or ARQ send.
type FileChunk struct {
	RelPath string
	Offset  uint64
	Flags   uint16
	Data    []byte
}

// WatcherConfig configures a local file Watcher.
type WatcherConfig struct {
	DiscoverFiles    func() []SourceSpec
	GenerateEnv      func() []byte
	DiscoverInterval time.Duration
	EnvInterval      time.Duration
	Now              func() time.Time
}

type trackedFile struct {
	spec     SourceSpec
	offset   int64
	lastSize int64
	lastMod  time.Time
	fileID   fileIdentity
	headLen  int
	headHash [32]byte
	snapHash [32]byte
	started  bool
}

// Watcher tracks a set of local files without holding file handles open
// between ticks, detecting live appends, snapshots, and file truncations.
type Watcher struct {
	cfg WatcherConfig
	now func() time.Time

	files        map[string]*trackedFile
	order        []string
	lastDiscover time.Time
	lastEnvCheck time.Time
	envDirty     bool
	envStarted   bool
	lastEnvHash  [32]byte
}

// NewWatcher creates a fresh Watcher.
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
		cfg:      cfg,
		now:      nowFn,
		files:    make(map[string]*trackedFile),
		envDirty: true,
	}
}

// MarkEnvDirty requests an env_check.txt refresh on the next eligible poll.
func (w *Watcher) MarkEnvDirty() {
	w.envDirty = true
}

// Poll discovers newly created files, checks env_check.txt snapshots, and
// streams incremental changes via emit. If emit returns false (backpressure),
// Poll stops immediately without losing its offset position.
func (w *Watcher) Poll(emit func(FileChunk) bool) {
	now := w.now()
	if w.cfg.DiscoverFiles != nil && (w.lastDiscover.IsZero() || now.Sub(w.lastDiscover) >= w.cfg.DiscoverInterval) {
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
	for _, rel := range w.order {
		tf := w.files[rel]
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
			return
		}
	}
}

func (w *Watcher) refreshSources(now time.Time) {
	w.lastDiscover = now
	discovered := w.cfg.DiscoverFiles()
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
				existing.offset = 0
				changed = true
			} else if spec.MaxBytes > 0 {
				existing.spec.MaxBytes = spec.MaxBytes
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
		if w.envStarted {
			w.envDirty = true
		}
	}
}

func relPathPriority(rel string) int {
	switch {
	case rel == "env_check.txt":
		return 0
	case strings.HasPrefix(rel, "civ6_logs/"):
		return 1
	case strings.HasPrefix(rel, "relay_logs/"):
		return 2
	case strings.HasPrefix(rel, "relay_captures/"):
		return 3
	default:
		return 4
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

func (w *Watcher) pollStreamFile(tf *trackedFile, emit func(FileChunk) bool) bool {
	info, err := os.Stat(tf.spec.FullPath)
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	maxBytes := tf.spec.MaxBytes
	if maxBytes <= 0 || maxBytes > MaxFileSizeBytes {
		maxBytes = MaxFileSizeBytes
	}
	targetCap := min(info.Size(), maxBytes)
	if tf.started && info.Size() == tf.lastSize && info.ModTime().Equal(tf.lastMod) && tf.offset >= targetCap {
		return true
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

	replaced := false
	if tf.started {
		switch {
		case size < tf.offset:
			replaced = true
		case tf.fileID.valid && id.valid && !tf.fileID.equal(id):
			replaced = true
		case tf.headLen > 0 && size >= int64(tf.headLen):
			var head [headFingerprintBytes]byte
			n, rerr := f.ReadAt(head[:tf.headLen], 0)
			if rerr == nil || rerr == io.EOF {
				if n < tf.headLen || sha256.Sum256(head[:n]) != tf.headHash {
					replaced = true
				}
			}
		}
	}

	needReset := !tf.started || replaced
	if needReset {
		tf.offset = 0
		tf.headLen = 0
		tf.headHash = [32]byte{}
	}

	// Mirror newly discovered empty files once.
	if size == 0 {
		if needReset {
			if !emit(FileChunk{RelPath: tf.spec.RelPath, Offset: 0, Flags: ChunkFlagReset}) {
				return false
			}
			tf.started = true
			tf.fileID = id
		}
		tf.lastSize = 0
		tf.lastMod = st.ModTime()
		return true
	}

	if tf.offset >= maxBytes {
		tf.lastSize = size
		tf.lastMod = st.ModTime()
		return true
	}

	chunkSize := TextChunkSize
	if tf.spec.Binary {
		chunkSize = BinaryChunkSize
	}
	buf := make([]byte, chunkSize)
	sent := 0
	for tf.offset < size && tf.offset < maxBytes && sent < maxChunksPerFilePerPoll {
		want := int64(chunkSize)
		if rem := maxBytes - tf.offset; rem < want {
			want = rem
		}
		n, rerr := f.ReadAt(buf[:want], tf.offset)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			var flags uint16
			if needReset {
				flags |= ChunkFlagReset
			}
			if !emit(FileChunk{
				RelPath: tf.spec.RelPath,
				Offset:  uint64(tf.offset),
				Flags:   flags,
				Data:    data,
			}) {
				return false
			}
			if tf.offset == 0 {
				hlen := min(n, headFingerprintBytes)
				tf.headLen = hlen
				tf.headHash = sha256.Sum256(data[:hlen])
				tf.fileID = id
			}
			tf.offset += int64(n)
			tf.started = true
			needReset = false
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
