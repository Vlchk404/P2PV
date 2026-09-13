// Command p2pv joins a P2PV virtual LAN.
//
//	p2pv join <network> [password]   join a network and stay connected
//	p2pv networks                    list remembered networks
//	p2pv forget <network>            forget a remembered network
//	p2pv id                          show this device's identity
//
// Joining needs administrator rights on Windows and root on Linux, because
// creating a virtual network interface does.
package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/Vlchk404/p2pv/internal/client"
	"github.com/Vlchk404/p2pv/internal/netid"
	"github.com/Vlchk404/p2pv/internal/store"
)

// defaultServer is the coordinator used when -server is not given. A build
// without a baked-in coordinator leaves it empty, and join then demands
// -server instead of failing somewhere deeper.
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
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "join":
		err = cmdJoin(args[1:])
	case "networks":
		err = cmdNetworks()
	case "forget":
		err = cmdForget(args[1:])
	case "id":
		err = cmdID()
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "p2pv: unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "p2pv: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`P2PV -- your own LAN over the internet.

Usage:
  p2pv join <network> [password]   join a network and stay connected
  p2pv networks                    list remembered networks
  p2pv forget <network>            forget a remembered network
  p2pv id                          show this device's identity

Flags for join:
  -server host:port   coordinator to use` + serverHint() + `
  -iface name         interface name (default P2PV)
  -lan-discovery      route multicast into the tunnel (see below)
  -v                  verbose logging

The first person to use a network name sets its password. Everyone who types
the same name and password lands on the same virtual LAN.

Joining creates a network interface, so it needs administrator rights.

-lan-discovery makes games appear by themselves in "LAN games" lists, but it
sends all multicast to the tunnel -- printers, speakers and other devices on
your real network stop being discovered while you are connected. Without it,
everything still works: connect to a friend's virtual address directly (in
Minecraft: Multiplayer -> Direct Connect -> 100.88.x.y:25565).
`)
}

func cmdJoin(args []string) error {
	var (
		server     = defaultServer
		iface      = "P2PV"
		verbose    bool
		multicast  bool
		positional []string
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-server":
			if i+1 >= len(args) {
				return fmt.Errorf("-server needs a value")
			}
			server = args[i+1]
			i++
		case "-iface":
			if i+1 >= len(args) {
				return fmt.Errorf("-iface needs a value")
			}
			iface = args[i+1]
			i++
		case "-v":
			verbose = true
		case "-lan-discovery":
			multicast = true
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) == 0 {
		return fmt.Errorf("usage: p2pv join <network> [password]")
	}
	name := positional[0]

	if server == "" {
		return fmt.Errorf("no coordinator: pass -server host:port " +
			"(running your own is described in the README)")
	}

	// Three ways to get the network key, in order of preference: a password on
	// the command line, a remembered network, or an interactive prompt.
	var network *netid.Network
	switch {
	case len(positional) > 1:
		var err error
		network, err = netid.Derive(name, positional[1])
		if err != nil {
			return err
		}
	default:
		if saved, err := store.LoadNetwork(strings.ToLower(strings.TrimSpace(name))); err == nil {
			network = saved
			fmt.Printf("Using the saved password for %q.\n", network.Name)
		} else {
			password, err := promptPassword(name)
			if err != nil {
				return err
			}
			network, err = netid.Derive(name, password)
			if err != nil {
				return err
			}
		}
	}

	if err := store.SaveNetwork(network); err != nil {
		// Not fatal: the session works, it just will not be remembered.
		fmt.Fprintf(os.Stderr, "warning: could not remember this network: %v\n", err)
	}

	identity, err := store.LoadOrCreateIdentity()
	if err != nil {
		return err
	}

	c, err := client.New(client.Config{
		Server:         server,
		Network:        network,
		Identity:       identity,
		IfaceName:      iface,
		Verbose:        verbose,
		MulticastRoute: multicast,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Joining %q via %s...\n", network.Name, server)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Println("\nDisconnecting.")
		c.Close()
	}()

	// Print the peer table periodically: this is the CLI stand-in for the
	// Radmin-style window, and it is what people actually need to see -- who is
	// online and at which address.
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			printStatus(c.Status())
		}
	}()

	return c.Run()
}

func printStatus(st client.Status) {
	if st.VirtualIP == "" {
		return
	}
	fmt.Printf("\n-- %s -- this machine: %s (%s)\n", st.Network, st.VirtualIP, st.Hostname)
	if len(st.Peers) == 0 {
		fmt.Println("   no peers online yet")
		return
	}
	for _, p := range st.Peers {
		state := "connecting"
		switch {
		case p.Connected && p.Direct:
			state = "direct"
		case p.Connected:
			state = "relayed"
		}
		fmt.Printf("   %-15s %-20s %s\n", p.VirtualIP, p.Hostname, state)
	}
}

func cmdNetworks() error {
	names, err := store.NetworkNames()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("No remembered networks. Join one with: p2pv join <network> <password>")
		return nil
	}
	fmt.Println("Remembered networks:")
	for _, n := range names {
		fmt.Printf("  %s\n", n)
	}
	return nil
}

func cmdForget(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: p2pv forget <network>")
	}
	name := strings.ToLower(strings.TrimSpace(args[0]))
	if err := store.ForgetNetwork(name); err != nil {
		return err
	}
	fmt.Printf("Forgot %q.\n", name)
	return nil
}

func cmdID() error {
	identity, err := store.LoadOrCreateIdentity()
	if err != nil {
		return err
	}
	dir, _ := store.Dir()
	fmt.Printf("Peer ID:    %s\n", identity.PeerID())
	fmt.Printf("Public key: %s\n", base64.StdEncoding.EncodeToString(identity.Public[:]))
	fmt.Printf("Stored in:  %s\n", dir)
	return nil
}

// promptPassword reads a password without echoing it, falling back to a plain
// read when stdin is not a terminal (a pipe, or a service manager).
func promptPassword(network string) (string, error) {
	fmt.Printf("Password for %q: ", network)

	fd := int(syscall.Stdin)
	if term.IsTerminal(fd) {
		raw, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
