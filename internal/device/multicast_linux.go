//go:build linux

package device

import "fmt"

func addMulticastRoute(ifname string) error {
	if out, err := run("ip", "route", "add", "224.0.0.0/4", "dev", ifname); err != nil {
		return fmt.Errorf("device: add multicast route on %q: %w: %s", ifname, err, out)
	}
	return nil
}
