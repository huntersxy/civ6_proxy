package speedtest

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"slices"
	"time"
)

var (
	ErrBusy        = errors.New("relay speed test is busy")
	ErrUnavailable = errors.New("relay speed test is disabled")
	ErrNoReply     = errors.New("relay did not answer the speed test handshake")
	ErrInterrupted = errors.New("relay stopped answering during the speed test")
)

type Options struct {
	Duration time.Duration
	MaxMbps  int
}

type Transfer struct {
	CurrentMbps, AverageMbps, PeakMbps float64
	Sent, Received                     uint32
	Final                              bool
}

func (transfer Transfer) Bytes() uint64 {
	return uint64(transfer.Received) * datagramSize
}

func (transfer Transfer) Loss() float64 {
	return 100 * float64(transfer.Sent-min(transfer.Sent, transfer.Received)) / float64(max(1, transfer.Sent))
}

type Latency struct {
	Samples             int
	Median, P95, Jitter time.Duration
}

type Result struct {
	Phase            Phase
	Elapsed          time.Duration
	PhaseDuration    time.Duration
	MaxMbps          int
	Download, Upload Transfer
	Latency          [3]Latency
}

type client struct {
	ctx    context.Context
	conn   *net.UDPConn
	id     token
	serial uint32
	result Result
	update func(Result)
	buffer [datagramSize + 1]byte
}

func Run(ctx context.Context, address string, options Options, update func(Result)) (Result, error) {
	if options.Duration == 0 {
		options.Duration = DefaultDuration
	}
	if options.Duration < time.Second || options.Duration > maxDuration || options.MaxMbps < 0 || options.MaxMbps > MaxMbps {
		return Result{}, errors.New("invalid speed test duration or bitrate")
	}
	options.Duration = options.Duration.Truncate(time.Millisecond)

	ctx, cancel := context.WithTimeout(ctx, 2*options.Duration+14*time.Second)
	defer cancel()
	dialer := net.Dialer{Timeout: 4 * time.Second}
	socket, err := dialer.DialContext(ctx, "udp4", address)
	if err != nil {
		return Result{}, err
	}
	conn := socket.(*net.UDPConn)
	defer conn.Close()
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		close(interrupted)
	})
	_ = conn.SetReadBuffer(4 << 20)
	_ = conn.SetWriteBuffer(1 << 20)
	test := &client{ctx: ctx, conn: conn, update: update}
	defer func() {
		if !stopInterrupt() {
			<-interrupted
		}
		if test.id != (token{}) {
			test.stop()
		}
	}()
	if err := test.connect(options); err != nil {
		return test.result, err
	}
	for _, phase := range []Phase{Baseline, Download, Upload} {
		if err := test.measure(phase, options.Duration); err != nil {
			return test.result, err
		}
	}
	return test.result, nil
}

func (test *client) connect(options Options) error {
	request := message{kind: hello, millis: uint32(options.Duration.Milliseconds()), sent: uint32(options.MaxMbps)}
	if _, err := rand.Read(request.id[:]); err != nil {
		return err
	}
	reply, err := test.exchange(request, challenge)
	if err != nil {
		return err
	}
	request.kind, request.proof = open, reply.proof
	reply, err = test.exchange(request, ready)
	if err != nil {
		return err
	}
	if reply.proof == (token{}) || reply.millis != request.millis || reply.sent == 0 || reply.sent > MaxMbps ||
		(request.sent != 0 && reply.sent > request.sent) {
		return ErrNoReply
	}
	test.id = reply.proof
	test.result.MaxMbps = int(reply.sent)
	return nil
}

func (test *client) exchange(request message, want kind) (message, error) {
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if err := test.write(request); err != nil {
			return message{}, err
		}
		reply, err := test.readUntil(minTime(end, time.Now().Add(pollInterval)), func(in message) bool {
			return in.id == request.id && (in.kind == want || in.kind == busy || in.kind == unavailable)
		})
		if err != nil {
			return message{}, err
		}
		switch reply.kind {
		case want:
			return reply, nil
		case busy:
			return message{}, ErrBusy
		case unavailable:
			return message{}, ErrUnavailable
		}
	}
	return message{}, ErrNoReply
}

type probeSample struct {
	serial uint32
	sentAt time.Time
}

type uploadTrain struct {
	*sendProgress
	started time.Time
	done    chan sendResult
	result  *sendResult
}

func (test *client) measure(phase Phase, duration time.Duration) error {
	if phase == Baseline {
		duration = baselineTime
	}
	test.result.Phase, test.result.Elapsed, test.result.PhaseDuration = phase, 0, duration
	test.publish()
	started := time.Now()
	lastReply, maxEnd := started, started.Add(duration+2*leaseTime)
	var probes [16]probeSample
	var remote message
	var delay latencySamples
	var rate transferSamples
	var downloaded packetTracker
	var downSettledAt time.Time
	var upload *uploadTrain
	if phase == Download {
		downloaded = trackPackets(duration, test.result.MaxMbps)
	}
	ctx, cancel := context.WithCancel(test.ctx)
	defer func() {
		cancel()
		if upload != nil && upload.result == nil {
			_ = test.conn.SetWriteDeadline(time.Now())
			<-upload.done
		}
	}()

	for now := started; now.Before(maxEnd); now = time.Now() {
		if err := test.ctx.Err(); err != nil {
			return err
		}
		request := message{kind: probe, phase: phase, id: test.id}
		switch phase {
		case Download:
			request.sent, request.received = downloaded.seen, downloaded.kept
		case Upload:
			if upload != nil && upload.result == nil {
				select {
				case result := <-upload.done:
					upload.result = &result
				default:
				}
			}
			if upload != nil && upload.result != nil {
				request.final, request.sent = true, upload.result.sent
			}
		}
		test.serial++
		request.serial = test.serial
		probes[request.serial%uint32(len(probes))] = probeSample{request.serial, now}
		if err := test.write(request); err != nil {
			return err
		}
		end := now.Add(pollInterval)
		if phase == Baseline {
			end = minTime(end, started.Add(duration))
		}
		if _, err := test.readUntil(end, func(in message) bool {
			if in.id != test.id || in.phase != phase {
				return false
			}
			switch in.kind {
			case data:
				if phase == Download {
					downloaded.add(in.serial)
				}
			case status:
				stamp := &probes[in.serial%uint32(len(probes))]
				if stamp.serial == in.serial && !stamp.sentAt.IsZero() && in.received <= in.sent &&
					in.sent <= packetLimit(duration, test.result.MaxMbps) {
					lastReply = time.Now()
					if !in.final && !request.final {
						delay.add(lastReply.Sub(stamp.sentAt))
					}
					stamp.sentAt = time.Time{}
					if in.serial > remote.serial {
						remote = in
						if phase == Upload {
							if upload == nil {
								upload = test.startUpload(ctx, duration)
							}
							upload.setFeedback(in.sent, in.received)
						}
					}
				}
			}
			return false
		}); err != nil {
			return err
		}

		now = time.Now()
		test.result.Elapsed = min(duration, now.Sub(started))
		test.result.Latency[phase] = delay.snapshot()
		switch phase {
		case Baseline:
			if now.Sub(started) >= duration {
				test.publish()
				return nil
			}
		case Download:
			if remote.final && downSettledAt.IsZero() {
				downSettledAt = now.Add(settleTime)
			}
			if !downSettledAt.IsZero() && !now.Before(downSettledAt) {
				test.result.Download = rate.finish(max(remote.sent, downloaded.seen), downloaded.kept, phaseDuration(remote.millis))
				test.publish()
				return nil
			}
			test.result.Download = rate.sample(downloaded.seen, downloaded.kept, now.Sub(started))
		case Upload:
			test.result.Elapsed = 0
			if upload != nil {
				test.result.Elapsed = min(duration, now.Sub(upload.started))
				if upload.result != nil && remote.final && remote.sent == upload.result.sent {
					test.result.Upload = rate.finish(remote.sent, remote.received, upload.result.elapsed)
					test.publish()
					return nil
				}
			}
			test.result.Upload = rate.sample(remote.sent, remote.received, phaseDuration(remote.millis))
		}
		test.publish()
		if now.Sub(lastReply) >= leaseTime {
			return ErrInterrupted
		}
	}
	return ErrInterrupted
}

func (test *client) startUpload(ctx context.Context, duration time.Duration) *uploadTrain {
	upload := &uploadTrain{sendProgress: &sendProgress{}, started: time.Now(), done: make(chan sendResult, 1)}
	_ = test.conn.SetWriteDeadline(upload.started.Add(duration + leaseTime))
	limit := test.result.MaxMbps
	go func() {
		upload.done <- sendTrain(ctx, test.id, Upload, duration, limit, upload.sendProgress,
			func() bool { return true }, func(packet []byte) error {
				_, err := test.conn.Write(packet)
				return err
			})
	}()
	return upload
}

func (test *client) readUntil(end time.Time, consume func(message) bool) (message, error) {
	_ = test.conn.SetReadDeadline(end)
	for test.ctx.Err() == nil {
		size, err := test.conn.Read(test.buffer[:])
		if err != nil {
			if test.ctx.Err() != nil {
				return message{}, test.ctx.Err()
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return message{}, nil
			}
			return message{}, err
		}
		if in, ok := decode(test.buffer[:size]); ok && consume(in) {
			return in, nil
		}
	}
	return message{}, test.ctx.Err()
}

func (test *client) write(msg message) error {
	var packet [controlSize]byte
	msg.encode(packet[:])
	_, err := test.conn.Write(packet[:])
	if test.ctx.Err() != nil {
		return test.ctx.Err()
	}
	return err
}

func (test *client) stop() {
	_ = test.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	for attempt := 0; attempt < 3; attempt++ {
		_ = test.write(message{kind: stop, id: test.id})
	}
}

func (test *client) publish() {
	if test.update != nil {
		test.update(test.result)
	}
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}

type latencySamples struct {
	rtts      []time.Duration
	jitterSum time.Duration
}

func (delay *latencySamples) add(rtt time.Duration) {
	if size := len(delay.rtts); size > 0 {
		diff := rtt - delay.rtts[size-1]
		delay.jitterSum += max(diff, -diff)
	}
	delay.rtts = append(delay.rtts, rtt)
}

func (delay *latencySamples) snapshot() Latency {
	size := len(delay.rtts)
	if size == 0 {
		return Latency{}
	}
	sorted := slices.Clone(delay.rtts)
	slices.Sort(sorted)
	median := (sorted[(size-1)/2] + sorted[size/2]) / 2
	return Latency{Samples: size, Median: median, P95: sorted[(size*95-1)/100],
		Jitter: delay.jitterSum / time.Duration(max(1, size-1))}
}

type transferSamples struct {
	result       Transfer
	lastElapsed  time.Duration
	lastReceived uint32
}

func (rate *transferSamples) sample(sent, received uint32, elapsed time.Duration) Transfer {
	rate.result.Sent, rate.result.Received = sent, received
	rate.result.AverageMbps = mbps(received, elapsed)
	if span := elapsed - rate.lastElapsed; span >= feedbackWindow && received >= rate.lastReceived {
		rate.result.CurrentMbps = mbps(received-rate.lastReceived, span)
		rate.result.PeakMbps = max(rate.result.PeakMbps, rate.result.CurrentMbps)
		rate.lastElapsed, rate.lastReceived = elapsed, received
	}
	return rate.result
}

func (rate *transferSamples) finish(sent, received uint32, elapsed time.Duration) Transfer {
	transfer := rate.sample(sent, received, elapsed)
	transfer.Final = true
	transfer.PeakMbps = max(transfer.PeakMbps, transfer.AverageMbps)
	return transfer
}

func mbps(packets uint32, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(packets) * (8 * datagramSize / 1e6) / elapsed.Seconds()
}
