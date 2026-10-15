package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"usagewindowstarter/src/go/core"
)

const cliUsage = "usage: usage-window-starter [status | start <claude|codex> | models]"

// runCLI is a dev tool for checking things without the app, e.g. what the parser
// sees after a CLI update. It reads the app's config and state. "start" changes
// state, so when the app is running it asks the app to do it.
func runCLI(args []string) int {
	switch args[0] {
	case "status":
		status(core.OpenStore())
	case "start":
		if len(args) < 2 || (args[1] != "claude" && args[1] != "codex") {
			fmt.Fprintln(os.Stderr, "usage: usage-window-starter start <claude|codex>")
			return 1
		}
		p := core.Provider(args[1])
		r, err := startSession(p)
		if errors.Is(err, core.ErrNotResponding) {
			fmt.Fprintln(os.Stderr, "the running app didn't answer")
			return 1
		}
		if err != nil {
			// e.g. a version from before the CLI could ask it
			fmt.Fprintln(os.Stderr, "couldn't reach the running app (quit it to start from here):", err)
			return 1
		}
		if r.Error != "" {
			fmt.Fprintln(os.Stderr, "failed:", r.Error)
			return 1
		}
		fmt.Printf("%s: %s\n", core.Label[p], core.FmtWindow(r.FiveHour, false, time.Now().UnixMilli()))
	case "models":
		cfg := core.OpenStore().Config()
		var ids []string
		for _, m := range core.ClaudeModels {
			ids = append(ids, m.ID)
		}
		fmt.Println("Claude:", strings.Join(ids, ", "), "(or any full model id)")
		models, err := core.ListCodexModels(cfg.Codex.Bin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Codex:", err)
			return 1
		}
		ids = nil
		for _, m := range models {
			id := m.ID
			if m.IsDefault {
				id += " (default)"
			}
			ids = append(ids, id)
		}
		fmt.Println("Codex: ", strings.Join(ids, ", "))
	case "login-item": // run the bundled binary: "<app>/Contents/MacOS/main login-item on"
		arg := "status"
		if len(args) > 1 {
			arg = args[1]
		}
		var err error
		switch arg {
		case "on":
			err = setLaunchAtLogin(true)
		case "off":
			err = setLaunchAtLogin(false)
		}
		names := map[int]string{loginItemNotRegistered: "not registered", loginItemEnabled: "enabled",
			loginItemRequiresApproval: "needs approval in System Settings", loginItemNotFound: "not found"}
		fmt.Println("login item:", names[loginItemStatus()])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	case "-h", "--help", "help":
		fmt.Println(cliUsage)
	default:
		fmt.Fprintln(os.Stderr, cliUsage)
		return 1
	}
	return 0
}

// startSession has the running app start p's session, or starts it here when the
// app isn't running.
func startSession(p core.Provider) (core.StartResult, error) {
	lock, _, err := core.AcquireInstanceLock()
	if errors.Is(err, core.ErrAlreadyRunning) {
		fmt.Fprintln(os.Stderr, "Asking the running app to start it…")
		return core.RequestStart(p, 5*time.Minute)
	}
	if lock != nil {
		lock.Close() // only checking: holding it would make the app think it's running
	}
	return core.NewScheduler(core.OpenStore(), nil).StartNow(p), nil
}

func status(store *core.Store) {
	cfg := store.Config()
	var enabled []core.Provider
	for _, p := range core.Providers {
		if cfg.Enabled(p) {
			enabled = append(enabled, p)
		}
	}
	blocks := make([]string, len(enabled))
	var wg sync.WaitGroup
	for i, p := range enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := store.Provider(p)
			var b strings.Builder
			fmt.Fprintf(&b, "%s (%s)\n", core.Label[p], cfg.Model(p))
			if snap, err := core.Check[p](cfg); err != nil {
				fmt.Fprintf(&b, "  check failed: %s\n", err)
			} else {
				now := time.Now().UnixMilli()
				d := core.ShouldStart(snap, &st, now, cfg)
				fmt.Fprintf(&b, "  5h:     %s\n", core.FmtWindow(&snap.FiveHour, false, now))
				fmt.Fprintf(&b, "  weekly: %s\n", core.FmtWindow(snap.Weekly, true, now))
				if d.Start {
					b.WriteString("  would:  START a 5h session now\n")
				} else {
					fmt.Fprintf(&b, "  would:  wait (%s)\n", d.Reason)
				}
			}
			last := "never"
			if st.LastStartAt != 0 {
				last = time.UnixMilli(st.LastStartAt).Format("Mon Jan 2 3:04 PM")
			}
			fmt.Fprintf(&b, "  last 5h session started: %s", last)
			blocks[i] = b.String()
		}()
	}
	wg.Wait()
	fmt.Println(strings.Join(blocks, "\n\n"))
}
