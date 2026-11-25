package pcap

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultRetention = 72 * time.Hour
	linkTypeRaw      = 101
	snapLength       = 65535
	queueSize        = 512
	flushInterval    = 250 * time.Millisecond
	cleanupInterval  = time.Hour
)

type packetRecord struct {
	at   time.Time
	data []byte
}

type Recorder struct {
	dir       string
	roomCode  string
	retention time.Duration
	queue     chan packetRecord
	done      chan struct{}

	submitMu sync.RWMutex
	closed   bool

	stateMu sync.RWMutex
	path    string
	err     error

	written atomic.Uint64
	dropped atomic.Uint64
}

func NewRecorder(dir, roomCode string) (*Recorder, error) {
	return newRecorderAt(dir, roomCode, DefaultRetention, time.Now())
}

func PruneExpired(dir string, now time.Time, activePath string) error {
	err := pruneExpired(dir, now.Add(-DefaultRetention), activePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func newRecorderAt(dir, roomCode string, retention time.Duration, now time.Time) (*Recorder, error) {
	if retention <= 0 {
		retention = DefaultRetention
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := pruneExpired(dir, now.Add(-retention), ""); err != nil {
		return nil, err
	}
	roomCode = safeName(roomCode)
	file, writer, path, err := openCaptureFile(dir, roomCode, now)
	if err != nil {
		return nil, err
	}
	r := &Recorder{
		dir: dir, roomCode: roomCode, retention: retention,
		queue: make(chan packetRecord, queueSize), done: make(chan struct{}), path: path,
	}
	go r.run(file, writer, now.Local().Format("2006010215"))
	return r, nil
}

func (r *Recorder) Record(packet []byte) {
	r.recordAt(time.Now(), packet)
}

func (r *Recorder) recordAt(at time.Time, packet []byte) {
	if len(packet) == 0 || len(packet) > snapLength {
		return
	}
	r.submitMu.RLock()
	defer r.submitMu.RUnlock()
	if r.closed {
		return
	}
	if len(r.queue) == cap(r.queue) {
		r.dropped.Add(1)
		return
	}
	item := packetRecord{at: at, data: append([]byte(nil), packet...)}
	select {
	case r.queue <- item:
	default:
		r.dropped.Add(1)
	}
}

func (r *Recorder) disable() {
	r.submitMu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.submitMu.Unlock()
}

func (r *Recorder) Path() string {
	r.stateMu.RLock()
	defer r.stateMu.RUnlock()
	return r.path
}

func (r *Recorder) Error() error {
	r.stateMu.RLock()
	defer r.stateMu.RUnlock()
	return r.err
}

func (r *Recorder) Stats() (written, dropped uint64) {
	return r.written.Load(), r.dropped.Load()
}

func (r *Recorder) setPath(path string) {
	r.stateMu.Lock()
	r.path = path
	r.stateMu.Unlock()
}

func (r *Recorder) setError(err error) {
	if err == nil {
		return
	}
	r.stateMu.Lock()
	r.err = errors.Join(r.err, err)
	r.stateMu.Unlock()
}

func (r *Recorder) Close() error {
	r.submitMu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.submitMu.Unlock()
	<-r.done
	return r.Error()
}

func (r *Recorder) run(file *os.File, writer *bufio.Writer, hour string) {
	defer close(r.done)
	defer func() {
		if file != nil {
			if err := flushAndClose(writer, file); err != nil {
				r.setError(err)
			}
		}
		if err := pruneExpired(r.dir, time.Now().Add(-r.retention), ""); err != nil {
			r.setError(err)
		}
	}()

	flushTicker := time.NewTicker(flushInterval)
	cleanupTicker := time.NewTicker(cleanupInterval)
	defer flushTicker.Stop()
	defer cleanupTicker.Stop()

	for {
		select {
		case item, ok := <-r.queue:
			if !ok {
				return
			}
			itemHour := item.at.Local().Format("2006010215")
			if itemHour != hour {
				if err := flushAndClose(writer, file); err != nil {
					r.setError(err)
					r.disable()
					return
				}
				nextFile, nextWriter, _, err := openCaptureFile(r.dir, r.roomCode, item.at)
				if err != nil {
					r.setError(err)
					r.disable()
					return
				}
				file, writer = nextFile, nextWriter
				hour = itemHour
				r.setPath(file.Name())
			}
			if err := writeRecord(writer, item); err != nil {
				r.setError(err)
				r.disable()
				return
			}
			r.written.Add(1)
		case <-flushTicker.C:
			if err := writer.Flush(); err != nil {
				r.setError(err)
				r.disable()
				return
			}
		case <-cleanupTicker.C:
			if err := pruneExpired(r.dir, time.Now().Add(-r.retention), r.Path()); err != nil {
				r.setError(err)
			}
		}
	}
}

func openCaptureFile(dir, roomCode string, at time.Time) (*os.File, *bufio.Writer, string, error) {
	base := fmt.Sprintf("civ6relay-%s-%s", at.Local().Format("20060102-15"), roomCode)
	for suffix := 0; suffix < 1000; suffix++ {
		name := base
		if suffix != 0 {
			name += fmt.Sprintf("-%02d", suffix)
		}
		path := filepath.Join(dir, name+".pcap")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, nil, "", err
		}
		writer := bufio.NewWriterSize(file, 256<<10)
		if err := writeGlobalHeader(writer); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return nil, nil, "", err
		}
		return file, writer, path, nil
	}
	return nil, nil, "", errors.New("too many PCAP files share the same timestamp")
}

func writeGlobalHeader(writer *bufio.Writer) error {
	var header [24]byte
	binary.LittleEndian.PutUint32(header[0:4], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(header[4:6], 2)
	binary.LittleEndian.PutUint16(header[6:8], 4)
	binary.LittleEndian.PutUint32(header[16:20], snapLength)
	binary.LittleEndian.PutUint32(header[20:24], linkTypeRaw)
	_, err := writer.Write(header[:])
	return err
}

func writeRecord(writer *bufio.Writer, item packetRecord) error {
	var header [16]byte
	seconds := item.at.Unix()
	if seconds < 0 {
		seconds = 0
	}
	binary.LittleEndian.PutUint32(header[0:4], uint32(seconds))
	binary.LittleEndian.PutUint32(header[4:8], uint32(item.at.Nanosecond()/int(time.Microsecond)))
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(item.data)))
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(item.data)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(item.data)
	return err
}

func flushAndClose(writer *bufio.Writer, file *os.File) error {
	return errors.Join(writer.Flush(), file.Close())
}

func pruneExpired(dir string, cutoff time.Time, active string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".pcap") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if filepath.Clean(path) == filepath.Clean(active) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func safeName(value string) string {
	var out strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			out.WriteRune(char)
		}
	}
	if out.Len() == 0 {
		return "room"
	}
	return out.String()
}
