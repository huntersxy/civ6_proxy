package tunnel

import (
	"io"
	"log"
	"net"
	"testing"
	"time"

	"civ6relay/internal/relay"
)

func startTestRelay(t *testing.T) (host string, port int, stop func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	srv := relay.New(relay.Config{Logger: log.New(io.Discard, "", 0)})
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 2048)
		for {
			n, peer, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			for _, out := range srv.Handle(buf[:n], peer) {
				_, _ = conn.WriteToUDP(out.Data, out.Addr)
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
					_, _ = conn.WriteToUDP(out.Data, out.Addr)
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
		hostData <- append([]byte(nil), payload...)
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
		guestData <- append([]byte(nil), payload...)
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
