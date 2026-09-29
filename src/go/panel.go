package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"electrobun"

	"usagewindowstarter/src/go/core"
)

// The panel is the dropdown under the menu bar icon: a hidden, frameless,
// see-through window that is shown centred under the icon.
const panelWidth = 340

var panel struct {
	mu        sync.Mutex
	windowID  uint32
	webviewID uint32
	height    float64
	// x, y is where the panel was placed when it opened. It stays there while open:
	// the menu bar text changes width during checks, which moves the icon.
	x, y float64
	// blurredAt is when the panel last hid on losing focus; a click on the icon
	// itself causes that too.
	blurredAt time.Time
	// idle closes the panel once hidden (after panelIdle), which ends its WebKit
	// processes (about 40 MB).
	idle *time.Timer
}

// panelIdle is how long a hidden panel is kept. Recreating it takes about 0.2 s,
// which isn't worth ~40 MB, so by default it goes right away.
var panelIdle time.Duration

func init() {
	panel.height = 320
	// To keep it around instead: USAGE_WINDOW_STARTER_PANEL_IDLE=3m.
	if d, err := time.ParseDuration(os.Getenv("USAGE_WINDOW_STARTER_PANEL_IDLE")); err == nil && d > 0 {
		panelIdle = d
	}
}

func panelIDs() (windowID, webviewID uint32) {
	panel.mu.Lock()
	defer panel.mu.Unlock()
	return panel.windowID, panel.webviewID
}

type panelProvider struct {
	ID       core.Provider     `json:"id"`
	Label    string            `json:"label"`
	Model    string            `json:"model"`
	Enabled  bool              `json:"enabled"`
	Busy     bool              `json:"busy"`
	Activity string            `json:"activity"` // "checking", "starting" or ""
	FiveHour *core.LimitWindow `json:"fiveHour"`
	Weekly   *core.LimitWindow `json:"weekly"`
	Error    string            `json:"error,omitempty"`
}

type panelStatePayload struct {
	Providers  []panelProvider `json:"providers"`
	AutoStart  bool            `json:"autoStart"`
	Resting    bool            `json:"resting"`
	AppVersion string          `json:"appVersion"`
	ActiveFrom *string         `json:"activeFrom"`
	Update     struct {
		Phase   string `json:"phase"`
		Version string `json:"version"`
	} `json:"update"`
}

func panelState() panelStatePayload {
	cfg := core.LoadConfig()
	state := core.LoadState()
	var s panelStatePayload
	for _, p := range core.Providers {
		pp := panelProvider{ID: p, Label: core.Label[p], Model: cfg.Model(p), Enabled: cfg.Enabled(p), Busy: app.scheduler.IsBusy(p), Activity: app.scheduler.Activity(p)}
		if st := state[p]; st != nil {
			pp.Error = st.LastError
			if st.LastSnapshot != nil {
				pp.FiveHour = &st.LastSnapshot.FiveHour
				pp.Weekly = st.LastSnapshot.Weekly
			}
		}
		s.Providers = append(s.Providers, pp)
	}
	s.AutoStart = cfg.AutoStart
	s.AppVersion = core.AppVersion
	s.Resting = !core.InActiveHours(cfg.ActiveHours, time.Now().UnixMilli())
	if cfg.ActiveHours != nil {
		s.ActiveFrom = &cfg.ActiveHours.Start
	}
	s.Update.Phase, s.Update.Version = app.updates.status()
	return s
}

func panelVisible() bool {
	id, _ := panelIDs()
	return id != 0 && app.core.IsWindowVisible(id)
}

func createPanel() {
	c := app.core
	opts := electrobun.NewWindowOptions(appName, electrobun.NewRect(0, 0, panelWidth, panel.height))
	opts.Style = electrobun.WindowStyle{FullSizeContentView: true}
	opts.TitleBarStyle = "hidden"
	opts.Transparent = true
	opts.Hidden = true
	opts.Activate = false
	// Visibility already reads false here when the panel lost focus to another app,
	// so record every blur.
	opts.Callbacks = electrobun.WindowCallbacks{Blur: func(uint32) {
		panel.mu.Lock()
		panel.blurredAt = time.Now()
		panel.mu.Unlock()
		hidePanel()
	}}
	windowID, err := c.CreateWindow(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[usage-window-starter] panel window:", err)
		return
	}
	_ = c.SetWindowAlwaysOnTop(windowID, true)
	// Otherwise, after switching Spaces, it would reopen on the Space it was last shown on.
	_ = c.SetWindowVisibleOnAllWorkspaces(windowID, true)

	wv := electrobun.NewWebviewOptions(windowID, "views://panel/index.html", electrobun.NewRect(0, 0, panelWidth, panel.height))
	wv.SecretKey = secretKey
	wv.Partition = "persist:default"
	wv.Callbacks = electrobun.WebviewCallbacks{
		DecideNavigation: electrobun.AllowAllNavigation,
		Event:            electrobun.NoopWebviewEvent,
		EventBridge:      electrobun.NoopWebviewPostMessage,
		HostBridge:       hostBridge,
	}
	webviewID, err := c.CreateWebview(wv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[usage-window-starter] panel view:", err)
		return
	}
	registerView(webviewID, &rpcHandlers{
		requests: map[string]func(json.RawMessage) (any, error){
			"getState": func(json.RawMessage) (any, error) { return panelState(), nil },
		},
		messages: map[string]func(json.RawMessage){
			"action": onPanelAction,
			"resize": func(raw json.RawMessage) {
				var r struct {
					Height float64 `json:"height"`
				}
				if decode(raw, &r) != nil || r.Height <= 0 {
					return
				}
				panel.mu.Lock()
				changed := r.Height != panel.height
				panel.height = r.Height
				x, y, id := panel.x, panel.y, panel.windowID
				panel.mu.Unlock()
				// Also while hidden: the page can report its height between placePanel
				// and ShowWindow.
				if changed && id != 0 {
					_ = app.core.SetWindowFrame(id, electrobun.NewRect(x, y, panelWidth, r.Height))
				}
			},
			"close": func(json.RawMessage) { hidePanel() },
		},
	})
	panel.mu.Lock()
	panel.windowID, panel.webviewID = windowID, webviewID
	panel.mu.Unlock()
}

// destroyPanel closes the hidden panel; the next click creates it again.
func destroyPanel() {
	if panelVisible() {
		return
	}
	panel.mu.Lock()
	windowID, webviewID := panel.windowID, panel.webviewID
	panel.windowID, panel.webviewID = 0, 0
	panel.mu.Unlock()
	if windowID == 0 {
		return
	}
	forgetView(webviewID)
	_ = app.core.CloseWindow(windowID)
}

// placePanel centres the panel under the menu bar icon, kept inside that screen.
// Only on opening; see panel.x.
func placePanel() {
	c := app.core
	icon, err := c.GetTrayBounds(app.trayID)
	if err != nil {
		return
	}
	primary, err := c.GetPrimaryDisplay()
	if err != nil {
		return
	}
	// Tray bounds come in Cocoa coordinates (origin at the bottom-left of the primary
	// screen); displays, work areas and windows use a top-left origin.
	iconTop := primary.Bounds.Height - (icon.Y + icon.Height)
	cx, cy := icon.X+icon.Width/2, iconTop+icon.Height/2
	display := primary
	if all, err := c.GetAllDisplays(); err == nil {
		for _, d := range all {
			b := d.Bounds
			if cx >= b.X && cx < b.X+b.Width && cy >= b.Y && cy < b.Y+b.Height {
				display = d
				break
			}
		}
	}
	area := display.WorkArea
	x := math.Round(math.Min(math.Max(cx-panelWidth/2, area.X+8), area.X+area.Width-panelWidth-8))
	y := math.Round(iconTop + icon.Height + 4)
	panel.mu.Lock()
	h, id := panel.height, panel.windowID
	panel.x, panel.y = x, y
	panel.mu.Unlock()
	_ = c.SetWindowFrame(id, electrobun.NewRect(x, y, panelWidth, h))
}

func showPanel() {
	panel.mu.Lock()
	if panel.idle != nil {
		panel.idle.Stop()
	}
	created := panel.windowID != 0
	panel.mu.Unlock()
	cfg := core.LoadConfig()
	if cfg.RefreshOnOpenSec > 0 {
		go app.scheduler.Refresh(time.Duration(cfg.RefreshOnOpenSec)*time.Second, time.Now().UnixMilli())
	}
	if !created {
		createPanel()
	}
	// Show it now, while still handling the click: macOS only lets the app take focus
	// in response to one, and a new panel's page can take seconds to load when WebKit
	// is cold (after a while idle). Shown later, the panel didn't get focus or hid
	// again at once, so it took several clicks. Until the page paints, the window is
	// see-through.
	revealPanel()
}

func revealPanel() {
	windowID, webviewID := panelIDs()
	if windowID == 0 {
		return
	}
	sendMessage(webviewID, "state", panelState())
	placePanel()
	_ = app.core.ShowWindow(windowID, true)
	_ = app.core.ActivateWindow(windowID)
}

func hidePanel() {
	windowID, _ := panelIDs()
	if windowID == 0 {
		return
	}
	_ = app.core.HideWindow(windowID)
	panel.mu.Lock()
	if panel.idle != nil {
		panel.idle.Stop()
	}
	// Always via a timer: never close the window inside its own blur callback.
	panel.idle = time.AfterFunc(panelIdle, destroyPanel)
	panel.mu.Unlock()
}

func togglePanel() {
	if panelVisible() {
		hidePanel()
		return
	}
	panel.mu.Lock()
	sinceBlur := time.Since(panel.blurredAt)
	panel.mu.Unlock()
	// This click just took focus from the panel and hid it: leave it closed.
	if sinceBlur > 300*time.Millisecond {
		showPanel()
	}
}

func onPanelAction(raw json.RawMessage) {
	var a struct {
		Name     string        `json:"name"`
		Provider core.Provider `json:"provider"`
	}
	if decode(raw, &a) != nil {
		return
	}
	switch a.Name {
	case "check":
		app.scheduler.RunDue(true, time.Now().UnixMilli())
	case "start":
		if a.Provider == core.Claude || a.Provider == core.Codex {
			app.scheduler.StartNow(a.Provider)
		}
	case "toggleAuto":
		cfg := core.LoadConfig()
		cfg.AutoStart = !cfg.AutoStart
		_ = core.SaveConfig(cfg)
		refreshTray()
		if cfg.AutoStart {
			app.scheduler.RunDue(true, time.Now().UnixMilli())
		}
	case "settings":
		hidePanel()
		openSettings()
	case "log":
		hidePanel()
		_, _ = app.core.OpenPath(core.LogFile)
	case "updateCheck":
		app.updates.check(true)
	case "updateInstall":
		hidePanel()
		app.updates.install()
	case "quit":
		quit()
	}
}
