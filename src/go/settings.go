package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"electrobun"

	"usagewindowstarter/src/go/core"
)

var settings struct {
	mu        sync.Mutex
	windowID  uint32
	webviewID uint32
}

type settingsPayload struct {
	Config           core.Config        `json:"config"`
	LaunchAtLogin    bool               `json:"launchAtLogin"`
	CanLaunchAtLogin bool               `json:"canLaunchAtLogin"` // false outside a .app bundle (dev)
	ClaudeModels     []core.ModelOption `json:"claudeModels"`
	AppVersion       string             `json:"appVersion"`
}

type okResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Reply string `json:"reply,omitempty"`
}

func openSettings() {
	c := app.core
	settings.mu.Lock()
	defer settings.mu.Unlock()
	if settings.windowID != 0 {
		_ = c.ShowWindow(settings.windowID, true)
		_ = c.ActivateWindow(settings.windowID)
		return
	}
	const w, h = 560, 720
	opts := electrobun.NewWindowOptions(appName+" settings", electrobun.NewRect(0, 0, w, h))
	opts.Centered = true
	opts.Callbacks = electrobun.WindowCallbacks{Close: func(uint32) {
		settings.mu.Lock()
		forgetView(settings.webviewID)
		settings.windowID, settings.webviewID = 0, 0
		settings.mu.Unlock()
	}}
	windowID, err := c.CreateWindow(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[usage-window-starter] settings window:", err)
		return
	}
	wv := electrobun.NewWebviewOptions(windowID, "views://settings/index.html", electrobun.NewRect(0, 0, w, h))
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
		fmt.Fprintln(os.Stderr, "[usage-window-starter] settings view:", err)
		_ = c.CloseWindow(windowID)
		return
	}
	registerView(webviewID, settingsHandlers())
	settings.windowID, settings.webviewID = windowID, webviewID
	_ = c.ActivateWindow(windowID)
}

// configFromView sanitizes a config sent by the settings view.
func configFromView(raw json.RawMessage) (core.Config, error) {
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return core.Config{}, err
	}
	return core.SanitizeConfig(generic, core.DefaultConfig()), nil
}

func settingsHandlers() *rpcHandlers {
	return &rpcHandlers{
		requests: map[string]func(json.RawMessage) (any, error){
			"getSettings": func(json.RawMessage) (any, error) {
				return settingsPayload{
					Config:           app.store.Config(),
					LaunchAtLogin:    isLaunchAtLogin(),
					CanLaunchAtLogin: core.AppBundlePath() != "",
					ClaudeModels:     core.ClaudeModels,
					AppVersion:       core.AppVersion,
				}, nil
			},
			"listCodexModels": func(raw json.RawMessage) (any, error) {
				var p struct {
					Bin string `json:"bin"`
				}
				_ = decode(raw, &p)
				models, err := core.ListCodexModels(p.Bin)
				if err != nil {
					return map[string]string{"error": err.Error()}, nil
				}
				return map[string]any{"models": models}, nil
			},
			"saveSettings": func(raw json.RawMessage) (any, error) {
				var p struct {
					Config        json.RawMessage `json:"config"`
					LaunchAtLogin bool            `json:"launchAtLogin"`
				}
				if err := decode(raw, &p); err != nil {
					return okResult{Error: err.Error()}, nil
				}
				cfg, err := configFromView(p.Config)
				if err == nil && p.LaunchAtLogin != isLaunchAtLogin() {
					err = setLaunchAtLogin(p.LaunchAtLogin)
				}
				if err == nil {
					err = app.store.SetConfig(cfg)
				}
				if err != nil {
					return okResult{Error: err.Error()}, nil
				}
				// Model or path changes should clear stale errors and be tried on the next check.
				refreshTray()
				go app.scheduler.RunDue(true, time.Now().UnixMilli())
				return okResult{OK: true}, nil
			},
			"testModel": func(raw json.RawMessage) (any, error) {
				var p struct {
					Provider core.Provider   `json:"provider"`
					Config   json.RawMessage `json:"config"`
				}
				if err := decode(raw, &p); err != nil {
					return okResult{Error: err.Error()}, nil
				}
				if p.Provider != core.Claude && p.Provider != core.Codex {
					return okResult{Error: errors.New("unknown provider").Error()}, nil
				}
				cfg, err := configFromView(p.Config)
				if err != nil {
					return okResult{Error: err.Error()}, nil
				}
				reply, err := app.scheduler.TestModel(p.Provider, cfg)
				refreshTray()
				if err != nil {
					return okResult{Error: err.Error()}, nil
				}
				return okResult{OK: true, Reply: reply}, nil
			},
		},
		messages: map[string]func(json.RawMessage){
			"closeSettings": func(json.RawMessage) {
				settings.mu.Lock()
				id := settings.windowID
				settings.mu.Unlock()
				if id != 0 {
					_ = app.core.CloseWindow(id)
				}
			},
		},
	}
}
