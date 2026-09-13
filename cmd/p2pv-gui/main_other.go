//go:build !windows

// The window build is Windows-only: it is built on walk, which wraps the Win32
// widgets. The CLI and the tray build cover other platforms.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "p2pv-gui: окно есть только под Windows -- используйте p2pv или p2pv-tray")
	os.Exit(1)
}
