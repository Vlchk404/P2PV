// Package proto defines the P2PV wire protocol.
//
// Everything travels over a single UDP port, both on the server and on each
// client. The first byte of every datagram is the message type, which tells
// the reader how to parse the rest.
//
// Two planes share that port:
//
//   - The control plane (client <-> server): JSON after the type byte. Low
//     volume, so readability beats compactness here.
//   - The data plane (client <-> client): binary, either sent directly to the
//     peer or wrapped in TypeRelay and bounced off the server. The server can
//     never read the inner payload; it only rewrites the addressing fields.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
)

// Message types. The first byte of every datagram.
const (
	// Control plane, client -> server.
	TypeRegister  = 0x01 // join a network, get a virtual IP and the peer list
	TypeKeepalive = 0x02 // hold the NAT binding open, refresh last-seen
	TypeBye       = 0x03 // leave cleanly instead of waiting for the timeout
	TypePunchReq  = 0x04 // ask the server to tell a peer to punch back at us

	// Control plane, server -> client.
	TypeRegistered = 0x11 // virtual IP + current peer list
	TypePeerList   = 0x12 // a peer joined, left, or changed endpoint
	TypePunchInv   = 0x13 // a peer wants a direct path; punch at these candidates
	TypeError      = 0x14 // registration refused, with a reason

	// Data plane, peer <-> peer (direct).
	TypePunch     = 0x21 // hole-punching probe
	TypePunchAck  = 0x22 // "I received your probe on this path"
	TypeHandshake = 0x23 // Noise IKpsk2 handshake message
	TypeTransport = 0x24 // encrypted IP packet

	// Data plane, via server.
	TypeRelay = 0x31 // wraps any of the 0x2x messages for forwarding
)

// Handshake payloads carry a subtype in their first byte, so a receiver knows
// which half of the two-message IK exchange it is looking at without having to
// infer it from the length.
const (
	HandshakeInit = 0x01 // initiator -> responder
	HandshakeResp = 0x02 // responder -> initiator
)

// MTU is the virtual interface MTU.
//
// The budget, worst case (a relayed transport packet over IPv4):
//
//	1500 physical - 20 IPv4 - 8 UDP = 1472 available
//	1472 - 17 relay header - 1 type - 8 peer ID - 8 counter - 16 tag = 1422
//
// 1400 leaves room for a PPPoE or tunnelled uplink without fragmenting.
const MTU = 1400

// PeerIDLen is the length of a peer ID: the first 8 bytes of
// BLAKE2s-256(static public key). Short enough to keep the per-packet header
// small, long enough that a collision inside one small network is not a
// practical concern.
const PeerIDLen = 8

// PeerID identifies a device by its static key.
type PeerID [PeerIDLen]byte

func (p PeerID) String() string { return fmt.Sprintf("%x", p[:]) }

// IsZero reports whether the ID is unset.
func (p PeerID) IsZero() bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

// ParsePeerID decodes an ID from the wire.
func ParsePeerID(b []byte) (PeerID, error) {
	var id PeerID
	if len(b) < PeerIDLen {
		return id, errors.New("proto: peer ID too short")
	}
	copy(id[:], b[:PeerIDLen])
	return id, nil
}

// --- Control plane payloads (JSON) ---

// Register asks the server to join a network.
//
// Password is never sent. The client derives a pre-shared key from it locally
// and sends only Verifier, a one-way hash of that key, so the server can check
// membership without ever learning the key that protects peer traffic.
type Register struct {
	NetworkID  string   `json:"nid"`      // BLAKE2s(name), hex
	Verifier   string   `json:"verifier"` // BLAKE2s(psk, "verifier"), hex
	StaticPub  string   `json:"pub"`      // X25519 public key, base64
	Hostname   string   `json:"host"`     // display name in the peer list
	LocalAddrs []string `json:"local"`    // LAN candidates for same-network peers
}

// Registered is the server's answer to a successful Register.
type Registered struct {
	VirtualIP string `json:"vip"`   // assigned address, e.g. 100.88.3.7
	Netmask   string `json:"mask"`  // /24 of the network
	PeerID    string `json:"id"`    // the client's own ID, hex
	Peers     []Peer `json:"peers"` // everyone else currently online
}

// Peer describes another member of the network.
type Peer struct {
	PeerID     string   `json:"id"`
	Hostname   string   `json:"host"`
	StaticPub  string   `json:"pub"`
	VirtualIP  string   `json:"vip"`
	Endpoint   string   `json:"ep"`    // public UDP address as the server sees it
	LocalAddrs []string `json:"local"` // LAN candidates
	Online     bool     `json:"online"`
}

// PeerList is pushed whenever the membership or an endpoint changes.
type PeerList struct {
	Peers []Peer `json:"peers"`
}

// PunchReq asks the server to invite a peer to punch back.
type PunchReq struct {
	Target string `json:"target"` // peer ID, hex
}

// PunchInv tells a client that Peer wants a direct path.
type PunchInv struct {
	Peer Peer `json:"peer"`
}

// Error explains a refusal.
type Error struct {
	Reason string `json:"reason"`
}

// EncodeControl builds a control-plane datagram.
func EncodeControl(msgType byte, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(body))
	out = append(out, msgType)
	return append(out, body...), nil
}

// DecodeControl parses a control-plane datagram body into v.
func DecodeControl(datagram []byte, v any) error {
	if len(datagram) < 1 {
		return errors.New("proto: empty datagram")
	}
	return json.Unmarshal(datagram[1:], v)
}

// --- Data plane ---

// Direct peer messages are framed as:
//
//	[0]      type (0x2x)
//	[1:9]    sender peer ID
//	[9:]     payload
const directHeaderLen = 1 + PeerIDLen

// EncodeDirect frames a peer-to-peer message.
func EncodeDirect(msgType byte, from PeerID, payload []byte) []byte {
	out := make([]byte, directHeaderLen+len(payload))
	out[0] = msgType
	copy(out[1:], from[:])
	copy(out[directHeaderLen:], payload)
	return out
}

// DecodeDirect splits a peer-to-peer message into sender and payload.
// The returned payload aliases datagram; copy it if you need to retain it.
func DecodeDirect(datagram []byte) (from PeerID, payload []byte, err error) {
	if len(datagram) < directHeaderLen {
		return from, nil, errors.New("proto: direct message too short")
	}
	copy(from[:], datagram[1:directHeaderLen])
	return from, datagram[directHeaderLen:], nil
}

// Relayed messages are framed as:
//
//	[0]      TypeRelay
//	[1:9]    destination peer ID
//	[9:17]   source peer ID (the server overwrites this from its session table)
//	[17:]    an inner direct message, including its own type byte
const relayHeaderLen = 1 + 2*PeerIDLen

// EncodeRelay wraps inner (a full direct message) for forwarding via the server.
func EncodeRelay(dst, src PeerID, inner []byte) []byte {
	out := make([]byte, relayHeaderLen+len(inner))
	out[0] = TypeRelay
	copy(out[1:], dst[:])
	copy(out[1+PeerIDLen:], src[:])
	copy(out[relayHeaderLen:], inner)
	return out
}

// DecodeRelay splits a relay envelope. The returned inner slice aliases datagram.
func DecodeRelay(datagram []byte) (dst, src PeerID, inner []byte, err error) {
	if len(datagram) < relayHeaderLen {
		return dst, src, nil, errors.New("proto: relay message too short")
	}
	copy(dst[:], datagram[1:1+PeerIDLen])
	copy(src[:], datagram[1+PeerIDLen:relayHeaderLen])
	return dst, src, datagram[relayHeaderLen:], nil
}

// SetRelaySource overwrites the source field in place. The server calls this so
// a client cannot forge who a relayed packet came from.
func SetRelaySource(datagram []byte, src PeerID) {
	if len(datagram) >= relayHeaderLen {
		copy(datagram[1+PeerIDLen:relayHeaderLen], src[:])
	}
}

// Transport payloads are framed as:
//
//	[0:8]    counter (big endian), the AEAD nonce and replay sequence
//	[8:]     ciphertext, including the 16-byte Poly1305 tag
const counterLen = 8

// EncodeTransport frames an encrypted packet.
func EncodeTransport(counter uint64, ciphertext []byte) []byte {
	out := make([]byte, counterLen+len(ciphertext))
	binary.BigEndian.PutUint64(out[:counterLen], counter)
	copy(out[counterLen:], ciphertext)
	return out
}

// DecodeTransport splits a transport payload. The returned ciphertext aliases payload.
func DecodeTransport(payload []byte) (counter uint64, ciphertext []byte, err error) {
	if len(payload) < counterLen {
		return 0, nil, errors.New("proto: transport payload too short")
	}
	return binary.BigEndian.Uint64(payload[:counterLen]), payload[counterLen:], nil
}

// ParseUDPAddr is a tolerant net.UDPAddr parser for addresses carried as strings.
func ParseUDPAddr(s string) *net.UDPAddr {
	if s == "" {
		return nil
	}
	addr, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		return nil
	}
	return addr
}
