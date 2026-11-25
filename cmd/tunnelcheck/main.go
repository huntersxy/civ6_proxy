// Command tunnelcheck is a headless relay smoke test: it opens two tunnel
// clients, creates a room, joins it, and exchanges unicast and broadcast data.
//
// Usage: tunnelcheck -relay 127.0.0.1:42056
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"civ6relay/internal/tunnel"
)

func main() {
	relayAddr := flag.String("relay", "127.0.0.1:42056", "relay host:port")
	timeout := flag.Duration("timeout", 5*time.Second, "handshake timeout")
	flag.Parse()

	host, portText, err := net.SplitHostPort(*relayAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid relay address %q: %v\n", *relayAddr, err)
		os.Exit(2)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid port %q\n", portText)
		os.Exit(2)
	}

	a, err := tunnel.New(host, port)
	if err != nil {
		fail("create client A", err)
	}
	defer a.Disconnect()
	welcomeA, err := a.CreateRoom("go-host", *timeout)
	if err != nil {
		fail("create room", err)
	}
	fmt.Printf("room=%s host_member=%d\n", welcomeA.RoomCode, welcomeA.MemberID)

	b, err := tunnel.New(host, port)
	if err != nil {
		fail("create client B", err)
	}
	defer b.Disconnect()
	gotA := make(chan []byte, 4)
	gotB := make(chan []byte, 4)
	a.SetDataHandler(func(_ uint8, _, _ uint16, payload []byte) { gotA <- append([]byte(nil), payload...) })
	b.SetDataHandler(func(_ uint8, _, _ uint16, payload []byte) { gotB <- append([]byte(nil), payload...) })
	welcomeB, err := b.JoinRoom(welcomeA.RoomCode, "go-guest", *timeout)
	if err != nil {
		fail("join room", err)
	}
	fmt.Printf("joined member=%d peers=%d\n", welcomeB.MemberID, len(welcomeB.Members))

	if !b.SendGameData(welcomeA.MemberID, 62056, 62900, []byte("hello-host"), false) {
		fail("send unicast", nil)
	}
	if err := expect(gotA, "hello-host", *timeout); err != nil {
		fail("unicast A<-B", err)
	}
	if !a.SendGameData(255, 62900, 62056, []byte("broadcast"), true) {
		fail("send broadcast", nil)
	}
	if err := expect(gotB, "broadcast", *timeout); err != nil {
		fail("broadcast B<-A", err)
	}
	fmt.Println("PASS")
}

func expect(ch chan []byte, want string, timeout time.Duration) error {
	select {
	case got := <-ch:
		if string(got) != want {
			return fmt.Errorf("got %q, want %q", got, want)
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for %q", want)
	}
}

func fail(what string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", what, err)
	} else {
		fmt.Fprintf(os.Stderr, "FAIL %s\n", what)
	}
	os.Exit(1)
}
