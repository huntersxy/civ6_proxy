package speedtest

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"sync/atomic"
	"time"
)

const (
	paceInterval    = 4 * time.Millisecond
	feedbackWindow  = 500 * time.Millisecond
	feedbackTimeout = 1500 * time.Millisecond
	congestionHold  = time.Second
	congestionFade  = time.Second
	packetsPerMbps  = 1e6 / (8 * datagramSize)
)

type sendProgress struct {
	sent     atomic.Uint32
	feedback atomic.Uint64
}

func (progress *sendProgress) setFeedback(seen, received uint32) {
	if received > seen || seen > progress.sent.Load() {
		return
	}
	sample := uint64(seen)<<32 | uint64(received)
	if sample > progress.feedback.Load() {
		progress.feedback.Store(sample)
	}
}

type sendResult struct {
	sent    uint32
	elapsed time.Duration
}

type deliverySample struct {
	seen, received uint32
}

type rateController struct {
	rate, minRate, maxRate float64
	lastCongestion         time.Time
	lastDelivery           time.Time
	lastSeen, epochStart   uint32
	anchor                 deliverySample
	lossPending            bool
}

func newRateController(mbps int, now time.Time) rateController {
	maxRate := float64(mbps) * packetsPerMbps
	return rateController{
		rate: min(maxRate, 2*packetsPerMbps), minRate: min(maxRate, 0.25*packetsPerMbps),
		maxRate: maxRate, lastDelivery: now,
	}
}

func (controller *rateController) update(now time.Time, sent uint32, sample deliverySample) {
	if sample.seen > controller.lastSeen {
		controller.lastSeen, controller.lastDelivery = sample.seen, now
	}
	if sent > controller.lastSeen && now.Sub(controller.lastDelivery) >= feedbackTimeout {
		controller.reduce(now, sent, 0.65)
		controller.lastDelivery = now
		return
	}
	if sample.seen <= controller.anchor.seen || sample.seen < controller.epochStart || sample.received < controller.anchor.received {
		return
	}
	if controller.anchor.seen < controller.epochStart {
		controller.anchor = sample
		return
	}
	expected := sample.seen - controller.anchor.seen
	if expected < 16 {
		return
	}
	received := min(expected, sample.received-controller.anchor.received)
	controller.anchor = sample
	delivery := float64(received) / float64(expected)
	previousLoss := controller.lossPending
	controller.lossPending = expected-received >= 2 && delivery < 0.97
	switch {
	case controller.lossPending && (previousLoss || delivery <= 0.82):
		controller.reduce(now, sent, max(0.25, min(0.95, 0.97*delivery)))
	case delivery >= 0.98 && now.Sub(controller.lastCongestion) >= congestionHold:
		growth := 1.5
		if !controller.lastCongestion.IsZero() {
			recovery := now.Sub(controller.lastCongestion) - congestionHold
			growth = 1.10 + 0.15*min(1, float64(recovery)/float64(congestionFade))
		}
		controller.changeRate(controller.rate*growth, sent)
	}
}

func (controller *rateController) reduce(now time.Time, sent uint32, factor float64) {
	controller.lastCongestion = now
	controller.lossPending = false
	controller.changeRate(controller.rate*factor, sent)
}

func (controller *rateController) changeRate(rate float64, sent uint32) {
	rate = max(controller.minRate, min(controller.maxRate, rate))
	if rate != controller.rate {
		controller.rate, controller.epochStart = rate, sent
	}
}

func sendTrain(ctx context.Context, id token, phase Phase, duration time.Duration, mbps int,
	progress *sendProgress, leaseValid func() bool, write func([]byte) error) sendResult {
	var packet [datagramSize]byte
	message{kind: data, phase: phase, id: id}.encode(packet[:])
	_, _ = rand.Read(packet[headerSize:])

	ticker := time.NewTicker(paceInterval)
	defer ticker.Stop()
	started := time.Now()
	end := started.Add(duration)
	lastTick := started
	var sent uint32
	var budget float64
	limit := packetLimit(duration, mbps)
	controller := newRateController(mbps, started)
	running := func(now time.Time) bool {
		return now.Before(end) && ctx.Err() == nil && leaseValid()
	}
	for now := started; running(now); now = time.Now() {
		previousRate := controller.rate
		feedback := progress.feedback.Load()
		controller.update(now, sent, deliverySample{seen: uint32(feedback >> 32), received: uint32(feedback)})
		rate := controller.rate
		if rate != previousRate {
			lastTick = now.Add(-min(now.Sub(lastTick), paceInterval))
		}
		budget = min(budget+now.Sub(lastTick).Seconds()*rate, max(2, rate*(2*paceInterval).Seconds()))
		lastTick = now
		for batch := 0; budget >= 1 && sent < limit; batch++ {
			if batch%32 == 0 && !running(time.Now()) {
				break
			}
			binary.BigEndian.PutUint32(packet[24:28], sent)
			if err := write(packet[:]); err != nil {
				budget = 0
				break
			}
			budget--
			sent++
		}
		progress.sent.Store(sent)
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return sendResult{sent: sent, elapsed: min(duration, time.Since(started))}
}
