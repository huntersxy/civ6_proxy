package speedtest

import (
	"encoding/binary"
	"time"
)

const (
	magic        = "C6S1"
	headerSize   = 40
	controlSize  = headerSize + 16
	datagramSize = 1200

	DefaultDuration = 10 * time.Second
	MaxMbps         = 1000
	maxDuration     = 20 * time.Second
	baselineTime    = 1500 * time.Millisecond
	pollInterval    = 125 * time.Millisecond
	leaseTime       = 2500 * time.Millisecond
	settleTime      = 350 * time.Millisecond
	cooldown        = 3 * time.Second
)

type Phase uint8

const (
	Baseline Phase = iota
	Download
	Upload
)

type kind uint8

const (
	hello kind = 1 + iota
	challenge
	open
	ready
	unavailable
	busy
	probe
	status
	data
	stop
)

type token [16]byte

type message struct {
	kind                   kind
	phase                  Phase
	final                  bool
	id                     token
	serial, sent, received uint32
	millis                 uint32
	proof                  token
}

func IsDatagram(packet []byte) bool {
	return len(packet) >= len(magic) && string(packet[:len(magic)]) == magic
}

func decode(packet []byte) (message, bool) {
	if len(packet) < controlSize || !IsDatagram(packet) {
		return message{}, false
	}
	msg := message{
		kind:   kind(packet[4]),
		phase:  Phase(packet[5]),
		final:  packet[6] == 1,
		serial: binary.BigEndian.Uint32(packet[24:28]),
	}
	copy(msg.id[:], packet[8:24])
	if msg.kind == data {
		return msg, len(packet) == datagramSize && (msg.phase == Download || msg.phase == Upload)
	}
	if len(packet) != controlSize || msg.kind < hello || msg.kind > stop || msg.phase > Upload {
		return message{}, false
	}
	msg.sent = binary.BigEndian.Uint32(packet[28:32])
	msg.received = binary.BigEndian.Uint32(packet[32:36])
	msg.millis = binary.BigEndian.Uint32(packet[36:40])
	copy(msg.proof[:], packet[headerSize:controlSize])
	return msg, true
}

func (msg message) encode(packet []byte) {
	copy(packet, magic)
	packet[4], packet[5] = byte(msg.kind), byte(msg.phase)
	if msg.final {
		packet[6] = 1
	}
	copy(packet[8:24], msg.id[:])
	binary.BigEndian.PutUint32(packet[24:28], msg.serial)
	binary.BigEndian.PutUint32(packet[28:32], msg.sent)
	binary.BigEndian.PutUint32(packet[32:36], msg.received)
	binary.BigEndian.PutUint32(packet[36:40], msg.millis)
	if msg.kind != data {
		copy(packet[headerSize:controlSize], msg.proof[:])
	}
}

func packetLimit(duration time.Duration, mbps int) uint32 {
	return uint32(duration.Milliseconds()*int64(mbps)*1000/(8*datagramSize)) + 1
}

func phaseDuration(millis uint32) time.Duration {
	return time.Duration(millis) * time.Millisecond
}

type packetTracker struct {
	bits       []uint64
	limit      uint32
	seen, kept uint32
}

func trackPackets(duration time.Duration, mbps int) packetTracker {
	limit := packetLimit(duration, mbps)
	return packetTracker{bits: make([]uint64, (limit+63)/64), limit: limit}
}

func (tracker *packetTracker) add(serial uint32) {
	if serial >= tracker.limit {
		return
	}
	word, bit := serial>>6, uint64(1)<<(serial&63)
	if tracker.bits[word]&bit != 0 {
		return
	}
	tracker.bits[word] |= bit
	tracker.kept++
	tracker.seen = max(tracker.seen, serial+1)
}
