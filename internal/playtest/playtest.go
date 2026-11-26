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
)

var ErrRoomClosed = errors.New("simulation room closed during the test")

type profile struct {
	handshake, discoveryTick, discoveryWait, playTime, gameTick, settle time.Duration
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

	mu      sync.Mutex
	packets []datagram
}

// Run simulates a two-player match through the configured relay without using
// the Windows bridge or sending any local game traffic.
func Run(ctx context.Context, address string, update func(Result)) (Result, error) {
	return run(ctx, address, profile{
		handshake: 5 * time.Second, discoveryTick: 320 * time.Millisecond, discoveryWait: time.Second,
		playTime: 30 * time.Second, gameTick: 120 * time.Millisecond, settle: protocol.ReassemblyTimeout,
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
	for _, endpoint := range []side{guestSide, hostSide} {
		if client := test.clients[endpoint]; client != nil {
			client.Disconnect()
		}
	}
	return test.snapshot(cutoff), err
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
			test.send(guestSide, find, 16, 0, false)
		}
	}); err != nil {
		return err
	}
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
	test.send(hostSide, state, 180, 0, false)
	if slot%3 == 0 {
		test.send(guestSide, state, 104, 0, false)
	}
	cycle := slot % 50
	inSync := cycle >= 25 && cycle < 30
	if inSync {
		for _, size := range []int{640, 1580, 3120} {
			test.send(hostSide, syncData, size, 0, true)
		}
	}
	if slot%2 == 0 {
		test.send(side(slot/2%2), command, 212, 0, inSync)
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
	if kind == syncData && len(payload) > len(test.tag)+4 {
		// Fill synthetic sync payloads with pseudo-random bytes so large
		// test datagrams exercise wire-level fragmentation even with compression.
		x := uint32(id)*2654435761 ^ 0x9e3779b9
		for i := len(test.tag) + 4; i < len(payload); i++ {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			payload[i] = byte(x)
		}
	}
	status := sendFailed
	if sender.SendGameData(target, sourcePort, targetPort, payload, kind == find) {
		status = submitted
	}
	test.mu.Lock()
	test.packets[id-1].status = status
	test.mu.Unlock()
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
	test.mu.Unlock()
	switch kind {
	case find:
		test.send(to, found, 1104, id, false)
	case command:
		test.send(to, ack, 72, id, inSync)
	}
}
