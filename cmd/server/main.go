// Command civ6relay-server runs the Civ6Relay UDP room relay.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"civ6relay/internal/relay"
)

var version = "dev"

func main() {
	var (
		listen   = flag.String("listen", ":42056", "UDP listen address")
		maxRooms = flag.Int("max-rooms", 1024, "maximum concurrent rooms")
		readers  = flag.Int("readers", 4, "number of UDP reader goroutines")
		quiet    = flag.Bool("quiet", false, "suppress room lifecycle logs")
		stats    = flag.Duration("stats", time.Minute, "periodic stats interval (0 disables)")
		showVer  = flag.Bool("version", false, "print version and exit")
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

	var logger *log.Logger
	if *quiet {
		logger = log.New(io.Discard, "", 0)
	} else {
		logger = log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	}

	addr, err := net.ResolveUDPAddr("udp4", *listen)
	if err != nil {
		logger.Fatalf("resolve %s: %v", *listen, err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		logger.Fatalf("listen %s: %v", *listen, err)
	}
	_ = conn.SetReadBuffer(8 << 20)
	_ = conn.SetWriteBuffer(8 << 20)

	srv := relay.New(relay.Config{MaxRooms: *maxRooms, Logger: logger})
	logger.Printf("civ6relay %s listening on %s (readers=%d max_rooms=%d)", version, conn.LocalAddr(), *readers, *maxRooms)

	var received atomic.Uint64
	var sent atomic.Uint64
	var wg sync.WaitGroup
	stop := make(chan struct{})

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
				for _, out := range srv.Cleanup() {
					if _, err := conn.WriteToUDP(out.Data, out.Addr); err == nil {
						sent.Add(1)
					}
				}
			}
		}
	}()

	// Reader loops.
	for i := 0; i < *readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 2048)
			for {
				n, peer, err := conn.ReadFromUDP(buf)
				if err != nil {
					select {
					case <-stop:
						return
					default:
					}
					// A single bad datagram should not kill the relay. Back off
					// briefly so a persistent socket error cannot spin.
					time.Sleep(20 * time.Millisecond)
					continue
				}
				received.Add(1)
				for _, out := range srv.Handle(buf[:n], peer) {
					if _, err := conn.WriteToUDP(out.Data, out.Addr); err == nil {
						sent.Add(1)
					}
				}
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
					logger.Printf("stats rooms=%d members=%d rx=%d tx=%d", rooms, members, received.Load(), sent.Load())
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
	logger.Printf("shutdown complete")
}
