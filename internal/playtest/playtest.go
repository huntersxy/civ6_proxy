package playtest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"civ6relay/internal/protocol"
	"civ6relay/internal/tunnel"
)

type Phase uint8

const (
	Creating Phase = iota
	Joining
	Discovering
	Playing
	Settling
)

const (
	gamePort            = 62056
	discoveryPort       = 62900
	discoverySourcePort = 49152
	findAttempts        = 5
	updateEvery         = 400 * time.Millisecond

	// Sizes calibrated from a 4h14m real Civilization VI LAN capture:
	// 86.9% of host outbound datagrams are 1180 B, max observed is 1399 B,
	// heartbeats are 12 B with 14 B replies, and reverse traffic is dominated
	// by 14 B ACKs at ~80% of the outbound packet rate.
	heartbeatSize     = 12
	ackSize           = 14
	steadyHostSize    = 74
	steadyGuestSize   = 90
	civSyncSize       = 1180
	civMaxFragSize    = 1399
	discoveryFindSize = 16
	discoveryReplySz  = 1104
)

var ErrRoomClosed = errors.New("simulation room closed during the test")

type profile struct {
	handshake        time.Duration
	discoveryTick    time.Duration
	discoveryWait    time.Duration
	playTime         time.Duration
	gameTick         time.Duration
	settle           time.Duration
	cycleSlots       int // total ticks per steady+sync cycle
	syncStartSlot    int // tick offset within cycle when sync burst starts
	syncBurstPerTick int // 1180 B packets sent by host per sync tick
	probeEverySlots  int // heartbeat RTT probe cadence (ticks)
	fragEverySync    int // emit one incompressible >1224 B packet every N sync ticks
}

type side uint8

const (
	hostSide side = iota
	guestSide
)

func (side side) other() side { return side ^ 1 }

type packetKind uint8

const (
	find packetKind = iota
	found
	state
	command
	ack
	syncData
	syncFrag
	syncAck
)

type sendStatus uint8

const (
	submitting sendStatus = iota
	submitted
	sendFailed
)

func (kind packetKind) ports() (source, target uint16) {
	switch kind {
	case find:
		return discoverySourcePort, discoveryPort
	case found:
		return discoveryPort, discoverySourcePort
	default:
		return gamePort, gamePort
	}
}

type datagram struct {
	kind                   packetKind
	from                   side
	inSync                 bool
	size, replyTo          int
	status                 sendStatus
	sentAt, received, echo time.Time
}

type simulation struct {
	ctx     context.Context
	profile profile
	update  func(Result)
	clients [2]*tunnel.Client
	tag     [8]byte

	phase                       Phase
	phaseStart                  time.Time
	phaseDuration, create, join time.Duration

	mu              sync.Mutex
	packets         []datagram
	syncTickCount   int
	syncRecvCount   int
	hostWireAtStart uint64
}

// Run simulates a two-player Civilization VI match through the configured relay
// using traffic shapes calibrated from real LAN captures (12 B/14 B heartbeats,
// sustained 1180 B ~45%-compressible turn-sync bursts at ~4.7 Mbps / 500 pps,
// 14 B reverse ACK streams at ~400 pps, plus periodic 1399 B fragmented checks).
func Run(ctx context.Context, address string, update func(Result)) (Result, error) {
	return run(ctx, address, profile{
		handshake:        5 * time.Second,
		discoveryTick:    320 * time.Millisecond,
		discoveryWait:    time.Second,
		playTime:         30 * time.Second,
		gameTick:         20 * time.Millisecond, // 50 ticks/s
		settle:           protocol.ReassemblyTimeout,
		cycleSlots:       750, // 15 s cycle (5 s steady + 10 s sync), 2 cycles in 30 s
		syncStartSlot:    250, // sync during [5 s, 15 s) and [20 s, 30 s)
		syncBurstPerTick: 10,  // 500 pkt/s * 1180 B = 4.72 Mbps raw game rate
		probeEverySlots:  10,  // 200 ms heartbeat cadence
		fragEverySync:    25,  // 2 fragmented 1399 B checks per second during sync
	}, update)
}

func run(ctx context.Context, address string, profile profile, update func(Result)) (Result, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return Result{}, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return Result{}, fmt.Errorf("invalid relay port %q", portText)
	}
	ctx, cancel := context.WithCancel(ctx)
	test := simulation{ctx: ctx, profile: profile, update: update}
	copy(test.tag[:], protocol.RandomBytes(len(test.tag)))
	err = test.play(host, port)
	cutoff := time.Now()
	cancel()
	res := test.snapshot(cutoff)
	for _, endpoint := range []side{guestSide, hostSide} {
		if client := test.clients[endpoint]; client != nil {
			client.Disconnect()
		}
	}
	return res, err
}

func (test *simulation) play(host string, port int) error {
	test.begin(Creating, 0)
	code, err := test.enter(hostSide, host, port, "", "Playtest Host")
	if err != nil {
		return fmt.Errorf("create simulation room: %w", err)
	}
	test.create = max(time.Nanosecond, time.Since(test.phaseStart))
	test.begin(Joining, 0)
	if _, err := test.enter(guestSide, test.clients[hostSide].RelayAddr().IP.String(), port, code, "Playtest Guest"); err != nil {
		return fmt.Errorf("join simulation room: %w", err)
	}
	test.join = max(time.Nanosecond, time.Since(test.phaseStart))
	for _, endpoint := range []side{hostSide, guestSide} {
		test.clients[endpoint].SetDataHandler(func(source uint8, sourcePort, targetPort uint16, payload []byte) {
			test.receive(endpoint, source, sourcePort, targetPort, payload)
		})
	}

	config := test.profile
	if err := test.pace(Discovering, (findAttempts-1)*config.discoveryTick+config.discoveryWait, config.discoveryTick, func(slot int) {
		if slot < findAttempts {
			test.send(guestSide, find, discoveryFindSize, 0, false)
		}
	}); err != nil {
		return err
	}
	test.mu.Lock()
	if c := test.clients[hostSide]; c != nil {
		test.hostWireAtStart = c.Metrics().TunnelTxBytes
	}
	test.mu.Unlock()
	if err := test.pace(Playing, config.playTime, config.gameTick, test.playTick); err != nil {
		return err
	}
	return test.pace(Settling, config.settle, updateEvery, nil)
}

func (test *simulation) enter(endpoint side, host string, port int, code, name string) (string, error) {
	ctx, cancel := context.WithTimeout(test.ctx, test.profile.handshake)
	defer cancel()
	client, err := tunnel.NewContext(ctx, host, port)
	if err != nil {
		return "", err
	}
	test.clients[endpoint] = client
	var welcome *protocol.WelcomeMessage
	if endpoint == hostSide {
		welcome, err = client.CreateRoomContext(ctx, name)
	} else {
		welcome, err = client.JoinRoomContext(ctx, code, name)
	}
	if err != nil {
		return "", err
	}
	return welcome.RoomCode, nil
}

func (test *simulation) begin(phase Phase, duration time.Duration) {
	test.phase, test.phaseStart, test.phaseDuration = phase, time.Now(), duration
	test.publish(test.phaseStart)
}

func (test *simulation) pace(phase Phase, duration, tick time.Duration, send func(int)) error {
	test.begin(phase, duration)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	deadline := time.NewTimer(max(0, duration-time.Since(test.phaseStart)))
	defer deadline.Stop()
	nextUpdate := test.phaseStart.Add(updateEvery)
	lastSlot := -1
	for {
		if err := test.ctx.Err(); err != nil {
			return err
		}
		if !test.clients[hostSide].Connected() || !test.clients[guestSide].Connected() {
			return ErrRoomClosed
		}
		now := time.Now()
		elapsed := now.Sub(test.phaseStart)
		if elapsed >= duration {
			return nil
		}
		if slot := int(elapsed / tick); send != nil && slot > lastSlot {
			// Cap catch-up to one tick so brief scheduler stalls do not dump
			// multiple burst ticks back-to-back.
			send(slot)
			lastSlot = slot
		}
		if !now.Before(nextUpdate) {
			test.publish(time.Now())
			nextUpdate = now.Add(updateEvery)
		}
		select {
		case <-test.ctx.Done():
		case <-deadline.C:
		case <-ticker.C:
		}
	}
}

func (test *simulation) playTick(slot int) {
	cfg := test.profile
	cycle := slot % max(1, cfg.cycleSlots)
	inSync := cycle >= cfg.syncStartSlot

	// Periodic 12 B heartbeat probe (echoed as 14 B by peer) measures RTT in
	// both steady state and turn-sync bursts, matching Civ 6's real probes.
	if slot%max(1, cfg.probeEverySlots) == 0 {
		test.send(hostSide, command, heartbeatSize, 0, inSync)
	}

	if !inSync {
		// Idle/steady turn: low-rate 74 B / 90 B gameplay updates (~4-6 pkt/s).
		if slot%12 == 3 {
			test.send(hostSide, state, steadyHostSize, 0, false)
		}
		if slot%25 == 7 {
			test.send(guestSide, state, steadyGuestSize, 0, false)
		}
		return
	}

	// Turn-sync burst: sustained 1180 B datagrams with Civ 6-like ~45%
	// compressed ratio (~4.7 Mbps raw / ~500 pps), plus periodic incompressible
	// 1399 B datagrams to exercise wire fragmentation.
	for i := 0; i < cfg.syncBurstPerTick; i++ {
		test.send(hostSide, syncData, civSyncSize, 0, true)
	}
	test.syncTickCount++
	if cfg.fragEverySync > 0 && test.syncTickCount%cfg.fragEverySync == 1 {
		test.send(hostSide, syncFrag, civMaxFragSize, 0, true)
	}
}

func (test *simulation) send(from side, kind packetKind, size, replyTo int, inSync bool) {
	if test.ctx.Err() != nil {
		return
	}
	payload := make([]byte, size)
	copy(payload, test.tag[:])
	sourcePort, targetPort := kind.ports()
	sender, target := test.clients[from], test.clients[from.other()].MemberID()
	test.mu.Lock()
	id := len(test.packets) + 1
	test.packets = append(test.packets, datagram{
		kind: kind, from: from, inSync: inSync, size: size, replyTo: replyTo, sentAt: time.Now(),
	})
	test.mu.Unlock()
	binary.BigEndian.PutUint32(payload[len(test.tag):], uint32(id))
	fillPayload(payload, kind, id)
	status := sendFailed
	if sender.SendGameData(target, sourcePort, targetPort, payload, kind == find) {
		status = submitted
	}
	test.mu.Lock()
	test.packets[id-1].status = status
	test.mu.Unlock()
}

// fillPayload synthesizes payloads matching real Civ 6 entropy profiles:
//   - syncData (1180 B): mixes structured repeating tile/state records (~58%)
//     with pseudo-random state bytes (~42%) so flate.BestSpeed compresses to
//     ~45% of raw size, matching the 44.3% measured across real 4-hour captures.
//   - syncFrag (1399 B): fully pseudo-random so it stays >MaxFragmentPayload
//     after compression and genuinely exercises multi-fragment reassembly.
func fillPayload(payload []byte, kind packetKind, id int) {
	const headerLen = 12
	if len(payload) <= headerLen {
		return
	}
	x := uint32(id)*2654435761 ^ 0x9e3779b9
	nextByte := func() byte {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		return byte(x)
	}
	switch kind {
	case syncFrag:
		for i := headerLen; i < len(payload); i++ {
			payload[i] = nextByte()
		}
	case syncData:
		// First ~58% structured Civ 6-like map/tile/struct records (highly
		// compressible), remaining ~42% pseudo-random delta state.
		split := headerLen + (len(payload)-headerLen)*58/100
		pattern := [4]byte{0x00, 0xf1, 0xd8, 0xff}
		for i := headerLen; i < split; i++ {
			payload[i] = pattern[i&3]
		}
		for i := split; i < len(payload); i++ {
			payload[i] = nextByte()
		}
	case state, found:
		for i := headerLen; i < len(payload); i += 4 {
			payload[i] = nextByte()
		}
	}
}

func (test *simulation) receive(to side, source uint8, sourcePort, targetPort uint16, payload []byte) {
	now := time.Now()
	if len(payload) < len(test.tag)+4 || !bytes.Equal(payload[:len(test.tag)], test.tag[:]) ||
		source != test.clients[to.other()].MemberID() {
		return
	}
	id := int(binary.BigEndian.Uint32(payload[len(test.tag):]))
	test.mu.Lock()
	if id < 1 || id > len(test.packets) {
		test.mu.Unlock()
		return
	}
	packet := &test.packets[id-1]
	wantSource, wantTarget := packet.kind.ports()
	if packet.from == to || packet.size != len(payload) || !packet.received.IsZero() ||
		now.Sub(packet.sentAt) >= test.profile.settle || sourcePort != wantSource || targetPort != wantTarget {
		test.mu.Unlock()
		return
	}
	packet.received = now
	if packet.replyTo != 0 {
		test.packets[packet.replyTo-1].echo = now
	}
	kind, inSync := packet.kind, packet.inSync
	sendSyncAck := false
	if kind == syncData {
		test.syncRecvCount++
		// Reply with a 14 B ACK for 4 out of 5 sync datagrams (~80%),
		// matching the real Civ 6 reverse ACK cadence.
		sendSyncAck = test.syncRecvCount%5 != 0
	}
	test.mu.Unlock()
	switch kind {
	case find:
		test.send(to, found, discoveryReplySz, id, false)
	case command:
		test.send(to, ack, ackSize, id, inSync)
	case syncData:
		if sendSyncAck {
			test.send(to, syncAck, ackSize, 0, true)
		}
	}
}
