package proto

import (
	"bytes"
	"testing"
)

func TestDirectRoundTrip(t *testing.T) {
	from := PeerID{1, 2, 3, 4, 5, 6, 7, 8}
	payload := []byte("encrypted packet")

	msg := EncodeDirect(TypeTransport, from, payload)
	if msg[0] != TypeTransport {
		t.Fatalf("type byte = 0x%02x, want 0x%02x", msg[0], TypeTransport)
	}

	gotFrom, gotPayload, err := DecodeDirect(msg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotFrom != from {
		t.Fatalf("from = %v, want %v", gotFrom, from)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("payload = %q, want %q", gotPayload, payload)
	}
}

func TestRelayRoundTrip(t *testing.T) {
	dst := PeerID{9, 9, 9, 9, 9, 9, 9, 9}
	src := PeerID{1, 1, 1, 1, 1, 1, 1, 1}
	inner := EncodeDirect(TypeTransport, src, []byte("payload"))

	envelope := EncodeRelay(dst, src, inner)
	gotDst, gotSrc, gotInner, err := DecodeRelay(envelope)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotDst != dst || gotSrc != src {
		t.Fatalf("dst/src = %v/%v, want %v/%v", gotDst, gotSrc, dst, src)
	}
	if !bytes.Equal(gotInner, inner) {
		t.Fatal("inner message did not survive the round trip")
	}
}

// TestSetRelaySource covers the server's anti-spoofing rewrite: whatever source
// a client claims, the server replaces it with the one from its session table.
func TestSetRelaySource(t *testing.T) {
	dst := PeerID{9}
	claimed := PeerID{0xaa}
	actual := PeerID{0xbb}
	inner := []byte{TypeTransport, 1, 2, 3}

	envelope := EncodeRelay(dst, claimed, inner)
	SetRelaySource(envelope, actual)

	gotDst, gotSrc, gotInner, err := DecodeRelay(envelope)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotSrc != actual {
		t.Fatalf("source = %v, want the server's value %v", gotSrc, actual)
	}
	if gotDst != dst {
		t.Fatal("rewriting the source disturbed the destination")
	}
	if !bytes.Equal(gotInner, inner) {
		t.Fatal("rewriting the source disturbed the payload")
	}
}

// TestSetRelaySourceShortDatagram makes sure the rewrite cannot panic on a
// runt packet -- it runs on unvalidated input straight off the network.
func TestSetRelaySourceShortDatagram(t *testing.T) {
	for n := 0; n < relayHeaderLen; n++ {
		SetRelaySource(make([]byte, n), PeerID{1})
	}
}

func TestTransportRoundTrip(t *testing.T) {
	const counter = uint64(1 << 40)
	ciphertext := []byte("aead output")

	gotCounter, gotCT, err := DecodeTransport(EncodeTransport(counter, ciphertext))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotCounter != counter {
		t.Fatalf("counter = %d, want %d", gotCounter, counter)
	}
	if !bytes.Equal(gotCT, ciphertext) {
		t.Fatalf("ciphertext = %q, want %q", gotCT, ciphertext)
	}
}

// TestShortMessagesRejected checks every decoder against truncated input.
func TestShortMessagesRejected(t *testing.T) {
	if _, _, err := DecodeDirect(make([]byte, directHeaderLen-1)); err == nil {
		t.Error("DecodeDirect accepted a short message")
	}
	if _, _, _, err := DecodeRelay(make([]byte, relayHeaderLen-1)); err == nil {
		t.Error("DecodeRelay accepted a short message")
	}
	if _, _, err := DecodeTransport(make([]byte, counterLen-1)); err == nil {
		t.Error("DecodeTransport accepted a short payload")
	}
	if _, err := ParsePeerID(make([]byte, PeerIDLen-1)); err == nil {
		t.Error("ParsePeerID accepted a short ID")
	}
}

// TestMTUFitsRelayedPacket is the size budget that keeps P2PV from fragmenting.
// A full-MTU packet, encrypted and wrapped for the relay, must still fit in a
// 1500-byte Ethernet frame after the outer IPv4 and UDP headers.
func TestMTUFitsRelayedPacket(t *testing.T) {
	const (
		aeadTag   = 16
		outerIPv4 = 20
		outerUDP  = 8
		ethernet  = 1500
	)

	transport := EncodeTransport(0, make([]byte, MTU+aeadTag))
	direct := EncodeDirect(TypeTransport, PeerID{}, transport)
	relayed := EncodeRelay(PeerID{}, PeerID{}, direct)

	total := outerIPv4 + outerUDP + len(relayed)
	if total > ethernet {
		t.Fatalf("a relayed full-MTU packet needs %d bytes on the wire, over the %d-byte frame", total, ethernet)
	}
	t.Logf("relayed full-MTU packet: %d bytes on the wire, %d to spare", total, ethernet-total)
}
