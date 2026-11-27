//go:build windows

package win

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"civ6relay/internal/packet"
	"civ6relay/internal/protocol"
	"civ6relay/internal/tunnel"
)

const (
	discoveryPortMin   = 62900
	discoveryPortMax   = 62999
	firstVirtualMember = protocol.FirstMemberID
	lastVirtualMember  = protocol.LastMemberID
	injectionQueueSize = 1024
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
	client          *tunnel.Client
	memberID        uint8
	memberIPs       [lastVirtualMember + 1]net.IP
	route           RouteInfo
	gameHandle      *DivertHandle
	discoveryHandle *DivertHandle
	logf            func(level, msg string)
	recordPacket    func([]byte)

	injectCh chan injectRequest
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	running   atomic.Bool
	discovery atomic.Uint64
	captured  atomic.Uint64
	injected  atomic.Uint64
	dropped   atomic.Uint64
	ipID      uint16
}

// StartBridge opens WinDivert and starts relaying for the given subnet.
func StartBridge(client *tunnel.Client, subnet *net.IPNet, route RouteInfo, driverDir string, logf func(level, msg string), recordPacket func([]byte)) (*Bridge, error) {
	if client == nil || subnet == nil || route.LocalIP.To4() == nil || route.IfIndex == 0 {
		return nil, errors.New("bridge requires a client and subnet")
	}
	memberID := client.MemberID()
	if memberID < firstVirtualMember || memberID > lastVirtualMember {
		return nil, errors.New("bridge requires an active room member")
	}
	if logf == nil {
		logf = func(string, string) {}
	}
	gameFilter, discoveryFilter := bridgeFilters(subnet)
	discovery, err := openDivert(driverDir, discoveryFilter, windivertFlagSniff|windivertFlagRecvOnly)
	if err != nil {
		return nil, fmt.Errorf("打开房间发现抓包失败: %w", err)
	}
	game, err := OpenDivert(driverDir, gameFilter)
	if err != nil {
		discovery.Close()
		return nil, fmt.Errorf("打开游戏流量抓包失败: %w", err)
	}
	b := &Bridge{
		client:          client,
		memberID:        memberID,
		route:           route,
		gameHandle:      game,
		discoveryHandle: discovery,
		logf:            logf,
		recordPacket:    recordPacket,
		injectCh:        make(chan injectRequest, injectionQueueSize),
		stop:            make(chan struct{}),
	}
	for id := uint8(firstVirtualMember); id <= lastVirtualMember; id++ {
		b.memberIPs[id] = VirtualMemberIP(subnet, id)
	}
	rnd := protocol.RandomBytes(2)
	b.ipID = uint16(rnd[0])<<8 | uint16(rnd[1])
	b.running.Store(true)
	client.SetDataHandler(b.enqueue)
	b.wg.Add(3)
	go b.recvLoop(game, false)
	go b.recvLoop(discovery, true)
	go b.injectLoop()
	return b, nil
}

func bridgeFilters(subnet *net.IPNet) (game, discovery string) {
	game = fmt.Sprintf("outbound and ip and udp and ip.DstAddr >= %s and ip.DstAddr <= %s",
		VirtualMemberIP(subnet, firstVirtualMember), VirtualMemberIP(subnet, lastVirtualMember))
	discovery = fmt.Sprintf("outbound and ip and udp and ip.DstAddr == 255.255.255.255 and udp.DstPort >= %d and udp.DstPort <= %d",
		discoveryPortMin, discoveryPortMax)
	return
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
		b.client.SetDataHandler(nil)
		close(b.stop)
		b.gameHandle.Shutdown()
		b.discoveryHandle.Shutdown()
		b.wg.Wait()
		b.gameHandle.Close()
		b.discoveryHandle.Close()
		clearQueuedPackets(b.injectCh)
	})
}

func clearQueuedPackets(ch chan injectRequest) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func (b *Bridge) recvLoop(handle *DivertHandle, discovery bool) {
	defer b.wg.Done()
	buf := make([]byte, packet.MaxIPv4Size)
	var addr wdAddress
	var received uint32
	for b.running.Load() {
		pkt, err := handle.recv(buf, &addr, &received)
		if err != nil {
			if b.running.CompareAndSwap(true, false) {
				b.logf("error", fmt.Sprintf("WinDivert 已停止: %v", err))
				go b.Stop()
			}
			return
		}
		if b.running.Load() {
			b.handlePacket(pkt, discovery)
		}
	}
}

func (b *Bridge) handlePacket(raw []byte, discovery bool) {
	d, err := packet.ParseIPv4UDP(raw)
	if err != nil {
		b.dropped.Add(1)
		return
	}
	if b.recordPacket != nil {
		b.recordPacket(raw)
	}
	if discovery {
		b.discovery.Add(1)
		if !b.client.SendGameData(255, d.SourcePort, d.TargetPort, d.Payload, true) {
			b.dropped.Add(1)
		}
		return
	}
	target := d.TargetIP[3]
	if target != b.memberID && b.client.SendGameData(target, d.SourcePort, d.TargetPort, d.Payload, false) {
		b.captured.Add(1)
	} else {
		b.dropped.Add(1)
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
		payload:      payload,
	}
	select {
	case b.injectCh <- req:
	default:
		b.dropped.Add(1)
	}
}

func (b *Bridge) injectLoop() {
	defer b.wg.Done()
	buf := make([]byte, 0, packet.MaxIPv4Size)
	addr := wdAddress{Bits: windivertIPChecksum | windivertUDPChecksum, IfIdx: b.route.IfIndex}
	var nextLog time.Time
	for b.running.Load() {
		select {
		case <-b.stop:
			return
		case req := <-b.injectCh:
			if err := b.injectOne(buf, &addr, req); err != nil {
				b.dropped.Add(1)
				if b.running.Load() && time.Now().After(nextLog) {
					b.logf("warn", fmt.Sprintf("注入游戏数据包失败: %v", err))
					nextLog = time.Now().Add(5 * time.Second)
				}
			} else {
				b.injected.Add(1)
			}
		}
	}
}

func (b *Bridge) injectOne(buf []byte, addr *wdAddress, req injectRequest) error {
	if req.sourceMember < firstVirtualMember || req.sourceMember > lastVirtualMember {
		return errors.New("无效的虚拟成员地址")
	}
	b.ipID++
	raw, err := packet.AppendIPv4UDP(buf, b.memberIPs[req.sourceMember], b.route.LocalIP, req.sourcePort, req.targetPort, req.payload, 128, b.ipID)
	if err != nil {
		return err
	}
	if err := b.gameHandle.Send(raw, addr); err != nil {
		return err
	}
	if b.recordPacket != nil {
		b.recordPacket(raw)
	}
	return nil
}
