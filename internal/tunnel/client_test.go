package tunnel

import (
	"bytes"
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

func startTestRelay(t *testing.T) (host string, port int, stop func()) {
	t.Helper()
	return startFilteredTestRelay(t, nil)
}

func startFilteredTestRelay(t *testing.T, filter func([]byte) bool) (host string, port int, stop func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	srv := relay.New(relay.Config{Logger: log.New(io.Discard, "", 0)})
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 2048)
		var batch relay.Batch
		for {
			n, peer, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if filter != nil && !filter(buf[:n]) {
				continue
			}
			srv.HandleInto(buf[:n], peer, &batch)
			for _, out := range batch.Datagrams {
				_, _ = conn.WriteToUDPAddrPort(out.Data, out.Addr)
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				for _, out := range srv.Cleanup() {
					_, _ = conn.WriteToUDPAddrPort(out.Data, out.Addr)
				}
			}
		}
	}()
	addr := conn.LocalAddr().(*net.UDPAddr)
	stop = func() {
		close(done)
		_ = conn.Close()
	}
	return addr.IP.String(), addr.Port, stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestTunnelEndToEnd(t *testing.T) {
	hostAddr, port, stopRelay := startTestRelay(t)
	defer stopRelay()

	host, err := New(hostAddr, port)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Disconnect()
	hostData := make(chan []byte, 16)
	host.SetDataHandler(func(src uint8, sport, dport uint16, payload []byte) {
		hostData <- payload
	})
	welcomeHost, err := host.CreateRoom("host", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if welcomeHost.MemberID != 2 || !welcomeHost.Host {
		t.Fatalf("unexpected host welcome: %+v", welcomeHost)
	}

	guest, err := New(hostAddr, port)
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Disconnect()
	guestData := make(chan []byte, 16)
	guest.SetDataHandler(func(src uint8, sport, dport uint16, payload []byte) {
		guestData <- payload
	})
	events := make(chan Event, 16)
	guest.SetEventHandler(func(ev Event) { events <- ev })

	welcomeGuest, err := guest.JoinRoom(welcomeHost.RoomCode, "guest", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if welcomeGuest.MemberID != 3 || len(welcomeGuest.Members) != 2 {
		t.Fatalf("unexpected guest welcome: %+v", welcomeGuest)
	}

	waitFor(t, "host peer update", func() bool { return len(host.Peers()) == 2 })
	waitFor(t, "first RTT probe", func() bool { return host.Metrics().HasRTT && guest.Metrics().HasRTT })

	// Guest -> host unicast.
	if !guest.SendGameData(2, 62056, 62900, []byte("hello-host"), false) {
		t.Fatal("guest send failed")
	}
	select {
	case payload := <-hostData:
		if string(payload) != "hello-host" {
			t.Fatalf("unexpected payload %q", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host did not receive unicast")
	}

	// Host -> room broadcast.
	if !host.SendGameData(255, 62900, 62901, []byte("broadcast"), true) {
		t.Fatal("broadcast send failed")
	}
	select {
	case payload := <-guestData:
		if string(payload) != "broadcast" {
			t.Fatalf("unexpected broadcast payload %q", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("guest did not receive broadcast")
	}

	largePayload := make([]byte, protocol.MaxUDPPayload)
	for index := range largePayload {
		largePayload[index] = byte(index % 251)
	}
	if !guest.SendGameData(2, 62056, 62056, largePayload, false) {
		t.Fatal("fragmented send failed")
	}
	select {
	case payload := <-hostData:
		if !bytes.Equal(payload, largePayload) {
			t.Fatalf("reassembled payload differed (%d vs %d bytes)", len(payload), len(largePayload))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host did not receive fragmented data")
	}

	// Host leave -> guest gets ROOM_CLOSED.
	host.Disconnect()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == EventRoomClosed {
				if ev.Reason != "host_left" {
					t.Fatalf("unexpected close reason %q", ev.Reason)
				}
				return
			}
		case <-deadline:
			t.Fatal("guest did not receive ROOM_CLOSED")
		}
	}
}

func TestCancelledHandshakeIgnoresWelcome(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port
	client, err := New("127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.CreateRoomContext(ctx, "host")
		result <- err
	}()

	buf := make([]byte, protocol.MaxOuterDatagram)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	count, source, err := conn.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatal(err)
	}
	server := relay.New(relay.Config{Logger: log.New(io.Discard, "", 0)})
	var batch relay.Batch
	server.HandleInto(buf[:count], source, &batch)
	if len(batch.Datagrams) != 1 {
		t.Fatal("expected a welcome reply")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v instead of cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not cancel the request")
	}

	raw := batch.Datagrams[0].Data
	welcome, err := protocol.DecodeView(raw)
	if err != nil {
		t.Fatal(err)
	}
	client.handleHandshakeResponse(&welcome)
	if client.Connected() {
		t.Fatal("cancelled request installed room credentials")
	}
}

func TestTunnelCustomMaxMembersAndLargeRoster(t *testing.T) {
	hostAddr, port, stopRelay := startTestRelay(t)
	defer stopRelay()

	// 1. Custom limit = 2.
	host1, err := New(hostAddr, port)
	if err != nil {
		t.Fatal(err)
	}
	defer host1.Disconnect()
	w1, err := host1.CreateRoomMaxMembers("host1", 2, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if w1.MaxMembers != 2 || host1.MaxMembers() != 2 {
		t.Fatalf("expected MaxMembers=2, got welcome=%d client=%d", w1.MaxMembers, host1.MaxMembers())
	}
	g1, err := New(hostAddr, port)
	if err != nil {
		t.Fatal(err)
	}
	defer g1.Disconnect()
	if _, err := g1.JoinRoom(w1.RoomCode, "g1", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	g2, err := New(hostAddr, port)
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Disconnect()
	_, err = g2.JoinRoom(w1.RoomCode, "g2", 3*time.Second)
	var te *Error
	if !errors.As(err, &te) || te.Code != "room_full" {
		t.Fatalf("expected room_full for 3rd member, got %v", err)
	}

	// 2. Default unlimited (0): 12 members with long names (triggers fragmented WELCOME & PEERS).
	host2, err := New(hostAddr, port)
	if err != nil {
		t.Fatal(err)
	}
	defer host2.Disconnect()
	longPrefix := "玩家一二三四五六七八九十甲乙丙丁戊己庚辛"
	w2, err := host2.CreateRoom(longPrefix+"01", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if w2.MaxMembers != 0 || host2.MaxMembers() != 0 {
		t.Fatalf("expected default MaxMembers=0, got %d/%d", w2.MaxMembers, host2.MaxMembers())
	}
	for i := 2; i <= 12; i++ {
		c, err := New(hostAddr, port)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Disconnect()
		name := longPrefix + string([]byte{'0' + byte(i/10), '0' + byte(i%10)})
		wj, err := c.JoinRoom(w2.RoomCode, name, 3*time.Second)
		if err != nil {
			t.Fatalf("member %d join failed: %v", i, err)
		}
		if len(wj.Members) != i {
			t.Fatalf("member %d saw %d peers", i, len(wj.Members))
		}
	}
	waitFor(t, "host2 sees all 12 peers", func() bool { return len(host2.Peers()) == 12 })
}

func TestTunnelCompressionReducesWireBytes(t *testing.T) {
	hostAddr, port, stopRelay := startTestRelay(t)
	defer stopRelay()

	host, err := New(hostAddr, port)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Disconnect()
	welcome, err := host.CreateRoom("host", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	guest, err := New(hostAddr, port)
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Disconnect()
	received := make(chan []byte, 4)
	guest.SetDataHandler(func(_ uint8, _, _ uint16, payload []byte) { received <- payload })
	if _, err := guest.JoinRoom(welcome.RoomCode, "guest", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "peer list", func() bool { return len(host.Peers()) == 2 })

	// Send compressible 1180-byte Civ6-like payload.
	civPayload := bytes.Repeat([]byte{0x00, 0xf1, 0xd8, 0xff}, 295)
	beforeBytes := host.Metrics().TunnelTxBytes
	if !host.SendGameData(3, 62056, 62056, civPayload, false) {
		t.Fatal("send failed")
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, civPayload) {
			t.Fatal("decompressed payload mismatch")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("guest did not receive compressed payload")
	}
	sentWire := host.Metrics().TunnelTxBytes - beforeBytes
	if sentWire >= uint64(len(civPayload)/2) {
		t.Fatalf("expected wire bytes to be compressed well below half of %d, got %d", len(civPayload), sentWire)
	}
}
