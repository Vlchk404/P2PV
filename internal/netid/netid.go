// Package netid derives network identifiers and keys from a network name and
// password, the way Radmin VPN and Hamachi present it to the user: you type a
// name and a password, and you are in.
//
// Three values come out of those two strings:
//
//	NetworkID  = BLAKE2s(name)                  -- public, routes you to the right network
//	PSK        = Argon2id(password, salt=NetworkID) -- secret, authenticates peer handshakes
//	Verifier   = BLAKE2s(PSK, "verifier")       -- what the server stores
//
// The server only ever sees NetworkID and Verifier. Because Verifier is a
// one-way hash of the PSK, the server can check that a client knows the
// password without being able to derive the PSK itself -- so a compromised or
// hostile relay cannot impersonate a peer or sit in the middle of a handshake.
// It can still drop or delay packets, and it still learns who talks to whom.
package netid

import (
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/blake2s"
)

// Argon2id parameters. Deliberately modest: this runs once at connect time on
// whatever laptop a friend happens to have, and the password is a shared secret
// among friends rather than a password database entry.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
)

// KeyLen is the length of a PSK and of a network ID.
const KeyLen = 32

// Network holds everything derived from a name and password.
type Network struct {
	Name     string
	ID       [KeyLen]byte // BLAKE2s(name)
	PSK      [KeyLen]byte // Argon2id(password, ID)
	Verifier [KeyLen]byte // BLAKE2s(PSK, "verifier")
}

// IDHex returns the network ID as hex, the form used on the wire.
func (n *Network) IDHex() string { return hex.EncodeToString(n.ID[:]) }

// VerifierHex returns the verifier as hex, the form used on the wire.
func (n *Network) VerifierHex() string { return hex.EncodeToString(n.Verifier[:]) }

// Derive computes a Network from a name and password.
//
// The name is normalised (trimmed and lowercased) so that "MyLAN" and "mylan "
// reach the same network -- a friend retyping the name from memory should not
// silently land in an empty one.
func Derive(name, password string) (*Network, error) {
	norm := strings.ToLower(strings.TrimSpace(name))
	if norm == "" {
		return nil, errors.New("netid: network name is empty")
	}
	if password == "" {
		return nil, errors.New("netid: network password is empty")
	}

	n := &Network{Name: norm}

	idHash := blake2s.Sum256([]byte("p2pv-network-id\x00" + norm))
	n.ID = idHash

	psk := argon2.IDKey([]byte(password), n.ID[:], argonTime, argonMemory, argonThreads, argonKeyLen)
	copy(n.PSK[:], psk)

	n.Verifier = verifierOf(n.PSK)
	return n, nil
}

// FromPSK rebuilds a Network from a stored PSK, skipping the Argon2id pass.
// The client saves the PSK rather than the password, so reconnecting to a
// remembered network neither stores the password nor re-runs key derivation.
func FromPSK(name string, id, psk [KeyLen]byte) *Network {
	return &Network{
		Name:     name,
		ID:       id,
		PSK:      psk,
		Verifier: verifierOf(psk),
	}
}

func verifierOf(psk [KeyLen]byte) [KeyLen]byte {
	h, _ := blake2s.New256(nil)
	h.Write([]byte("p2pv-verifier\x00"))
	h.Write(psk[:])
	var out [KeyLen]byte
	copy(out[:], h.Sum(nil))
	return out
}
