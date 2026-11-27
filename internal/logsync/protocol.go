// Package logsync implements real-time streaming log collection from room
// members and the local host into structured per-session host directories.
package logsync

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"
)

const (
	Magic        = "C6LG"
	ProtoVersion = 1

	MsgCtrl     uint8 = 1
	MsgHello    uint8 = 2
	MsgHelloAck uint8 = 3
	MsgChunk    uint8 = 4
	MsgAck      uint8 = 5

	// Chunk flags.
	ChunkFlagReset    uint16 = 1 << 0
	ChunkFlagSnapshot uint16 = 1 << 1
	ChunkFlagEOF      uint16 = 1 << 2

	headerBaseLen  = 6  // 4 magic + 1 version + 1 type
	chunkHeaderLen = 38 // 6 base + 8 nonce + 8 senderID + 4 seq + 2 flags + 8 offset + 2 pathLen
	ackFrameLen    = 38 // 6 base + 8 nonce + 8 senderID + 4 cumAck + 8 sackMask + 4 window

	// TextChunkSize (~4 KiB) deflates into a single UDP relay frame (~600-1100 B).
	TextChunkSize = 4096
	// BinaryChunkSize keeps incompressible binary files (e.g. PCAP) in a single UDP frame.
	BinaryChunkSize = 1024
	MaxChunkData    = 8192

	MaxFileSizeBytes   = 64 << 20  // 64 MiB safety cap per file
	MaxMemberBytes     = 256 << 20 // 256 MiB safety cap per member per session
	MaxRelPathLen      = 240
	DefaultMaxSessions = 30
	DefaultRetention   = 14 * 24 * time.Hour
)

var (
	ErrInvalidFrame = errors.New("invalid logsync frame")
	ErrInvalidPath  = errors.New("invalid logsync relative path")
)

// CtrlMessage is sent by the host to start, keep alive, or stop streaming.
type CtrlMessage struct {
	Enabled   bool   `json:"enabled"`
	SessionID string `json:"session_id,omitempty"`
	Nonce     uint64 `json:"nonce"`
}

// HelloMessage is sent by a guest upon accepting a CtrlMessage(enabled=true).
type HelloMessage struct {
	Nonce      uint64 `json:"nonce"`
	SenderID   uint64 `json:"sender_id"`
	Computer   string `json:"computer,omitempty"`
	User       string `json:"user,omitempty"`
	MemberName string `json:"member_name,omitempty"`
	Protocol   uint8  `json:"protocol"`
}

// HelloAckMessage is sent by the host to confirm the guest's session directory is ready.
type HelloAckMessage struct {
	Nonce    uint64 `json:"nonce"`
	SenderID uint64 `json:"sender_id"`
}

// ChunkMessage carries one ordered slice of a collected file.
type ChunkMessage struct {
	Nonce    uint64
	SenderID uint64
	Seq      uint32
	Flags    uint16
	Offset   uint64
	RelPath  string
	Data     []byte
}

// AckMessage acknowledges contiguous chunks plus a 64-slot selective ACK mask.
type AckMessage struct {
	Nonce    uint64
	SenderID uint64
	CumAck   uint32
	SackMask uint64
	Window   uint32
}

// PeekMessageType checks the magic/version prefix and returns the message type.
func PeekMessageType(payload []byte) (uint8, bool) {
	if len(payload) < headerBaseLen {
		return 0, false
	}
	if string(payload[0:4]) != Magic || payload[4] != ProtoVersion {
		return 0, false
	}
	return payload[5], true
}

func encodeJSONMsg(msgType uint8, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := make([]byte, headerBaseLen+len(body))
	copy(out[0:4], Magic)
	out[4] = ProtoVersion
	out[5] = msgType
	copy(out[headerBaseLen:], body)
	return out, nil
}

func decodeJSONMsg(payload []byte, expectedType uint8, v any) error {
	mt, ok := PeekMessageType(payload)
	if !ok || mt != expectedType || len(payload) <= headerBaseLen {
		return ErrInvalidFrame
	}
	return json.Unmarshal(payload[headerBaseLen:], v)
}

func EncodeCtrl(msg CtrlMessage) ([]byte, error)       { return encodeJSONMsg(MsgCtrl, msg) }
func DecodeCtrl(payload []byte) (CtrlMessage, error) {
	var msg CtrlMessage
	err := decodeJSONMsg(payload, MsgCtrl, &msg)
	return msg, err
}

func EncodeHello(msg HelloMessage) ([]byte, error)     { return encodeJSONMsg(MsgHello, msg) }
func DecodeHello(payload []byte) (HelloMessage, error) {
	var msg HelloMessage
	err := decodeJSONMsg(payload, MsgHello, &msg)
	return msg, err
}

func EncodeHelloAck(msg HelloAckMessage) ([]byte, error) { return encodeJSONMsg(MsgHelloAck, msg) }
func DecodeHelloAck(payload []byte) (HelloAckMessage, error) {
	var msg HelloAckMessage
	err := decodeJSONMsg(payload, MsgHelloAck, &msg)
	return msg, err
}

// EncodeChunk serializes a ChunkMessage in compact binary format.
func EncodeChunk(msg ChunkMessage) ([]byte, error) {
	rel, err := ValidateRelPath(msg.RelPath)
	if err != nil {
		return nil, err
	}
	if len(msg.Data) > MaxChunkData {
		return nil, ErrInvalidFrame
	}
	out := make([]byte, chunkHeaderLen+len(rel)+len(msg.Data))
	copy(out[0:4], Magic)
	out[4] = ProtoVersion
	out[5] = MsgChunk
	binary.BigEndian.PutUint64(out[6:14], msg.Nonce)
	binary.BigEndian.PutUint64(out[14:22], msg.SenderID)
	binary.BigEndian.PutUint32(out[22:26], msg.Seq)
	binary.BigEndian.PutUint16(out[26:28], msg.Flags)
	binary.BigEndian.PutUint64(out[28:36], msg.Offset)
	binary.BigEndian.PutUint16(out[36:38], uint16(len(rel)))
	copy(out[38:38+len(rel)], rel)
	copy(out[38+len(rel):], msg.Data)
	return out, nil
}

// DecodeChunk parses a binary ChunkMessage and validates its relative path.
func DecodeChunk(payload []byte) (ChunkMessage, error) {
	var msg ChunkMessage
	mt, ok := PeekMessageType(payload)
	if !ok || mt != MsgChunk || len(payload) < chunkHeaderLen {
		return msg, ErrInvalidFrame
	}
	pathLen := int(binary.BigEndian.Uint16(payload[36:38]))
	if pathLen == 0 || pathLen > MaxRelPathLen || chunkHeaderLen+pathLen > len(payload) {
		return msg, ErrInvalidFrame
	}
	dataLen := len(payload) - (chunkHeaderLen + pathLen)
	if dataLen > MaxChunkData {
		return msg, ErrInvalidFrame
	}
	rel, err := ValidateRelPath(string(payload[38 : 38+pathLen]))
	if err != nil {
		return msg, err
	}
	msg = ChunkMessage{
		Nonce:    binary.BigEndian.Uint64(payload[6:14]),
		SenderID: binary.BigEndian.Uint64(payload[14:22]),
		Seq:      binary.BigEndian.Uint32(payload[22:26]),
		Flags:    binary.BigEndian.Uint16(payload[26:28]),
		Offset:   binary.BigEndian.Uint64(payload[28:36]),
		RelPath:  rel,
		Data:     append([]byte(nil), payload[38+pathLen:]...),
	}
	if msg.Flags&ChunkFlagReset != 0 && msg.Offset != 0 {
		return ChunkMessage{}, ErrInvalidFrame
	}
	return msg, nil
}

// EncodeAck serializes a compact 38-byte AckMessage.
func EncodeAck(msg AckMessage) []byte {
	var out [ackFrameLen]byte
	copy(out[0:4], Magic)
	out[4] = ProtoVersion
	out[5] = MsgAck
	binary.BigEndian.PutUint64(out[6:14], msg.Nonce)
	binary.BigEndian.PutUint64(out[14:22], msg.SenderID)
	binary.BigEndian.PutUint32(out[22:26], msg.CumAck)
	binary.BigEndian.PutUint64(out[26:34], msg.SackMask)
	binary.BigEndian.PutUint32(out[34:38], msg.Window)
	return out[:]
}

// DecodeAck parses a 38-byte AckMessage.
func DecodeAck(payload []byte) (AckMessage, error) {
	var msg AckMessage
	mt, ok := PeekMessageType(payload)
	if !ok || mt != MsgAck || len(payload) != ackFrameLen {
		return msg, ErrInvalidFrame
	}
	msg = AckMessage{
		Nonce:    binary.BigEndian.Uint64(payload[6:14]),
		SenderID: binary.BigEndian.Uint64(payload[14:22]),
		CumAck:   binary.BigEndian.Uint32(payload[22:26]),
		SackMask: binary.BigEndian.Uint64(payload[26:34]),
		Window:   binary.BigEndian.Uint32(payload[34:38]),
	}
	return msg, nil
}

// ValidateRelPath verifies that rel matches one of the strict allowed
// collection destinations ("env_check.txt" or "<allowed_subdir>/<safe_file>").
func ValidateRelPath(rel string) (string, error) {
	if rel == "" || len(rel) > MaxRelPathLen || strings.ContainsRune(rel, '\\') {
		return "", ErrInvalidPath
	}
	if rel == "env_check.txt" {
		return rel, nil
	}
	dir, file, ok := strings.Cut(rel, "/")
	if !ok || strings.ContainsRune(file, '/') {
		return "", ErrInvalidPath
	}
	switch dir {
	case "civ6_logs", "relay_logs", "relay_captures":
	default:
		return "", ErrInvalidPath
	}
	if !isSafeFileName(file) {
		return "", ErrInvalidPath
	}
	cleaned := path.Clean(rel)
	if cleaned != rel {
		return "", ErrInvalidPath
	}
	return rel, nil
}

func isSafeFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.HasPrefix(name, " ") || strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return false
		}
	}
	return !isWindowsReservedName(name)
}

func isWindowsReservedName(name string) bool {
	stem := name
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	switch strings.ToUpper(strings.TrimSpace(stem)) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

// SanitizeDirComponent produces a clean, readable, cross-platform directory
// component safe for Windows and Unix paths.
func SanitizeDirComponent(s, fallback string) string {
	var b strings.Builder
	lastUnderscore := false
	runes := 0
	for _, r := range strings.TrimSpace(s) {
		if runes >= 24 {
			break
		}
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) || unicode.IsSpace(r) {
			if !lastUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				lastUnderscore = true
				runes++
			}
			continue
		}
		b.WriteRune(r)
		lastUnderscore = (r == '_')
		runes++
	}
	out := strings.Trim(b.String(), "._ ")
	if out == "" {
		out = fallback
	}
	if isWindowsReservedName(out) {
		out = "_" + out
	}
	return out
}

// RandomUint64 returns a non-zero cryptographically random uint64.
func RandomUint64() uint64 {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic("crypto/rand failure: " + err.Error())
		}
		if v := binary.BigEndian.Uint64(b[:]); v != 0 {
			return v
		}
	}
}

// FormatByteSize returns a compact human-readable byte count.
func FormatByteSize(n uint64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024.0)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.2f MB", float64(n)/(1024.0*1024.0))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1024.0*1024.0*1024.0))
	}
}
