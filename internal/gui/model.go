//go:build windows

package gui

import (
	"sync"

	"github.com/lxn/walk"

	"github.com/Vlchk404/p2pv/internal/client"
)

// peerModel feeds the peer table.
//
// walk pulls each cell through Value, so the snapshot behind it is guarded: the
// refresh goroutine replaces it while the UI thread is reading.
type peerModel struct {
	walk.TableModelBase

	mu    sync.RWMutex
	peers []client.PeerStatus
}

func newPeerModel() *peerModel { return &peerModel{} }

func (m *peerModel) RowCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.peers)
}

func (m *peerModel) Value(row, col int) any {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if row < 0 || row >= len(m.peers) {
		return ""
	}
	p := m.peers[row]

	switch col {
	case 0:
		return p.VirtualIP
	case 1:
		return p.Hostname
	case 2:
		return connectionText(p)
	}
	return ""
}

// connectionText is the human answer to "how am I talking to this person".
func connectionText(p client.PeerStatus) string {
	switch {
	case p.Connected && p.Direct:
		return "напрямую"
	case p.Connected:
		return "через сервер"
	default:
		return "подключается"
	}
}

func (m *peerModel) setPeers(peers []client.PeerStatus) {
	m.mu.Lock()
	changed := len(m.peers) != len(peers)
	m.peers = peers
	m.mu.Unlock()

	// PublishRowsReset rebuilds the whole table and loses the selection, so it
	// is only used when the row count actually changes. Otherwise the cells are
	// refreshed in place, which keeps a selected row selected while its state
	// column changes from "через сервер" to "напрямую".
	if changed {
		m.PublishRowsReset()
		return
	}
	if n := len(peers); n > 0 {
		m.PublishRowsChanged(0, n-1)
	}
}

func (m *peerModel) peerAt(row int) (client.PeerStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if row < 0 || row >= len(m.peers) {
		return client.PeerStatus{}, false
	}
	return m.peers[row], true
}
