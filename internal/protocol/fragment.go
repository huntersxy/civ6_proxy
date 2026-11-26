package protocol

import (
	"container/list"
	"errors"
	"slices"
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
	key        assemblyKey
	createdAt  time.Time
	compressed bool
	chunks     [][]byte
	received   uint64
	remaining  int
	size       int
}

// Reassembler joins fragmented DATA frames, mirroring the Python client.
type Reassembler struct {
	mu      sync.Mutex
	timeout time.Duration
	max     int
	now     func() time.Time
	entries map[assemblyKey]*list.Element
	order   list.List
}

// NewReassembler returns a reassembler with a 2 second timeout and 256
// concurrent assemblies.
func NewReassembler() *Reassembler {
	return &Reassembler{
		timeout: ReassemblyTimeout,
		max:     256,
		now:     time.Now,
		entries: make(map[assemblyKey]*list.Element),
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
	if f.FragmentCount == 0 || f.FragmentCount > MaxFragmentCount || f.FragmentIndex >= f.FragmentCount {
		return nil, ErrFragments
	}
	if f.FragmentCount == 1 {
		return f.Payload, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.expireLocked(now)
	key := assemblyKey{f.RoomID, f.SourceMember, f.PacketID, f.SourcePort, f.TargetPort}
	compressed := f.Flags&FlagCompressed != 0
	entry := r.entries[key]
	if entry == nil {
		if len(r.entries) >= r.max {
			r.removeLocked(r.order.Front())
		}
		entry = r.order.PushBack(&assembly{
			key: key, createdAt: now, compressed: compressed,
			chunks: make([][]byte, f.FragmentCount), remaining: int(f.FragmentCount),
		})
		r.entries[key] = entry
	}
	a := entry.Value.(*assembly)
	if len(a.chunks) != int(f.FragmentCount) || a.compressed != compressed {
		r.removeLocked(entry)
		return nil, errors.New("fragment metadata changed during reassembly")
	}
	mask := uint64(1) << f.FragmentIndex
	if a.received&mask != 0 {
		return nil, nil
	}
	if a.size+len(f.Payload) > MaxUDPPayload {
		r.removeLocked(entry)
		return nil, errors.New("reassembled payload is too large")
	}
	a.chunks[f.FragmentIndex] = slices.Clone(f.Payload)
	a.received |= mask
	a.size += len(f.Payload)
	a.remaining--
	if a.remaining != 0 {
		return nil, nil
	}
	payload := make([]byte, 0, a.size)
	for _, chunk := range a.chunks {
		payload = append(payload, chunk...)
	}
	r.removeLocked(entry)
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
	for entry := r.order.Front(); entry != nil; entry = r.order.Front() {
		if now.Sub(entry.Value.(*assembly).createdAt) < r.timeout {
			break
		}
		r.removeLocked(entry)
		expired++
	}
	return expired
}

func (r *Reassembler) removeLocked(entry *list.Element) {
	delete(r.entries, entry.Value.(*assembly).key)
	r.order.Remove(entry)
}
