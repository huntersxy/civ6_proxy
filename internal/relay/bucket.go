package relay

// tokenBucket is a two-dimensional (packets and bytes) token bucket with a
// fixed burst window, matching the Python server's rate limiter.
type tokenBucket struct {
	packetRate float64
	byteRate   float64
	burst      float64
	packets    float64
	bytes      float64
	updated    float64
}

func newTokenBucket(packetRate, byteRate, burst float64) tokenBucket {
	return tokenBucket{
		packetRate: packetRate,
		byteRate:   byteRate,
		burst:      burst,
		packets:    packetRate * burst,
		bytes:      byteRate * burst,
	}
}

func (b *tokenBucket) allow(size int, now float64) bool {
	if b.updated == 0 {
		b.updated = now
	}
	elapsed := now - b.updated
	if elapsed < 0 {
		elapsed = 0
	}
	b.updated = now
	b.packets = minFloat(b.packetRate*b.burst, b.packets+elapsed*b.packetRate)
	b.bytes = minFloat(b.byteRate*b.burst, b.bytes+elapsed*b.byteRate)
	if b.packets < 1 || b.bytes < float64(size) {
		return false
	}
	b.packets--
	b.bytes -= float64(size)
	return true
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
