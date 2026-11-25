package protocol

import (
	"errors"
	"sync"
	"time"
)

type assemblyKey struct {
	RoomID     uint64
	Source     uint8
	PacketID   uint32
	SourcePort uint16
	TargetPort uint16
}

type assembly struct {
	createdAt time.Time
	count     uint16
	chunks    map[uint16][]byte
	size      int
}

// Reassembler joins fragmented DATA frames, mirroring the Python client.
type Reassembler struct {
	mu      sync.Mutex
	timeout time.Duration
	max     int
	now     func() time.Time
	entries map[assemblyKey]*assembly
}

// NewReassembler returns a reassembler with a 2 second timeout and 256
// concurrent assemblies.
func NewReassembler() *Reassembler {
	return &Reassembler{
		timeout: ReassemblyTimeout,
		max:     256,
		now:     time.Now,
		entries: make(map[assemblyKey]*assembly),
	}
}

// SetClock overrides the clock (tests only).
func (r *Reassembler) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = now
}

// Add feeds a DATA frame and returns the complete payload when available.
func (r *Reassembler) Add(f *Frame) ([]byte, error) {
	if f.Kind != KindData {
		return nil, errors.New("only DATA frames can be reassembled")
	}
	if f.FragmentCount == 1 {
		return f.Payload, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.expireLocked(now)
	key := assemblyKey{f.RoomID, f.SourceMember, f.PacketID, f.SourcePort, f.TargetPort}
	a := r.entries[key]
	if a == nil {
		if len(r.entries) >= r.max {
			var oldestKey assemblyKey
			var oldest time.Time
			first := true
			for k, v := range r.entries {
				if first || v.createdAt.Before(oldest) {
					oldestKey, oldest, first = k, v.createdAt, false
				}
			}
			delete(r.entries, oldestKey)
		}
		a = &assembly{createdAt: now, count: f.FragmentCount, chunks: make(map[uint16][]byte)}
		r.entries[key] = a
	} else if a.count != f.FragmentCount {
		delete(r.entries, key)
		return nil, errors.New("fragment count changed during reassembly")
	}
	if _, exists := a.chunks[f.FragmentIndex]; !exists {
		chunk := make([]byte, len(f.Payload))
		copy(chunk, f.Payload)
		a.chunks[f.FragmentIndex] = chunk
		a.size += len(chunk)
	}
	if a.size > MaxUDPPayload {
		delete(r.entries, key)
		return nil, errors.New("reassembled payload is too large")
	}
	if len(a.chunks) != int(a.count) {
		return nil, nil
	}
	payload := make([]byte, 0, a.size)
	for i := 0; i < int(a.count); i++ {
		chunk, ok := a.chunks[uint16(i)]
		if !ok {
			return nil, nil
		}
		payload = append(payload, chunk...)
	}
	delete(r.entries, key)
	return payload, nil
}

// Expire drops assemblies older than the timeout and returns the count.
func (r *Reassembler) Expire() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expireLocked(r.now())
}

func (r *Reassembler) expireLocked(now time.Time) int {
	expired := 0
	for key, a := range r.entries {
		if now.Sub(a.createdAt) >= r.timeout {
			delete(r.entries, key)
			expired++
		}
	}
	return expired
}
