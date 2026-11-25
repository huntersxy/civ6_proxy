package playtest

import (
	"slices"
	"time"

	"civ6relay/internal/protocol"
)

type Discovery struct {
	Attempts, Broadcasts, Replies int
}

type Delivery struct {
	Sent, Received, Settled, Missing int
}

type Latency struct {
	Probes, Replies     int
	Median, P95, Jitter time.Duration
}

type Result struct {
	Phase                       Phase
	PhaseElapsed, PhaseDuration time.Duration
	Create, Join                time.Duration
	Discovery                   Discovery
	HostToGuest, GuestToHost    Delivery
	Fragmented                  Delivery
	SendFailures                int
	SteadyRTT, SyncRTT          Latency
}

func (test *simulation) publish(now time.Time) {
	if test.update != nil {
		test.update(test.snapshot(now))
	}
}

func (test *simulation) snapshot(now time.Time) Result {
	result := Result{
		Phase: test.phase, PhaseElapsed: now.Sub(test.phaseStart), PhaseDuration: test.phaseDuration,
		Create: test.create, Join: test.join,
	}
	if test.phaseDuration > 0 {
		result.PhaseElapsed = min(result.PhaseElapsed, test.phaseDuration)
	}
	test.mu.Lock()
	defer test.mu.Unlock()
	var steady, burst []time.Duration
	for _, packet := range test.packets {
		if packet.sentAt.After(now) {
			continue
		}
		received := arrived(packet.received, now)
		if packet.status == sendFailed {
			result.SendFailures++
		}
		switch packet.kind {
		case find:
			result.Discovery.Attempts++
			if received {
				result.Discovery.Broadcasts++
			}
			if arrived(packet.echo, now) {
				result.Discovery.Replies++
			}
			continue
		case found:
			continue
		}
		if packet.status != submitted {
			continue
		}
		settled := now.Sub(packet.sentAt) >= test.profile.settle
		delivery := &result.HostToGuest
		if packet.from == guestSide {
			delivery = &result.GuestToHost
		}
		delivery.record(received, settled)
		if packet.kind == syncData && packet.size > protocol.MaxFragmentPayload {
			result.Fragmented.record(received, settled)
		}
		if packet.kind == command {
			delay, samples := &result.SteadyRTT, &steady
			if packet.inSync {
				delay, samples = &result.SyncRTT, &burst
			}
			delay.Probes++
			if arrived(packet.echo, now) {
				*samples = append(*samples, max(time.Nanosecond, packet.echo.Sub(packet.sentAt)))
			}
		}
	}
	result.SteadyRTT.measure(steady)
	result.SyncRTT.measure(burst)
	return result
}

func arrived(at, cutoff time.Time) bool {
	return !at.IsZero() && !at.After(cutoff)
}

func (delivery *Delivery) record(received, settled bool) {
	delivery.Sent++
	if received {
		delivery.Received++
	}
	if settled {
		delivery.Settled++
		if !received {
			delivery.Missing++
		}
	}
}

func (latency *Latency) measure(samples []time.Duration) {
	count := len(samples)
	latency.Replies = count
	if count == 0 {
		return
	}
	for index := 1; index < count; index++ {
		delta := samples[index] - samples[index-1]
		latency.Jitter += max(delta, -delta)
	}
	latency.Jitter /= time.Duration(max(1, count-1))
	slices.Sort(samples)
	latency.Median = (samples[(count-1)/2] + samples[count/2]) / 2
	latency.P95 = samples[(count*95+99)/100-1]
}
