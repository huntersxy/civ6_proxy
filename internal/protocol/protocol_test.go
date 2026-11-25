package protocol

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestControlEncodingUsesCompactUTF8(t *testing.T) {
	got, err := EncodeControl(HandshakeRequest{RequestID: "0123456789abcdef", Name: "测试"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"request_id":"0123456789abcdef","name":"测试"}` {
		t.Fatalf("unexpected control payload %q", got)
	}
}

func TestFrameEncodeUsesCurrentVersion(t *testing.T) {
	wantPayload := []byte(`{"request_id":"0123456789abcdef","name":"测试"}`)
	got, err := (&Frame{Kind: KindCreate, PacketID: 0x11223344, Payload: wantPayload}).Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[:4]) != Magic || got[4] != Version {
		t.Fatalf("unexpected protocol header %q version=%d", got[:4], got[4])
	}
	frame, err := Decode(got)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Kind != KindCreate || frame.PacketID != 0x11223344 || string(frame.Payload) != string(wantPayload) {
		t.Fatalf("decoded fields mismatch: %+v", frame)
	}
	if string(got[HeaderSize:HeaderSize+len(wantPayload)]) != string(wantPayload) {
		t.Fatal("control JSON was not encoded as UTF-8")
	}
}

func TestDecodeRejectsPreviousProtocolVersion(t *testing.T) {
	raw, err := (&Frame{Kind: KindCreate}).Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	copy(raw[:4], "C6R1")
	raw[4] = 1
	if _, err := Decode(raw); !errors.Is(err, ErrMagic) {
		t.Fatalf("previous protocol magic returned %v", err)
	}
	copy(raw[:4], Magic)
	if _, err := Decode(raw); !errors.Is(err, ErrVersion) {
		t.Fatalf("previous protocol version returned %v", err)
	}
}

func TestAuthenticatedFrameRoundTripAndVerification(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	frame := Frame{
		Kind: KindData, RoomID: 0x0123456789abcdef, SourceMember: 2,
		TargetMember: 3, FragmentIndex: 1, FragmentCount: 3,
		SourcePort: 62056, TargetPort: 62056, Sequence: 0x0102030405060708,
		PacketID: 0xdeadbeef, Payload: []byte("\x00\x01hello"),
	}
	raw, err := frame.Encode(key)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Kind != frame.Kind || decoded.RoomID != frame.RoomID || decoded.SourceMember != frame.SourceMember ||
		decoded.TargetMember != frame.TargetMember || decoded.FragmentIndex != frame.FragmentIndex ||
		decoded.FragmentCount != frame.FragmentCount || decoded.SourcePort != frame.SourcePort ||
		decoded.TargetPort != frame.TargetPort || decoded.Sequence != frame.Sequence || decoded.PacketID != frame.PacketID ||
		string(decoded.Payload) != string(frame.Payload) {
		t.Fatalf("decoded fields mismatch: %+v", decoded)
	}
	if !decoded.Verified(key) {
		t.Fatal("HMAC verification failed")
	}
	key[0] ^= 1
	if decoded.Verified(key) {
		t.Fatal("verification succeeded with wrong key")
	}
}

func TestEncodeAuthenticatedRoundTrip(t *testing.T) {
	key := RandomBytes(32)
	f := Frame{
		Kind: KindData, Flags: FlagBroadcast | FlagFragmented, RoomID: 42,
		SourceMember: 2, TargetMember: 255, FragmentIndex: 2, FragmentCount: 4,
		SourcePort: 62056, TargetPort: 62900, Sequence: 7, PacketID: 9,
		Payload: []byte("payload"),
	}
	raw, err := f.Encode(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > MaxOuterDatagram {
		t.Fatalf("datagram too large: %d", len(raw))
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Flags&FlagAuthenticated == 0 || !got.Verified(key) {
		t.Fatal("round trip lost authentication")
	}
	if got.FragmentIndex != 2 || got.Payload == nil || string(got.Payload) != "payload" {
		t.Fatalf("round trip fields mismatch: %+v", got)
	}
}

func TestAppendEncodeReusesBuffer(t *testing.T) {
	key := make([]byte, 32)
	auth := NewAuthenticator(key)
	frame := Frame{Kind: KindData, Payload: []byte("room data"), FragmentCount: 1}
	buffer := make([]byte, 0, MaxOuterDatagram+1)
	buffer = append(buffer, 42)
	raw, err := frame.AppendEncode(buffer, auth)
	if err != nil || &raw[0] != &buffer[0] || raw[0] != 42 || !auth.Verify(raw[1:]) {
		t.Fatalf("invalid encoded frame or buffer: %v", err)
	}
	decoded, err := DecodeView(raw[1:])
	if err != nil {
		t.Fatal(err)
	}
	raw[1+HeaderSize] ^= 1
	if decoded.Payload[0] == 'r' || auth.Verify(raw[1:]) {
		t.Fatal("payload view or tag validation failed")
	}
	raw, err = frame.AppendEncode(buffer, nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = Decode(raw[1:])
	if err != nil || decoded.AuthTag != [AuthTagSize]byte{} || decoded.Flags&FlagAuthenticated != 0 {
		t.Fatalf("unsigned frame reused the previous tag: %+v %v", decoded, err)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte("short"),
		append(make([]byte, HeaderSize+AuthTagSize), 0),
	}
	for _, c := range cases {
		if _, err := Decode(c); err == nil {
			t.Fatalf("expected error for %x", c)
		}
	}
}
func TestReplayWindow(t *testing.T) {
	var w ReplayWindow
	if !w.Accept(100) {
		t.Fatal("first packet rejected")
	}
	if w.Accept(100) {
		t.Fatal("duplicate accepted")
	}
	if !w.Accept(101) {
		t.Fatal("next packet rejected")
	}
	if !w.Accept(99) {
		t.Fatal("out of order inside window rejected")
	}
	if w.Accept(99) {
		t.Fatal("duplicate out of order accepted")
	}
	if !w.Accept(400) {
		t.Fatal("big jump rejected")
	}
	if w.Accept(100) {
		t.Fatal("old packet accepted after jump")
	}
	if !w.Accept(399) {
		t.Fatal("previous packet rejected after jump")
	}
	if w.Accept(144) {
		t.Fatal("packet exactly 256 behind accepted")
	}
}

func TestSplitPayload(t *testing.T) {
	chunks, err := SplitPayload(nil)
	if err != nil || len(chunks) != 1 || len(chunks[0]) != 0 {
		t.Fatalf("empty payload: %v %v", chunks, err)
	}
	payload := make([]byte, MaxFragmentPayload*2+5)
	chunks, err = SplitPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 || len(chunks[0]) != MaxFragmentPayload || len(chunks[2]) != 5 {
		t.Fatalf("unexpected split sizes: %d", len(chunks))
	}
}

func TestReassembler(t *testing.T) {
	r := NewReassembler()
	now := time.Unix(0, 0)
	r.SetClock(func() time.Time { return now })
	base := Frame{
		Kind: KindData, RoomID: 1, SourceMember: 2, PacketID: 5,
		SourcePort: 62056, TargetPort: 62056, FragmentCount: 3,
	}
	f0 := base
	f0.FragmentIndex = 0
	f0.Payload = []byte("aa")
	f1 := base
	f1.FragmentIndex = 1
	f1.Payload = []byte("bb")
	f2 := base
	f2.FragmentIndex = 2
	f2.Payload = []byte("cc")

	if out, err := r.Add(&f0); err != nil || out != nil {
		t.Fatalf("first fragment: %v %v", out, err)
	}
	if out, err := r.Add(&f2); err != nil || out != nil {
		t.Fatalf("third fragment: %v %v", out, err)
	}
	out, err := r.Add(&f1)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "aabbcc" {
		t.Fatalf("got %q", out)
	}
	now = now.Add(3 * time.Second)
	if got := r.Expire(); got != 0 {
		t.Fatalf("unexpected expired count %d", got)
	}
}

func TestNormalizeName(t *testing.T) {
	if got, err := NormalizeName("  玩家甲  "); err != nil || got != "玩家甲" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := NormalizeName(""); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := NormalizeName(strings.Repeat("a", 25)); err == nil {
		t.Fatal("long name accepted")
	}
	if _, err := NormalizeName("bad\x01name"); err == nil {
		t.Fatal("control character accepted")
	}
}

func TestNormalizeRoomCode(t *testing.T) {
	code, err := NormalizeRoomCode("ab4t-dnes-0d")
	if err != nil || code != "AB4TDNES0D" {
		t.Fatalf("got %q %v", code, err)
	}
	for _, bad := range []string{"", "ABCDEFGHI", "ABCDEFGHIO", "ABCDEFGHI-"} {
		if _, err := NormalizeRoomCode(bad); err == nil {
			t.Fatalf("accepted invalid code %q", bad)
		}
	}
}
