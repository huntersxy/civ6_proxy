package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// Peer is one member entry in control messages.
type Peer struct {
	ID     uint8  `json:"id"`
	Name   string `json:"name"`
	Host   bool   `json:"host"`
	Online bool   `json:"online"`
}

// HandshakeRequest is the CREATE/JOIN payload.
type HandshakeRequest struct {
	RequestID    string `json:"request_id"`
	Name         string `json:"name"`
	RoomCode     string `json:"room_code,omitempty"`
	ResumeMember uint8  `json:"resume_member,omitempty"`
	ResumeToken  string `json:"resume_token,omitempty"`
}

// WelcomeMessage is the WELCOME payload.
type WelcomeMessage struct {
	RequestID string `json:"request_id"`
	RoomCode  string `json:"room_code"`
	RoomID    string `json:"room_id"`
	MemberID  uint8  `json:"member_id"`
	Host      bool   `json:"host"`
	Token     string `json:"token"`
	Members   []Peer `json:"members"`
}

// PeerListMessage is the PEERS payload.
type PeerListMessage struct {
	Members []Peer `json:"members"`
}

// ErrorMessage is the ERROR payload.
type ErrorMessage struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// RoomClosedMessage is the ROOM_CLOSED payload.
type RoomClosedMessage struct {
	Reason string `json:"reason"`
}

// PingMessage is the PING payload.
type PingMessage struct {
	SentNS int64 `json:"sent_ns"`
}

// EncodeControl serializes a control message as compact, ASCII-only JSON.
//
// The Python reference decodes control payloads as ASCII before parsing JSON,
// so non-ASCII runes must be emitted as \uXXXX escapes.
func EncodeControl(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	raw := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	return escapeNonASCII(raw), nil
}

func escapeNonASCII(in []byte) []byte {
	ascii := true
	for _, c := range in {
		if c >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return in
	}
	out := make([]byte, 0, len(in)+16)
	for i := 0; i < len(in); {
		c := in[i]
		if c < 0x80 {
			out = append(out, c)
			i++
			continue
		}
		r, size := utf8.DecodeRune(in[i:])
		if r == utf8.RuneError && size == 1 {
			out = append(out, c)
			i++
			continue
		}
		if r <= 0xFFFF {
			out = appendU4(out, uint16(r))
		} else {
			r -= 0x10000
			out = appendU4(out, uint16(0xD800+(r>>10)))
			out = appendU4(out, uint16(0xDC00+(r&0x3FF)))
		}
		i += size
	}
	return out
}

func appendU4(dst []byte, v uint16) []byte {
	const hex = "0123456789abcdef"
	return append(dst, '\\', 'u',
		hex[(v>>12)&0xF], hex[(v>>8)&0xF], hex[(v>>4)&0xF], hex[v&0xF])
}

// DecodeControl parses a JSON control message.
func DecodeControl(data []byte, v any) error {
	if len(data) == 0 {
		return errors.New("empty control payload")
	}
	return json.Unmarshal(data, v)
}
