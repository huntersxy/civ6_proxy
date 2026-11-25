// Command tunnelcheck is a headless relay smoke test: it opens two tunnel
// clients, creates a room, joins it, and exchanges unicast and broadcast data.
//
// Usage: tunnelcheck -relay 127.0.0.1:42056
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"time"

	"civ6relay/internal/playtest"
	"civ6relay/internal/tunnel"
)

func main() {
	relayAddr := flag.String("relay", "127.0.0.1:42056", "relay host:port")
	timeout := flag.Duration("timeout", 5*time.Second, "handshake timeout")
	simulate := flag.Bool("playtest", false, "run the same synthetic match as the desktop client")
	flag.Parse()
	if *simulate {
		if err := checkPlay(*relayAddr); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL %v\n", err)
			os.Exit(1)
		}
		fmt.Println("PASS")
		return
	}

	host, portText, err := net.SplitHostPort(*relayAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid relay address %q: %v\n", *relayAddr, err)
		os.Exit(2)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "invalid port %q\n", portText)
		os.Exit(2)
	}
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "timeout must be positive")
		os.Exit(2)
	}

	if err := check(host, port, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %v\n", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func checkPlay(address string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	result, err := playtest.Run(ctx, address, nil)
	fmt.Printf("create=%s join=%s discovery_broadcast=%d/%d discovery_reply=%d/%d send_errors=%d\n",
		result.Create.Round(time.Microsecond), result.Join.Round(time.Microsecond),
		result.Discovery.Broadcasts, result.Discovery.Attempts, result.Discovery.Replies, result.Discovery.Attempts, result.SendFailures)
	for index, delivery := range []playtest.Delivery{result.HostToGuest, result.GuestToHost, result.Fragmented} {
		fmt.Printf("%s sent=%d received=%d missing=%d/%d\n", [...]string{"host_to_guest", "guest_to_host", "fragmented"}[index],
			delivery.Sent, delivery.Received, delivery.Missing, delivery.Settled)
	}
	for index, latency := range []playtest.Latency{result.SteadyRTT, result.SyncRTT} {
		fmt.Printf("%s replies=%d/%d median=%s p95=%s jitter=%s\n", [...]string{"steady", "sync"}[index],
			latency.Replies, latency.Probes, latency.Median.Round(time.Microsecond), latency.P95.Round(time.Microsecond), latency.Jitter.Round(time.Microsecond))
	}
	if err != nil {
		return err
	}
	if result.Discovery.Replies == 0 || result.SteadyRTT.Replies == 0 || result.SyncRTT.Replies == 0 || result.Fragmented.Received == 0 {
		return fmt.Errorf("the relay did not complete all simulated exchanges")
	}
	return nil
}

func check(host string, port int, timeout time.Duration) error {
	hostContext, cancelHost := context.WithTimeout(context.Background(), timeout)
	defer cancelHost()
	hostClient, err := tunnel.NewContext(hostContext, host, port)
	if err != nil {
		return fmt.Errorf("open host tunnel: %w", err)
	}
	defer hostClient.Disconnect()
	hostRTT := make(chan time.Duration, 1)
	hostClient.SetEventHandler(func(event tunnel.Event) {
		if event.Type == tunnel.EventRTT {
			select {
			case hostRTT <- event.RTT:
			default:
			}
		}
	})
	welcomeHost, err := hostClient.CreateRoomContext(hostContext, "go-host")
	if err != nil {
		return fmt.Errorf("create room: %w", err)
	}
	fmt.Printf("room=%s host_member=%d\n", welcomeHost.RoomCode, welcomeHost.MemberID)

	guestContext, cancelGuest := context.WithTimeout(context.Background(), timeout)
	defer cancelGuest()
	guestClient, err := tunnel.NewContext(guestContext, host, port)
	if err != nil {
		return fmt.Errorf("open guest tunnel: %w", err)
	}
	defer guestClient.Disconnect()
	gotHost := capture(hostClient)
	gotGuest := capture(guestClient)
	welcomeGuest, err := guestClient.JoinRoomContext(guestContext, welcomeHost.RoomCode, "go-guest")
	if err != nil {
		return fmt.Errorf("join room: %w", err)
	}
	fmt.Printf("joined member=%d peers=%d\n", welcomeGuest.MemberID, len(welcomeGuest.Members))

	unicast := []byte("hello-host")
	if !guestClient.SendGameData(welcomeHost.MemberID, 62056, 62900, unicast, false) {
		return fmt.Errorf("send unicast")
	}
	if err := expect(gotHost, unicast, timeout); err != nil {
		return fmt.Errorf("receive unicast: %w", err)
	}
	broadcast := []byte("broadcast")
	if !hostClient.SendGameData(255, 62900, 62056, broadcast, true) {
		return fmt.Errorf("send broadcast")
	}
	if err := expect(gotGuest, broadcast, timeout); err != nil {
		return fmt.Errorf("receive broadcast: %w", err)
	}
	fragmented := bytes.Repeat([]byte("fragment-check"), 300)
	if !hostClient.SendGameData(welcomeGuest.MemberID, 62056, 62056, fragmented, false) {
		return fmt.Errorf("send fragmented datagram")
	}
	if err := expect(gotGuest, fragmented, timeout); err != nil {
		return fmt.Errorf("receive fragmented datagram: %w", err)
	}
	fmt.Printf("fragmented_datagram=%d bytes\n", len(fragmented))
	select {
	case rtt := <-hostRTT:
		if rtt < time.Microsecond {
			fmt.Println("relay_rtt=<1µs")
		} else {
			fmt.Printf("relay_rtt=%s\n", rtt.Round(time.Microsecond))
		}
	case <-time.After(timeout):
		return fmt.Errorf("relay RTT probe timed out")
	}
	return nil
}

func capture(client *tunnel.Client) <-chan []byte {
	received := make(chan []byte, 4)
	client.SetDataHandler(func(_ uint8, _, _ uint16, payload []byte) {
		select {
		case received <- payload:
		default:
		}
	})
	return received
}

func expect(ch <-chan []byte, want []byte, timeout time.Duration) error {
	select {
	case got := <-ch:
		if !bytes.Equal(got, want) {
			return fmt.Errorf("payload differs (got %d bytes, want %d)", len(got), len(want))
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for %q", want)
	}
}
