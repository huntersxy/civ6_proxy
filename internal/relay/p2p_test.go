package relay

import (
	"bytes"
	"net"
	"strconv"
	"testing"
	"time"

	"civ6relay/internal/protocol"
)

func TestP2PRendezvousAuthorizesHostAndVerifiedEndpoints(t *testing.T) {
	server, clock := newTestServer(t)
	hostAddr, guestAddr, legacyAddr, changedAddr := addr(1), addr(2), addr(3), addr(4)
	host := doHandshake(t, server, protocol.KindCreate, protocol.HandshakeRequest{
		Name: "host", RequestID: "0123456789abcdef",
	}, hostAddr).welcome
	if host == nil {
		t.Fatal("no host welcome")
	}
	join := func(name string, endpoint *net.UDPAddr) *protocol.WelcomeMessage {
		t.Helper()
		request := protocol.HandshakeRequest{Name: name, RequestID: protocol.RandomHex(12), RoomCode: host.RoomCode}
		result := doHandshake(t, server, protocol.KindJoin, request, endpoint)
		if result.welcome == nil {
			t.Fatalf("no welcome for %s: %+v", name, result.errMsg)
		}
		return result.welcome
	}
	guest := join("guest", guestAddr)
	join("legacy", legacyAddr)
	roomID, err := strconv.ParseUint(host.RoomID, 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	var sequence [10]uint64
	send := func(member *protocol.WelcomeMessage, endpoint *net.UDPAddr, request protocol.P2PRequest, seq uint64) []Outbound {
		t.Helper()
		payload, err := protocol.EncodeControl(request)
		if err != nil {
			t.Fatal(err)
		}
		raw := clientFrame(t, tokenOf(t, member), roomID, member.MemberID, protocol.KindP2PRequest, 0, seq, payload)
		return server.Handle(raw, endpoint)
	}
	request := func(member *protocol.WelcomeMessage, endpoint *net.UDPAddr, request protocol.P2PRequest) protocol.P2PState {
		t.Helper()
		sequence[member.MemberID]++
		seq := sequence[member.MemberID]
		out := send(member, endpoint, request, seq)
		frames := decodeOutbound(t, out)
		var matched *protocol.Frame
		for index := range frames {
			if frames[index].Kind == protocol.KindP2PState && frames[index].TargetMember == member.MemberID &&
				out[index].Addr.String() == endpoint.String() {
				matched = &frames[index]
				break
			}
		}
		if matched == nil || !matched.Verified(tokenOf(t, member)) {
			t.Fatalf("missing authenticated rendezvous response for member %d: %+v", member.MemberID, frames)
		}
		var reply protocol.P2PState
		if err := protocol.DecodeControl(matched.Payload, &reply); err != nil || reply.Request != seq {
			t.Fatalf("invalid rendezvous response: %+v, %v", reply, err)
		}
		return reply
	}
	verify := func(member *protocol.WelcomeMessage, endpoint *net.UDPAddr, mode protocol.P2PMode) protocol.P2PState {
		t.Helper()
		challenge := request(member, endpoint, protocol.P2PRequest{Mode: mode})
		if len(challenge.Challenge) != protocol.P2PProofSize || len(challenge.Peers) != 0 {
			t.Fatalf("expected endpoint challenge without peers: %+v", challenge)
		}
		return request(member, endpoint, protocol.P2PRequest{Mode: mode, Proof: challenge.Challenge})
	}
	if reply := verify(guest, guestAddr, protocol.P2POn); reply.Enabled || len(reply.Peers) != 0 {
		t.Fatalf("guest enabled P2P: %+v", reply)
	}
	if reply := verify(host, hostAddr, protocol.P2PPoll); reply.Enabled || len(reply.Peers) != 0 {
		t.Fatalf("P2P did not default to off: %+v", reply)
	}
	hostState := request(host, hostAddr, protocol.P2PRequest{Mode: protocol.P2POn})
	guestState := request(guest, guestAddr, protocol.P2PRequest{Mode: protocol.P2POff})
	if !hostState.Enabled || !guestState.Enabled || len(hostState.Peers) != 1 || len(guestState.Peers) != 1 {
		t.Fatalf("invalid membership or guest changed P2P mode: %+v, %+v", hostState, guestState)
	}
	hostPeer, guestPeer := hostState.Peers[0], guestState.Peers[0]
	if hostPeer.MemberID != guest.MemberID || guestPeer.MemberID != host.MemberID ||
		len(hostPeer.Key) != protocol.P2PKeySize || !bytes.Equal(hostPeer.Key, guestPeer.Key) ||
		hostPeer.Endpoint.String() != guestAddr.String() || guestPeer.Endpoint.String() != hostAddr.String() {
		t.Fatalf("incorrect endpoints or asymmetric pair key: %+v, %+v", hostPeer, guestPeer)
	}
	if reply := verify(guest, changedAddr, protocol.P2PPoll); !reply.Enabled || len(reply.Peers) != 1 || bytes.Equal(hostPeer.Key, reply.Peers[0].Key) {
		t.Fatalf("rebinding did not replace pair credentials: %+v", reply)
	}
	if reply := request(host, hostAddr, protocol.P2PRequest{}); len(reply.Peers) != 1 || reply.Peers[0].Endpoint.String() != changedAddr.String() {
		t.Fatalf("rebinding was not published: %+v", reply)
	}
	sequence[host.MemberID]++
	staleOn := sequence[host.MemberID]
	if reply := request(host, hostAddr, protocol.P2PRequest{Mode: protocol.P2POff}); reply.Enabled || len(reply.Peers) != 0 {
		t.Fatalf("host failed to turn off P2P: %+v", reply)
	}
	if out := send(host, hostAddr, protocol.P2PRequest{Mode: protocol.P2POn}, staleOn); len(out) != 0 {
		t.Fatal("stale room-setting command was accepted")
	}
	if reply := request(guest, changedAddr, protocol.P2PRequest{}); reply.Enabled || len(reply.Peers) != 0 {
		t.Fatalf("guest did not see disable: %+v", reply)
	}
	clock.Advance(protocol.P2PLease + time.Millisecond)
	if reply := request(host, hostAddr, protocol.P2PRequest{Mode: protocol.P2POn}); len(reply.Challenge) == 0 || reply.Enabled {
		t.Fatalf("expired endpoint was not revalidated: %+v", reply)
	}
}
