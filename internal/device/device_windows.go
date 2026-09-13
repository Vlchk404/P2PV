//go:build windows

package device

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/tun"
)

// Open creates the Wintun adapter and configures its address.
//
// Wintun needs wintun.dll beside the executable (or on the DLL search path) and
// administrator rights to create an adapter. Both failures are reported with
// what to do about them, because both are what actually goes wrong on a fresh
// machine.
func Open(name, ip, netmask string, mtu int) (Device, error) {
	dev, err := tun.CreateTUN(name, mtu)
	if err != nil {
		return nil, fmt.Errorf("device: create Wintun adapter %q: %w\n"+
			"  - wintun.dll must sit next to p2pv.exe (run scripts\\get-wintun.ps1)\n"+
			"  - p2pv must run as administrator", name, err)
	}

	wrapped, err := wrap(dev)
	if err != nil {
		return nil, err
	}

	if err := configureWindows(wrapped.Name(), ip, netmask, mtu); err != nil {
		wrapped.Close()
		return nil, err
	}
	return wrapped, nil
}

// configureWindows assigns the address and MTU via netsh.
//
// netsh rather than the IP Helper API: it keeps the dependency list to the tun
// package alone, and it is inspectable -- a user can run the same command by
// hand to see what happened.
//
// The adapter takes a moment to appear in the network stack after creation, so
// the address is retried briefly; without that, a first run fails on a fast
// machine and succeeds on the second attempt.
func configureWindows(ifname, ip, netmask string, mtu int) error {
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		out, err := run("netsh", "interface", "ip", "set", "address",
			fmt.Sprintf("name=%s", ifname), "static", ip, netmask)
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("%w: %s", err, out)
		time.Sleep(300 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("device: assign %s to %q: %w", ip, ifname, lastErr)
	}

	// Wintun's own MTU is set at creation; this keeps the IPv4 stack in step.
	if out, err := run("netsh", "interface", "ipv4", "set", "subinterface",
		fmt.Sprintf("%s", ifname), fmt.Sprintf("mtu=%d", mtu), "store=active"); err != nil {
		return fmt.Errorf("device: set MTU on %q: %w: %s", ifname, err, out)
	}
	return nil
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Small helpers for parsing netsh table output.

func splitLines(s string) []string { return strings.Split(s, "\n") }

func splitFields(s string) []string { return strings.Fields(s) }

// joinFrom rejoins fields from index i onward, for columns whose value may
// contain spaces -- an interface name usually does.
func joinFrom(fields []string, i int) string {
	if i >= len(fields) {
		return ""
	}
	return strings.Join(fields[i:], " ")
}
