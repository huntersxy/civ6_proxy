package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"civ6relay/internal/protocol"
)

func BenchmarkForwardData(b *testing.B) {
	for _, scenario := range []struct {
		name    string
		members int
		target  uint8
	}{
		{"unicast", 2, 3},
		{"broadcast", 8, 255},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			start := time.Now()
			server := New(Config{
				Logger: log.New(io.Discard, "", 0),
				Now:    func() time.Time { return start.Add(time.Since(start) * 10000) },
			})
			type sender struct {
				addr *net.UDPAddr
				key  []byte
				raw  []byte
			}
			senders := make([]sender, runtime.GOMAXPROCS(0))
			for index := range senders {
				host := addr(index * 8)
				welcome := doHandshake(b, server, protocol.KindCreate, protocol.HandshakeRequest{
					RequestID: fmt.Sprintf("%08x", index*8), Name: "host",
				}, host).welcome
				if welcome == nil {
					b.Fatal("create failed")
				}
				for offset := 1; offset < scenario.members; offset++ {
					guest := doHandshake(b, server, protocol.KindJoin, protocol.HandshakeRequest{
						RequestID: fmt.Sprintf("%08x", index*8+offset), Name: "guest", RoomCode: welcome.RoomCode,
					}, addr(index*8+offset)).welcome
					if guest == nil {
						b.Fatal("join failed")
					}
				}
				key := tokenOf(b, welcome)
				raw := clientFrame(b, key, parseRoomID(b, welcome.RoomID), welcome.MemberID, protocol.KindData, scenario.target, 1, make([]byte, 512))
				senders[index] = sender{host, key, raw}
			}

			var worker atomic.Uint32
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				input := senders[worker.Add(1)-1]
				endpoint := input.addr.AddrPort()
				var batch Batch
				mac := hmac.New(sha256.New, input.key)
				tagOffset := len(input.raw) - protocol.AuthTagSize
				var digest [sha256.Size]byte
				var sequence uint64
				for pb.Next() {
					sequence++
					binary.BigEndian.PutUint64(input.raw[28:36], sequence)
					mac.Reset()
					mac.Write(input.raw[:tagOffset])
					copy(input.raw[tagOffset:], mac.Sum(digest[:0]))
					server.HandleInto(input.raw, endpoint, &batch)
					if len(batch.Datagrams) != scenario.members-1 {
						b.Error("forwarding failed")
						return
					}
				}
			})
		})
	}
}
