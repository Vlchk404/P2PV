//go:build linux

package device

import (
	"fmt"
	"os/exec"
	"strings"

	"golang.zx2c4.com/wireguard/tun"
)

// Open creates a tun interface and configures its address.
//
// Linux support exists so the client can be tested headlessly on a VPS -- which
// is how the relay path and hole punching get verified without two machines
// behind two different NATs.
func Open(name, ip, netmask string, mtu int) (Device, error) {
	dev, err := tun.CreateTUN(name, mtu)
	if err != nil {
		return nil, fmt.Errorf("device: create tun %q: %w (need root or CAP_NET_ADMIN)", name, err)
	}

	wrapped, err := wrap(dev)
	if err != nil {
		return nil, err
	}

	if err := configureLinux(wrapped.Name(), ip, netmask, mtu); err != nil {
		wrapped.Close()
		return nil, err
	}
	return wrapped, nil
}

func configureLinux(ifname, ip, netmask string, mtu int) error {
	prefix := maskToPrefix(netmask)
	if out, err := run("ip", "addr", "add", fmt.Sprintf("%s/%d", ip, prefix), "dev", ifname); err != nil {
		return fmt.Errorf("device: assign %s to %q: %w: %s", ip, ifname, err, out)
	}
	if out, err := run("ip", "link", "set", "dev", ifname, "mtu", fmt.Sprint(mtu), "up"); err != nil {
		return fmt.Errorf("device: bring up %q: %w: %s", ifname, err, out)
	}
	return nil
}

// maskToPrefix converts a dotted netmask to a prefix length, defaulting to /24.
func maskToPrefix(netmask string) int {
	var b [4]int
	if n, err := fmt.Sscanf(netmask, "%d.%d.%d.%d", &b[0], &b[1], &b[2], &b[3]); err != nil || n != 4 {
		return 24
	}
	prefix := 0
	for _, octet := range b {
		for bit := 7; bit >= 0; bit-- {
			if octet&(1<<bit) != 0 {
				prefix++
			}
		}
	}
	return prefix
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
