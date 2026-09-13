//go:build windows

// Package gui is the P2PV window: the Radmin-VPN-shaped view of a network.
//
// What a person needs from a LAN tool is short: am I connected, what is my
// address, who else is here, and what is their address. The window is built
// around exactly that, with the connection form above it and the engine log
// below for when something goes wrong.
package gui

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/Vlchk404/p2pv/internal/client"
	"github.com/Vlchk404/p2pv/internal/netid"
	"github.com/Vlchk404/p2pv/internal/store"
)

// refreshInterval is how often the peer table is redrawn from engine status.
const refreshInterval = 2 * time.Second

// The two reasons the peer table can be empty. Saying which one it is saves the
// user from wondering whether the program is broken.
const (
	hintDisconnected = "Подключитесь к сети, чтобы увидеть остальных участников."
	hintAlone        = "В этой сети пока только вы. Передайте друзьям имя сети и пароль."
)

// Window holds the widgets and the engine they display.
type Window struct {
	mw *walk.MainWindow

	networkBox  *walk.ComboBox
	passwordBox *walk.LineEdit
	connectBtn  *walk.PushButton
	lanCheck    *walk.CheckBox
	serverEdit  *walk.LineEdit

	statusLabel *walk.Label
	ipLabel     *walk.Label
	emptyLabel  *walk.Label
	table       *walk.TableView
	logBox      *walk.TextEdit

	model *peerModel
	tray  *walk.NotifyIcon

	// The engine and its lifecycle. Guarded because the UI thread reads them
	// while the connection goroutine writes them.
	mu        sync.Mutex
	engine    *client.Client
	connected bool

	// defaultServer is what the server field is pre-filled with.
	defaultServer string
}

// Run opens the window and blocks until it is closed.
func Run(defaultServer string) error {
	w := &Window{
		model:         newPeerModel(),
		defaultServer: defaultServer,
	}

	names, _ := store.NetworkNames()

	err := MainWindow{
		AssignTo: &w.mw,
		Title:    "P2PV",
		MinSize:  Size{Width: 620, Height: 520},
		// Tall enough for the form, the table and the log at their minimum
		// heights -- declare less and walk grows the window on open anyway.
		Size:   Size{Width: 720, Height: 620},
		Layout: VBox{MarginsZero: false},
		Children: []Widget{
			// Connection form. Two columns, not four: with four, walk sized the
			// network field off its longest remembered name and squeezed the
			// password box against the right edge. One field per row gives every
			// field the same width and keeps the labels aligned.
			GroupBox{
				Title:  "Подключение",
				Layout: Grid{Columns: 2},
				Children: []Widget{
					Label{Text: "Сеть:"},
					ComboBox{
						AssignTo:              &w.networkBox,
						Editable:              true,
						Model:                 names,
						ToolTipText:           "Имя сети. Первый, кто её создал, задал пароль.",
						OnCurrentIndexChanged: w.onNetworkPicked,
					},

					Label{Text: "Пароль:"},
					LineEdit{
						AssignTo:     &w.passwordBox,
						PasswordMode: true,
						ToolTipText:  "Для запомненной сети можно оставить пустым.",
					},

					Label{Text: "Сервер:"},
					LineEdit{
						AssignTo:    &w.serverEdit,
						Text:        defaultServer,
						ToolTipText: "Координатор. Можно указать свой.",
					},

					// Empty cell so the checkbox lines up under the fields
					// rather than under the labels.
					Label{Text: ""},
					CheckBox{
						AssignTo: &w.lanCheck,
						Text:     "Поиск игр по сети",
						ToolTipText: "Пускает multicast в туннель: игры появляются в списке «Сети LAN». " +
							"Пока включено, принтеры и колонки в реальной локальной сети не находятся.",
					},

					PushButton{
						AssignTo:   &w.connectBtn,
						Text:       "Подключиться",
						ColumnSpan: 2,
						MinSize:    Size{Height: 32},
						OnClicked:  w.onConnectClicked,
					},
				},
			},

			// Status line.
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					Label{AssignTo: &w.statusLabel, Text: "Не подключено"},
					HSpacer{},
					Label{AssignTo: &w.ipLabel, Text: ""},
				},
			},

			// Peer list. The hint above it says why the list is empty -- without
			// it a disconnected window is just a blank grid.
			Label{
				AssignTo: &w.emptyLabel,
				Text:     hintDisconnected,
			},
			TableView{
				AssignTo:         &w.table,
				Model:            w.model,
				MinSize:          Size{Height: 180},
				AlternatingRowBG: true,
				ColumnsOrderable: true,
				Columns: []TableViewColumn{
					{Title: "Адрес", Width: 130},
					{Title: "Компьютер", Width: 260},
					// The last column takes the remaining width, otherwise walk
					// draws an empty filler column to the right of it.
					{Title: "Соединение", Width: 160},
				},
				LastColumnStretched: true,
				OnItemActivated:     w.onPeerActivated,
				ContextMenuItems: []MenuItem{
					Action{Text: "Скопировать адрес", OnTriggered: w.onCopyPeer},
					Action{Text: "Скопировать адрес с портом Minecraft", OnTriggered: w.onCopyPeerMinecraft},
				},
			},

			// Engine log.
			GroupBox{
				Title:  "Журнал",
				Layout: VBox{},
				Children: []Widget{
					TextEdit{
						AssignTo: &w.logBox,
						ReadOnly: true,
						VScroll:  true,
						MinSize:  Size{Height: 110},
					},
				},
			},
		},
	}.Create()
	if err != nil {
		return err
	}

	w.setupTray()
	w.mw.Closing().Attach(w.onClosing)

	if len(names) > 0 {
		w.networkBox.SetText(names[0])
	}
	w.logf("P2PV готов. Введите имя сети и пароль.")

	go w.refreshLoop()

	w.mw.Run()
	return nil
}

// onNetworkPicked clears the password box when a remembered network is chosen,
// since a remembered network does not need one.
func (w *Window) onNetworkPicked() {
	name := strings.TrimSpace(w.networkBox.Text())
	if name == "" {
		return
	}
	if _, err := store.LoadNetwork(strings.ToLower(name)); err == nil {
		w.passwordBox.SetText("")
		w.passwordBox.SetToolTipText("Сеть запомнена -- пароль не нужен.")
	} else {
		w.passwordBox.SetToolTipText("Новая сеть -- нужен пароль.")
	}
}

func (w *Window) onConnectClicked() {
	w.mu.Lock()
	connected := w.connected
	w.mu.Unlock()

	if connected {
		w.disconnect()
		return
	}
	w.connect()
}

func (w *Window) connect() {
	name := strings.TrimSpace(w.networkBox.Text())
	if name == "" {
		w.warn("Укажите имя сети.")
		return
	}
	password := w.passwordBox.Text()

	network, err := resolveNetwork(name, password)
	if err != nil {
		w.warn(err.Error())
		return
	}

	identity, err := store.LoadOrCreateIdentity()
	if err != nil {
		w.warn(fmt.Sprintf("Не удалось прочитать ключ устройства: %v", err))
		return
	}

	server := strings.TrimSpace(w.serverEdit.Text())
	if server == "" {
		server = w.defaultServer
	}
	if server == "" {
		w.warn("Укажите адрес сервера координатора в поле «Сервер».")
		return
	}

	engine, err := client.New(client.Config{
		Server:         server,
		Network:        network,
		Identity:       identity,
		MulticastRoute: w.lanCheck.Checked(),
		Log:            &guiLog{w: w},
	})
	if err != nil {
		w.warn(err.Error())
		return
	}

	if err := store.SaveNetwork(network); err != nil {
		w.logf("предупреждение: сеть не запомнена: %v", err)
	} else {
		w.refreshNetworkList()
	}

	w.mu.Lock()
	w.engine = engine
	w.connected = true
	w.mu.Unlock()

	w.connectBtn.SetText("Отключиться")
	w.statusLabel.SetText(fmt.Sprintf("Подключение к «%s»...", network.Name))
	w.setHint("", false)
	w.setFormEnabled(false)
	w.logf("подключение к «%s» через %s", network.Name, server)

	// The engine blocks until it stops, so it runs off the UI thread. Its
	// failure has to come back to the window: the two usual ones -- no
	// administrator rights and a missing wintun.dll -- both happen here.
	go func() {
		err := engine.Run()
		w.mw.Synchronize(func() {
			w.finishDisconnect()
			if err != nil {
				w.logf("ошибка: %v", err)
				w.statusLabel.SetText("Ошибка подключения")
				w.warn(err.Error())
			} else {
				w.statusLabel.SetText("Не подключено")
			}
		})
	}()
}

func (w *Window) disconnect() {
	w.mu.Lock()
	engine := w.engine
	w.mu.Unlock()

	if engine != nil {
		w.logf("отключение")
		engine.Close()
	}
}

// finishDisconnect resets the window to its disconnected state. Runs on the UI
// thread.
func (w *Window) finishDisconnect() {
	w.mu.Lock()
	w.engine = nil
	w.connected = false
	w.mu.Unlock()

	w.connectBtn.SetText("Подключиться")
	w.ipLabel.SetText("")
	w.setFormEnabled(true)
	w.model.setPeers(nil)
	w.setHint(hintDisconnected, true)
	w.setTrayState(0, 0)
}

func (w *Window) setFormEnabled(enabled bool) {
	w.networkBox.SetEnabled(enabled)
	w.passwordBox.SetEnabled(enabled)
	w.serverEdit.SetEnabled(enabled)
	w.lanCheck.SetEnabled(enabled)
}

// refreshLoop redraws the peer table from engine status.
func (w *Window) refreshLoop() {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	for range ticker.C {
		w.mu.Lock()
		engine := w.engine
		w.mu.Unlock()

		if engine == nil {
			continue
		}
		st := engine.Status()
		w.mw.Synchronize(func() { w.render(st) })
	}
}

func (w *Window) render(st client.Status) {
	if st.VirtualIP == "" {
		w.statusLabel.SetText("Подключение...")
		return
	}

	peers := make([]client.PeerStatus, len(st.Peers))
	copy(peers, st.Peers)
	sort.Slice(peers, func(i, j int) bool { return peers[i].VirtualIP < peers[j].VirtualIP })
	w.model.setPeers(peers)

	online, direct := 0, 0
	for _, p := range peers {
		if p.Connected {
			online++
			if p.Direct {
				direct++
			}
		}
	}

	w.statusLabel.SetText(fmt.Sprintf("Сеть «%s» -- участников онлайн: %d", st.Network, online))
	w.ipLabel.SetText(fmt.Sprintf("этот компьютер: %s (%s)", st.VirtualIP, st.Hostname))
	w.setHint(hintAlone, len(peers) == 0)
	w.setTrayState(online, direct)
}

// setHint shows or hides the explanation above the peer table. The label keeps
// its space when hidden -- letting the table grow and shrink under it would
// make the whole window jump every time the last peer leaves.
func (w *Window) setHint(text string, show bool) {
	if show {
		w.emptyLabel.SetText(text)
	} else {
		w.emptyLabel.SetText("")
	}
}

func (w *Window) onPeerActivated() {
	w.onCopyPeer()
}

func (w *Window) onCopyPeer() {
	if ip := w.selectedIP(); ip != "" {
		w.copy(ip)
	}
}

// onCopyPeerMinecraft copies the address with Minecraft's default server port,
// which is what goes into Direct Connect for a dedicated server.
func (w *Window) onCopyPeerMinecraft() {
	if ip := w.selectedIP(); ip != "" {
		w.copy(ip + ":25565")
	}
}

func (w *Window) selectedIP() string {
	idx := w.table.CurrentIndex()
	p, ok := w.model.peerAt(idx)
	if !ok {
		return ""
	}
	return p.VirtualIP
}

func (w *Window) copy(text string) {
	if err := walk.Clipboard().SetText(text); err != nil {
		w.logf("не удалось скопировать: %v", err)
		return
	}
	w.logf("скопировано: %s", text)
}

func (w *Window) warn(message string) {
	walk.MsgBox(w.mw, "P2PV", message, walk.MsgBoxIconWarning)
}

// logf appends a line to the log pane, from any goroutine.
func (w *Window) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	w.mw.Synchronize(func() {
		stamp := time.Now().Format("15:04:05")
		w.logBox.AppendText(stamp + "  " + line + "\r\n")
	})
}

// resolveNetwork turns what the user typed into a network key: a typed password
// wins, otherwise a remembered network is used, otherwise it is an error worth
// explaining rather than a silent failure.
func resolveNetwork(name, password string) (*netid.Network, error) {
	if password != "" {
		return netid.Derive(name, password)
	}
	n, err := store.LoadNetwork(strings.ToLower(strings.TrimSpace(name)))
	if err != nil {
		return nil, fmt.Errorf("сеть «%s» ещё не запомнена -- введите пароль", name)
	}
	return n, nil
}

func (w *Window) refreshNetworkList() {
	names, err := store.NetworkNames()
	if err != nil {
		return
	}
	current := w.networkBox.Text()
	_ = w.networkBox.SetModel(names)
	w.networkBox.SetText(current)
}

// guiLog routes the engine's log into the window.
type guiLog struct{ w *Window }

func (g *guiLog) Write(p []byte) (int, error) {
	g.w.logf("%s", strings.TrimRight(string(p), "\r\n"))
	return len(p), nil
}
