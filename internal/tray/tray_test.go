package tray

import (
	"fmt"
	"sync"
	"testing"
)

// The tracker is read by click handlers and written by the render loop, on
// different goroutines. These tests pin both the indexing rules and the
// concurrency, since a wrong index here would copy one peer's address onto
// another peer's menu row.

func TestTrackSelfAndPeers(t *testing.T) {
	t.Cleanup(reset)

	track(-1, "100.88.0.2")
	track(0, "100.88.0.3")
	track(1, "100.88.0.4")

	if got := trackedIP(-1); got != "100.88.0.2" {
		t.Errorf("self: got %q, want 100.88.0.2", got)
	}
	if got := trackedIP(0); got != "100.88.0.3" {
		t.Errorf("peer 0: got %q, want 100.88.0.3", got)
	}
	if got := trackedIP(1); got != "100.88.0.4" {
		t.Errorf("peer 1: got %q, want 100.88.0.4", got)
	}
}

func TestTrackClearedRowReturnsEmpty(t *testing.T) {
	t.Cleanup(reset)

	// A row that goes from occupied to hidden must stop reporting the old
	// address, or clicking a stale row would copy a peer who has left.
	track(3, "100.88.0.9")
	track(3, "")

	if got := trackedIP(3); got != "" {
		t.Errorf("cleared row: got %q, want empty", got)
	}
}

func TestTrackOutOfRangeIsIgnored(t *testing.T) {
	t.Cleanup(reset)

	// Out-of-range writes must not panic: peerSlots caps the menu, but the
	// peer list from the server is not bounded by it.
	track(peerSlots, "100.88.0.50")
	track(peerSlots+10, "100.88.0.51")

	if got := trackedIP(peerSlots); got != "" {
		t.Errorf("out of range: got %q, want empty", got)
	}
}

func TestTrackConcurrentAccess(t *testing.T) {
	t.Cleanup(reset)

	// Run under -race: this is the pattern the render loop and the click
	// handlers actually produce.
	var wg sync.WaitGroup
	for i := 0; i < peerSlots; i++ {
		wg.Add(2)
		go func(idx int) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				track(idx, fmt.Sprintf("100.88.0.%d", idx))
			}
		}(i)
		go func(idx int) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				_ = trackedIP(idx)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < peerSlots; i++ {
		want := fmt.Sprintf("100.88.0.%d", i)
		if got := trackedIP(i); got != want {
			t.Errorf("peer %d: got %q, want %q", i, got, want)
		}
	}
}

func TestClipboardCommandsNonEmpty(t *testing.T) {
	// Every supported platform needs at least one way to copy, or the menu's
	// click action would be silently useless.
	cmds := clipboardCommands()
	if len(cmds) == 0 {
		t.Fatal("no clipboard commands for this platform")
	}
	for _, c := range cmds {
		if len(c) == 0 || c[0] == "" {
			t.Errorf("empty clipboard command in %v", cmds)
		}
	}
}

func reset() {
	trackMu.Lock()
	defer trackMu.Unlock()
	trackSelf = ""
	for i := range trackPeer {
		trackPeer[i] = ""
	}
}
