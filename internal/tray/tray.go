// Package tray runs P2PV as a system tray icon.
//
// The tray is the primary interface on Windows: the thing people actually want
// is a small icon that says whether they are connected, what their address is,
// and who else is online -- which is what Radmin VPN's window is for.
//
// The client engine runs in this same process rather than talking to a service.
// That means the tray needs administrator rights, exactly as the CLI does, and
// in exchange there is no privileged daemon sitting on the machine when nobody
// is playing.
package tray

import (
	"embed"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/Vlchk404/p2pv/internal/client"
)

//go:embed icons/*.ico
var iconFS embed.FS

// peerSlots is how many peers the menu can show.
//
// systray can hide a menu item but not remove one, so the rows are allocated up
// front and hidden while unused. Twelve covers the group of friends this is for;
// any beyond that are summarised in one line rather than silently dropped.
const peerSlots = 12

// refreshInterval is how often the menu is redrawn from client status.
const refreshInterval = 3 * time.Second

// Config describes what the tray should connect to. It carries a ready-made
// client config so that credential handling stays in one place -- the caller
// resolves the password, the tray never prompts.
type Config struct {
	Client client.Config
	Log    *log.Logger
}

type menu struct {
	status   *systray.MenuItem
	address  *systray.MenuItem
	peers    []*systray.MenuItem
	overflow *systray.MenuItem
	quit     *systray.MenuItem
}

// Run shows the tray icon and runs the client until the user quits.
//
// It must be called from the main goroutine: systray takes over the thread's
// message loop, which on Windows has to be the main thread.
func Run(cfg Config) error {
	if cfg.Log == nil {
		cfg.Log = log.New(log.Writer(), "", log.LstdFlags)
	}

	c, err := client.New(cfg.Client)
	if err != nil {
		return err
	}

	// The client's error is captured here and reported after systray returns,
	// so a failure to bring up the interface is not swallowed by the UI.
	errCh := make(chan error, 1)

	onReady := func() {
		m := build(cfg)
		setIcon("offline.ico")

		// A failed engine has to be visible in the menu, not just returned at
		// exit. The two most common first-run failures -- no administrator
		// rights and a missing wintun.dll -- both happen here, and a tray that
		// says "Connecting..." forever gives the user nothing to act on.
		go func() {
			err := c.Run()
			if err != nil {
				cfg.Log.Printf("tunnel failed: %v", err)
				setFailure(err)
				render(c.Status(), m) // now, not on the next tick
			}
			errCh <- err
		}()
		go watch(c, m)
		go func() {
			<-m.quit.ClickedCh
			c.Close()
			systray.Quit()
		}()
	}

	systray.Run(onReady, func() { c.Close() })

	// systray.Run returns once Quit is called; report whatever the engine hit.
	select {
	case err := <-errCh:
		return err
	case <-time.After(time.Second):
		return nil
	}
}

func build(cfg Config) *menu {
	systray.SetTitle("P2PV")
	systray.SetTooltip("P2PV -- connecting...")

	m := &menu{}
	m.status = systray.AddMenuItem("Connecting...", "")
	m.status.Disable()
	m.address = systray.AddMenuItem("This machine: --", "Click to copy this address")

	systray.AddSeparator()
	header := systray.AddMenuItem("Peers", "")
	header.Disable()

	for i := 0; i < peerSlots; i++ {
		item := systray.AddMenuItem("", "Click to copy this address")
		item.Hide()
		m.peers = append(m.peers, item)

		// The item is captured directly, not looked up as m.peers[idx]: the
		// loop is still appending to that slice, and a goroutine reading it
		// concurrently would be a data race. The address is read through the
		// tracker so a click copies what the row shows now, not what it showed
		// when the row was created.
		go func(item *systray.MenuItem, idx int) {
			for range item.ClickedCh {
				if ip := trackedIP(idx); ip != "" {
					copyToClipboard(ip, cfg.Log)
				}
			}
		}(item, i)
	}

	m.overflow = systray.AddMenuItem("", "")
	m.overflow.Disable()
	m.overflow.Hide()

	systray.AddSeparator()
	m.quit = systray.AddMenuItem("Disconnect and quit", "Leave the network")

	go func() {
		for range m.address.ClickedCh {
			if ip := trackedIP(-1); ip != "" {
				copyToClipboard(ip, cfg.Log)
			}
		}
	}()

	return m
}

// watch redraws the menu from client status on a timer.
func watch(c *client.Client, m *menu) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for range ticker.C {
		render(c.Status(), m)
	}
}

func render(st client.Status, m *menu) {
	// A failure outranks any status: the engine is no longer running, so the
	// status snapshot will never change again.
	if err := failure(); err != nil {
		headline, _, _ := strings.Cut(err.Error(), "\n")
		m.status.SetTitle("Failed: " + headline)
		m.address.SetTitle("This machine: --")
		systray.SetTooltip("P2PV -- " + headline)
		setIcon("offline.ico")
		track(-1, "")
		for _, item := range m.peers {
			item.Hide()
		}
		m.overflow.Hide()
		return
	}

	if st.VirtualIP == "" {
		m.status.SetTitle("Connecting...")
		m.address.SetTitle("This machine: --")
		systray.SetTooltip("P2PV -- connecting...")
		setIcon("offline.ico")
		track(-1, "")
		for _, item := range m.peers {
			item.Hide()
		}
		m.overflow.Hide()
		return
	}

	online, direct := 0, 0
	for _, p := range st.Peers {
		if p.Connected {
			online++
			if p.Direct {
				direct++
			}
		}
	}

	m.status.SetTitle(fmt.Sprintf("%s -- %d online", st.Network, online))
	m.address.SetTitle(fmt.Sprintf("This machine: %s", st.VirtualIP))
	track(-1, st.VirtualIP)
	systray.SetTooltip(fmt.Sprintf("P2PV -- %s as %s, %d peers online",
		st.Network, st.VirtualIP, online))

	// Icon colour reports the worst live path, because that is what the user
	// would want to know: amber means somebody is on the slower relay route.
	switch {
	case online == 0:
		setIcon("offline.ico")
	case direct < online:
		setIcon("relayed.ico")
	default:
		setIcon("connected.ico")
	}

	// Stable order, so rows do not jump around between refreshes.
	peers := make([]client.PeerStatus, len(st.Peers))
	copy(peers, st.Peers)
	sort.Slice(peers, func(i, j int) bool { return peers[i].VirtualIP < peers[j].VirtualIP })

	shown := len(peers)
	if shown > peerSlots {
		shown = peerSlots
	}

	for i := 0; i < shown; i++ {
		p := peers[i]
		state := "connecting"
		switch {
		case p.Connected && p.Direct:
			state = "direct"
		case p.Connected:
			state = "relayed"
		}
		m.peers[i].SetTitle(fmt.Sprintf("%s  %s  (%s)", p.VirtualIP, p.Hostname, state))
		track(i, p.VirtualIP)
		m.peers[i].Show()
	}
	for i := shown; i < peerSlots; i++ {
		m.peers[i].Hide()
		track(i, "")
	}

	if extra := len(peers) - shown; extra > 0 {
		m.overflow.SetTitle(fmt.Sprintf("... and %d more", extra))
		m.overflow.Show()
	} else {
		m.overflow.Hide()
	}
}

func setIcon(name string) {
	data, err := iconFS.ReadFile("icons/" + name)
	if err != nil {
		return // embedded at build time; nothing useful to do at runtime
	}
	systray.SetIcon(data)
}

// IconBytes returns one of the embedded icons by file name, so the window build
// shows the same artwork as the tray build rather than keeping a second copy.
func IconBytes(name string) ([]byte, error) {
	return iconFS.ReadFile("icons/" + name)
}

// The engine's terminal error, if it had one. Written by the goroutine running
// the client and read by the render loop.
var (
	failMu  sync.RWMutex
	failErr error
)

func setFailure(err error) {
	failMu.Lock()
	failErr = err
	failMu.Unlock()
}

func failure() error {
	failMu.RLock()
	defer failMu.RUnlock()
	return failErr
}
