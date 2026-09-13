//go:build windows

package gui

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/lxn/walk"

	"github.com/Vlchk404/p2pv/internal/tray"
)

// The window minimises to the tray rather than to the taskbar: this is a thing
// that runs for a whole evening of playing, and it should stay out of the way
// without being easy to close by accident.

func (w *Window) setupTray() {
	ni, err := walk.NewNotifyIcon(w.mw)
	if err != nil {
		w.logf("значок в трее недоступен: %v", err)
		return
	}
	w.tray = ni

	if icon, err := trayIcon("offline.ico"); err == nil {
		_ = ni.SetIcon(icon)
	}
	_ = ni.SetToolTip("P2PV -- не подключено")
	_ = ni.SetVisible(true)

	// Left click restores the window, which is what people expect from a tray
	// icon and what Radmin VPN does.
	ni.MouseUp().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			w.restore()
		}
	})

	show := walk.NewAction()
	_ = show.SetText("Показать окно")
	show.Triggered().Attach(w.restore)
	_ = ni.ContextMenu().Actions().Add(show)

	quit := walk.NewAction()
	_ = quit.SetText("Выход")
	quit.Triggered().Attach(func() {
		w.disconnect()
		walk.App().Exit(0)
	})
	_ = ni.ContextMenu().Actions().Add(quit)
}

func (w *Window) restore() {
	w.mw.Show()
	win := w.mw.AsFormBase()
	_ = win
	w.mw.SetVisible(true)
	w.mw.BringToTop()
}

// onClosing hides the window to the tray instead of quitting, unless the
// connection is already down -- closing a window should not silently drop
// everyone's game.
func (w *Window) onClosing(canceled *bool, reason walk.CloseReason) {
	w.mu.Lock()
	connected := w.connected
	w.mu.Unlock()

	if !connected || w.tray == nil {
		w.disconnect()
		return
	}

	*canceled = true
	w.mw.Hide()
	if w.tray != nil {
		_ = w.tray.ShowInfo("P2PV", "Продолжаю работать в трее. Правый клик -- выход.")
	}
}

// setTrayState keeps the tray icon and its tooltip in step with the connection.
func (w *Window) setTrayState(online, direct int) {
	if w.tray == nil {
		return
	}

	name := "offline.ico"
	tip := "P2PV -- не подключено"
	switch {
	case online == 0:
	case direct < online:
		name = "relayed.ico"
		tip = fmt.Sprintf("P2PV -- онлайн: %d, часть через сервер", online)
	default:
		name = "connected.ico"
		tip = fmt.Sprintf("P2PV -- онлайн: %d, все напрямую", online)
	}

	if icon, err := trayIcon(name); err == nil {
		_ = w.tray.SetIcon(icon)
	}
	_ = w.tray.SetToolTip(tip)
}

// trayIcon loads one of the icons embedded in the tray package, so both the
// tray build and the window build show the same artwork.
//
// walk reads icons from files and Go has no ICO decoder, so the embedded bytes
// are spilled to a temp file once and cached by name.
func trayIcon(name string) (*walk.Icon, error) {
	iconMu.Lock()
	defer iconMu.Unlock()

	if icon, ok := iconCache[name]; ok {
		return icon, nil
	}

	data, err := tray.IconBytes(name)
	if err != nil {
		return nil, err
	}

	dir, err := iconDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}

	icon, err := walk.NewIconFromFile(path)
	if err != nil {
		return nil, err
	}
	iconCache[name] = icon
	return icon, nil
}

var (
	iconMu    sync.Mutex
	iconCache = map[string]*walk.Icon{}
	iconPath  string
)

func iconDir() (string, error) {
	if iconPath != "" {
		return iconPath, nil
	}
	dir, err := os.MkdirTemp("", "p2pv-icons-")
	if err != nil {
		return "", err
	}
	iconPath = dir
	return dir, nil
}
