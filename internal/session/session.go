// Package session establishes and carries the encrypted channel between two
// peers. The server is never a party to it: even when it relays every packet,
// it holds no key and sees only ciphertext.
//
// The handshake is Noise IKpsk2 (X25519, ChaCha20-Poly1305, BLAKE2s), from the
// flynn/noise implementation of the Noise Protocol Framework. IK fits this
// design exactly:
//
//	I -- the initiator sends its own static key, encrypted
//	K -- the responder's static key is already known, from the server's peer list
//
// so the channel comes up in one round trip. The psk2 variant mixes in the
// network pre-shared key, which the server cannot derive (see package netid).
// That means a peer must both hold a key listed in the network and know the
// network password. Each handshake uses fresh ephemeral keys, so recorded
// traffic stays unreadable even if a static key leaks later.
package session

import (
	"crypto/rand"
	"errors"
	"sync"

	"github.com/flynn/noise"
	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/curve25519"

	"github.com/Vlchk404/p2pv/internal/proto"
)

var cipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

// KeyLen is the length of an X25519 key.
const KeyLen = 32

// Identity is a device's long-term X25519 key pair.
type Identity struct {
	Private [KeyLen]byte
	Public  [KeyLen]byte
}

// NewIdentity generates a fresh identity.
func NewIdentity() (*Identity, error) {
	kp, err := cipherSuite.GenerateKeypair(rand.Reader)
	if err != nil {
		return nil, err
	}
	id := &Identity{}
	copy(id.Private[:], kp.Private)
	copy(id.Public[:], kp.Public)
	return id, nil
}

// IdentityFromPrivate rebuilds an identity from a stored private key.
func IdentityFromPrivate(priv [KeyLen]byte) (*Identity, error) {
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	id := &Identity{Private: priv}
	copy(id.Public[:], pub)
	return id, nil
}

// PeerID returns the identity's peer ID: the first 8 bytes of BLAKE2s(public key).
func (id *Identity) PeerID() proto.PeerID { return PeerIDOf(id.Public) }

// PeerIDOf derives a peer ID from a static public key.
func PeerIDOf(pub [KeyLen]byte) proto.PeerID {
	h := blake2s.Sum256(pub[:])
	var out proto.PeerID
	copy(out[:], h[:proto.PeerIDLen])
	return out
}

func (id *Identity) keypair() noise.DHKey {
	return noise.DHKey{Private: id.Private[:], Public: id.Public[:]}
}

// Handshake drives one in-progress Noise IKpsk2 exchange.
type Handshake struct {
	hs        *noise.HandshakeState
	initiator bool
}

func newConfig(id *Identity, netID [32]byte, initiator bool, peerStatic []byte) noise.Config {
	return noise.Config{
		CipherSuite:   cipherSuite,
		Random:        rand.Reader,
		Pattern:       noise.HandshakeIK,
		Initiator:     initiator,
		StaticKeypair: id.keypair(),
		PeerStatic:    peerStatic,
		// The network ID is bound into the transcript as a prologue, so a
		// handshake cannot be replayed into a different network.
		Prologue:              netID[:],
		PresharedKey:          nil, // set below, per the psk2 placement
		PresharedKeyPlacement: 2,
	}
}

// StartInitiator begins a handshake toward a peer whose static key is known.
// The returned bytes are the first Noise message, to be sent as TypeHandshake.
func StartInitiator(id *Identity, netID, psk [32]byte, peerStatic [KeyLen]byte) (*Handshake, []byte, error) {
	cfg := newConfig(id, netID, true, peerStatic[:])
	cfg.PresharedKey = psk[:]
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return nil, nil, err
	}
	msg, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	if cs1 != nil || cs2 != nil {
		return nil, nil, errors.New("session: IK completed on the first message")
	}
	return &Handshake{hs: hs, initiator: true}, msg, nil
}

// Respond consumes an initiator's first message and produces the reply.
// The handshake completes here for the responder, yielding a live Session.
func Respond(id *Identity, netID, psk [32]byte, msg []byte) (*Session, [KeyLen]byte, []byte, error) {
	var peerStatic [KeyLen]byte

	cfg := newConfig(id, netID, false, nil)
	cfg.PresharedKey = psk[:]
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return nil, peerStatic, nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, msg); err != nil {
		return nil, peerStatic, nil, err
	}

	reply, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, peerStatic, nil, err
	}
	if cs1 == nil || cs2 == nil {
		return nil, peerStatic, nil, errors.New("session: responder handshake did not complete")
	}

	ps := hs.PeerStatic()
	if len(ps) != KeyLen {
		return nil, peerStatic, nil, errors.New("session: peer static key missing")
	}
	copy(peerStatic[:], ps)

	// Split() returns the cipher states in a fixed order, so the roles are
	// mirrored: what the initiator sends with, the responder receives with.
	// TestHandshakeDirection pins this down.
	return &Session{send: cs2, recv: cs1}, peerStatic, reply, nil
}

// Finish consumes the responder's reply and completes the initiator's side.
func (h *Handshake) Finish(msg []byte) (*Session, error) {
	if !h.initiator {
		return nil, errors.New("session: Finish is for the initiator")
	}
	_, cs1, cs2, err := h.hs.ReadMessage(nil, msg)
	if err != nil {
		return nil, err
	}
	if cs1 == nil || cs2 == nil {
		return nil, errors.New("session: initiator handshake did not complete")
	}
	return &Session{send: cs1, recv: cs2}, nil
}

// PeerStatic returns the peer's static key once the handshake has read it.
func (h *Handshake) PeerStatic() ([KeyLen]byte, error) {
	var out [KeyLen]byte
	ps := h.hs.PeerStatic()
	if len(ps) != KeyLen {
		return out, errors.New("session: peer static key not yet known")
	}
	copy(out[:], ps)
	return out, nil
}

// replayWindowSize is the number of packets behind the highest counter that are
// still accepted, to tolerate UDP reordering without accepting replays.
const replayWindowSize = 64

// Session is a live encrypted channel to one peer.
//
// Both the interface reader and the socket reader touch a session, so every
// method is safe for concurrent use.
type Session struct {
	mu   sync.Mutex
	send *noise.CipherState
	recv *noise.CipherState

	counter uint64 // next nonce to send

	highest  uint64 // highest counter accepted so far
	seen     uint64 // bitmap of the window below highest
	anyRecvd bool
}

// Seal encrypts one packet and returns its counter and ciphertext.
func (s *Session) Seal(plaintext []byte) (uint64, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	counter := s.counter
	s.send.SetNonce(counter)
	ct, err := s.send.Encrypt(nil, nil, plaintext)
	if err != nil {
		return 0, nil, err
	}
	s.counter++
	return counter, ct, nil
}

// Open decrypts one packet, rejecting replays and stale counters.
//
// UDP gives no ordering, so the nonce travels with the packet and the window is
// only advanced after the AEAD tag verifies -- otherwise a forged packet with a
// high counter could push the window forward and lock out real traffic.
func (s *Session) Open(counter uint64, ciphertext []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkReplay(counter); err != nil {
		return nil, err
	}

	s.recv.SetNonce(counter)
	pt, err := s.recv.Decrypt(nil, nil, ciphertext)
	if err != nil {
		return nil, err
	}

	s.acceptCounter(counter)
	return pt, nil
}

func (s *Session) checkReplay(counter uint64) error {
	if !s.anyRecvd {
		return nil
	}
	if counter > s.highest {
		return nil
	}
	behind := s.highest - counter
	if behind >= replayWindowSize {
		return errors.New("session: counter outside replay window")
	}
	if s.seen&(1<<behind) != 0 {
		return errors.New("session: replayed counter")
	}
	return nil
}

func (s *Session) acceptCounter(counter uint64) {
	if !s.anyRecvd {
		s.anyRecvd = true
		s.highest = counter
		s.seen = 1
		return
	}
	if counter > s.highest {
		shift := counter - s.highest
		if shift >= replayWindowSize {
			s.seen = 0
		} else {
			s.seen <<= shift
		}
		s.seen |= 1
		s.highest = counter
		return
	}
	s.seen |= 1 << (s.highest - counter)
}
