package client

import (
	"net"
	"testing"
)

// ipv4Packet builds a minimal IPv4 header with the given addresses.
func ipv4Packet(src, dst string) []byte {
	p := make([]byte, 20)
	p[0] = 4<<4 | 5 // version 4, header length 5 words
	copy(p[12:16], net.ParseIP(src).To4())
	copy(p[16:20], net.ParseIP(dst).To4())
	return p
}

func TestDestAndSourceIP(t *testing.T) {
	packet := ipv4Packet("100.88.3.7", "100.88.3.9")

	if dst, ok := destIP(packet); !ok || dst != "100.88.3.9" {
		t.Fatalf("destIP = %q, %v; want 100.88.3.9, true", dst, ok)
	}
	if src, ok := sourceIP(packet); !ok || src != "100.88.3.7" {
		t.Fatalf("sourceIP = %q, %v; want 100.88.3.7, true", src, ok)
	}
}

// TestNonIPv4Rejected covers the guards that keep malformed or non-IPv4 frames
// out of the routing path. An IPv6 packet reaching destIP would otherwise be
// parsed as if bytes 16..20 were an address.
func TestNonIPv4Rejected(t *testing.T) {
	cases := map[string][]byte{
		"empty":     {},
		"truncated": make([]byte, 10),
		"ipv6":      append([]byte{6 << 4}, make([]byte, 39)...),
	}
	for name, packet := range cases {
		if _, ok := destIP(packet); ok {
			t.Errorf("%s: destIP accepted a non-IPv4 packet", name)
		}
		if _, ok := sourceIP(packet); ok {
			t.Errorf("%s: sourceIP accepted a non-IPv4 packet", name)
		}
	}
}

func TestIsMulticast(t *testing.T) {
	tests := map[string]bool{
		"224.0.2.60":      true,  // Minecraft LAN discovery
		"224.0.0.1":       true,  // all hosts
		"239.255.255.250": true,  // SSDP, top of the range
		"223.255.255.255": false, // just below
		"240.0.0.1":       false, // just above
		"100.88.3.9":      false,
		"255.255.255.255": false, // broadcast, not multicast
		"not an ip":       false,
	}
	for addr, want := range tests {
		if got := isMulticast(addr); got != want {
			t.Errorf("isMulticast(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestBroadcastOf(t *testing.T) {
	tests := []struct {
		ip, mask, want string
	}{
		{"100.88.3.7", "255.255.255.0", "100.88.3.255"},
		{"100.88.3.7", "255.255.0.0", "100.88.255.255"},
		{"10.0.0.1", "255.0.0.0", "10.255.255.255"},
		{"bad", "255.255.255.0", ""},
	}
	for _, tt := range tests {
		if got := broadcastOf(tt.ip, tt.mask); got != tt.want {
			t.Errorf("broadcastOf(%q, %q) = %q, want %q", tt.ip, tt.mask, got, tt.want)
		}
	}
}

// TestInitiatorRuleIsAsymmetric is the property that keeps handshakes from
// colliding: for any two distinct peers, exactly one of them initiates. If both
// did, each would clobber the other's half-finished handshake state.
func TestInitiatorRuleIsAsymmetric(t *testing.T) {
	lower := &Client{id: mustPeerID(t, "0011223344556677")}
	higher := &Client{id: mustPeerID(t, "00112233445566ff")}

	if !lower.isInitiatorFor(higher.id) {
		t.Error("the lower ID should initiate")
	}
	if higher.isInitiatorFor(lower.id) {
		t.Error("the higher ID should not initiate")
	}
	// A peer must never try to handshake with itself.
	if lower.isInitiatorFor(lower.id) {
		t.Error("a peer should not initiate toward itself")
	}
}

func mustPeerID(t *testing.T, hexStr string) [8]byte {
	t.Helper()
	id, err := parseHexPeerID(hexStr)
	if err != nil {
		t.Fatalf("parse peer ID %q: %v", hexStr, err)
	}
	return id
}
