package device

// Multicast routing is separate from address configuration because it is a
// trade-off rather than a setting with a right answer.
//
// A layer 3 tunnel carries a multicast datagram only if the sender's route for
// 224.0.0.0/4 points at the tunnel, and a receiver joins a group only on the
// interface its route selects. So without a multicast route, LAN game discovery
// cannot work over the tunnel.
//
// But 224.0.0.0/4 is the whole multicast range. Pointing it at the tunnel takes
// mDNS, SSDP and everything else with it -- so printers, speakers and cast
// devices on the real LAN stop being discovered while P2PV is connected. That
// is a bad default for a program someone leaves running.
//
// Hence: off by default, one flag to turn on, and Direct Connect always works
// regardless.

// AddMulticastRoute points the multicast range at the tunnel so that LAN game
// discovery can work, at the cost of the machine's real-LAN discovery.
func AddMulticastRoute(ifname string) error { return addMulticastRoute(ifname) }
