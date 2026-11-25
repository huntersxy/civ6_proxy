package speedtest

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func loopbackRelay(t *testing.T, maxMbps int, drop func(message) bool) (string, *Server) {
	t.Helper()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(maxMbps, func(packet []byte, addr netip.AddrPort) error {
		if msg, ok := decode(packet); ok && drop != nil && drop(msg) {
			return nil
		}
		_, err := socket.WriteToUDPAddrPort(packet, addr)
		return err
	})
	if err != nil {
		socket.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var packet [datagramSize + 1]byte
		for {
			size, addr, err := socket.ReadFromUDPAddrPort(packet[:])
			if err != nil {
				return
			}
			server.Handle(packet[:size], addr)
		}
	}()
	t.Cleanup(func() {
		socket.Close()
		<-done
		server.Close()
	})
	return socket.LocalAddr().String(), server
}

func TestRoundTripUsesReceiverCounts(t *testing.T) {
	t.Parallel()
	var sent, dropped atomic.Uint32
	address, _ := loopbackRelay(t, 24, func(msg message) bool {
		if msg.kind == data {
			sent.Add(1)
			if msg.serial%13 == 0 {
				dropped.Add(1)
				return true
			}
		}
		return false
	})
	result, err := Run(context.Background(), address, Options{Duration: 2 * time.Second, MaxMbps: 18}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.MaxMbps != 18 || !result.Download.Final || !result.Upload.Final {
		t.Fatalf("incomplete speed test: %+v", result)
	}
	if got := result.Download; got.Sent != sent.Load() || got.Sent-got.Received < dropped.Load() ||
		got.Loss() > 35 || got.AverageMbps <= 0 || got.PeakMbps < got.AverageMbps {
		t.Fatalf("unexpected download result: %+v, sent=%d, injected loss=%d", got, sent.Load(), dropped.Load())
	}
	if got := result.Upload; got.Sent == 0 || got.Received == 0 || got.Received > got.Sent || got.AverageMbps <= 0 ||
		got.AverageMbps > 18.1 || got.PeakMbps < max(10, got.AverageMbps) {
		t.Fatalf("unexpected upload result: %+v", got)
	}
	for phase, latency := range result.Latency {
		if latency.Samples < 2 || latency.P95 < latency.Median || latency.Median < 0 {
			t.Fatalf("missing latency for phase %d: %+v", phase, latency)
		}
	}
}

func TestCancelStopsBothDirections(t *testing.T) {
	for _, phase := range []Phase{Download, Upload} {
		t.Run(map[Phase]string{Download: "download", Upload: "upload"}[phase], func(t *testing.T) {
			t.Parallel()
			address, server := loopbackRelay(t, 12, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var canceledAt time.Time
			result, err := Run(ctx, address, Options{Duration: 2 * time.Second}, func(snapshot Result) {
				if snapshot.Phase == phase && ((phase == Download && snapshot.Download.Received > 0) ||
					(phase == Upload && snapshot.Upload.Received > 0)) {
					canceledAt = time.Now()
					cancel()
				}
			})
			if !errors.Is(err, context.Canceled) || canceledAt.IsZero() || result.Phase != phase {
				t.Fatalf("test did not cancel during phase %d: %+v, %v", phase, result, err)
			}
			if elapsed := time.Since(canceledAt); elapsed > time.Second {
				t.Fatalf("client took %v to stop", elapsed)
			}
			eventually(t, time.Second, func() bool {
				server.mu.Lock()
				defer server.mu.Unlock()
				return server.active == nil && !server.sending
			})
		})
	}
}

func TestChallengeBindsEndpointAndDownloadExpires(t *testing.T) {
	t.Parallel()
	peer := netip.MustParseAddrPort("192.0.2.1:10001")
	other := netip.MustParseAddrPort("192.0.2.1:10002")
	var mu sync.Mutex
	var replies []message
	var packets atomic.Uint32
	server, err := NewServer(5, func(packet []byte, target netip.AddrPort) error {
		if target != peer {
			t.Error("unexpected response to unverified endpoint")
		}
		msg, _ := decode(packet)
		if msg.kind == data {
			packets.Add(1)
		} else {
			mu.Lock()
			replies = append(replies, msg)
			mu.Unlock()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	send := func(msg message, target netip.AddrPort) {
		var packet [controlSize]byte
		msg.encode(packet[:])
		server.Handle(packet[:], target)
	}

	request := message{kind: hello, id: token{1}, millis: 7000}
	send(request, peer)
	if len(replies) != 1 || replies[0].kind != challenge {
		t.Fatalf("expected a challenge, got %v", replies)
	}
	request.kind, request.proof = open, replies[0].proof
	send(request, other)
	if len(replies) != 1 || server.active != nil {
		t.Fatal("accepted proof from an incorrect endpoint")
	}
	send(request, peer)
	if len(replies) != 2 || replies[1].kind != ready || packets.Load() != 0 {
		t.Fatal("handshake did not stay idle until the first verified phase probe")
	}
	send(message{kind: probe, phase: Download, id: replies[1].proof}, peer)
	eventually(t, time.Second, func() bool { return packets.Load() > 0 })
	eventually(t, leaseTime+time.Second, func() bool {
		server.Sweep()
		server.mu.Lock()
		defer server.mu.Unlock()
		return server.active == nil && !server.sending
	})
	if got := packets.Load(); got > packetLimit(leaseTime, 5) {
		t.Fatalf("sent too many packets with no client heartbeat: %d", got)
	}
}

func TestUnavailableAndUnresponsiveRelay(t *testing.T) {
	t.Parallel()
	address, _ := loopbackRelay(t, 0, nil)
	if _, err := Run(context.Background(), address, Options{}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("disabled test returned %v", err)
	}
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if _, err := Run(context.Background(), socket.LocalAddr().String(), Options{}, nil); !errors.Is(err, ErrNoReply) {
		t.Fatalf("legacy relay returned %v", err)
	}
}

func TestTrackerDeduplicatesAndAcceptsLatePackets(t *testing.T) {
	tracker := trackPackets(time.Second, 1)
	for _, serial := range []uint32{1, 5, 1, 2, 0, tracker.limit, tracker.limit + 1000} {
		tracker.add(serial)
	}
	if tracker.seen != 6 || tracker.kept != 4 {
		t.Fatalf("seen=%d kept=%d", tracker.seen, tracker.kept)
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	for end := time.Now().Add(timeout); time.Now().Before(end); {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition still false after %s", timeout)
}
