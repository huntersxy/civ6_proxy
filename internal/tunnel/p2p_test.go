package tunnel

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"civ6relay/internal/protocol"
)

func newTestRoom(t *testing.T, address string, port int) (*Client, string) {
	t.Helper()
	client, err := New(address, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Disconnect)
	welcome, err := client.CreateRoom("host", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client, welcome.RoomCode
}

func joinTestRoom(t *testing.T, address string, port int, code, name string) *Client {
	t.Helper()
	client, err := New(address, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Disconnect)
	if _, err := client.JoinRoom(code, name, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	return client
}

func waitForP2P(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(9 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestP2PFullMeshAndRoomToggle(t *testing.T) {
	var relayData atomic.Int64
	var pathPings, pathPongs [10]atomic.Int64
	address, port, stop := startFilteredTestRelay(t, func(raw []byte) bool {
		if frame, err := protocol.DecodeView(raw); err == nil && frame.Kind == protocol.KindData {
			relayData.Add(1)
		} else if err == nil && frame.Kind == protocol.KindPathPing {
			pathPings[frame.SourceMember].Add(1)
		} else if err == nil && frame.Kind == protocol.KindPathPong {
			pathPongs[frame.SourceMember].Add(1)
		}
		return true
	})
	t.Cleanup(stop)
	host, code := newTestRoom(t, address, port)
	guest := joinTestRoom(t, address, port, code, "guest")
	deadline := time.Now().Add(9 * time.Second)
	for (host.P2PStatus().RelayRTT[guest.MemberID()] == 0 || guest.P2PStatus().RelayRTT[host.MemberID()] == 0) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if host.P2PStatus().RelayRTT[guest.MemberID()] == 0 || guest.P2PStatus().RelayRTT[host.MemberID()] == 0 {
		t.Fatalf("relay RTT not measured: ping by host/guest=%d/%d, pong by host/guest=%d/%d, host=%v guest=%v, host probe=%s guest probe=%s",
			pathPings[host.MemberID()].Load(), pathPings[guest.MemberID()].Load(),
			pathPongs[host.MemberID()].Load(), pathPongs[guest.MemberID()].Load(),
			host.P2PStatus().RelayRTT, guest.P2PStatus().RelayRTT,
			relayProbeDebug(host, guest.MemberID()), relayProbeDebug(guest, host.MemberID()))
	}
	clients := []*Client{host, guest}
	for _, client := range clients {
		waitForP2P(t, "relay feature detection", func() bool { return client.P2PStatus().Supported })
		if client.P2PStatus().Enabled {
			t.Fatal("P2P enabled by default")
		}
	}
	if err := guest.SetP2PEnabled(true); err == nil {
		t.Fatal("guest switched P2P on")
	}
	if err := host.SetP2PEnabled(true); err != nil {
		t.Fatal(err)
	}
	waitForP2P(t, "initial pair", func() bool { return guest.P2PStatus().Peers[host.MemberID()] == Direct })
	deadline = time.Now().Add(9 * time.Second)
	for (host.P2PStatus().PeerRTT[guest.MemberID()] == 0 || guest.P2PStatus().PeerRTT[host.MemberID()] == 0) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if host.P2PStatus().PeerRTT[guest.MemberID()] == 0 || guest.P2PStatus().PeerRTT[host.MemberID()] == 0 {
		t.Fatalf("direct RTT not measured: host=%v guest=%v host path=%s guest path=%s",
			host.P2PStatus().PeerRTT, guest.P2PStatus().PeerRTT,
			directProbeDebug(host, guest.MemberID()), directProbeDebug(guest, host.MemberID()))
	}
	late := joinTestRoom(t, address, port, code, "late")
	clients = append(clients, late)
	waitForP2P(t, "every pair including late joiner", func() bool {
		for _, client := range clients {
			status := client.P2PStatus()
			for _, other := range clients {
				if other != client && status.Peers[other.MemberID()] != Direct {
					return false
				}
			}
		}
		return true
	})
	packets := make(chan []byte, 8)
	late.SetDataHandler(func(_ uint8, _, _ uint16, payload []byte) { packets <- payload })
	transfer := func(payload []byte, broadcast bool) {
		t.Helper()
		if !guest.SendGameData(late.MemberID(), 62056, 62056, payload, broadcast) {
			t.Fatal("failed to send game datagram")
		}
		select {
		case got := <-packets:
			if !bytes.Equal(got, payload) {
				t.Fatal("game datagram changed")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("game datagram did not arrive")
		}
	}
	transfer(bytes.Repeat([]byte{0xfe, 0xed}, 2000), false)
	if relayData.Load() != 0 || guest.Metrics().DirectTxFrames != 4 || late.Metrics().DirectRxFrames != 4 {
		t.Fatal("guest-to-guest fragmented datagram did not bypass the relay")
	}
	transfer([]byte("discovery"), true)
	if relayData.Load() != 1 || late.Metrics().DirectRxFrames != 4 {
		t.Fatal("broadcast did not stay on the relay")
	}
	if err := host.SetP2PEnabled(false); err != nil {
		t.Fatal(err)
	}
	waitForP2P(t, "room-wide switch-off", func() bool {
		return !host.P2PStatus().Pending && !guest.P2PStatus().Enabled && !late.P2PStatus().Enabled
	})
	deadline = time.Now().Add(9 * time.Second)
	for (host.P2PStatus().RelayRTT[guest.MemberID()] == 0 || guest.P2PStatus().RelayRTT[host.MemberID()] == 0) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if host.P2PStatus().RelayRTT[guest.MemberID()] == 0 || guest.P2PStatus().RelayRTT[host.MemberID()] == 0 {
		t.Fatalf("relay RTT disappeared after disabling direct paths: host=%v guest=%v", host.P2PStatus().RelayRTT, guest.P2PStatus().RelayRTT)
	}
	transfer([]byte("relayed after switch-off"), false)
	if relayData.Load() != 2 || late.Metrics().DirectRxFrames != 4 {
		t.Fatal("disabled room still used a direct route")
	}
}

func relayProbeDebug(client *Client, peerID uint8) string {
	client.sendMu.Lock()
	defer client.sendMu.Unlock()
	room := client.room.Load()
	if room == nil {
		return "no room"
	}
	state := &room.p2p
	return fmt.Sprintf("pending=%d rtt=%s next=%s peers=%v", state.relayPending[peerID], state.relayRTT[peerID],
		state.relayNextProbe[peerID].Format(time.RFC3339Nano), *room.peers.Load())
}

func directProbeDebug(client *Client, peerID uint8) string {
	client.sendMu.Lock()
	defer client.sendMu.Unlock()
	room := client.room.Load()
	if room == nil {
		return "no room"
	}
	state := &room.p2p
	path := state.paths[peerID]
	if path == nil {
		return fmt.Sprintf("no path enabled=%v mode=%d lease=%s", state.enabled, state.mode, state.leaseUntil.Format(time.RFC3339Nano))
	}
	return fmt.Sprintf("rtt=%s verified=%s cursor=%d probes=%v", path.rtt, path.verifiedUntil.Format(time.RFC3339Nano), path.probeCursor, path.probes)
}

func TestP2PProbeTimeoutFallsBackWithoutLeavingTheRoom(t *testing.T) {
	address, port, stop := startTestRelay(t)
	t.Cleanup(stop)
	host, code := newTestRoom(t, address, port)
	guest := joinTestRoom(t, address, port, code, "guest")
	waitForP2P(t, "relay feature detection", func() bool { return host.P2PStatus().Supported })
	if err := host.SetP2PEnabled(true); err != nil {
		t.Fatal(err)
	}
	waitForP2P(t, "direct path", func() bool { return host.P2PStatus().Peers[guest.MemberID()] == Direct })
	guest.sendMu.Lock()
	guestRoom := guest.room.Load()
	guestRoom.p2p.mode, guestRoom.p2p.modeSeq = protocol.P2POff, 0
	guest.sendMu.Unlock()
	waitForP2P(t, "lost direct probes", func() bool { return host.P2PStatus().Peers[guest.MemberID()] == ViaRelay })
	packets := make(chan []byte, 1)
	guest.SetDataHandler(func(_ uint8, _, _ uint16, payload []byte) { packets <- payload })
	want := []byte("still in the same room")
	if !host.SendGameData(guest.MemberID(), 62056, 62056, want, false) {
		t.Fatal("relay fallback send failed")
	}
	select {
	case got := <-packets:
		if !bytes.Equal(got, want) || !guest.Connected() || host.P2PStatus().Peers[guest.MemberID()] != ViaRelay {
			t.Fatal("fallback changed the payload or left the room")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay fallback did not arrive")
	}
}
