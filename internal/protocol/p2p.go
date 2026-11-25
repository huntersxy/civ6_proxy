package protocol

import (
	"net/netip"
	"time"
)

const (
	P2PLease     = 8 * time.Second
	P2PProofSize = 16
	P2PKeySize   = 32
)

type P2PMode uint8

const (
	P2PPoll P2PMode = iota
	P2POff
	P2POn
)

type P2PRequest struct {
	Mode  P2PMode `json:"mode,omitempty"`
	Proof []byte  `json:"proof,omitempty"`
}

type P2PState struct {
	Request   uint64         `json:"request"`
	Enabled   bool           `json:"enabled"`
	Challenge []byte         `json:"challenge,omitempty"`
	Peers     []P2PCandidate `json:"peers,omitempty"`
}

type P2PCandidate struct {
	MemberID uint8          `json:"id"`
	Endpoint netip.AddrPort `json:"endpoint"`
	Key      []byte         `json:"key"`
}
