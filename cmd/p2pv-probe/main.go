// Command p2pv-probe is a test helper for verifying that a P2PV tunnel
// actually carries traffic.
//
// It exists because the useful checks are not ICMP: a game needs a TCP stream,
// and LAN discovery needs a UDP datagram sent to a broadcast or multicast
// address to arrive at everyone. This is a stand-in for netcat that does not
// need to be installed on the host under test.
//
//	p2pv-probe tcp-listen <port>              accept one connection, print what arrives
//	p2pv-probe tcp-send <host:port> <text>    connect and send
//	p2pv-probe udp-listen <port>              print the first datagram received
//	p2pv-probe udp-send <host:port> <text>    send one datagram
//	p2pv-probe mc-listen <group:port>         join a multicast group and listen
//	p2pv-probe mc-send <group:port> <text>    send to a multicast group
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

const timeout = 10 * time.Second

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: p2pv-probe <mode> <addr> [text]")
		os.Exit(2)
	}
	mode, addr := os.Args[1], os.Args[2]
	text := ""
	if len(os.Args) > 3 {
		text = os.Args[3]
	}

	var err error
	switch mode {
	case "tcp-listen":
		err = tcpListen(addr)
	case "tcp-send":
		err = tcpSend(addr, text)
	case "udp-listen":
		err = udpListen(addr)
	case "udp-send":
		err = udpSend(addr, text)
	case "mc-listen":
		err = multicastListen(addr)
	case "mc-send":
		err = udpSend(addr, text)
	default:
		err = fmt.Errorf("unknown mode %q", mode)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "p2pv-probe: %v\n", err)
		os.Exit(1)
	}
}

func tcpListen(port string) error {
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return err
	}
	defer ln.Close()

	if tcpLn, ok := ln.(*net.TCPListener); ok {
		tcpLn.SetDeadline(time.Now().Add(timeout))
	}
	conn, err := ln.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		return err
	}
	fmt.Printf("%s", buf[:n])
	return nil
}

func tcpSend(addr, text string) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(text))
	return err
}

func udpListen(port string) error {
	conn, err := net.ListenPacket("udp", ":"+port)
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 4096)
	n, from, err := conn.ReadFrom(buf)
	if err != nil {
		return err
	}
	fmt.Printf("%s (from %s)", buf[:n], from)
	return nil
}

func udpSend(addr, text string) error {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(text))
	return err
}

// multicastListen joins a group the way Minecraft's LAN discovery does, so the
// check exercises the same path a game would.
func multicastListen(addr string) error {
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return err
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, udpAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 4096)
	n, from, err := conn.ReadFromUDP(buf)
	if err != nil {
		return err
	}
	fmt.Printf("%s (from %s)", buf[:n], from)
	return nil
}
