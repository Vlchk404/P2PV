package tray

import (
	"log"
	"os/exec"
	"strings"
	"sync"
)

// The addresses currently displayed, so a menu click copies what the row shows
// right now. The render loop and the click handlers run on different
// goroutines, hence the mutex.
var (
	trackMu   sync.RWMutex
	trackSelf string
	trackPeer [peerSlots]string
)

// track records the address shown in a row. Index -1 is this machine.
func track(idx int, ip string) {
	trackMu.Lock()
	defer trackMu.Unlock()
	if idx < 0 {
		trackSelf = ip
		return
	}
	if idx < peerSlots {
		trackPeer[idx] = ip
	}
}

// trackedIP reads back the address shown in a row. Index -1 is this machine.
func trackedIP(idx int) string {
	trackMu.RLock()
	defer trackMu.RUnlock()
	if idx < 0 {
		return trackSelf
	}
	if idx < peerSlots {
		return trackPeer[idx]
	}
	return ""
}

// copyToClipboard puts an address on the clipboard.
//
// Shelling out to the platform tool avoids a GUI toolkit dependency for one
// small feature. It is best-effort: a missing tool on Linux logs and is
// otherwise ignored, because failing to copy an address should never interrupt
// a running tunnel.
func copyToClipboard(text string, logger *log.Logger) {
	for _, candidate := range clipboardCommands() {
		cmd := exec.Command(candidate[0], candidate[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			logger.Printf("copied %s to the clipboard", text)
			return
		}
	}
	logger.Printf("could not copy %s: no clipboard tool available", text)
}
