package protocol

import "crypto/rand"
import "encoding/binary"

const (
	replayWindowWords = 16
	ReplayWindowSize  = replayWindowWords * 64 // 1024 packets
)

// ReplayWindow is a 1024-packet sliding replay window.
type ReplayWindow struct {
	highest     uint64
	bitmap      [replayWindowWords]uint64
	initialized bool
}

// Accept records sequence and reports whether it is fresh.
func (w *ReplayWindow) Accept(sequence uint64) bool {
	if !w.initialized {
		w.initialized = true
		w.highest = sequence
		w.bitmap = [replayWindowWords]uint64{1}
		return true
	}
	if sequence > w.highest {
		shift := sequence - w.highest
		if shift >= ReplayWindowSize {
			w.bitmap = [replayWindowWords]uint64{1}
		} else {
			w.bitmap = shiftLeftWindow(w.bitmap, shift)
			w.bitmap[0] |= 1
		}
		w.highest = sequence
		return true
	}
	distance := w.highest - sequence
	if distance >= ReplayWindowSize {
		return false
	}
	word := distance / 64
	mask := uint64(1) << (distance % 64)
	if w.bitmap[word]&mask != 0 {
		return false
	}
	w.bitmap[word] |= mask
	return true
}

// Reset clears the window.
func (w *ReplayWindow) Reset() {
	*w = ReplayWindow{}
}

func shiftLeftWindow(b [replayWindowWords]uint64, n uint64) [replayWindowWords]uint64 {
	var out [replayWindowWords]uint64
	wordShift := int(n / 64)
	bitShift := uint(n % 64)
	for i := len(out) - 1; i >= 0; i-- {
		src := i - wordShift
		if src < 0 {
			continue
		}
		out[i] = b[src] << bitShift
		if bitShift > 0 && src-1 >= 0 {
			out[i] |= b[src-1] >> (64 - bitShift)
		}
	}
	return out
}

// Counter generates increasing 64-bit sequences with a random start.
type Counter struct {
	value uint64
}

// NewCounter returns a counter starting at a random value in [1, 2^31],
// matching the Python implementation.
func NewCounter() *Counter {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand failure: " + err.Error())
	}
	v := uint64(binary.BigEndian.Uint32(b[:]) & 0x7FFFFFFF)
	return &Counter{value: v + 1}
}

// Next returns the current value and advances the counter.
func (c *Counter) Next() uint64 {
	v := c.value
	if v >= ^uint64(0) {
		panic("sequence exhausted")
	}
	c.value++
	return v
}
