package speedtest

import (
	"math"
	"testing"
	"time"
)

func TestRateControllerWaitsForCurrentRateFeedback(t *testing.T) {
	start := time.Now()
	controller := newRateController(400, start)
	initial := controller.rate
	controller.update(start.Add(250*time.Millisecond), 55, deliverySample{seen: 50, received: 50})
	ramped := controller.rate
	if ramped <= initial {
		t.Fatal("clean feedback did not increase the send rate")
	}
	controller.update(start.Add(350*time.Millisecond), 75, deliverySample{seen: 54, received: 50})
	controller.update(start.Add(500*time.Millisecond), 140, deliverySample{seen: 130, received: 106})
	if controller.rate != ramped {
		t.Fatal("feedback spanning the previous rate changed the rate again")
	}
	controller.update(start.Add(750*time.Millisecond), 227, deliverySample{seen: 215, received: 174})
	backedOff := controller.rate
	if target := ramped * 0.8 * 0.97; math.Abs(backedOff-target) > 0.01 {
		t.Fatalf("send rate after 20%% loss = %.2f, want %.2f packets/s", backedOff, target)
	}
	controller.update(start.Add(time.Second), 291, deliverySample{seen: 280, received: 239})
	controller.update(start.Add(1250*time.Millisecond), 360, deliverySample{seen: 347, received: 306})
	if controller.rate != backedOff {
		t.Fatal("rate increased during congestion recovery")
	}
	controller.update(start.Add(1750*time.Millisecond), 495, deliverySample{seen: 482, received: 441})
	if controller.rate <= backedOff || controller.rate > backedOff*1.11 {
		t.Fatal("rate did not resume stepping up cautiously")
	}
}

func TestRateControllerTracksBottleneck(t *testing.T) {
	for _, test := range []struct {
		name                string
		capacity            float64
		minAverage, maxLoss float64
		minPeak             float64
		transient           bool
	}{
		{name: "narrow download", capacity: 4, minAverage: 3.4, maxLoss: 6, minPeak: 3.7},
		{name: "narrow upload", capacity: 20, minAverage: 14, maxLoss: 8, minPeak: 18},
		{name: "upload recovers after a loss spike", capacity: 20, minAverage: 10, maxLoss: 6, minPeak: 18, transient: true},
		{name: "fast link", capacity: 400, minAverage: 200, maxLoss: 1, minPeak: 380},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := time.Now()
			controller := newRateController(400, started)
			var sent, received uint32
			var feedback deliverySample
			var peak float64
			for elapsed := pollInterval; elapsed <= DefaultDuration; elapsed += pollInterval {
				batch := uint32(controller.rate * pollInterval.Seconds())
				delivered := min(batch, uint32(test.capacity*packetsPerMbps*pollInterval.Seconds()))
				if test.transient && elapsed > time.Second && elapsed <= 1500*time.Millisecond {
					delivered = delivered * 2 / 3
				}
				sent, received = sent+batch, received+delivered
				peak = max(peak, mbps(delivered, pollInterval))
				controller.update(started.Add(elapsed), sent, feedback)
				feedback = deliverySample{seen: sent, received: received}
			}
			average := mbps(received, DefaultDuration)
			loss := Transfer{Sent: sent, Received: received}.Loss()
			if average < test.minAverage || loss > test.maxLoss || peak < test.minPeak {
				t.Fatalf("avg %.2f Mbps, peak %.2f Mbps, loss %.2f%%", average, peak, loss)
			}
		})
	}
}

func TestRateControllerBacksOffWithoutFeedback(t *testing.T) {
	start := time.Now()
	controller := newRateController(400, start)
	initial := controller.rate
	controller.update(start.Add(feedbackTimeout), 200, deliverySample{})
	if controller.rate >= initial {
		t.Fatal("send rate stayed high after feedback stopped")
	}
}

func TestRateControllerConfirmsLightLoss(t *testing.T) {
	start := time.Now()
	controller := newRateController(400, start)
	initial := controller.rate
	controller.update(start.Add(pollInterval), 110, deliverySample{seen: 100, received: 92})
	if controller.rate != initial {
		t.Fatal("one light-loss sample should hold the rate")
	}
	controller.update(start.Add(2*pollInterval), 210, deliverySample{seen: 200, received: 184})
	if controller.rate >= initial {
		t.Fatal("persistent light loss did not reduce the rate")
	}
}

func TestRateControllerTamesBufferbloat(t *testing.T) {
	// Simulate a 46 Mbps residential uplink with a deep lossless FIFO modem buffer (0% packet loss,
	// pure queuing delay swell — the exact scenario that used to choke probes past leaseTime=2.5s).
	const (
		capacityMbps = 46.0
		baseRTT      = 48 * time.Millisecond
	)
	started := time.Now()
	controller := newRateControllerWithBaseline(200, started, baseRTT)
	capPps := capacityMbps * packetsPerMbps

	var sent, delivered uint32
	var queued float64
	var maxRTT time.Duration
	for elapsed := pollInterval; elapsed <= DefaultDuration; elapsed += pollInterval {
		batch := uint32(controller.rate * pollInterval.Seconds())
		sent += batch
		queued += float64(batch)
		drained := min(queued, capPps*pollInterval.Seconds())
		queued -= drained
		delivered += uint32(drained)

		queueDelay := time.Duration(queued / capPps * float64(time.Second))
		rtt := baseRTT + queueDelay
		if rtt > maxRTT {
			maxRTT = rtt
		}
		controller.update(started.Add(elapsed), sent, deliverySample{
			seen:     delivered,
			received: delivered,
			rtt:      rtt,
		})
	}
	avgMbps := mbps(delivered, DefaultDuration)
	if maxRTT >= 1200*time.Millisecond {
		t.Fatalf("bufferbloat queue delay grew too high: maxRTT=%v (avg=%.2f Mbps)", maxRTT, avgMbps)
	}
	if avgMbps < 34 || avgMbps > capacityMbps+1 {
		t.Fatalf("unexpected throughput under bufferbloat: avg=%.2f Mbps, maxRTT=%v", avgMbps, maxRTT)
	}
}
