// Package client is the P2PV node: it owns the virtual interface, keeps the
// peer table, and moves packets.
//
// Three goroutines do the work:
//
//	readDevice -- virtual interface -> peer (encrypt, then send direct or relay)
//	readSocket -- UDP -> virtual interface (control messages, or decrypt)
//	maintain   -- keepalives, punch retries, path health
//
// Paths and sessions are deliberately independent. A session is keyed to a peer,
// not to a route, so a conversation can start over the relay and silently move
// to a direct path the moment hole punching succeeds -- no rekey, no
// interruption to the game.
package client

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/Vlchk404/p2pv/internal/device"
	"github.com/Vlchk404/p2pv/internal/netid"
	"github.com/Vlchk404/p2pv/internal/proto"
	"github.com/Vlchk404/p2pv/internal/session"
)

const (
	// serverKeepalive refreshes our NAT binding toward the server and our
	// last-seen timestamp. Well under the server's 80s timeout.
	serverKeepalive = 25 * time.Second

	// pathKeepalive holds a direct peer path open. NAT UDP bindings commonly
	// expire between 30s and 120s, so 15s is comfortably inside the worst case.
	pathKeepalive = 15 * time.Second

	// pathDeadline is how long a direct path may go unacknowledged before we
	// fall back to the relay and start punching again.
	pathDeadline = 30 * time.Second

	// punchInterval is the retry cadence while trying to open a direct path.
	punchInterval = 2 * time.Second

	// punchAttempts is how many rounds to try before settling for the relay.
	// Punching continues in the background afterwards.
	punchAttempts = 5

	maintainTick = 1 * time.Second
	readBuffer   = 2048
)

// Config configures a Client.
type Config struct {
	Server    string // host:port of the coordinator
	Network   *netid.Network
	Identity  *session.Identity
	Hostname  string
	IfaceName string
	Verbose   bool

	// MulticastRoute points 224.0.0.0/4 at the tunnel, which is what LAN game
	// discovery needs. Off by default: it also takes the machine's real-LAN
	// mDNS and SSDP discovery with it. See internal/device/multicast.go.
	MulticastRoute bool

	// Log is where the engine writes its log. Nil means stdout, which suits the
	// CLI; a GUI has no console and passes its own writer so the same messages
	// land in a window instead of nowhere.
	Log io.Writer
}

// Client is a P2PV node.
type Client struct {
	cfg Config
	log *log.Logger
	id  proto.PeerID
	pub string // our static key, base64

	conn       *net.UDPConn
	serverAddr *net.UDPAddr
	dev        device.Device

	mu        sync.RWMutex
	peers     map[proto.PeerID]*peer
	byIP      map[string]*peer
	virtualIP string
	netmask   string
	broadcast string // the /24 broadcast address, e.g. 100.88.3.255

	registered chan struct{}
	regOnce    sync.Once
	closing    chan struct{}
	closeOnce  sync.Once
}

// peer is another node in the network.
type peer struct {
	id        proto.PeerID
	hostname  string
	staticPub [session.KeyLen]byte
	virtualIP string

	// Candidate addresses: the public endpoint the server observed, plus any
	// LAN addresses the peer reported. The LAN candidates are what let two
	// friends in the same house connect without leaving the building.
	publicEP   *net.UDPAddr
	localAddrs []*net.UDPAddr

	// directPath is the candidate that has answered a probe, or nil while we
	// are still relaying.
	directPath *net.UDPAddr
	lastAck    time.Time

	sess      *session.Session
	handshake *session.Handshake

	punchRounds int
	lastPunch   time.Time
	lastHello   time.Time
}

// usingRelay reports whether traffic to this peer currently goes via the server.
func (p *peer) usingRelay() bool { return p.directPath == nil }

// New creates a Client.
func New(cfg Config) (*Client, error) {
	if cfg.IfaceName == "" {
		cfg.IfaceName = "P2PV"
	}
	if cfg.Hostname == "" {
		cfg.Hostname, _ = osHostname()
	}

	addr, err := net.ResolveUDPAddr("udp", cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("client: resolve server %q: %w", cfg.Server, err)
	}

	out := cfg.Log
	if out == nil {
		out = logWriter()
	}

	return &Client{
		cfg:        cfg,
		log:        log.New(out, "", log.LstdFlags),
		id:         cfg.Identity.PeerID(),
		pub:        base64.StdEncoding.EncodeToString(cfg.Identity.Public[:]),
		serverAddr: addr,
		peers:      make(map[proto.PeerID]*peer),
		byIP:       make(map[string]*peer),
		registered: make(chan struct{}),
		closing:    make(chan struct{}),
	}, nil
}

// PeerID returns this node's ID.
func (c *Client) PeerID() proto.PeerID { return c.id }

// Run connects, brings up the interface, and serves until Close.
func (c *Client) Run() error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return fmt.Errorf("client: open socket: %w", err)
	}
	c.conn = conn
	defer conn.Close()

	go c.readSocket()

	if err := c.register(); err != nil {
		return err
	}

	// Wait for the server to assign an address before creating the interface:
	// the interface cannot be configured without one.
	select {
	case <-c.registered:
	case <-time.After(15 * time.Second):
		return errors.New("client: no response from server (check the address, and that UDP is not blocked)")
	case <-c.closing:
		return nil
	}

	c.mu.RLock()
	vip, mask := c.virtualIP, c.netmask
	c.mu.RUnlock()

	dev, err := device.Open(c.cfg.IfaceName, vip, mask, proto.MTU)
	if err != nil {
		return err
	}
	c.dev = dev
	defer dev.Close()

	c.log.Printf("interface %s up, virtual IP %s", dev.Name(), vip)
	c.log.Printf("peers can reach this machine at %s", vip)

	if c.cfg.MulticastRoute {
		if err := device.AddMulticastRoute(dev.Name()); err != nil {
			// Not fatal: everything except LAN auto-discovery still works.
			c.log.Printf("multicast route not added: %v", err)
		} else {
			c.log.Printf("multicast routed over %s (LAN game discovery enabled)", dev.Name())
		}
	}

	go c.readDevice()
	go c.maintain()

	<-c.closing
	c.sendBye()
	return nil
}

// Close shuts the client down.
func (c *Client) Close() {
	c.closeOnce.Do(func() { close(c.closing) })
}

func (c *Client) register() error {
	req := proto.Register{
		NetworkID:  c.cfg.Network.IDHex(),
		Verifier:   c.cfg.Network.VerifierHex(),
		StaticPub:  c.pub,
		Hostname:   c.cfg.Hostname,
		LocalAddrs: c.localCandidates(),
	}
	return c.sendControl(c.serverAddr, proto.TypeRegister, req)
}

// localCandidates lists this machine's LAN addresses on the port we listen on,
// so that peers on the same physical network can skip the internet entirely.
func (c *Client) localCandidates() []string {
	port := c.conn.LocalAddr().(*net.UDPAddr).Port

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() || !ip.IsPrivate() {
			continue
		}
		// Skip our own virtual interface; tunnelling through the tunnel would
		// be a loop.
		c.mu.RLock()
		isSelf := ip.String() == c.virtualIP
		c.mu.RUnlock()
		if isSelf {
			continue
		}
		out = append(out, net.JoinHostPort(ip.String(), fmt.Sprint(port)))
	}
	return out
}

// --- socket side ---

func (c *Client) readSocket() {
	buf := make([]byte, readBuffer)
	for {
		n, from, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if n == 0 {
			continue
		}
		c.handleDatagram(buf[:n], from)
	}
}

func (c *Client) handleDatagram(datagram []byte, from *net.UDPAddr) {
	switch datagram[0] {
	case proto.TypeRegistered:
		c.handleRegistered(datagram)
	case proto.TypePeerList:
		c.handlePeerList(datagram)
	case proto.TypePunchInv:
		c.handlePunchInv(datagram)
	case proto.TypeError:
		c.handleServerError(datagram)

	case proto.TypeRelay:
		// Unwrap and process as if it had arrived directly, but do not treat
		// the server's address as a direct path to the peer.
		_, src, inner, err := proto.DecodeRelay(datagram)
		if err != nil || len(inner) == 0 {
			return
		}
		c.handlePeerMessage(inner, src, nil)

	case proto.TypePunch, proto.TypePunchAck, proto.TypeHandshake, proto.TypeTransport:
		src, _, err := proto.DecodeDirect(datagram)
		if err != nil {
			return
		}
		c.handlePeerMessage(datagram, src, from)
	}
}

func (c *Client) handleRegistered(datagram []byte) {
	var resp proto.Registered
	if err := proto.DecodeControl(datagram, &resp); err != nil {
		c.log.Printf("malformed registration response: %v", err)
		return
	}

	c.mu.Lock()
	c.virtualIP = resp.VirtualIP
	c.netmask = resp.Netmask
	c.broadcast = broadcastOf(resp.VirtualIP, resp.Netmask)
	c.mu.Unlock()

	c.regOnce.Do(func() { close(c.registered) })
	c.mergePeers(resp.Peers)
}

func (c *Client) handlePeerList(datagram []byte) {
	var list proto.PeerList
	if err := proto.DecodeControl(datagram, &list); err != nil {
		return
	}
	c.mergePeers(list.Peers)
}

func (c *Client) handleServerError(datagram []byte) {
	var e proto.Error
	if err := proto.DecodeControl(datagram, &e); err != nil {
		return
	}
	if e.Reason == "not registered" {
		// The server forgot us -- it restarted, or we were away too long.
		// Re-register rather than going quiet.
		c.log.Printf("server asked us to re-register")
		if err := c.register(); err != nil {
			c.log.Printf("re-register: %v", err)
		}
		return
	}
	c.log.Printf("server refused: %s", e.Reason)
	if e.Reason == "wrong network password" {
		c.Close()
	}
}

// mergePeers reconciles the peer table with a list from the server.
func (c *Client) mergePeers(list []proto.Peer) {
	seen := make(map[proto.PeerID]bool, len(list))

	for _, pv := range list {
		id, err := parseHexPeerID(pv.PeerID)
		if err != nil || id == c.id {
			continue
		}
		pub, err := decodeKey(pv.StaticPub)
		if err != nil {
			continue
		}
		// The ID must match the key, or the server is confused (or lying).
		if session.PeerIDOf(pub) != id {
			c.log.Printf("ignoring peer %s: ID does not match its key", pv.Hostname)
			continue
		}
		seen[id] = true

		c.mu.Lock()
		p, existed := c.peers[id]
		if !existed {
			p = &peer{id: id}
			c.peers[id] = p
		}
		p.hostname = pv.Hostname
		p.staticPub = pub
		if p.virtualIP != pv.VirtualIP {
			delete(c.byIP, p.virtualIP)
			p.virtualIP = pv.VirtualIP
			c.byIP[pv.VirtualIP] = p
		}

		newEP := proto.ParseUDPAddr(pv.Endpoint)
		if newEP != nil && (p.publicEP == nil || p.publicEP.String() != newEP.String()) {
			p.publicEP = newEP
			// The peer roamed: the old direct path is stale.
			p.directPath = nil
			p.punchRounds = 0
		}
		p.localAddrs = p.localAddrs[:0]
		for _, la := range pv.LocalAddrs {
			if addr := proto.ParseUDPAddr(la); addr != nil {
				p.localAddrs = append(p.localAddrs, addr)
			}
		}
		c.mu.Unlock()

		if !existed {
			c.log.Printf("peer %s (%s) is online", pv.Hostname, pv.VirtualIP)
			// Connect eagerly, so the first game packet does not pay for the
			// handshake. With a handful of friends this costs nothing.
			c.startConnect(p)
		}
	}

	// Drop peers the server no longer lists.
	c.mu.Lock()
	var gone []*peer
	for id, p := range c.peers {
		if !seen[id] {
			gone = append(gone, p)
			delete(c.peers, id)
			delete(c.byIP, p.virtualIP)
		}
	}
	c.mu.Unlock()

	for _, p := range gone {
		c.log.Printf("peer %s (%s) went offline", p.hostname, p.virtualIP)
	}
}

// startConnect begins hole punching and, if we are the designated initiator,
// the handshake.
//
// Only the peer with the numerically lower ID initiates. Without that rule both
// sides start a handshake at once and each replaces the other's half-finished
// state, which shows up as a session that never settles.
func (c *Client) startConnect(p *peer) {
	c.punch(p)
	c.requestPunch(p)
	if c.isInitiatorFor(p.id) {
		c.startHandshake(p)
	}
}

func (c *Client) isInitiatorFor(other proto.PeerID) bool {
	for i := 0; i < proto.PeerIDLen; i++ {
		if c.id[i] != other[i] {
			return c.id[i] < other[i]
		}
	}
	return false
}

// punch fires a probe at every candidate address for a peer. One of them may
// open a NAT binding; the ack tells us which.
func (c *Client) punch(p *peer) {
	msg := proto.EncodeDirect(proto.TypePunch, c.id, nil)

	c.mu.Lock()
	p.lastPunch = time.Now()
	p.punchRounds++
	candidates := p.candidatesLocked()
	c.mu.Unlock()

	for _, addr := range candidates {
		c.conn.WriteToUDP(msg, addr)
	}
}

// candidatesLocked lists every address worth probing. Caller holds c.mu.
func (p *peer) candidatesLocked() []*net.UDPAddr {
	out := make([]*net.UDPAddr, 0, len(p.localAddrs)+1)
	out = append(out, p.localAddrs...)
	if p.publicEP != nil {
		out = append(out, p.publicEP)
	}
	return out
}

// requestPunch asks the server to make the peer probe us at the same time.
// Simultaneous probes are what actually open both NATs.
func (c *Client) requestPunch(p *peer) {
	c.sendControl(c.serverAddr, proto.TypePunchReq, proto.PunchReq{Target: p.id.String()})
}

func (c *Client) handlePunchInv(datagram []byte) {
	var inv proto.PunchInv
	if err := proto.DecodeControl(datagram, &inv); err != nil {
		return
	}
	id, err := parseHexPeerID(inv.Peer.PeerID)
	if err != nil {
		return
	}

	c.mu.RLock()
	p := c.peers[id]
	c.mu.RUnlock()
	if p == nil {
		// We have not seen this peer in a list yet; it will arrive shortly.
		return
	}
	c.punch(p)
}

// handlePeerMessage dispatches a data-plane message. via is the address it
// arrived from for direct messages, or nil if it came through the relay.
func (c *Client) handlePeerMessage(msg []byte, src proto.PeerID, via *net.UDPAddr) {
	c.mu.RLock()
	p := c.peers[src]
	c.mu.RUnlock()
	if p == nil {
		return
	}

	_, payload, err := proto.DecodeDirect(msg)
	if err != nil {
		return
	}

	switch msg[0] {
	case proto.TypePunch:
		// Answer on the same path it came in on, which is the path that works.
		if via != nil {
			ack := proto.EncodeDirect(proto.TypePunchAck, c.id, nil)
			c.conn.WriteToUDP(ack, via)
		}

	case proto.TypePunchAck:
		if via == nil {
			return
		}
		c.mu.Lock()
		first := p.directPath == nil
		p.directPath = via
		p.lastAck = time.Now()
		host := p.hostname
		c.mu.Unlock()
		if first {
			c.log.Printf("direct path to %s via %s", host, via)
		}

	case proto.TypeHandshake:
		c.handleHandshake(p, payload, via)

	case proto.TypeTransport:
		c.handleTransport(p, payload)
	}
}

func (c *Client) startHandshake(p *peer) {
	hs, msg, err := session.StartInitiator(c.cfg.Identity, c.cfg.Network.ID, c.cfg.Network.PSK, p.staticPub)
	if err != nil {
		c.log.Printf("handshake to %s: %v", p.hostname, err)
		return
	}

	c.mu.Lock()
	p.handshake = hs
	p.lastHello = time.Now()
	c.mu.Unlock()

	payload := append([]byte{proto.HandshakeInit}, msg...)
	c.sendToPeer(p, proto.TypeHandshake, payload)
}

func (c *Client) handleHandshake(p *peer, payload []byte, via *net.UDPAddr) {
	if len(payload) < 2 {
		return
	}
	body := payload[1:]

	switch payload[0] {
	case proto.HandshakeInit:
		sess, peerStatic, reply, err := session.Respond(c.cfg.Identity, c.cfg.Network.ID, c.cfg.Network.PSK, body)
		if err != nil {
			if c.cfg.Verbose {
				c.log.Printf("handshake from %s rejected: %v", p.hostname, err)
			}
			return
		}
		// The key in the handshake must be the key the server advertised,
		// otherwise a network member could answer for someone else.
		if peerStatic != p.staticPub {
			c.log.Printf("handshake from %s rejected: key does not match the peer list", p.hostname)
			return
		}

		c.mu.Lock()
		p.sess = sess
		p.handshake = nil
		if via != nil {
			p.directPath = via
			p.lastAck = time.Now()
		}
		c.mu.Unlock()

		c.sendToPeer(p, proto.TypeHandshake, append([]byte{proto.HandshakeResp}, reply...))
		c.log.Printf("encrypted channel with %s established", p.hostname)

	case proto.HandshakeResp:
		c.mu.Lock()
		hs := p.handshake
		c.mu.Unlock()
		if hs == nil {
			return // duplicate reply, or not our handshake
		}

		sess, err := hs.Finish(body)
		if err != nil {
			c.log.Printf("handshake with %s failed: %v", p.hostname, err)
			c.mu.Lock()
			p.handshake = nil
			c.mu.Unlock()
			return
		}

		c.mu.Lock()
		p.sess = sess
		p.handshake = nil
		if via != nil {
			p.directPath = via
			p.lastAck = time.Now()
		}
		c.mu.Unlock()

		c.log.Printf("encrypted channel with %s established", p.hostname)
	}
}

func (c *Client) handleTransport(p *peer, payload []byte) {
	c.mu.RLock()
	sess := p.sess
	c.mu.RUnlock()
	if sess == nil {
		return
	}

	counter, ciphertext, err := proto.DecodeTransport(payload)
	if err != nil {
		return
	}
	packet, err := sess.Open(counter, ciphertext)
	if err != nil {
		if c.cfg.Verbose {
			c.log.Printf("drop packet from %s: %v", p.hostname, err)
		}
		return
	}

	// A peer may only claim its own address. Without this check any member
	// could inject packets that appear to come from someone else on the LAN.
	if src, ok := sourceIP(packet); !ok || src != p.virtualIP {
		if c.cfg.Verbose {
			c.log.Printf("drop spoofed packet from %s (claims %s)", p.hostname, src)
		}
		return
	}

	if _, err := c.dev.Write(packet); err != nil && c.cfg.Verbose {
		c.log.Printf("write to interface: %v", err)
	}
}

// --- device side ---

func (c *Client) readDevice() {
	buf := make([]byte, readBuffer)
	for {
		n, err := c.dev.Read(buf)
		if err != nil {
			select {
			case <-c.closing:
			default:
				c.log.Printf("interface read: %v", err)
				c.Close()
			}
			return
		}
		if n == 0 {
			continue
		}
		c.routePacket(buf[:n])
	}
}

// routePacket decides where an outbound packet goes.
//
// Broadcast and multicast are fanned out to every peer in software. This is
// what makes LAN game discovery possible at all on a layer 3 tunnel: Minecraft
// announces open worlds to 224.0.2.60:4445, and without fan-out that
// announcement would die at the interface.
func (c *Client) routePacket(packet []byte) {
	dst, ok := destIP(packet)
	if !ok {
		return // not IPv4; IPv6 is not carried yet
	}

	c.mu.RLock()
	broadcast := c.broadcast
	c.mu.RUnlock()

	switch {
	case dst == broadcast || dst == "255.255.255.255" || isMulticast(dst):
		c.fanout(packet)
	default:
		c.mu.RLock()
		p := c.byIP[dst]
		c.mu.RUnlock()
		if p != nil {
			c.sendPacket(p, packet)
		}
	}
}

func (c *Client) fanout(packet []byte) {
	c.mu.RLock()
	targets := make([]*peer, 0, len(c.peers))
	for _, p := range c.peers {
		if p.sess != nil {
			targets = append(targets, p)
		}
	}
	c.mu.RUnlock()

	for _, p := range targets {
		c.sendPacket(p, packet)
	}
}

// sendPacket encrypts one IP packet and sends it to a peer.
func (c *Client) sendPacket(p *peer, packet []byte) {
	c.mu.RLock()
	sess := p.sess
	c.mu.RUnlock()

	if sess == nil {
		// No channel yet. Nudge it along and drop this packet; TCP will retry,
		// and game discovery repeats on its own.
		c.wake(p)
		return
	}

	counter, ciphertext, err := sess.Seal(packet)
	if err != nil {
		c.log.Printf("encrypt for %s: %v", p.hostname, err)
		return
	}
	c.sendToPeer(p, proto.TypeTransport, proto.EncodeTransport(counter, ciphertext))
}

// wake restarts connection setup, rate limited so a burst of traffic to an
// unreachable peer does not become a packet storm.
func (c *Client) wake(p *peer) {
	c.mu.RLock()
	recent := time.Since(p.lastPunch) < punchInterval
	c.mu.RUnlock()
	if recent {
		return
	}
	c.startConnect(p)
}

// sendToPeer sends a data-plane message, directly if a path is known and via
// the relay otherwise.
func (c *Client) sendToPeer(p *peer, msgType byte, payload []byte) {
	msg := proto.EncodeDirect(msgType, c.id, payload)

	c.mu.RLock()
	path := p.directPath
	c.mu.RUnlock()

	if path != nil {
		if _, err := c.conn.WriteToUDP(msg, path); err == nil {
			return
		}
		// The path just broke; fall through to the relay.
		c.mu.Lock()
		p.directPath = nil
		c.mu.Unlock()
	}
	c.conn.WriteToUDP(proto.EncodeRelay(p.id, c.id, msg), c.serverAddr)
}

// --- maintenance ---

func (c *Client) maintain() {
	ticker := time.NewTicker(maintainTick)
	defer ticker.Stop()

	lastServerPing := time.Now()

	for {
		select {
		case <-c.closing:
			return
		case now := <-ticker.C:
			if now.Sub(lastServerPing) >= serverKeepalive {
				c.sendControl(c.serverAddr, proto.TypeKeepalive, struct {
					PeerID string `json:"id"`
				}{c.id.String()})
				lastServerPing = now
			}
			c.maintainPeers(now)
		}
	}
}

func (c *Client) maintainPeers(now time.Time) {
	type action struct {
		p         *peer
		probe     bool
		hello     bool
		keepalive bool
		lostPath  bool
	}

	c.mu.Lock()
	var actions []action
	for _, p := range c.peers {
		var a action
		a.p = p

		switch {
		case p.directPath != nil && now.Sub(p.lastAck) > pathDeadline:
			// The direct path went quiet. Drop back to the relay so the game
			// keeps running, and start looking for a new path.
			p.directPath = nil
			p.punchRounds = 0
			a.lostPath = true
			a.probe = true

		case p.directPath != nil && now.Sub(p.lastPunch) >= pathKeepalive:
			// Probes double as path keepalives: the ack refreshes lastAck and
			// the NAT binding at the same time.
			a.keepalive = true

		case p.directPath == nil && now.Sub(p.lastPunch) >= punchInterval:
			// Keep punching even after falling back to the relay -- NAT state
			// changes, and a direct path is worth having.
			a.probe = true
		}

		// Retry a handshake that never completed.
		if p.sess == nil && c.isInitiatorFor(p.id) && now.Sub(p.lastHello) >= punchInterval {
			a.hello = true
		}

		if a.probe || a.hello || a.keepalive || a.lostPath {
			actions = append(actions, a)
		}
	}
	c.mu.Unlock()

	for _, a := range actions {
		if a.lostPath {
			c.log.Printf("direct path to %s lost, falling back to relay", a.p.hostname)
		}
		if a.probe || a.keepalive {
			c.punch(a.p)
		}
		if a.probe {
			c.mu.RLock()
			rounds := a.p.punchRounds
			c.mu.RUnlock()
			// Re-broker through the server for the first few rounds; after that
			// the peer's NAT is unlikely to be opened by more brokering alone.
			if rounds <= punchAttempts {
				c.requestPunch(a.p)
			}
		}
		if a.hello {
			c.startHandshake(a.p)
		}
	}
}

func (c *Client) sendBye() {
	c.sendControl(c.serverAddr, proto.TypeBye, struct {
		PeerID string `json:"id"`
	}{c.id.String()})
}

func (c *Client) sendControl(to *net.UDPAddr, msgType byte, payload any) error {
	datagram, err := proto.EncodeControl(msgType, payload)
	if err != nil {
		return err
	}
	_, err = c.conn.WriteToUDP(datagram, to)
	return err
}

// --- status, for the CLI and the tray ---

// Status is a snapshot of the client's state.
type Status struct {
	Hostname  string
	VirtualIP string
	Network   string
	Peers     []PeerStatus
}

// PeerStatus is a snapshot of one peer.
type PeerStatus struct {
	Hostname  string
	VirtualIP string
	PeerID    string
	Connected bool
	Direct    bool
	Endpoint  string
}

// Status returns a snapshot suitable for display.
func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	st := Status{
		Hostname:  c.cfg.Hostname,
		VirtualIP: c.virtualIP,
		Network:   c.cfg.Network.Name,
	}
	for _, p := range c.peers {
		ps := PeerStatus{
			Hostname:  p.hostname,
			VirtualIP: p.virtualIP,
			PeerID:    p.id.String(),
			Connected: p.sess != nil,
			Direct:    !p.usingRelay(),
		}
		switch {
		case p.directPath != nil:
			ps.Endpoint = p.directPath.String()
		case p.publicEP != nil:
			ps.Endpoint = "relay (" + p.publicEP.String() + ")"
		default:
			ps.Endpoint = "relay"
		}
		st.Peers = append(st.Peers, ps)
	}
	return st
}

// --- packet helpers ---

// destIP returns the destination address of an IPv4 packet.
func destIP(packet []byte) (string, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return "", false
	}
	return net.IP(packet[16:20]).String(), true
}

// sourceIP returns the source address of an IPv4 packet.
func sourceIP(packet []byte) (string, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return "", false
	}
	return net.IP(packet[12:16]).String(), true
}

// isMulticast reports whether an address is in 224.0.0.0/4.
func isMulticast(addr string) bool {
	ip := net.ParseIP(addr).To4()
	return ip != nil && ip[0] >= 224 && ip[0] <= 239
}

// broadcastOf computes the broadcast address of the subnet holding ip.
func broadcastOf(ip, netmask string) string {
	i := net.ParseIP(ip).To4()
	m := net.ParseIP(netmask).To4()
	if i == nil || m == nil {
		return ""
	}
	out := make(net.IP, 4)
	for k := 0; k < 4; k++ {
		out[k] = i[k] | ^m[k]
	}
	return out.String()
}

func parseHexPeerID(s string) (proto.PeerID, error) {
	var id proto.PeerID
	raw, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	return proto.ParsePeerID(raw)
}

func decodeKey(b64 string) ([session.KeyLen]byte, error) {
	var out [session.KeyLen]byte
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return out, err
	}
	if len(raw) != session.KeyLen {
		return out, errors.New("key is not 32 bytes")
	}
	copy(out[:], raw)
	return out, nil
}
