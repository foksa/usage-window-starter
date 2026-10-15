package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"electrobun"

	"usagewindowstarter/src/go/core"
)

const appName = "Usage Window Starter"

// secretKey is required by the webview API; with the RPC socket off (port 0) packets
// go over the in-process host bridge.
const secretKey = "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32"

var app struct {
	core      *electrobun.Core
	bundle    electrobun.BundlePaths
	viewsDir  string
	trayID    uint32
	store     *core.Store
	scheduler *core.Scheduler
	updates   *Updates
	lock      *os.File
}

func runApp() error {
	// One copy per data folder: two would both check and send start messages.
	lock, pid, err := core.AcquireInstanceLock()
	if errors.Is(err, core.ErrAlreadyRunning) {
		// Show the running copy's panel instead, so opening the app again isn't a no-op.
		if err := core.RequestShow(8 * time.Second); !errors.Is(err, core.ErrNotResponding) || pid <= 0 {
			return nil
		}
		// It took the request but didn't answer, so it's hung: replace it.
		fmt.Fprintln(os.Stderr, "[usage-window-starter] running copy", pid, "is not responding; replacing it")
		if lock, err = core.StopInstance(pid); err != nil {
			return fmt.Errorf("replace hung copy %d: %w", pid, err)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "[usage-window-starter] instance lock:", err)
	}
	app.lock = lock // held until the process exits

	c, err := electrobun.LoadCore()
	if err != nil {
		return err
	}
	bundle, err := electrobun.ResolveBundlePaths()
	if err != nil {
		return err
	}
	info, err := electrobun.ResolveAppInfoFromBundle(bundle)
	if err != nil {
		return err
	}
	app.core = c
	app.bundle = bundle
	app.viewsDir = filepath.Join(bundle.ResourcesDir, "app", "views")

	core.Notifier = func(title, body string) {
		_ = c.ShowNotification(electrobun.NotificationOptions{Title: title, Body: body})
	}
	if core.RemoveLegacyDaemon() {
		core.Notify(appName, "Replaced the old background daemon — the menu bar app now runs the checks.")
	}
	migrateLoginItem()

	app.store = core.OpenStore()
	app.scheduler = core.NewScheduler(app.store, refreshTray)
	app.updates = newUpdates(bundle, refreshTray)

	go startUI()
	go drainHostMessages()
	return c.RunMainThread(info)
}

// startUI runs once the native event loop is up.
func startUI() {
	time.Sleep(150 * time.Millisecond)
	c := app.core
	_ = c.SetExitOnLastWindowClosed(false) // a menu bar app: closing Settings must not quit it
	_ = c.SetDockIconVisible(false)
	if err := c.ConfigureWebviewRuntimeFromExecutableDir(app.bundle, 0); err != nil {
		fmt.Fprintln(os.Stderr, "[usage-window-starter] webview runtime:", err)
		return
	}

	trayID, err := c.CreateTray(electrobun.TrayOptions{
		Title:      "usage…",
		Image:      filepath.Join(app.viewsDir, "assets", "tray-template.png"),
		IsTemplate: true,
		Width:      18,
		Height:     18,
		Handler:    func(uint32, string) { togglePanel() },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "[usage-window-starter] tray:", err)
		return
	}
	app.trayID = trayID
	syncPanelLoaded() // without KeepPanelLoaded, the panel is created on first click

	refreshTray()
	app.scheduler.Run()
	app.updates.run()
	if os.Getenv("USAGE_WINDOW_STARTER_OPEN_SETTINGS") != "" {
		openSettings()
	}
	if os.Getenv("USAGE_WINDOW_STARTER_OPEN_PANEL") != "" {
		time.AfterFunc(1500*time.Millisecond, showPanel)
	}
	// A second copy that was started asks this one to show its panel, and the CLI
	// asks it to start sessions.
	if _, err := core.ServeRequests(core.Handlers{Show: showPanel, Start: app.scheduler.StartNow}); err != nil {
		fmt.Fprintln(os.Stderr, "[usage-window-starter] requests:", err)
	}
	// Keep the countdown in the menu bar fresh between checks.
	go func() {
		for range time.Tick(30 * time.Second) {
			refreshTray()
		}
	}()
}

func quit() {
	app.scheduler.Stop()
	_ = app.core.RemoveTray(app.trayID)
	_ = app.core.StopEventLoop()
}

var short = map[core.Provider]string{core.Claude: "C", core.Codex: "X"}

// trayTitle is the menu bar text: time left in each running 5h window, e.g.
// "C 4:49 · X 2:38". Outside active hours nothing is checked, so it shows
// "C – · X –" instead of stale data. With times turned off it's just the icon, plus
// "!" after an error and "⏸" when paused.
func trayTitle() string {
	cfg := app.store.Config()
	state := app.store.State()
	now := time.Now().UnixMilli()
	if !cfg.TrayShowTimes {
		var parts []string
		for _, p := range core.Providers {
			if cfg.Enabled(p) && state[p] != nil && state[p].LastError != "" {
				parts = append(parts, "!")
				break
			}
		}
		if !cfg.AutoStart {
			parts = append(parts, "⏸")
		}
		return strings.Join(parts, " ")
	}
	resting := !core.InActiveHours(cfg.ActiveHours, now)
	var parts []string
	for _, p := range core.Providers {
		if !cfg.Enabled(p) {
			continue
		}
		st := state[p]
		var part string
		// No "…" during checks: the text would change width and move the icon. The
		// panel shows "Checking…" instead.
		switch {
		case resting:
			part = "–"
		case st != nil && st.LastError != "":
			part = "!"
		case st == nil || st.LastSnapshot == nil:
			part = "?"
		default:
			w := st.LastSnapshot.FiveHour
			switch {
			case !w.Active || (w.ResetsAt != nil && *w.ResetsAt <= now):
				part = "–"
			case w.ResetsAt == nil:
				part = "on"
			default:
				part = core.FmtShort(*w.ResetsAt - now)
			}
		}
		parts = append(parts, short[p]+" "+part)
	}
	title := strings.Join(parts, " · ")
	if title == "" {
		title = appName
	}
	if !cfg.AutoStart {
		title += " ⏸"
	}
	return title
}

var refreshMu sync.Mutex

func refreshTray() {
	if app.trayID == 0 {
		return
	}
	refreshMu.Lock()
	defer refreshMu.Unlock()
	_ = app.core.SetTrayTitle(app.trayID, trayTitle())
	if panelVisible() {
		_, webviewID := panelIDs()
		sendMessage(webviewID, "state", panelState())
	}
}

// decode unmarshals RPC params, treating empty params as {}.
func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, v)
}
