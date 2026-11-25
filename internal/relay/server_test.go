package relay

import (
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"civ6relay/internal/protocol"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestServer(t *testing.T) (*Server, *testClock) {
	t.Helper()
	clock := newTestClock()
	s := New(Config{Now: clock.Now, Logger: testLogger(t)})
	return s, clock
}

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(testWriter{t}, "", 0)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", string(p))
	return len(p), nil
}

func addr(port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, byte(port%200+2)), Port: 20000 + port}
}

func encodeHandshake(t testing.TB, kind protocol.Kind, req protocol.HandshakeRequest) []byte {
	t.Helper()
	payload, err := protocol.EncodeControl(req)
	if err != nil {
		t.Fatal(err)
	}
	frame := protocol.Frame{Kind: kind, Payload: payload}
	raw, err := frame.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type hResult struct {
	welcome *protocol.WelcomeMessage
	errMsg  *protocol.ErrorMessage
	raw     []byte
}

func doHandshake(t testing.TB, s *Server, kind protocol.Kind, req protocol.HandshakeRequest, a *net.UDPAddr) hResult {
	t.Helper()
	out := s.Handle(encodeHandshake(t, kind, req), a)
	if len(out) == 0 {
		return hResult{}
	}
	frame, err := protocol.Decode(out[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	res := hResult{raw: out[0].Data}
	switch frame.Kind {
	case protocol.KindWelcome:
		var w protocol.WelcomeMessage
		if err := protocol.DecodeControl(frame.Payload, &w); err != nil {
			t.Fatal(err)
		}
		res.welcome = &w
	case protocol.KindError:
		var e protocol.ErrorMessage
		if err := protocol.DecodeControl(frame.Payload, &e); err != nil {
			t.Fatal(err)
		}
		res.errMsg = &e
	}
	return res
}

func tokenOf(t testing.TB, w *protocol.WelcomeMessage) []byte {
	t.Helper()
	token, err := base64.URLEncoding.DecodeString(w.Token)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 32 {
		t.Fatalf("token length %d", len(token))
	}
	return token
}

func clientFrame(t testing.TB, token []byte, roomID uint64, member uint8, kind protocol.Kind, target uint8, seq uint64, payload []byte) []byte {
	t.Helper()
	var flags protocol.Flags
	if target == 255 {
		flags |= protocol.FlagBroadcast
	}
	frame := protocol.Frame{
		Kind:          kind,
		Flags:         flags,
		RoomID:        roomID,
		SourceMember:  member,
		TargetMember:  target,
		Sequence:      seq,
		PacketID:      uint32(seq),
		FragmentCount: 1,
		SourcePort:    62056,
		TargetPort:    62056,
		Payload:       payload,
	}
	raw, err := frame.Encode(token)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodeOutbound(t *testing.T, out []Outbound) []protocol.Frame {
	t.Helper()
	frames := make([]protocol.Frame, 0, len(out))
	for _, o := range out {
		f, err := protocol.Decode(o.Data)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, f)
	}
	return frames
}

func TestCreateJoinAndPeers(t *testing.T) {
	s, _ := newTestServer(t)
	a, b := addr(1), addr(2)
	create := protocol.HandshakeRequest{RequestID: "0123456789abcdef", Name: "房主"}
	res := doHandshake(t, s, protocol.KindCreate, create, a)
	if res.welcome == nil {
		t.Fatalf("create failed: %+v", res.errMsg)
	}
	w := res.welcome
	if w.RoomCode == "" || len(w.RoomCode) != 10 || w.MemberID != 2 || !w.Host || len(w.Members) != 1 {
		t.Fatalf("unexpected welcome: %+v", w)
	}
	if _, err := protocol.NormalizeRoomCode(w.RoomCode); err != nil {
		t.Fatal(err)
	}
	tokenA := tokenOf(t, w)

	join := protocol.HandshakeRequest{RequestID: "fedcba9876543210", Name: "玩家乙", RoomCode: w.RoomCode}
	out := s.Handle(encodeHandshake(t, protocol.KindJoin, join), b)
	if len(out) != 3 {
		t.Fatalf("expected welcome+two peer updates, got %d datagrams", len(out))
	}
	frames := decodeOutbound(t, out)
	if frames[0].Kind != protocol.KindWelcome || frames[1].Kind != protocol.KindPeers || frames[2].Kind != protocol.KindPeers {
		t.Fatalf("unexpected frames: %v %v %v", frames[0].Kind, frames[1].Kind, frames[2].Kind)
	}
	var welcomeB protocol.WelcomeMessage
	if err := protocol.DecodeControl(frames[0].Payload, &welcomeB); err != nil {
		t.Fatal(err)
	}
	tokenB := tokenOf(t, &welcomeB)
	if welcomeB.MemberID != 3 || welcomeB.Host || len(welcomeB.Members) != 2 {
		t.Fatalf("unexpected join welcome: %+v", welcomeB)
	}
	if !frames[1].Verified(tokenA) {
		t.Fatal("host peers frame not signed for host token")
	}
	var peers protocol.PeerListMessage
	if err := protocol.DecodeControl(frames[1].Payload, &peers); err != nil {
		t.Fatal(err)
	}
	if len(peers.Members) != 2 || peers.Members[0].ID != 2 || peers.Members[1].ID != 3 {
		t.Fatalf("unexpected peers: %+v", peers)
	}

	// Join with a bad code returns room_not_found.
	bad := protocol.HandshakeRequest{RequestID: "1111222233334444", Name: "x", RoomCode: "ABCDEFGHJK"}
	res = doHandshake(t, s, protocol.KindJoin, bad, addr(3))
	if res.errMsg == nil || res.errMsg.Code != "room_not_found" {
		t.Fatalf("expected room_not_found, got %+v", res.errMsg)
	}
	_ = tokenB
}

func TestDataUnicastAndBroadcast(t *testing.T) {
	s, _ := newTestServer(t)
	a, b := addr(1), addr(2)
	resA := doHandshake(t, s, protocol.KindCreate, protocol.HandshakeRequest{RequestID: "aaaabbbbccccdddd", Name: "A"}, a)
	wA := resA.welcome
	tokenA := tokenOf(t, wA)
	roomID := parseRoomID(t, wA.RoomID)
	outJoin := s.Handle(encodeHandshake(t, protocol.KindJoin, protocol.HandshakeRequest{
		RequestID: "ddddccccbbbbaaaa", Name: "B", RoomCode: wA.RoomCode,
	}), b)
	var welcomeB protocol.WelcomeMessage
	frames := decodeOutbound(t, outJoin)
	if err := protocol.DecodeControl(frames[0].Payload, &welcomeB); err != nil {
		t.Fatal(err)
	}
	tokenB := tokenOf(t, &welcomeB)

	// Unicast A -> B.
	out := s.Handle(clientFrame(t, tokenA, roomID, 2, protocol.KindData, 3, 1, []byte("hello B")), a)
	if len(out) != 1 {
		t.Fatalf("expected 1 datagram, got %d", len(out))
	}
	f, err := protocol.Decode(out[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Verified(tokenB) || f.Kind != protocol.KindData || f.SourceMember != 2 || f.TargetMember != 3 ||
		string(f.Payload) != "hello B" || f.SourcePort != 62056 {
		t.Fatalf("unexpected forwarded frame: %+v", f)
	}

	// Broadcast A -> B only.
	out = s.Handle(clientFrame(t, tokenA, roomID, 2, protocol.KindData, 255, 2, []byte("broadcast")), a)
	if len(out) != 1 {
		t.Fatalf("broadcast targets: %d", len(out))
	}
	f, _ = protocol.Decode(out[0].Data)
	if f.TargetMember != 3 || f.Flags&protocol.FlagBroadcast == 0 {
		t.Fatalf("bad broadcast frame: %+v", f)
	}

	// PING -> PONG echoes payload.
	pingPayload, _ := protocol.EncodeControl(protocol.PingMessage{SentNS: 12345})
	out = s.Handle(clientFrame(t, tokenA, roomID, 2, protocol.KindPing, 0, 3, pingPayload), a)
	if len(out) != 1 {
		t.Fatalf("ping outputs: %d", len(out))
	}
	f, _ = protocol.Decode(out[0].Data)
	if f.Kind != protocol.KindPong || f.TargetMember != 2 || string(f.Payload) != string(pingPayload) || !f.Verified(tokenA) {
		t.Fatalf("bad pong: %+v", f)
	}

	// Wrong token is ignored.
	badToken := append([]byte(nil), tokenA...)
	badToken[0] ^= 0xff
	if out := s.Handle(clientFrame(t, badToken, roomID, 2, protocol.KindData, 3, 4, []byte("x")), a); len(out) != 0 {
		t.Fatal("frame with wrong token accepted")
	}
	// Replay is ignored.
	if out := s.Handle(clientFrame(t, tokenA, roomID, 2, protocol.KindData, 3, 1, []byte("x")), a); len(out) != 0 {
		t.Fatal("replayed frame accepted")
	}
}

func parseRoomID(t testing.TB, text string) uint64 {
	t.Helper()
	var id uint64
	if _, err := fmt.Sscanf(text, "%016x", &id); err != nil {
		t.Fatalf("room id %q: %v", text, err)
	}
	return id
}

func TestLeaveAndCloseRoom(t *testing.T) {
	s, _ := newTestServer(t)
	a, b := addr(1), addr(2)
	wA := doHandshake(t, s, protocol.KindCreate, protocol.HandshakeRequest{RequestID: "aaaabbbbccccdddd", Name: "A"}, a).welcome
	tokenA := tokenOf(t, wA)
	roomID := parseRoomID(t, wA.RoomID)
	outJoin := s.Handle(encodeHandshake(t, protocol.KindJoin, protocol.HandshakeRequest{
		RequestID: "ddddccccbbbbaaaa", Name: "B", RoomCode: wA.RoomCode,
	}), b)
	frames := decodeOutbound(t, outJoin)
	var wB protocol.WelcomeMessage
	_ = protocol.DecodeControl(frames[0].Payload, &wB)
	tokenB := tokenOf(t, &wB)

	out := s.Handle(clientFrame(t, tokenB, roomID, 3, protocol.KindLeave, 0, 1, nil), b)
	if len(out) != 1 {
		t.Fatalf("leave outputs: %d", len(out))
	}
	f, _ := protocol.Decode(out[0].Data)
	var peers protocol.PeerListMessage
	_ = protocol.DecodeControl(f.Payload, &peers)
	if f.Kind != protocol.KindPeers || len(peers.Members) != 1 || peers.Members[0].ID != 2 {
		t.Fatalf("unexpected post-leave peers: %+v", peers)
	}

	// Host leave closes the room and notifies the still-online host itself.
	out = s.Handle(clientFrame(t, tokenA, roomID, 2, protocol.KindLeave, 0, 1, nil), a)
	if len(out) != 1 {
		t.Fatalf("host leave outputs: %d", len(out))
	}
	f, _ = protocol.Decode(out[0].Data)
	if f.Kind != protocol.KindRoomClosed || !f.Verified(tokenA) {
		t.Fatalf("expected authenticated ROOM_CLOSED, got %v", f.Kind)
	}
	if rooms, _ := s.Stats(); rooms != 0 {
		t.Fatalf("room not removed: %d", rooms)
	}
	if res := doHandshake(t, s, protocol.KindJoin, protocol.HandshakeRequest{RequestID: "9999888877776666", Name: "C", RoomCode: wA.RoomCode}, addr(3)); res.errMsg == nil || res.errMsg.Code != "room_not_found" {
		t.Fatalf("closed room still joinable: %+v", res.errMsg)
	}
}

func TestPathPingAndPongAreRelayedToTheirPeer(t *testing.T) {
	s, _ := newTestServer(t)
	a, b := addr(1), addr(2)
	host := doHandshake(t, s, protocol.KindCreate, protocol.HandshakeRequest{
		RequestID: "aaaabbbbccccdddd", Name: "A",
	}, a).welcome
	guestFrames := decodeOutbound(t, s.Handle(encodeHandshake(t, protocol.KindJoin, protocol.HandshakeRequest{
		RequestID: "ddddccccbbbbaaaa", Name: "B", RoomCode: host.RoomCode,
	}), b))
	var guest protocol.WelcomeMessage
	if err := protocol.DecodeControl(guestFrames[0].Payload, &guest); err != nil {
		t.Fatal(err)
	}
	hostToken, guestToken := tokenOf(t, host), tokenOf(t, &guest)
	roomID := parseRoomID(t, host.RoomID)
	payload := []byte{0, 0, 0, 0, 0, 0, 0, 42}

	out := s.Handle(clientFrame(t, hostToken, roomID, host.MemberID, protocol.KindPathPing, guest.MemberID, 1, payload), a)
	if len(out) != 1 || out[0].Addr.Addr().Unmap() != b.AddrPort().Addr().Unmap() || out[0].Addr.Port() != uint16(b.Port) {
		t.Fatalf("path ping did not reach peer: %#v", out)
	}
	ping, err := protocol.Decode(out[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if !ping.Verified(guestToken) || ping.Kind != protocol.KindPathPing || ping.SourceMember != host.MemberID ||
		ping.TargetMember != guest.MemberID || string(ping.Payload) != string(payload) {
		t.Fatalf("unexpected relayed path ping: %+v", ping)
	}

	out = s.Handle(clientFrame(t, guestToken, roomID, guest.MemberID, protocol.KindPathPong, host.MemberID, 1, payload), b)
	if len(out) != 1 || out[0].Addr.Addr().Unmap() != a.AddrPort().Addr().Unmap() || out[0].Addr.Port() != uint16(a.Port) {
		t.Fatalf("path pong did not return to peer: %#v", out)
	}
	pong, err := protocol.Decode(out[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if !pong.Verified(hostToken) || pong.Kind != protocol.KindPathPong || pong.SourceMember != guest.MemberID ||
		pong.TargetMember != host.MemberID || string(pong.Payload) != string(payload) {
		t.Fatalf("unexpected relayed path pong: %+v", pong)
	}

	if out := s.Handle(clientFrame(t, hostToken, roomID, host.MemberID, protocol.KindPathPing, broadcastMember, 2, payload), a); len(out) != 0 {
		t.Fatalf("path ping was broadcast: %#v", out)
	}
	if out := s.Handle(clientFrame(t, hostToken, roomID, host.MemberID, protocol.KindPathPing, guest.MemberID, 3, payload[:7]), a); len(out) != 0 {
		t.Fatalf("invalid path ping payload was relayed: %#v", out)
	}
}

func TestCleanupOfflineAndHostTimeout(t *testing.T) {
	s, clock := newTestServer(t)
	a, b, c := addr(1), addr(2), addr(3)
	wA := doHandshake(t, s, protocol.KindCreate, protocol.HandshakeRequest{RequestID: "aaaabbbbccccdddd", Name: "A"}, a).welcome
	roomID := parseRoomID(t, wA.RoomID)
	_ = tokenOf(t, wA)
	for i, peer := range []struct {
		name string
		addr *net.UDPAddr
		req  string
	}{{"B", b, "ddddccccbbbbaaaa"}, {"C", c, "ccccddddeeeeffff"}} {
		out := s.Handle(encodeHandshake(t, protocol.KindJoin, protocol.HandshakeRequest{
			RequestID: peer.req, Name: peer.name, RoomCode: wA.RoomCode,
		}), peer.addr)
		if len(out) < 1 {
			t.Fatalf("join %d failed", i)
		}
	}

	// A and B go idle while C keeps sending PINGs. At 31s A and B are offline
	// but C is online, so cleanup produces PEERS only for C.
	clock.Advance(31 * time.Second)
	// C's token: join C again through resume to obtain it (fresh handshake).
	// Easier: fetch C's welcome from a new join response above is not stored;
	// recreate a lightweight active member by resuming with its token.
	var wC protocol.WelcomeMessage
	// Join a fourth member D at 31s so it is definitely online.
	d := addr(4)
	outD := s.Handle(encodeHandshake(t, protocol.KindJoin, protocol.HandshakeRequest{
		RequestID: "eeeeffff00001111", Name: "D", RoomCode: wA.RoomCode,
	}), d)
	if len(outD) == 0 {
		t.Fatal("join D failed")
	}
	_ = protocol.DecodeControl(decodeOutbound(t, outD)[0].Payload, &wC)
	tokenD := tokenOf(t, &wC)

	out := s.Cleanup()
	if len(out) == 0 {
		t.Fatal("expected peer updates for online member")
	}
	f, _ := protocol.Decode(out[0].Data)
	var peers protocol.PeerListMessage
	_ = protocol.DecodeControl(f.Payload, &peers)
	if len(peers.Members) != 4 {
		t.Fatalf("unexpected peers: %+v", peers)
	}
	online := map[uint8]bool{}
	for _, p := range peers.Members {
		online[p.ID] = p.Online
	}
	if online[2] || online[3] || !online[5] {
		t.Fatalf("unexpected online flags: %+v", peers.Members)
	}

	// At 61s the host times out; D (kept alive) gets ROOM_CLOSED.
	clock.Advance(30 * time.Second)
	if out := s.Handle(clientFrame(t, tokenD, roomID, 5, protocol.KindPing, 0, 1, nil), d); len(out) != 1 {
		t.Fatal("active member ping failed")
	}
	out = s.Cleanup()
	if len(out) == 0 {
		t.Fatal("expected room close on host timeout")
	}
	f, _ = protocol.Decode(out[0].Data)
	if f.Kind != protocol.KindRoomClosed {
		t.Fatalf("expected ROOM_CLOSED, got %v", f.Kind)
	}
	if rooms, _ := s.Stats(); rooms != 0 {
		t.Fatalf("room not closed: %d", rooms)
	}
}

func TestResume(t *testing.T) {
	s, _ := newTestServer(t)
	a, b, b2 := addr(1), addr(2), addr(4)
	wA := doHandshake(t, s, protocol.KindCreate, protocol.HandshakeRequest{RequestID: "aaaabbbbccccdddd", Name: "A"}, a).welcome
	roomID := parseRoomID(t, wA.RoomID)
	outJoin := s.Handle(encodeHandshake(t, protocol.KindJoin, protocol.HandshakeRequest{
		RequestID: "ddddccccbbbbaaaa", Name: "B", RoomCode: wA.RoomCode,
	}), b)
	var wB protocol.WelcomeMessage
	_ = protocol.DecodeControl(decodeOutbound(t, outJoin)[0].Payload, &wB)
	tokenB := tokenOf(t, &wB)

	res := doHandshake(t, s, protocol.KindJoin, protocol.HandshakeRequest{
		RequestID: "aaaabbbbccccdddd", Name: "B", RoomCode: wA.RoomCode,
		ResumeMember: 3, ResumeToken: base64.URLEncoding.EncodeToString(tokenB),
	}, b2)
	if res.welcome == nil {
		t.Fatalf("resume failed: %+v", res.errMsg)
	}
	if res.welcome.MemberID != 3 {
		t.Fatalf("resume changed member id: %d", res.welcome.MemberID)
	}
	newToken := tokenOf(t, res.welcome)
	if string(newToken) == string(tokenB) {
		t.Fatal("token was not rotated")
	}
	// Old token must no longer authenticate.
	if out := s.Handle(clientFrame(t, tokenB, roomID, 3, protocol.KindPing, 0, 1, nil), b); len(out) != 0 {
		t.Fatal("old token still valid after resume")
	}
	// New token works from the new endpoint.
	if out := s.Handle(clientFrame(t, newToken, roomID, 3, protocol.KindPing, 0, 1, nil), b2); len(out) != 1 {
		t.Fatal("resumed token rejected")
	}
}

func TestHandshakeCacheAndRateLimit(t *testing.T) {
	s, _ := newTestServer(t)
	a := addr(1)
	req := protocol.HandshakeRequest{RequestID: "aaaabbbbccccdddd", Name: "A"}
	raw := encodeHandshake(t, protocol.KindCreate, req)
	first := s.Handle(raw, a)
	second := s.Handle(raw, a)
	if len(first) != 1 || len(second) != 1 || string(first[0].Data) != string(second[0].Data) {
		t.Fatal("handshake retransmission was not answered from cache")
	}
	// Exhaust the per-IP handshake bucket and expect silence.
	rejected := 0
	for i := 0; i < 20; i++ {
		other := protocol.HandshakeRequest{RequestID: fmt.Sprintf("%016x", 0x1000+i), Name: "A"}
		if out := s.Handle(encodeHandshake(t, protocol.KindCreate, other), a); len(out) == 0 {
			rejected++
		}
	}
	if rejected == 0 {
		t.Fatal("handshake rate limit never triggered")
	}
}

func TestMaxRooms(t *testing.T) {
	clock := newTestClock()
	s := New(Config{MaxRooms: 1, Now: clock.Now, Logger: testLogger(t)})
	w := doHandshake(t, s, protocol.KindCreate, protocol.HandshakeRequest{RequestID: "aaaabbbbccccdddd", Name: "A"}, addr(1)).welcome
	if w == nil {
		t.Fatal("first create failed")
	}
	res := doHandshake(t, s, protocol.KindCreate, protocol.HandshakeRequest{RequestID: "bbbbccccddddeeee", Name: "B"}, addr(2))
	if res.errMsg == nil || res.errMsg.Code != "server_full" {
		t.Fatalf("expected server_full, got %+v", res.errMsg)
	}
}

func TestCreateWithAuthFlagRejected(t *testing.T) {
	s, _ := newTestServer(t)
	payload, _ := protocol.EncodeControl(protocol.HandshakeRequest{RequestID: "aaaabbbbccccdddd", Name: "A"})
	frame := protocol.Frame{Kind: protocol.KindCreate, Payload: payload, Flags: protocol.FlagAuthenticated}
	raw, err := frame.Encode([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if out := s.Handle(raw, addr(1)); len(out) != 0 {
		t.Fatal("authenticated CREATE was accepted")
	}
}

func TestJoinRejectsUnencodableRoster(t *testing.T) {
	server, _ := newTestServer(t)
	longName := strings.Repeat("🤝", 18)
	host := doHandshake(t, server, protocol.KindCreate, protocol.HandshakeRequest{
		RequestID: "00000001", Name: longName,
	}, addr(1)).welcome
	for index := 2; index <= 7; index++ {
		join := doHandshake(t, server, protocol.KindJoin, protocol.HandshakeRequest{
			RequestID: fmt.Sprintf("%08x", index), Name: longName, RoomCode: host.RoomCode,
		}, addr(index))
		if join.welcome == nil {
			t.Fatalf("member %d should fit: %+v", index, join.errMsg)
		}
	}
	overflow := doHandshake(t, server, protocol.KindJoin, protocol.HandshakeRequest{
		RequestID: "00000008", Name: longName, RoomCode: host.RoomCode,
	}, addr(8))
	if overflow.errMsg == nil || overflow.errMsg.Code != "roster_too_large" {
		t.Fatalf("expected roster error, got %+v", overflow)
	}
	guest := doHandshake(t, server, protocol.KindJoin, protocol.HandshakeRequest{
		RequestID: "00000009", Name: "guest", RoomCode: host.RoomCode,
	}, addr(8)).welcome
	if guest == nil || guest.MemberID != 9 || len(guest.Members) != 8 {
		t.Fatalf("failed join affected the roster: %+v", guest)
	}
}

func TestConfiguredMemberRateRefills(t *testing.T) {
	clock := newTestClock()
	server := New(Config{Now: clock.Now, Logger: testLogger(t), MemberPacketRate: 1})
	host := doHandshake(t, server, protocol.KindCreate, protocol.HandshakeRequest{
		RequestID: "00000001", Name: "host",
	}, addr(1)).welcome
	guest := doHandshake(t, server, protocol.KindJoin, protocol.HandshakeRequest{
		RequestID: "00000002", Name: "guest", RoomCode: host.RoomCode,
	}, addr(2)).welcome
	for index, count := range []int{1, 1, 0, 1} {
		if index == 3 {
			clock.Advance(time.Second)
		}
		data := clientFrame(t, tokenOf(t, host), parseRoomID(t, host.RoomID), host.MemberID,
			protocol.KindData, guest.MemberID, uint64(index+1), []byte("test"))
		if out := server.Handle(data, addr(1)); len(out) != count {
			t.Fatalf("packet %d: got %d outbound frames, want %d", index, len(out), count)
		}
	}
}
