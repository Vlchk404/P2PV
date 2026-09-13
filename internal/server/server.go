// Package server is the P2PV coordinator and relay.
//
// It does three jobs on one UDP port:
//
//  1. Rendezvous. Clients register with a network ID and a verifier; the server
//     assigns each device a stable virtual IP and tells everyone who else is
//     online, including the public endpoint it observes for them. Observing
//     that endpoint is the STUN-like part: a client behind NAT cannot know its
//     own public address, but the server sees it on every packet.
//
//  2. Punch brokering. When a client wants a direct path to a peer, the server
//     nudges both sides to send probes at the same time, which is what opens
//     the NAT bindings.
//
//  3. Relay fallback. When punching fails -- symmetric NAT, CGNAT, hostile
//     mobile networks -- clients wrap their already-encrypted packets in a
//     relay envelope and the server forwards them. It rewrites the source field
//     from its own session table so a client cannot forge who a packet is from,
//     and it cannot read the payload: the keys live only with the peers.
//
// The server holds no key material for any network. It stores a verifier, a
// one-way hash of the network pre-shared key, so it can check membership
// without being able to decrypt or impersonate anything.
package server

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Vlchk404/p2pv/internal/proto"
	"github.com/Vlchk404/p2pv/internal/session"
)

const (
	// peerTimeout is how long a client may go silent before it is considered
	// offline. Clients keepalive every 25s, so this tolerates two losses.
	peerTimeout = 80 * time.Second

	// sweepInterval is how often expiry runs.
	sweepInterval = 10 * time.Second

	// readBuffer must hold the largest relayed packet.
	readBuffer = 2048
)

// Config configures a Server.
type Config struct {
	Listen    string // UDP address to bind, e.g. ":27241"
	StateFile string // where to persist IP assignments
	Subnet    string // base for per-network /24s, e.g. "100.88.0.0"
	Verbose   bool
}

// Server is the coordinator and relay.
type Server struct {
	cfg  Config
	conn *net.UDPConn
	log  *log.Logger

	mu       sync.Mutex
	networks map[string]*network // keyed by network ID hex
	sessions map[proto.PeerID]*peer

	subnetBase [4]byte
	nextSubnet int
	stateDirty bool
}

// network is one virtual LAN.
type network struct {
	ID       string             `json:"id"`
	Verifier string             `json:"verifier"`
	Subnet   string             `json:"subnet"`  // e.g. "100.88.3.0"
	Devices  map[string]*device `json:"devices"` // keyed by peer ID hex
}

// device is a remembered member of a network. Persisted, so a friend keeps the
// same virtual IP across reconnects and server restarts -- the old P2PV did
// this too, and it matters in practice: people write the IP down.
type device struct {
	PeerID    string `json:"id"`
	StaticPub string `json:"pub"`
	Hostname  string `json:"host"`
	VirtualIP string `json:"vip"`
}

// peer is a live session. Not persisted.
type peer struct {
	id         proto.PeerID
	network    *network
	device     *device
	endpoint   *net.UDPAddr
	localAddrs []string
	lastSeen   time.Time
}

// New creates a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = ":27241"
	}
	if cfg.StateFile == "" {
		cfg.StateFile = "/var/lib/p2pv/state.json"
	}
	if cfg.Subnet == "" {
		cfg.Subnet = "100.88.0.0"
	}

	base := net.ParseIP(cfg.Subnet).To4()
	if base == nil {
		return nil, fmt.Errorf("server: invalid subnet %q", cfg.Subnet)
	}

	s := &Server{
		cfg:      cfg,
		log:      log.New(os.Stdout, "", log.LstdFlags),
		networks: make(map[string]*network),
		sessions: make(map[proto.PeerID]*peer),
	}
	copy(s.subnetBase[:], base)

	if err := s.loadState(); err != nil {
		return nil, err
	}
	return s, nil
}

// Run binds the socket and serves until the socket is closed.
func (s *Server) Run() error {
	addr, err := net.ResolveUDPAddr("udp", s.cfg.Listen)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	s.conn = conn
	s.log.Printf("P2PV server listening on %s (subnet base %s)", s.cfg.Listen, s.cfg.Subnet)

	go s.sweep()

	buf := make([]byte, readBuffer)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Printf("read error: %v", err)
			continue
		}
		if n == 0 {
			continue
		}
		s.handle(buf[:n], from)
	}
}

// Close stops the server.
func (s *Server) Close() error {
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

func (s *Server) handle(datagram []byte, from *net.UDPAddr) {
	switch datagram[0] {
	case proto.TypeRegister:
		s.handleRegister(datagram, from)
	case proto.TypeKeepalive:
		s.handleKeepalive(datagram, from)
	case proto.TypeBye:
		s.handleBye(datagram, from)
	case proto.TypePunchReq:
		s.handlePunchReq(datagram, from)
	case proto.TypeRelay:
		s.handleRelay(datagram, from)
	default:
		if s.cfg.Verbose {
			s.log.Printf("unknown message type 0x%02x from %s", datagram[0], from)
		}
	}
}

func (s *Server) handleRegister(datagram []byte, from *net.UDPAddr) {
	var req proto.Register
	if err := proto.DecodeControl(datagram, &req); err != nil {
		s.sendError(from, "malformed registration")
		return
	}

	pub, err := decodeKey(req.StaticPub)
	if err != nil {
		s.sendError(from, "invalid public key")
		return
	}
	if len(req.NetworkID) != 64 || len(req.Verifier) != 64 {
		s.sendError(from, "invalid network credentials")
		return
	}

	// The peer ID is derived from the key, not taken from the client, so a
	// client cannot claim someone else's ID.
	id := session.PeerIDOf(pub)

	s.mu.Lock()
	net_, err := s.networkFor(req.NetworkID, req.Verifier)
	if err != nil {
		s.mu.Unlock()
		s.sendError(from, err.Error())
		return
	}

	dev, err := s.deviceFor(net_, id, req)
	if err != nil {
		s.mu.Unlock()
		s.sendError(from, err.Error())
		return
	}

	// A device may reconnect from a new address; the session is replaced.
	p := &peer{
		id:         id,
		network:    net_,
		device:     dev,
		endpoint:   from,
		localAddrs: req.LocalAddrs,
		lastSeen:   time.Now(),
	}
	_, rejoin := s.sessions[id]
	s.sessions[id] = p

	resp := proto.Registered{
		VirtualIP: dev.VirtualIP,
		Netmask:   "255.255.255.0",
		PeerID:    id.String(),
		Peers:     s.peerListLocked(net_, id),
	}
	s.mu.Unlock()

	verb := "joined"
	if rejoin {
		verb = "rejoined"
	}
	s.log.Printf("%s %s network=%s ip=%s from=%s", req.Hostname, verb, shortID(net_.ID), dev.VirtualIP, from)

	s.sendControl(from, proto.TypeRegistered, resp)
	s.pushPeerList(net_, id)
	s.saveStateAsync()
}

// networkFor finds or creates a network, checking the verifier.
// Caller holds s.mu.
func (s *Server) networkFor(nid, verifier string) (*network, error) {
	n, ok := s.networks[nid]
	if !ok {
		// First member defines the network, including its password.
		subnet, err := s.allocateSubnetLocked()
		if err != nil {
			return nil, err
		}
		n = &network{
			ID:       nid,
			Verifier: verifier,
			Subnet:   subnet,
			Devices:  make(map[string]*device),
		}
		s.networks[nid] = n
		s.stateDirty = true
		s.log.Printf("network %s created, subnet %s", shortID(nid), subnet)
		return n, nil
	}
	if n.Verifier != verifier {
		return nil, errors.New("wrong network password")
	}
	return n, nil
}

// deviceFor finds or creates a device record with a stable virtual IP.
// Caller holds s.mu.
func (s *Server) deviceFor(n *network, id proto.PeerID, req proto.Register) (*device, error) {
	key := id.String()
	if dev, ok := n.Devices[key]; ok {
		// Guard against two keys hashing to one ID, and keep the hostname fresh.
		if dev.StaticPub != req.StaticPub {
			return nil, errors.New("peer ID collision")
		}
		if req.Hostname != "" && dev.Hostname != req.Hostname {
			dev.Hostname = req.Hostname
			s.stateDirty = true
		}
		return dev, nil
	}

	vip, err := s.allocateIPLocked(n)
	if err != nil {
		return nil, err
	}
	dev := &device{
		PeerID:    key,
		StaticPub: req.StaticPub,
		Hostname:  req.Hostname,
		VirtualIP: vip,
	}
	n.Devices[key] = dev
	s.stateDirty = true
	return dev, nil
}

// allocateSubnetLocked hands out the next /24. Caller holds s.mu.
func (s *Server) allocateSubnetLocked() (string, error) {
	used := make(map[string]bool, len(s.networks))
	for _, n := range s.networks {
		used[n.Subnet] = true
	}
	for i := 0; i < 256; i++ {
		third := byte((s.nextSubnet + i) % 256)
		candidate := fmt.Sprintf("%d.%d.%d.0", s.subnetBase[0], s.subnetBase[1], third)
		if !used[candidate] {
			s.nextSubnet = int(third) + 1
			return candidate, nil
		}
	}
	return "", errors.New("no free subnets")
}

// allocateIPLocked picks a free host address in the network's /24.
// Caller holds s.mu.
func (s *Server) allocateIPLocked(n *network) (string, error) {
	used := make(map[string]bool, len(n.Devices))
	for _, d := range n.Devices {
		used[d.VirtualIP] = true
	}
	base := net.ParseIP(n.Subnet).To4()
	if base == nil {
		return "", errors.New("corrupt subnet")
	}
	// .1 is reserved so the range looks conventional; .255 is broadcast.
	for host := 2; host < 255; host++ {
		candidate := fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], host)
		if !used[candidate] {
			return candidate, nil
		}
	}
	return "", errors.New("network full")
}

func (s *Server) handleKeepalive(datagram []byte, from *net.UDPAddr) {
	var req struct {
		PeerID string `json:"id"`
	}
	if err := proto.DecodeControl(datagram, &req); err != nil {
		return
	}
	id, err := parseHexPeerID(req.PeerID)
	if err != nil {
		return
	}

	s.mu.Lock()
	p, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		// The client thinks it is registered but we have forgotten it (restart,
		// or it timed out). Tell it to register again.
		s.sendError(from, "not registered")
		return
	}
	p.lastSeen = time.Now()
	moved := p.endpoint.String() != from.String()
	if moved {
		p.endpoint = from
	}
	n := p.network
	s.mu.Unlock()

	if moved {
		// Roaming (Wi-Fi to LTE, or a NAT rebinding): peers need the new address.
		s.log.Printf("%s moved to %s", shortID(id.String()), from)
		s.pushPeerList(n, id)
	}
}

func (s *Server) handleBye(datagram []byte, from *net.UDPAddr) {
	var req struct {
		PeerID string `json:"id"`
	}
	if err := proto.DecodeControl(datagram, &req); err != nil {
		return
	}
	id, err := parseHexPeerID(req.PeerID)
	if err != nil {
		return
	}

	s.mu.Lock()
	p, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.sessions, id)
	n := p.network
	host := p.device.Hostname
	s.mu.Unlock()

	s.log.Printf("%s left network=%s", host, shortID(n.ID))
	s.pushPeerList(n, id)
}

func (s *Server) handlePunchReq(datagram []byte, from *net.UDPAddr) {
	var req proto.PunchReq
	if err := proto.DecodeControl(datagram, &req); err != nil {
		return
	}

	// Identify the requester by its source address rather than trusting the
	// body, so a client cannot trigger punches on someone else's behalf.
	s.mu.Lock()
	requester := s.sessionByEndpointLocked(from)
	if requester == nil {
		s.mu.Unlock()
		return
	}
	target, err := parseHexPeerID(req.Target)
	if err != nil {
		s.mu.Unlock()
		return
	}
	tp, ok := s.sessions[target]
	if !ok || tp.network.ID != requester.network.ID {
		s.mu.Unlock()
		return
	}
	// Each side learns the other's candidates and starts probing.
	invToTarget := proto.PunchInv{Peer: s.peerViewLocked(requester)}
	invToRequester := proto.PunchInv{Peer: s.peerViewLocked(tp)}
	targetEP, requesterEP := tp.endpoint, requester.endpoint
	s.mu.Unlock()

	s.sendControl(targetEP, proto.TypePunchInv, invToTarget)
	s.sendControl(requesterEP, proto.TypePunchInv, invToRequester)
}

func (s *Server) handleRelay(datagram []byte, from *net.UDPAddr) {
	dst, _, _, err := proto.DecodeRelay(datagram)
	if err != nil {
		return
	}

	s.mu.Lock()
	src := s.sessionByEndpointLocked(from)
	if src == nil {
		s.mu.Unlock()
		return
	}
	src.lastSeen = time.Now()

	dp, ok := s.sessions[dst]
	if !ok || dp.network.ID != src.network.ID {
		s.mu.Unlock()
		return
	}
	dstEP := dp.endpoint
	srcID := src.id
	s.mu.Unlock()

	// Overwrite the source: the sender's claim is not trusted, ours is authoritative.
	proto.SetRelaySource(datagram, srcID)
	if _, err := s.conn.WriteToUDP(datagram, dstEP); err != nil && s.cfg.Verbose {
		s.log.Printf("relay write to %s: %v", dstEP, err)
	}
}

// sessionByEndpointLocked finds the live session at a UDP address.
// Caller holds s.mu.
func (s *Server) sessionByEndpointLocked(addr *net.UDPAddr) *peer {
	want := addr.String()
	for _, p := range s.sessions {
		if p.endpoint.String() == want {
			return p
		}
	}
	return nil
}

// peerViewLocked renders a peer for the wire. Caller holds s.mu.
func (s *Server) peerViewLocked(p *peer) proto.Peer {
	return proto.Peer{
		PeerID:     p.id.String(),
		Hostname:   p.device.Hostname,
		StaticPub:  p.device.StaticPub,
		VirtualIP:  p.device.VirtualIP,
		Endpoint:   p.endpoint.String(),
		LocalAddrs: p.localAddrs,
		Online:     true,
	}
}

// peerListLocked lists everyone in a network except exclude, online first.
// Caller holds s.mu.
func (s *Server) peerListLocked(n *network, exclude proto.PeerID) []proto.Peer {
	var out []proto.Peer
	for _, p := range s.sessions {
		if p.network.ID != n.ID || p.id == exclude {
			continue
		}
		out = append(out, s.peerViewLocked(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VirtualIP < out[j].VirtualIP })
	return out
}

// pushPeerList tells every online member of a network who is present now.
func (s *Server) pushPeerList(n *network, changed proto.PeerID) {
	s.mu.Lock()
	type delivery struct {
		addr *net.UDPAddr
		list proto.PeerList
	}
	var out []delivery
	for _, p := range s.sessions {
		if p.network.ID != n.ID {
			continue
		}
		out = append(out, delivery{
			addr: p.endpoint,
			list: proto.PeerList{Peers: s.peerListLocked(n, p.id)},
		})
	}
	s.mu.Unlock()

	for _, d := range out {
		s.sendControl(d.addr, proto.TypePeerList, d.list)
	}
}

// sweep expires silent sessions.
func (s *Server) sweep() {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for range ticker.C {
		if s.conn == nil {
			return
		}
		now := time.Now()

		s.mu.Lock()
		var expired []*peer
		for id, p := range s.sessions {
			if now.Sub(p.lastSeen) > peerTimeout {
				expired = append(expired, p)
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()

		for _, p := range expired {
			s.log.Printf("%s timed out network=%s", p.device.Hostname, shortID(p.network.ID))
			s.pushPeerList(p.network, p.id)
		}
		s.flushState()
	}
}

func (s *Server) sendControl(to *net.UDPAddr, msgType byte, payload any) {
	datagram, err := proto.EncodeControl(msgType, payload)
	if err != nil {
		s.log.Printf("encode 0x%02x: %v", msgType, err)
		return
	}
	if _, err := s.conn.WriteToUDP(datagram, to); err != nil && s.cfg.Verbose {
		s.log.Printf("write to %s: %v", to, err)
	}
}

func (s *Server) sendError(to *net.UDPAddr, reason string) {
	s.sendControl(to, proto.TypeError, proto.Error{Reason: reason})
}

// --- persistence ---

type persistedState struct {
	Networks map[string]*network `json:"networks"`
}

func (s *Server) loadState() error {
	data, err := os.ReadFile(s.cfg.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("server: state file %s is corrupt: %w", s.cfg.StateFile, err)
	}
	if st.Networks != nil {
		s.networks = st.Networks
	}
	for _, n := range s.networks {
		if n.Devices == nil {
			n.Devices = make(map[string]*device)
		}
	}
	s.log.Printf("loaded %d network(s) from %s", len(s.networks), s.cfg.StateFile)
	return nil
}

func (s *Server) saveStateAsync() {
	go s.flushState()
}

// flushState writes the state file if anything changed, via a temporary file so
// a crash mid-write cannot leave a truncated file behind.
func (s *Server) flushState() {
	s.mu.Lock()
	if !s.stateDirty {
		s.mu.Unlock()
		return
	}
	data, err := json.MarshalIndent(persistedState{Networks: s.networks}, "", "  ")
	s.stateDirty = false
	s.mu.Unlock()

	if err != nil {
		s.log.Printf("marshal state: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.StateFile), 0o700); err != nil {
		s.log.Printf("create state dir: %v", err)
		return
	}
	tmp := s.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.log.Printf("write state: %v", err)
		return
	}
	if err := os.Rename(tmp, s.cfg.StateFile); err != nil {
		s.log.Printf("replace state: %v", err)
	}
}

// --- helpers ---

func decodeKey(b64 string) ([32]byte, error) {
	var out [32]byte
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return out, err
	}
	if len(raw) != 32 {
		return out, errors.New("key is not 32 bytes")
	}
	copy(out[:], raw)
	return out, nil
}

func parseHexPeerID(s string) (proto.PeerID, error) {
	var id proto.PeerID
	raw, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	return proto.ParsePeerID(raw)
}

func shortID(hexID string) string {
	if len(hexID) > 8 {
		return hexID[:8]
	}
	return hexID
}
