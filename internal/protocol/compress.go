package protocol

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
)

const (
	// MinCompressSize skips compression on tiny control/ACK datagrams where
	// deflate framing rarely saves bytes.
	MinCompressSize = 64
)

var (
	// ErrDecompressedTooLarge is returned when a compressed DATA payload expands
	// beyond MaxUDPPayload.
	ErrDecompressedTooLarge = errors.New("decompressed payload is too large")
)

// Compressor reuses a raw-deflate (BestSpeed) writer across frames.
// Calls must be serialized.
type Compressor struct {
	writer *flate.Writer
	buf    bytes.Buffer
}

// NewCompressor returns a reusable BestSpeed payload compressor.
func NewCompressor() *Compressor {
	c := &Compressor{}
	w, err := flate.NewWriter(&c.buf, flate.BestSpeed)
	if err != nil {
		panic("flate.NewWriter failed: " + err.Error())
	}
	c.writer = w
	return c
}

// Compress attempts to deflate payload. It returns the compressed slice and
// true only when compression strictly shrinks payload; otherwise it returns
// payload unchanged and false. The returned slice is backed by Compressor's
// internal buffer and remains valid until the next Compress call.
func (c *Compressor) Compress(payload []byte) ([]byte, bool) {
	if len(payload) < MinCompressSize {
		return payload, false
	}
	c.buf.Reset()
	c.writer.Reset(&c.buf)
	if _, err := c.writer.Write(payload); err != nil {
		return payload, false
	}
	if err := c.writer.Close(); err != nil {
		return payload, false
	}
	if c.buf.Len() == 0 || c.buf.Len() >= len(payload) {
		return payload, false
	}
	return c.buf.Bytes(), true
}

// Decompressor reuses a raw-deflate reader across frames.
// Calls must be serialized.
type Decompressor struct {
	reader io.ReadCloser
	src    bytes.Reader
	buf    bytes.Buffer
}

// NewDecompressor returns a reusable payload decompressor.
func NewDecompressor() *Decompressor {
	return &Decompressor{}
}

// Decompress inflates data into a newly owned slice, enforcing MaxUDPPayload.
func (d *Decompressor) Decompress(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	d.src.Reset(data)
	if d.reader == nil {
		d.reader = flate.NewReader(&d.src)
	} else if err := d.reader.(flate.Resetter).Reset(&d.src, nil); err != nil {
		return nil, err
	}
	d.buf.Reset()
	limited := io.LimitReader(d.reader, MaxUDPPayload+1)
	if _, err := d.buf.ReadFrom(limited); err != nil {
		return nil, err
	}
	if d.buf.Len() > MaxUDPPayload {
		return nil, ErrDecompressedTooLarge
	}
	if err := d.reader.Close(); err != nil {
		return nil, err
	}
	return bytes.Clone(d.buf.Bytes()), nil
}
