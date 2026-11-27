// Package protocol implements the Civ6Relay v3 UDP wire format.
package protocol

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Magic              = "C6R3"
	Version            = 3
	HeaderSize         = 40
	AuthTagSize        = 16
	MaxOuterDatagram   = 1280 // 1224-byte payload; Civ 6's common 1180-byte datagrams fit one frame.
	MaxUDPPayload      = 65507
	MaxFragmentPayload = MaxOuterDatagram - HeaderSize - AuthTagSize
	MaxFragmentCount   = (MaxUDPPayload + MaxFragmentPayload - 1) / MaxFragmentPayload
	ReassemblyTimeout  = 2 * time.Second

	FirstMemberID   uint8 = 2
	LastMemberID    uint8 = 254
	BroadcastMember uint8 = 255
	MaxRoomMembers        = int(LastMemberID - FirstMemberID + 1)
)

// Kind is a frame kind.
type Kind uint8

const (
	KindCreate     Kind = 1
	KindJoin       Kind = 2
	KindWelcome    Kind = 3
	KindPeers      Kind = 4
	KindPing       Kind = 5
	KindPong       Kind = 6
	KindLeave      Kind = 7
	KindData       Kind = 8
	KindError      Kind = 9
	KindRoomClosed Kind = 10
	KindP2PRequest Kind = 11
	KindP2PState   Kind = 12
	KindPeerProbe  Kind = 13
	KindPeerAck    Kind = 14
	KindPathPing   Kind = 15
	KindPathPong   Kind = 16
)

func (k Kind) String() string {
	switch k {
	case KindCreate:
		return "CREATE"
	case KindJoin:
		return "JOIN"
	case KindWelcome:
		return "WELCOME"
	case KindPeers:
		return "PEERS"
	case KindPing:
		return "PING"
	case KindPong:
		return "PONG"
	case KindLeave:
		return "LEAVE"
	case KindData:
		return "DATA"
	case KindError:
		return "ERROR"
	case KindRoomClosed:
		return "ROOM_CLOSED"
	case KindP2PRequest:
		return "P2P_REQUEST"
	case KindP2PState:
		return "P2P_STATE"
	case KindPeerProbe:
		return "PEER_PROBE"
	case KindPeerAck:
		return "PEER_ACK"
	case KindPathPing:
		return "PATH_PING"
	case KindPathPong:
		return "PATH_PONG"
	default:
		return fmt.Sprintf("KIND(%d)", uint8(k))
	}
}

// Flags is the frame flag bitset.
type Flags uint16

const (
	FlagAuthenticated Flags = 1 << 0
	FlagBroadcast     Flags = 1 << 1
	FlagFragmented    Flags = 1 << 2
	FlagCompressed    Flags = 1 << 3
)

// Errors returned by Decode.
var (
	ErrTooShort      = errors.New("datagram is too short")
	ErrTooLarge      = errors.New("datagram is too large")
	ErrMagic         = errors.New("invalid magic")
	ErrVersion       = errors.New("unsupported protocol version")
	ErrKind          = errors.New("invalid frame kind")
	ErrPayloadLength = errors.New("payload length mismatch")
	ErrFragments     = errors.New("invalid fragment fields")
	ErrPayloadSize   = errors.New("frame payload is too large")
	errShortKey      = errors.New("authentication key is too short")
)

// Authenticator reuses the HMAC state for a single key. Calls must be serialized.
type Authenticator struct {
	mac    hash.Hash
	digest [sha256.Size]byte
}

func NewAuthenticator(key []byte) *Authenticator {
	return &Authenticator{mac: hmac.New(sha256.New, key)}
}

func (a *Authenticator) tag(data []byte) []byte {
	a.mac.Reset()
	_, _ = a.mac.Write(data)
	return a.mac.Sum(a.digest[:0])[:AuthTagSize]
}

// Verify checks the tag of an authenticated datagram as received on the wire.
func (a *Authenticator) Verify(datagram []byte) bool {
	if len(datagram) < HeaderSize+AuthTagSize || len(datagram) > MaxOuterDatagram ||
		Flags(binary.BigEndian.Uint16(datagram[6:8]))&FlagAuthenticated == 0 {
		return false
	}
	tagOffset := len(datagram) - AuthTagSize
	return hmac.Equal(a.tag(datagram[:tagOffset]), datagram[tagOffset:])
}

// Frame is one relay frame.
type Frame struct {
	Kind          Kind
	Flags         Flags
	RoomID        uint64
	SourceMember  uint8
	TargetMember  uint8
	FragmentIndex uint16
	FragmentCount uint16
	SourcePort    uint16
	TargetPort    uint16
	Sequence      uint64
	PacketID      uint32
	Payload       []byte
	AuthTag       [AuthTagSize]byte
}

func putHeader(dst []byte, f *Frame, flags Flags) {
	copy(dst[0:4], Magic)
	dst[4] = Version
	dst[5] = byte(f.Kind)
	binary.BigEndian.PutUint16(dst[6:8], uint16(flags))
	binary.BigEndian.PutUint64(dst[8:16], f.RoomID)
	dst[16] = f.SourceMember
	dst[17] = f.TargetMember
	binary.BigEndian.PutUint16(dst[18:20], f.FragmentIndex)
	binary.BigEndian.PutUint16(dst[20:22], f.FragmentCount)
	binary.BigEndian.PutUint16(dst[22:24], f.SourcePort)
	binary.BigEndian.PutUint16(dst[24:26], f.TargetPort)
	binary.BigEndian.PutUint16(dst[26:28], uint16(len(f.Payload)))
	binary.BigEndian.PutUint64(dst[28:36], f.Sequence)
	binary.BigEndian.PutUint32(dst[36:40], f.PacketID)
}

// Encode serializes the frame. A nil key produces an unauthenticated frame;
// a non-nil key sets the authenticated flag and appends a 16-byte HMAC tag.
func (f *Frame) Encode(key []byte) ([]byte, error) {
	var authenticator *Authenticator
	if key != nil {
		if len(key) < 16 {
			return nil, errShortKey
		}
		authenticator = NewAuthenticator(key)
	}
	return f.AppendEncode(nil, authenticator)
}

// AppendEncode appends a frame to dst, using spare capacity when available.
func (f *Frame) AppendEncode(dst []byte, authenticator *Authenticator) ([]byte, error) {
	if len(f.Payload) > MaxFragmentPayload {
		return nil, ErrPayloadSize
	}
	count := f.FragmentCount
	if count == 0 {
		count = 1
	}
	if count > MaxFragmentCount || f.FragmentIndex >= count {
		return nil, ErrFragments
	}
	flags := f.Flags
	if authenticator == nil {
		flags &^= FlagAuthenticated
	} else {
		flags |= FlagAuthenticated
	}
	size := HeaderSize + len(f.Payload) + AuthTagSize
	offset := len(dst)
	dst = slices.Grow(dst, size)[:offset+size]
	out := dst[offset:]
	f.FragmentCount = count
	putHeader(out[:HeaderSize], f, flags)
	copy(out[HeaderSize:], f.Payload)
	tagOffset := size - AuthTagSize
	if authenticator != nil {
		copy(out[tagOffset:], authenticator.tag(out[:tagOffset]))
	} else {
		clear(out[tagOffset:])
	}
	return dst, nil
}

// Decode parses a datagram and copies the payload.
func Decode(datagram []byte) (Frame, error) {
	f, err := DecodeView(datagram)
	if err == nil {
		f.Payload = slices.Clone(f.Payload)
	}
	return f, err
}

// DecodeView parses a datagram whose payload remains backed by datagram.
func DecodeView(datagram []byte) (Frame, error) {
	var f Frame
	if len(datagram) < HeaderSize+AuthTagSize {
		return f, ErrTooShort
	}
	if len(datagram) > MaxOuterDatagram {
		return f, ErrTooLarge
	}
	if string(datagram[0:4]) != Magic {
		return f, ErrMagic
	}
	if datagram[4] != Version {
		return f, ErrVersion
	}
	kind := Kind(datagram[5])
	if kind < KindCreate || kind > KindPathPong {
		return f, ErrKind
	}
	flags := Flags(binary.BigEndian.Uint16(datagram[6:8]))
	payloadLen := int(binary.BigEndian.Uint16(datagram[26:28]))
	if len(datagram) != HeaderSize+payloadLen+AuthTagSize {
		return f, ErrPayloadLength
	}
	count := binary.BigEndian.Uint16(datagram[20:22])
	index := binary.BigEndian.Uint16(datagram[18:20])
	if count < 1 || count > MaxFragmentCount || index >= count {
		return f, ErrFragments
	}
	f = Frame{
		Kind:          kind,
		Flags:         flags,
		RoomID:        binary.BigEndian.Uint64(datagram[8:16]),
		SourceMember:  datagram[16],
		TargetMember:  datagram[17],
		FragmentIndex: index,
		FragmentCount: count,
		SourcePort:    binary.BigEndian.Uint16(datagram[22:24]),
		TargetPort:    binary.BigEndian.Uint16(datagram[24:26]),
		Sequence:      binary.BigEndian.Uint64(datagram[28:36]),
		PacketID:      binary.BigEndian.Uint32(datagram[36:40]),
		Payload:       datagram[HeaderSize : HeaderSize+payloadLen : HeaderSize+payloadLen],
	}
	copy(f.AuthTag[:], datagram[HeaderSize+payloadLen:])
	return f, nil
}

// Verified reports whether the frame carries a valid HMAC for key.
func (f *Frame) Verified(key []byte) bool {
	if f.Flags&FlagAuthenticated == 0 {
		return false
	}
	var header [HeaderSize]byte
	putHeader(header[:], f, f.Flags)
	mac := hmac.New(sha256.New, key)
	mac.Write(header[:])
	mac.Write(f.Payload)
	sum := mac.Sum(nil)
	return hmac.Equal(sum[:AuthTagSize], f.AuthTag[:])
}

// WithSequence returns a copy with a new sequence and optionally a new target.
func (f *Frame) WithSequence(sequence uint64, targetMember uint8) Frame {
	out := *f
	out.Sequence = sequence
	out.TargetMember = targetMember
	return out
}

// SplitPayload splits a game datagram into relay-sized chunks.
func SplitPayload(payload []byte) ([][]byte, error) {
	if len(payload) > MaxUDPPayload {
		return nil, errors.New("UDP payload is too large")
	}
	if len(payload) == 0 {
		return [][]byte{{}}, nil
	}
	var chunks [][]byte
	for offset := 0; offset < len(payload); offset += MaxFragmentPayload {
		end := offset + MaxFragmentPayload
		if end > len(payload) {
			end = len(payload)
		}
		chunks = append(chunks, payload[offset:end])
	}
	return chunks, nil
}

// RandomBytes returns n cryptographically random bytes.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failure: " + err.Error())
	}
	return b
}

// RandomHex returns n random bytes hex-encoded (2n characters).
func RandomHex(n int) string {
	const digits = "0123456789abcdef"
	b := RandomBytes(n)
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
	return string(out)
}

// ValidRequestID reports whether s is 8..64 hexadecimal characters.
func ValidRequestID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// NormalizeRequestID validates and lower-cases a request ID.
func NormalizeRequestID(s string) (string, error) {
	if !ValidRequestID(s) {
		return "", errors.New("request_id is required")
	}
	return strings.ToLower(s), nil
}

// NormalizeName trims and validates a display name (1..24 printable runes).
func NormalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is required")
	}
	if utf8.RuneCountInString(name) > 24 {
		return "", errors.New("name must contain 1 to 24 printable characters")
	}
	for _, r := range name {
		if r < 32 {
			return "", errors.New("name must contain 1 to 24 printable characters")
		}
	}
	return name, nil
}

// RoomCodeAlphabet is the server's room code alphabet (Crockford-like, no I/L/O/U).
const RoomCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NormalizeRoomCode removes dashes, trims, upper-cases and validates a code.
func NormalizeRoomCode(code string) (string, error) {
	code = strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(code, "-", "")))
	if len(code) != 10 {
		return "", errors.New("invalid room code")
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(RoomCodeAlphabet, rune(code[i])) {
			return "", errors.New("invalid room code")
		}
	}
	return code, nil
}
