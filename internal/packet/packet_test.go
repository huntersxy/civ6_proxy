package packet

import (
	"encoding/hex"
	"net"
	"testing"
)

const (
	pythonPacket1 = "4500002112340000801161e90afafa02c0a8010af268f5b4000d0d3568656c6c6f"
	pythonPacket2 = "45000080ffff000040116b650afafa090a0000050001ffff006cf00c00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
)

func TestBuildMatchesPython(t *testing.T) {
	p, err := BuildIPv4UDP(net.IPv4(10, 250, 250, 2), net.IPv4(192, 168, 1, 10), 62056, 62900, []byte("hello"), 128, 0x1234)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(p); got != pythonPacket1 {
		t.Fatalf("packet mismatch\n got %s\nwant %s", got, pythonPacket1)
	}

	p2, err := BuildIPv4UDP(net.IPv4(10, 250, 250, 9), net.IPv4(10, 0, 0, 5), 1, 65535, make([]byte, 100), 64, 0xffff)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(p2); got != pythonPacket2 {
		t.Fatalf("packet2 mismatch\n got %s\nwant %s", got, pythonPacket2)
	}
}

func TestParseRoundTrip(t *testing.T) {
	p, err := BuildIPv4UDP(net.IPv4(10, 250, 250, 3), net.IPv4(192, 168, 1, 20), 62900, 62056, []byte("discovery"), 128, 7)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseIPv4UDP(p)
	if err != nil {
		t.Fatal(err)
	}
	if !d.SourceIP.Equal(net.IPv4(10, 250, 250, 3)) || !d.TargetIP.Equal(net.IPv4(192, 168, 1, 20)) ||
		d.SourcePort != 62900 || d.TargetPort != 62056 || string(d.Payload) != "discovery" {
		t.Fatalf("round trip mismatch: %+v", d)
	}
}

func TestParseRejects(t *testing.T) {
	if _, err := ParseIPv4UDP([]byte("short")); err == nil {
		t.Fatal("short packet accepted")
	}
	p, _ := BuildIPv4UDP(net.IPv4(1, 2, 3, 4), net.IPv4(5, 6, 7, 8), 1, 2, nil, 128, 0)
	p[9] = 6 // TCP
	if _, err := ParseIPv4UDP(p); err == nil {
		t.Fatal("non-UDP packet accepted")
	}
}
