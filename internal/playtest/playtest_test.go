package playtest

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"civ6relay/internal/protocol"
	"civ6relay/internal/relay"
)

var testProfile = profile{
	handshake:        time.Second,
	discoveryTick:    25 * time.Millisecond,
	discoveryWait:    125 * time.Millisecond,
	playTime:         820 * time.Millisecond,
	gameTick:         20 * time.Millisecond,
	settle:           180 * time.Millisecond,
	cycleSlots:       20,
	syncStartSlot:    10,
	syncBurstPerTick: 3,
	probeEverySlots:  2,
	fragEverySync:    5,
}

func startRelay(t *testing.T, drop func(protocol.Frame) bool) (string, int, *relay.Server) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server := relay.New(relay.Config{Logger: log.New(io.Discard, "", 0)})
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	go func() {
		defer close(done)
		var buf [protocol.MaxOuterDatagram]byte
		var batch relay.Batch
		for {
			size, peer, err := conn.ReadFromUDPAddrPort(buf[:])
			if err != nil {
				return
			}
			server.HandleInto(buf[:size], peer, &batch)
			for _, output := range batch.Datagrams {
				frame, err := protocol.DecodeView(output.Data)
				if err == nil && (drop == nil || !drop(frame)) {
					_, _ = conn.WriteToUDPAddrPort(output.Data, output.Addr)
				}
			}
		}
	}()
	address := conn.LocalAddr().(*net.UDPAddr)
	return address.String(), address.Port, server
}

func expectRooms(t *testing.T, server *relay.Server, wantRooms, wantMembers int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		rooms, members := server.Stats()
		if rooms == wantRooms && members == wantMembers {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rooms=%d members=%d; want %d, %d", rooms, members, wantRooms, wantMembers)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSimulatesRelayAndReportsDroppedReplyAndFragment(t *testing.T) {
	droppedReply, droppedFragment := false, false
	address, _, server := startRelay(t, func(frame protocol.Frame) bool {
		if frame.Kind != protocol.KindData {
			return false
		}
		if !droppedReply && frame.SourcePort == discoveryPort && frame.TargetPort == discoverySourcePort {
			droppedReply = true
			return true
		}
		if !droppedFragment && frame.FragmentCount > 1 && frame.FragmentIndex == 1 {
			droppedFragment = true
			return true
		}
		return false
	})
	result, err := run(context.Background(), address, testProfile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Create <= 0 || result.Join <= 0 || result.SendFailures != 0 {
		t.Fatalf("unexpected setup or send results: %+v", result)
	}
	if result.Discovery != (Discovery{Attempts: 5, Broadcasts: 5, Replies: 4}) {
		t.Fatalf("unexpected discovery: %+v", result.Discovery)
	}
	if stats := result.HostToGuest; stats.Sent != stats.Settled || stats.Missing != 1 || stats.Received != stats.Sent-1 {
		t.Fatalf("unexpected host delivery: %+v", stats)
	}
	if stats := result.GuestToHost; stats.Sent == 0 || stats.Received != stats.Sent || stats.Settled != stats.Sent || stats.Missing != 0 {
		t.Fatalf("unexpected guest delivery: %+v", stats)
	}
	if stats := result.Fragmented; stats.Sent < 2 || stats.Missing != 1 || stats.Received != stats.Sent-1 {
		t.Fatalf("unexpected fragmented delivery: %+v", stats)
	}
	if ratio := result.CompressionRatio(); result.HostRawBytes == 0 || ratio <= 0 || ratio >= 0.70 {
		t.Fatalf("expected realistic Civ 6 wire compression (<70%%), got raw=%d wire=%d ratio=%.3f",
			result.HostRawBytes, result.HostWireBytes, ratio)
	}
	for _, latency := range []Latency{result.SteadyRTT, result.SyncRTT} {
		if latency.Probes == 0 || latency.Probes != latency.Replies || latency.Median <= 0 || latency.P95 <= 0 {
			t.Fatalf("unexpected latency: %+v", latency)
		}
	}
	expectRooms(t, server, 0, 0)
}

func TestCancelStopsSendingAndLeavesExistingRoomAlone(t *testing.T) {
	address, port, server := startRelay(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	existing := simulation{ctx: ctx, profile: testProfile}
	if _, err := existing.enter(hostSide, "127.0.0.1", port, "", "Existing game"); err != nil {
		t.Fatal(err)
	}
	defer existing.clients[hostSide].Disconnect()

	config := testProfile
	config.playTime = 30 * time.Second
	started := time.Now()
	result, err := run(ctx, address, config, func(result Result) {
		if result.Phase == Playing && result.HostToGuest.Sent > 0 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || time.Since(started) > 3*time.Second {
		t.Fatalf("unexpected cancellation: %v after %v", err, time.Since(started))
	}
	if result.Phase != Playing || result.HostToGuest.Received == 0 || result.HostToGuest.Settled >= result.HostToGuest.Sent {
		t.Fatalf("cancellation discarded data or settled in-flight datagrams: %+v", result)
	}
	expectRooms(t, server, 1, 1)
}

func TestSnapshotSettlesOnlyMatureDatagrams(t *testing.T) {
	now := time.Now()
	old, recent := now.Add(-time.Second), now.Add(-50*time.Millisecond)
	test := simulation{profile: testProfile, phaseStart: old, packets: []datagram{
		{from: hostSide, kind: state, status: submitted, sentAt: old},
		{from: hostSide, kind: state, status: submitted, sentAt: recent},
		{from: hostSide, kind: state, status: submitted, sentAt: recent, received: recent.Add(time.Millisecond)},
		{from: hostSide, kind: state, status: sendFailed, sentAt: old},
		{from: hostSide, kind: state, status: submitting, sentAt: old},
		{from: hostSide, kind: state, status: submitted, sentAt: recent, received: now.Add(time.Millisecond)},
	}}
	result := test.snapshot(now)
	if result.HostToGuest != (Delivery{Sent: 4, Received: 1, Settled: 1, Missing: 1}) || result.SendFailures != 1 {
		t.Fatalf("unexpected counts: %+v", result)
	}
}
