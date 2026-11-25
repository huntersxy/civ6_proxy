package protocol

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

const (
	pythonControlHex = "7b22726571756573745f6964223a2230313233343536373839616263646566222c226e616d65223a225c75366434625c7538626435227d"
	pythonFrame1Hex  = "433652310101000000000000000000000000000000010000000000370000000000000000112233447b22726571756573745f6964223a2230313233343536373839616263646566222c226e616d65223a225c75366434625c7538626435227d00000000000000000000000000000000"
	pythonFrame2Hex  = "43365231010800070123456789abcdef020300010003f268f26800070102030405060708deadbeef000168656c6c6f051796d12d7496517657a00c41644154"
)

func TestControlMatchesPython(t *testing.T) {
	got, err := EncodeControl(HandshakeRequest{RequestID: "0123456789abcdef", Name: "测试"})
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != pythonControlHex {
		t.Fatalf("control mismatch\n got %x\nwant %s", got, pythonControlHex)
	}
}

func TestFrameEncodeMatchesPython(t *testing.T) {
	control, _ := EncodeControl(HandshakeRequest{RequestID: "0123456789abcdef", Name: "测试"})
	f := Frame{Kind: KindCreate, PacketID: 0x11223344, Payload: control}
	got, err := f.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != pythonFrame1Hex {
		t.Fatalf("frame mismatch\n got %x\nwant %s", got, pythonFrame1Hex)
	}
}

func TestFrameDecodePython(t *testing.T) {
	raw, _ := hex.DecodeString(pythonFrame2Hex)
	f, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != KindData || f.RoomID != 0x0123456789abcdef || f.SourceMember != 2 ||
		f.TargetMember != 3 || f.FragmentIndex != 1 || f.FragmentCount != 3 ||
		f.SourcePort != 62056 || f.TargetPort != 62056 || f.Sequence != 0x0102030405060708 ||
		f.PacketID != 0xdeadbeef || string(f.Payload) != "\x00\x01hello" {
		t.Fatalf("decoded fields mismatch: %+v", f)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	if !f.Verified(key) {
		t.Fatal("HMAC verification failed")
	}
	key[0] ^= 1
	if f.Verified(key) {
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

func TestDecodeRejectsGarbage(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte("short"),
		append(bytesOf(pythonFrame1Hex), 0),
	}
	for _, c := range cases {
		if _, err := Decode(c); err == nil {
			t.Fatalf("expected error for %x", c)
		}
	}
}

func bytesOf(h string) []byte {
	b, _ := hex.DecodeString(h)
	return b
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
