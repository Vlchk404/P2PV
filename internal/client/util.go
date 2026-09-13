package client

import (
	"io"
	"os"
)

// osHostname returns this machine's name, used as the display name in the peer
// list. Radmin VPN shows the computer name too, and it is what makes a peer
// list readable instead of a column of hex IDs.
func osHostname() (string, error) {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown", err
	}
	return name, nil
}

// logWriter is where client logs go. Stdout in the CLI; the tray build
// redirects this to a file, since a GUI process has no console to write to.
func logWriter() io.Writer { return os.Stdout }
