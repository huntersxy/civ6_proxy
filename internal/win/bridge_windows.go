//go:build windows

package win

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"civ6relay/internal/packet"
	"civ6relay/internal/protocol"
	"civ6relay/internal/tunnel"
)

const (
	discoveryPortMin = 62900
	discoveryPortMax = 62999
)

// BridgeStats is a snapshot of packet bridge counters.
type BridgeStats struct {
	Discovery int
	Captured  int
	Injected  int
	Dropped   int
}

type injectRequest struct {
	sourceMember uint8
	sourcePort   uint16
	targetPort   uint16
	payload      []byte
}

// Bridge captures Civ 6 LAN traffic with WinDivert and tunnels it through the
// relay client.
type Bridge struct {
	client  *tunnel.Client
	subnet  *net.IPNet
	localIP net.IP
	route   RouteInfo
	handle  *DivertHandle
	logf    func(level, msg string)

	injectCh chan injectRequest
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	running   atomic.Bool
	discovery atomic.Uint64
	captured  atomic.Uint64
	injected  atomic.Uint64
	dropped   atomic.Uint64
	ipID      atomic.Uint32
}

// StartBridge opens WinDivert and starts relaying for the given subnet.
func StartBridge(client *tunnel.Client, subnet *net.IPNet, route RouteInfo, driverDir string, logf func(level, msg string)) (*Bridge, error) {
	if client == nil || subnet == nil {
		return nil, errors.New("bridge requires a client and subnet")
	}
	if logf == nil {
		logf = func(string, string) {}
	}
	broadcast := VirtualMemberIP(subnet, 255)
	network := subnet.IP.Mask(subnet.Mask)
	filter := fmt.Sprintf(
		"outbound and ip and udp and ((ip.DstAddr == 255.255.255.255 and udp.DstPort >= %d and udp.DstPort <= %d) or (ip.DstAddr >= %s and ip.DstAddr <= %s))",
		discoveryPortMin, discoveryPortMax, network, broadcast,
	)
	handle, err := OpenDivert(driverDir, filter)
	if err != nil {
		return nil, err
	}
	b := &Bridge{
		client:   client,
		subnet:   subnet,
		localIP:  route.LocalIP,
		route:    route,
		handle:   handle,
		logf:     logf,
		injectCh: make(chan injectRequest, 2048),
		stop:     make(chan struct{}),
	}
	rnd := protocol.RandomBytes(2)
	b.ipID.Store(uint32(rnd[0])<<8 | uint32(rnd[1]))
	b.running.Store(true)
	client.SetDataHandler(b.enqueue)
	b.wg.Add(2)
	go b.recvLoop()
	go b.injectLoop()
	logf("info", fmt.Sprintf("WinDivert 已启动，虚拟网段 %s", subnet))
	return b, nil
}

// Running reports whether the capture loop is alive.
func (b *Bridge) Running() bool { return b != nil && b.running.Load() }

// Stats returns a snapshot of the bridge counters.
func (b *Bridge) Stats() BridgeStats {
	if b == nil {
		return BridgeStats{}
	}
	return BridgeStats{
		Discovery: int(b.discovery.Load()),
		Captured:  int(b.captured.Load()),
		Injected:  int(b.injected.Load()),
		Dropped:   int(b.dropped.Load()),
	}
}

// Stop shuts the bridge down.
func (b *Bridge) Stop() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() {
		b.running.Store(false)
		if b.handle != nil {
			b.handle.Shutdown()
		}
		close(b.stop)
		if b.handle != nil {
			b.handle.Close()
		}
		b.wg.Wait()
		b.client.SetDataHandler(nil)
	})
}

func (b *Bridge) recvLoop() {
	defer b.wg.Done()
	buf := make([]byte, windivertPacketBufSize)
	for {
		pkt, addr, err := b.handle.Recv(buf)
		if err != nil {
			if b.running.Load() {
				b.running.Store(false)
				b.logf("error", fmt.Sprintf("WinDivert 已停止: %v", err))
			}
			return
		}
		b.handlePacket(pkt, &addr)
	}
}

func (b *Bridge) handlePacket(raw []byte, addr *wdAddress) {
	d, err := packet.ParseIPv4UDP(raw)
	if err != nil {
		return
	}
	if d.TargetIP.Equal(net.IPv4bcast) {
		b.discovery.Add(1)
		b.client.SendGameData(255, d.SourcePort, d.TargetPort, d.Payload, true)
		// Keep the original broadcast on the real LAN as well.
		if err := b.handle.Send(raw, addr); err != nil {
			b.logf("warn", fmt.Sprintf("回注局域网广播失败: %v", err))
		}
		return
	}
	if b.subnet.Contains(d.TargetIP) {
		dst := d.TargetIP.To4()
		var target uint8
		if dst != nil {
			target = dst[3]
		}
		if target >= 2 && target <= 9 && target != b.client.MemberID() {
			if b.client.SendGameData(target, d.SourcePort, d.TargetPort, d.Payload, false) {
				b.captured.Add(1)
			} else {
				b.dropped.Add(1)
			}
		} else {
			b.dropped.Add(1)
		}
		return
	}
	// Defensive: a filter mismatch must not swallow unrelated traffic.
	if err := b.handle.Send(raw, addr); err != nil {
		b.logf("warn", fmt.Sprintf("回注未匹配数据包失败: %v", err))
	}
}

func (b *Bridge) enqueue(sourceMember uint8, sourcePort, targetPort uint16, payload []byte) {
	if !b.running.Load() {
		return
	}
	req := injectRequest{
		sourceMember: sourceMember,
		sourcePort:   sourcePort,
		targetPort:   targetPort,
		payload:      append([]byte(nil), payload...),
	}
	select {
	case b.injectCh <- req:
	default:
		b.dropped.Add(1)
	}
}

func (b *Bridge) injectLoop() {
	defer b.wg.Done()
	for {
		select {
		case <-b.stop:
			return
		case req := <-b.injectCh:
			b.injectOne(req)
		}
	}
}

func (b *Bridge) injectOne(req injectRequest) {
	sourceIP := VirtualMemberIP(b.subnet, req.sourceMember)
	if sourceIP == nil {
		b.dropped.Add(1)
		return
	}
	id := uint16(b.ipID.Add(1))
	raw, err := packet.BuildIPv4UDP(sourceIP, b.localIP, req.sourcePort, req.targetPort, req.payload, 128, id)
	if err != nil {
		b.dropped.Add(1)
		return
	}
	var addr wdAddress
	addr.setLayer(windivertLayerNetwork)
	addr.setOutbound(false)
	addr.IfIdx = b.route.IfIndex
	addr.SubIfIdx = 0
	if err := b.handle.Send(raw, &addr); err != nil {
		b.dropped.Add(1)
		b.logf("warn", fmt.Sprintf("注入游戏数据包失败: %v", err))
		return
	}
	b.injected.Add(1)
}
