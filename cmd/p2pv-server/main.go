// Command p2pv-server is the P2PV coordinator and relay.
//
// It runs on a VPS with a public address and needs one open UDP port. It holds
// no keys for any network: it brokers introductions, and forwards ciphertext
// when two peers cannot reach each other directly.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Vlchk404/p2pv/internal/server"
)

func main() {
	var cfg server.Config
	flag.StringVar(&cfg.Listen, "listen", ":27241", "UDP address to listen on")
	flag.StringVar(&cfg.StateFile, "state", "/var/lib/p2pv/state.json", "where to persist virtual IP assignments")
	flag.StringVar(&cfg.Subnet, "subnet", "100.88.0.0", "base for the per-network /24 ranges")
	flag.BoolVar(&cfg.Verbose, "v", false, "log dropped and unknown packets")
	flag.Parse()

	srv, err := server.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "p2pv-server: %v\n", err)
		os.Exit(1)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Println("shutting down")
		srv.Close()
	}()

	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "p2pv-server: %v\n", err)
		os.Exit(1)
	}
}
