// Command p2pv-tray runs P2PV as a system tray icon.
//
//	p2pv-tray [network] [password]
//
// With no arguments it joins the single remembered network, which is the
// ordinary case: join once from the CLI, then launch the tray from a shortcut
// and forget the command line exists.
//
// The tray has no console to prompt in, so a network that has never been joined
// needs its password on the command line -- or one `p2pv join` first, which
// remembers it.
//
// Needs administrator rights on Windows and root on Linux, because creating a
// virtual network interface does.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Vlchk404/p2pv/internal/client"
	"github.com/Vlchk404/p2pv/internal/netid"
	"github.com/Vlchk404/p2pv/internal/store"
	"github.com/Vlchk404/p2pv/internal/tray"
)

// defaultServer matches the CLI's default coordinator. Empty in a build
// without a baked-in one, and the tray then refuses to start without -server.
const defaultServer = ""

// serverHint renders the -server default for the usage text: nothing when this
// build has no coordinator baked in.
func serverHint() string {
	if defaultServer == "" {
		return ""
	}
	return " (default " + defaultServer + ")"
}

func main() {
	log.SetFlags(log.LstdFlags)

	var (
		server    = flag.String("server", defaultServer, "coordinator to use")
		iface     = flag.String("iface", "P2PV", "interface name")
		multicast = flag.Bool("lan-discovery", false,
			"route multicast into the tunnel so games appear in LAN lists")
		verbose = flag.Bool("v", false, "verbose logging")
	)
	flag.Usage = usage
	flag.Parse()

	if *server == "" {
		fmt.Fprintf(os.Stderr, "p2pv-tray: no coordinator: pass -server host:port "+
			"(running your own is described in the README)\n")
		os.Exit(1)
	}

	network, err := resolveNetwork(flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "p2pv-tray: %v\n", err)
		os.Exit(1)
	}

	identity, err := store.LoadOrCreateIdentity()
	if err != nil {
		fmt.Fprintf(os.Stderr, "p2pv-tray: %v\n", err)
		os.Exit(1)
	}

	if err := store.SaveNetwork(network); err != nil {
		log.Printf("warning: could not remember this network: %v", err)
	}

	log.Printf("joining %q via %s", network.Name, *server)

	err = tray.Run(tray.Config{
		Client: client.Config{
			Server:         *server,
			Network:        network,
			Identity:       identity,
			IfaceName:      *iface,
			Verbose:        *verbose,
			MulticastRoute: *multicast,
		},
		Log: log.Default(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "p2pv-tray: %v\n", err)
		os.Exit(1)
	}
}

// resolveNetwork works out which network to join, in order of preference: named
// with a password, named and remembered, or the only remembered one.
func resolveNetwork(args []string) (*netid.Network, error) {
	switch len(args) {
	case 0:
		names, err := store.NetworkNames()
		if err != nil {
			return nil, err
		}
		switch len(names) {
		case 0:
			return nil, fmt.Errorf("no remembered networks.\n" +
				"  Join one first:  p2pv join <network> <password>\n" +
				"  Or name it here: p2pv-tray <network> <password>")
		case 1:
			return store.LoadNetwork(names[0])
		default:
			return nil, fmt.Errorf("several remembered networks (%s).\n"+
				"  Name the one to join: p2pv-tray <network>",
				strings.Join(names, ", "))
		}
	case 1:
		name := strings.ToLower(strings.TrimSpace(args[0]))
		n, err := store.LoadNetwork(name)
		if err != nil {
			return nil, fmt.Errorf("network %q is not remembered, "+
				"so its password is needed: p2pv-tray %s <password>", args[0], args[0])
		}
		return n, nil
	default:
		return netid.Derive(args[0], args[1])
	}
}

func usage() {
	fmt.Print(`P2PV in the system tray.

Usage:
  p2pv-tray [network] [password]

Flags:
  -server host:port   coordinator to use` + serverHint() + `
  -iface name         interface name (default P2PV)
  -lan-discovery      route multicast into the tunnel (breaks real-LAN
                      discovery of printers and speakers while connected)
  -v                  verbose logging

With no arguments, joins the single remembered network. The icon shows green
for a direct connection, amber when a peer is on the slower relay path, and
grey when nobody is online. Click an address to copy it.

Creating a network interface needs administrator rights.
`)
}
