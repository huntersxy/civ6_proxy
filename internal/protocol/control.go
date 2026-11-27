package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
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
	MaxMembers   int    `json:"max_members,omitempty"`
	ResumeMember uint8  `json:"resume_member,omitempty"`
	ResumeToken  string `json:"resume_token,omitempty"`
}

// WelcomeMessage is the WELCOME payload.
type WelcomeMessage struct {
	RequestID  string `json:"request_id"`
	RoomCode   string `json:"room_code"`
	RoomID     string `json:"room_id"`
	MemberID   uint8  `json:"member_id"`
	Host       bool   `json:"host"`
	MaxMembers int    `json:"max_members,omitempty"`
	Token      string `json:"token"`
	Members    []Peer `json:"members"`
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

// EncodeControl serializes a control message as compact UTF-8 JSON.
func EncodeControl(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// DecodeControl parses a JSON control message.
func DecodeControl(data []byte, v any) error {
	if len(data) == 0 {
		return errors.New("empty control payload")
	}
	return json.Unmarshal(data, v)
}
