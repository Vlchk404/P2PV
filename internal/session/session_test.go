package session

import (
	"bytes"
	"testing"

	"github.com/Vlchk404/p2pv/internal/netid"
)

func testNetwork(t *testing.T) *netid.Network {
	t.Helper()
	n, err := netid.Derive("test-lan", "correct horse")
	if err != nil {
		t.Fatalf("derive network: %v", err)
	}
	return n
}

// completeHandshake runs a full IKpsk2 exchange and returns both sessions.
func completeHandshake(t *testing.T, n *netid.Network) (initiator, responder *Session) {
	t.Helper()

	alice, err := NewIdentity()
	if err != nil {
		t.Fatalf("alice identity: %v", err)
	}
	bob, err := NewIdentity()
	if err != nil {
		t.Fatalf("bob identity: %v", err)
	}

	hs, msg1, err := StartInitiator(alice, n.ID, n.PSK, bob.Public)
	if err != nil {
		t.Fatalf("start initiator: %v", err)
	}

	bobSess, gotAlicePub, msg2, err := Respond(bob, n.ID, n.PSK, msg1)
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	if gotAlicePub != alice.Public {
		t.Fatal("responder learned the wrong static key for the initiator")
	}

	aliceSess, err := hs.Finish(msg2)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return aliceSess, bobSess
}

// TestHandshakeDirection pins down which CipherState encrypts and which
// decrypts on each side. Split() returns them in a fixed order, so the roles
// are mirrored between initiator and responder -- getting that backwards would
// produce a channel that handshakes cleanly and then fails to carry any packet.
func TestHandshakeDirection(t *testing.T) {
	n := testNetwork(t)
	alice, bob := completeHandshake(t, n)

	toBob := []byte("minecraft handshake packet")
	counter, ct, err := alice.Seal(toBob)
	if err != nil {
		t.Fatalf("alice seal: %v", err)
	}
	got, err := bob.Open(counter, ct)
	if err != nil {
		t.Fatalf("bob open: %v", err)
	}
	if !bytes.Equal(got, toBob) {
		t.Fatalf("alice -> bob: got %q, want %q", got, toBob)
	}

	toAlice := []byte("server list ping response")
	counter, ct, err = bob.Seal(toAlice)
	if err != nil {
		t.Fatalf("bob seal: %v", err)
	}
	got, err = alice.Open(counter, ct)
	if err != nil {
		t.Fatalf("alice open: %v", err)
	}
	if !bytes.Equal(got, toAlice) {
		t.Fatalf("bob -> alice: got %q, want %q", got, toAlice)
	}
}

// TestWrongPassword confirms the psk actually authenticates: the same static
// keys with a different network password must not produce a channel.
func TestWrongPassword(t *testing.T) {
	right := testNetwork(t)
	wrong, err := netid.Derive("test-lan", "wrong password")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}

	alice, _ := NewIdentity()
	bob, _ := NewIdentity()

	hs, msg1, err := StartInitiator(alice, right.ID, right.PSK, bob.Public)
	if err != nil {
		t.Fatalf("start initiator: %v", err)
	}

	// The responder reads msg1 before the psk is mixed in, so the mismatch
	// surfaces on the initiator's read of msg2 at the latest.
	bobSess, _, msg2, err := Respond(bob, wrong.ID, wrong.PSK, msg1)
	if err != nil {
		return // rejected already, which is fine
	}
	if _, err := hs.Finish(msg2); err == nil {
		t.Fatal("handshake completed across mismatched network passwords")
	}
	_ = bobSess
}

// TestWrongNetworkName confirms the prologue binding: a handshake for one
// network must not complete against another, even with the same password.
func TestWrongNetworkName(t *testing.T) {
	a, _ := netid.Derive("lan-one", "shared pass")
	b, _ := netid.Derive("lan-two", "shared pass")

	alice, _ := NewIdentity()
	bob, _ := NewIdentity()

	hs, msg1, err := StartInitiator(alice, a.ID, a.PSK, bob.Public)
	if err != nil {
		t.Fatalf("start initiator: %v", err)
	}
	_, _, msg2, err := Respond(bob, b.ID, b.PSK, msg1)
	if err != nil {
		return
	}
	if _, err := hs.Finish(msg2); err == nil {
		t.Fatal("handshake completed across different networks")
	}
}

// TestReplayWindow covers the three cases the window exists for: reordered but
// fresh packets are accepted, exact replays are rejected, and counters that
// have fallen out of the window are rejected.
func TestReplayWindow(t *testing.T) {
	n := testNetwork(t)
	alice, bob := completeHandshake(t, n)

	type packet struct {
		counter uint64
		ct      []byte
	}
	var packets []packet
	for i := 0; i < 200; i++ {
		c, ct, err := alice.Seal([]byte{byte(i)})
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
		packets = append(packets, packet{c, ct})
	}

	// Out of order but inside the window: accepted.
	for _, i := range []int{5, 3, 9, 0, 7} {
		if _, err := bob.Open(packets[i].counter, packets[i].ct); err != nil {
			t.Fatalf("reordered packet %d rejected: %v", i, err)
		}
	}

	// Exact replay: rejected.
	if _, err := bob.Open(packets[3].counter, packets[3].ct); err == nil {
		t.Fatal("replayed packet accepted")
	}

	// Jump far ahead, then try a counter that has aged out of the window.
	if _, err := bob.Open(packets[199].counter, packets[199].ct); err != nil {
		t.Fatalf("packet 199 rejected: %v", err)
	}
	if _, err := bob.Open(packets[20].counter, packets[20].ct); err == nil {
		t.Fatal("packet outside the replay window accepted")
	}
}

// TestForgedPacketDoesNotAdvanceWindow is the reason the window is updated only
// after the AEAD verifies. If a forgery could move the window, one spoofed
// packet with a high counter would lock out every real packet behind it.
func TestForgedPacketDoesNotAdvanceWindow(t *testing.T) {
	n := testNetwork(t)
	alice, bob := completeHandshake(t, n)

	counter, ct, err := alice.Seal([]byte("real packet"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	forged := make([]byte, len(ct))
	copy(forged, ct)
	forged[0] ^= 0xff
	if _, err := bob.Open(1_000_000, forged); err == nil {
		t.Fatal("forged packet decrypted")
	}

	if _, err := bob.Open(counter, ct); err != nil {
		t.Fatalf("real packet rejected after a forgery: %v", err)
	}
}

// TestIdentityFromPrivate covers reloading a saved identity: the public key and
// peer ID must come back identical, or peers would not recognise the device
// across restarts.
func TestIdentityFromPrivate(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}
	again, err := IdentityFromPrivate(id.Private)
	if err != nil {
		t.Fatalf("from private: %v", err)
	}
	if again.Public != id.Public {
		t.Fatal("public key changed across reload")
	}
	if again.PeerID() != id.PeerID() {
		t.Fatal("peer ID changed across reload")
	}
}
