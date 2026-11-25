// Command civ6relay-server runs the Civ6Relay UDP room relay.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"civ6relay/internal/protocol"
	"civ6relay/internal/relay"
	"civ6relay/internal/speedtest"
)

var version = "dev"

func main() {
	var (
		listen     = flag.String("listen", ":42056", "UDP listen address")
		maxRooms   = flag.Int("max-rooms", 1024, "maximum concurrent rooms")
		readers    = flag.Int("readers", 4, "number of UDP reader goroutines")
		quiet      = flag.Bool("quiet", false, "suppress room lifecycle logs")
		stats      = flag.Duration("stats", time.Minute, "periodic stats interval (0 disables)")
		showVer    = flag.Bool("version", false, "print version and exit")
		memberPPS  = flag.Int("member-pps", relay.DefaultMemberPacketRate, "per-member limit in DATA frames per second")
		memberRate = flag.Int("member-byte-rate", relay.DefaultMemberByteRate, "per-member limit in DATA wire bytes per second")
		testMbps   = flag.Int("speedtest-mbps", 0, "maximum UDP speed test Mbps in either direction (0 disables)")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	if *readers < 1 {
		*readers = 1
	}
	if *readers > 64 {
		*readers = 64
	}
	if *readers > runtime.NumCPU()*2 {
		*readers = runtime.NumCPU() * 2
	}

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	if *memberPPS <= 0 || *memberRate <= 0 {
		logger.Fatal("-member-pps and -member-byte-rate must be positive")
	}
	if *testMbps < 0 || *testMbps > speedtest.MaxMbps {
		logger.Fatalf("-speedtest-mbps must be between 0 and %d", speedtest.MaxMbps)
	}
	roomLogger := logger
	if *quiet {
		roomLogger = log.New(io.Discard, "", 0)
	}

	addr, err := net.ResolveUDPAddr("udp4", *listen)
	if err != nil {
		logger.Fatalf("resolve %s: %v", *listen, err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		logger.Fatalf("listen %s: %v", *listen, err)
	}
	if err := conn.SetReadBuffer(8 << 20); err != nil {
		logger.Printf("set UDP receive buffer: %v", err)
	}
	if err := conn.SetWriteBuffer(8 << 20); err != nil {
		logger.Printf("set UDP send buffer: %v", err)
	}

	srv := relay.New(relay.Config{
		MaxRooms: *maxRooms, MemberPacketRate: *memberPPS, MemberByteRate: *memberRate, Logger: roomLogger,
	})
	tester, err := speedtest.NewServer(*testMbps, func(packet []byte, peer netip.AddrPort) error {
		_, err := conn.WriteToUDPAddrPort(packet, peer)
		return err
	})
	if err != nil {
		logger.Fatalf("initialize speed test: %v", err)
	}
	logger.Printf("civ6relay %s listening on %s (readers=%d max_rooms=%d member_pps=%d member_bytes_per_sec=%d speedtest_mbps=%d)",
		version, conn.LocalAddr(), *readers, *maxRooms, *memberPPS, *memberRate, *testMbps)

	var received atomic.Uint64
	var sent atomic.Uint64
	var sendErrors atomic.Uint64
	var readErrors atomic.Uint64
	var wg sync.WaitGroup
	stop := make(chan struct{})

	send := func(datagrams []relay.Outbound) {
		var written uint64
		for _, out := range datagrams {
			if _, err := conn.WriteToUDPAddrPort(out.Data, out.Addr); err == nil {
				written++
			}
		}
		if written > 0 {
			sent.Add(written)
		}
		if failed := uint64(len(datagrams)) - written; failed > 0 {
			sendErrors.Add(failed)
		}
	}

	// Cleanup loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				send(srv.Cleanup())
				tester.Sweep()
			}
		}
	}()

	// Reader loops.
	for i := 0; i < *readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, protocol.MaxUDPPayload)
			var batch relay.Batch
			for {
				n, peer, err := conn.ReadFromUDPAddrPort(buf)
				if err != nil {
					select {
					case <-stop:
						return
					default:
					}
					readErrors.Add(1)
					// A single bad datagram should not kill the relay. Back off
					// briefly so a persistent socket error cannot spin.
					time.Sleep(20 * time.Millisecond)
					continue
				}
				if speedtest.IsDatagram(buf[:n]) {
					tester.Handle(buf[:n], peer)
					continue
				}
				received.Add(1)
				srv.HandleInto(buf[:n], peer, &batch)
				send(batch.Datagrams)
			}
		}()
	}

	// Optional periodic stats.
	if *stats > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(*stats)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					rooms, members := srv.Stats()
					logger.Printf("stats rooms=%d members=%d rx=%d tx=%d rx_errors=%d tx_errors=%d",
						rooms, members, received.Load(), sent.Load(), readErrors.Load(), sendErrors.Load())
				}
			}
		}()
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	close(stop)
	_ = conn.Close()
	wg.Wait()
	tester.Close()
	logger.Printf("shutdown complete")
}
