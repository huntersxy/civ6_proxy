package pcap

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecorderWritesRawIPv4PCAPAndRotatesHourly(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, time.January, 1, 12, 59, 59, 0, time.Local)
	recorder, err := newRecorderAt(dir, "AB4T-DNES", 72*time.Hour, start)
	if err != nil {
		t.Fatal(err)
	}
	recorder.recordAt(start, []byte{0x45, 0x00, 0x01})
	nextHour := start.Add(2 * time.Second)
	recorder.recordAt(nextHour, []byte{0x45, 0x01, 0x02, 0x03})
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if written, dropped := recorder.Stats(); written != 2 || dropped != 0 {
		t.Fatalf("written=%d dropped=%d", written, dropped)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected one file per day, got %d", len(entries))
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) < 24+16+3 || binary.LittleEndian.Uint32(data[:4]) != 0xa1b2c3d4 ||
			binary.LittleEndian.Uint16(data[4:6]) != 2 || binary.LittleEndian.Uint16(data[6:8]) != 4 ||
			binary.LittleEndian.Uint32(data[16:20]) != snapLength || binary.LittleEndian.Uint32(data[20:24]) != linkTypeRaw {
			t.Fatalf("invalid PCAP header in %s", entry.Name())
		}
		capturedLength := int(binary.LittleEndian.Uint32(data[32:36]))
		if capturedLength <= 0 || len(data) != 24+16+capturedLength {
			t.Fatalf("invalid PCAP record in %s", entry.Name())
		}
	}
}

func TestPruneExpiredRemovesOnlyOldPCAPFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	oldPCAP := filepath.Join(dir, "old.pcap")
	recentPCAP := filepath.Join(dir, "recent.PCAP")
	oldLog := filepath.Join(dir, "old.log")
	for _, path := range []string{oldPCAP, recentPCAP, oldLog} {
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := now.Add(-73 * time.Hour)
	if err := os.Chtimes(oldPCAP, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldLog, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := pruneExpired(dir, now.Add(-DefaultRetention), ""); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{oldPCAP, recentPCAP, oldLog} {
		_, err := os.Stat(path)
		wantMissing := path == oldPCAP
		if wantMissing != os.IsNotExist(err) {
			t.Fatalf("path %s missing=%v, want %v", path, os.IsNotExist(err), wantMissing)
		}
	}
}

func TestRecorderCloseFlushesQueuedPackets(t *testing.T) {
	dir := t.TempDir()
	recorder, err := newRecorderAt(dir, "room", DefaultRetention, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		recorder.Record([]byte{byte(i)})
	}
	capturePath := recorder.Path()
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if recorder.Path() != capturePath {
		t.Fatal("recorder lost the last capture path")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("capture files=%d err=%v", len(entries), err)
	}
	data, err := os.Open(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if _, err := data.Seek(24, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		var header [16]byte
		if _, err := io.ReadFull(data, header[:]); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		length := int(binary.LittleEndian.Uint32(header[8:12]))
		if _, err := io.CopyN(io.Discard, data, int64(length)); err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 100 {
		t.Fatalf("wrote %d records, want 100", count)
	}
}

func TestRecorderCopiesPacketsBeforeReturning(t *testing.T) {
	recorder, err := newRecorderAt(t.TempDir(), "room", DefaultRetention, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	packet := []byte{0x45, 0x10, 0x20}
	recorder.Record(packet)
	packet[1], packet[2] = 0xff, 0xff
	path := recorder.Path()
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[40:]) != string([]byte{0x45, 0x10, 0x20}) {
		t.Fatalf("recorded bytes changed after capture: %x", data[40:])
	}
}
