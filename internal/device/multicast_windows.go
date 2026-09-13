//go:build windows

package device

import "fmt"

func addMulticastRoute(ifname string) error {
	// The interface index is what route add needs; netsh reports it.
	idx, err := interfaceIndex(ifname)
	if err != nil {
		return err
	}
	if out, err := run("route", "add", "224.0.0.0", "mask", "240.0.0.0",
		"0.0.0.0", "if", idx); err != nil {
		return fmt.Errorf("device: add multicast route on %q: %w: %s", ifname, err, out)
	}
	return nil
}

func interfaceIndex(ifname string) (string, error) {
	out, err := run("netsh", "interface", "ipv4", "show", "interfaces")
	if err != nil {
		return "", fmt.Errorf("device: list interfaces: %w: %s", err, out)
	}
	for _, line := range splitLines(out) {
		fields := splitFields(line)
		if len(fields) < 5 {
			continue
		}
		// Columns: Idx  Met  MTU  State  Name -- and the name may contain spaces.
		name := joinFrom(fields, 4)
		if name == ifname {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("device: interface %q not found", ifname)
}
