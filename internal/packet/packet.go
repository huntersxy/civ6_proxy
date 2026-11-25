// Package packet builds and parses the minimal IPv4/UDP packets needed to
// inject relayed game traffic into the local Windows network stack.
package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

const (
	ipv4HeaderLen = 20
	udpHeaderLen  = 8
)

// Datagram is a parsed IPv4/UDP packet.
type Datagram struct {
	SourceIP   net.IP
	TargetIP   net.IP
	SourcePort uint16
	TargetPort uint16
	Payload    []byte
}

// InternetChecksum computes the standard 16-bit one's complement checksum.
func InternetChecksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(data[0])<<8 | uint32(data[1])
		data = data[2:]
	}
	if len(data) == 1 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// BuildIPv4UDP synthesizes a complete IPv4/UDP packet with checksums.
func BuildIPv4UDP(sourceIP, targetIP net.IP, sourcePort, targetPort uint16, payload []byte, ttl uint8, identification uint16) ([]byte, error) {
	src := sourceIP.To4()
	dst := targetIP.To4()
	if src == nil || dst == nil {
		return nil, errors.New("IPv4 address required")
	}
	if len(payload) > 65507 {
		return nil, errors.New("UDP payload too large")
	}
	totalLen := ipv4HeaderLen + udpHeaderLen + len(payload)
	out := make([]byte, totalLen)
	h := out[:ipv4HeaderLen]
	h[0] = 0x45
	h[1] = 0
	binary.BigEndian.PutUint16(h[2:4], uint16(totalLen))
	binary.BigEndian.PutUint16(h[4:6], identification)
	binary.BigEndian.PutUint16(h[6:8], 0)
	h[8] = ttl
	h[9] = 17 // UDP
	binary.BigEndian.PutUint16(h[10:12], 0)
	copy(h[12:16], src)
	copy(h[16:20], dst)
	binary.BigEndian.PutUint16(h[10:12], InternetChecksum(h))

	u := out[ipv4HeaderLen : ipv4HeaderLen+udpHeaderLen]
	udpLen := udpHeaderLen + len(payload)
	binary.BigEndian.PutUint16(u[0:2], sourcePort)
	binary.BigEndian.PutUint16(u[2:4], targetPort)
	binary.BigEndian.PutUint16(u[4:6], uint16(udpLen))
	binary.BigEndian.PutUint16(u[6:8], 0)
	copy(out[ipv4HeaderLen+udpHeaderLen:], payload)

	// Pseudo header: source, destination, zero, protocol, UDP length.
	var pseudo [12]byte
	copy(pseudo[0:4], src)
	copy(pseudo[4:8], dst)
	pseudo[9] = 17
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(udpLen))
	checksum := InternetChecksum(append(pseudo[:], out[ipv4HeaderLen:]...))
	if checksum == 0 {
		checksum = 0xffff
	}
	binary.BigEndian.PutUint16(u[6:8], checksum)
	return out, nil
}

// ParseIPv4UDP extracts the UDP view of an IPv4 packet.
func ParseIPv4UDP(raw []byte) (Datagram, error) {
	var d Datagram
	if len(raw) < ipv4HeaderLen+udpHeaderLen {
		return d, errors.New("packet is too short")
	}
	if raw[0]>>4 != 4 {
		return d, errors.New("not IPv4")
	}
	ihl := int(raw[0]&0x0f) * 4
	if ihl < ipv4HeaderLen || len(raw) < ihl+udpHeaderLen {
		return d, errors.New("invalid IPv4 header")
	}
	totalLen := int(binary.BigEndian.Uint16(raw[2:4]))
	if totalLen < ihl+udpHeaderLen || totalLen > len(raw) {
		return d, errors.New("invalid IPv4 total length")
	}
	if raw[9] != 17 {
		return d, errors.New("not UDP")
	}
	udpLen := int(binary.BigEndian.Uint16(raw[ihl+4 : ihl+6]))
	if udpLen < udpHeaderLen || ihl+udpLen > totalLen {
		return d, errors.New("invalid UDP length")
	}
	d.SourceIP = net.IPv4(raw[12], raw[13], raw[14], raw[15])
	d.TargetIP = net.IPv4(raw[16], raw[17], raw[18], raw[19])
	d.SourcePort = binary.BigEndian.Uint16(raw[ihl : ihl+2])
	d.TargetPort = binary.BigEndian.Uint16(raw[ihl+2 : ihl+4])
	d.Payload = raw[ihl+udpHeaderLen : ihl+udpLen]
	return d, nil
}

// String formats a datagram for logs.
func (d Datagram) String() string {
	return fmt.Sprintf("%s:%d->%s:%d %dB", d.SourceIP, d.SourcePort, d.TargetIP, d.TargetPort, len(d.Payload))
}
