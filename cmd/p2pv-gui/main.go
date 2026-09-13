//go:build windows

// Command p2pv-gui is P2PV with a window: the Radmin-VPN-shaped client.
//
// It needs administrator rights, which the embedded manifest requests at
// launch, so Windows shows one UAC prompt and the window opens able to do its
// job rather than failing later on a button press.
package main

import (
	"github.com/lxn/walk"

	"github.com/Vlchk404/p2pv/internal/gui"
)

// defaultServer matches the CLI's default coordinator.
const defaultServer = ""

func main() {
	if err := gui.Run(defaultServer); err != nil {
		// No console here, so the failure has to be a dialog or it is invisible.
		walk.MsgBox(nil, "P2PV", "Не удалось открыть окно: "+err.Error(),
			walk.MsgBoxIconError)
	}
}
