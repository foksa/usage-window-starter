package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"electrobun"

	"usagewindowstarter/src/go/core"
	"usagewindowstarter/src/go/updater"
)

// checkEvery is how often to look for a new release in the background.
const checkEvery = 24 * time.Hour

// Updates checks GitHub Releases (release.baseUrl in electrobun.config.ts) and
// downloads a new version in the background. Installing waits for the user, since
// it restarts the app.
// updatePhase is where the updater is; the value is what the panel gets.
type updatePhase string

const (
	updateIdle        updatePhase = "idle"
	updateChecking    updatePhase = "checking"
	updateDownloading updatePhase = "downloading"
	updateReady       updatePhase = "ready"
	updateInstalling  updatePhase = "installing"
	updateError       updatePhase = "error"
)

type Updates struct {
	mu        sync.Mutex
	u         *updater.Updater
	phase     updatePhase
	version   string
	err       string
	lastCheck time.Time
	onChange  func()
}

func newUpdates(bundle electrobun.BundlePaths, onChange func()) *Updates {
	u, err := updater.New(bundle.ExeDir, bundle.ResourcesDir)
	up := &Updates{u: u, phase: updateIdle, onChange: onChange}
	if err != nil {
		up.err = err.Error()
	} else {
		core.AppVersion = u.Info.Version
		u.HelperLog = filepath.Join(core.HomeDir, "update-helper.log")
	}
	return up
}

func (up *Updates) status() (phase updatePhase, version string) {
	up.mu.Lock()
	defer up.mu.Unlock()
	return up.phase, up.version
}

func (up *Updates) set(phase updatePhase) {
	up.mu.Lock()
	up.phase = phase
	if phase != updateError {
		up.err = ""
	}
	up.mu.Unlock()
	up.onChange()
}

// run checks shortly after launch, then once a day. Hourly ticks compare
// wall-clock time, so sleep doesn't delay it.
func (up *Updates) run() {
	time.AfterFunc(3*time.Second, up.reportLastInstall)
	time.AfterFunc(time.Minute, func() { up.check(false) })
	go func() {
		for range time.Tick(time.Hour) {
			up.mu.Lock()
			due := time.Since(up.lastCheck) >= checkEvery
			up.mu.Unlock()
			if due {
				up.check(false)
			}
		}
	}()
}

// reportLastInstall tells the user how the last install went. The helper does its
// work after this app quits, so a failure is only known on the next launch.
func (up *Updates) reportLastInstall() {
	if up.u == nil {
		return
	}
	r, err := up.u.NewResult()
	if err != nil || r == nil {
		return
	}
	if r.Success {
		core.Log("event", "update-complete", "version", r.Version)
		core.Notify(appName, "Updated to version "+r.Version)
		return
	}
	core.Log("event", "update-error", "phase", "native-"+r.Phase, "version", r.Version, "error", r.Message)
	core.Notify(appName, fmt.Sprintf("Update to %s failed during %s: %s", r.Version, r.Phase, truncate(r.Message, 100)))
}

// check looks for a new version and downloads it. manual also reports
// "up to date" and errors.
func (up *Updates) check(manual bool) {
	// Claim the check under the lock: panel actions arrive on separate goroutines, and
	// two checks at once would share the manifest and the download files.
	up.mu.Lock()
	if up.phase != updateIdle && up.phase != updateError {
		up.mu.Unlock()
		return
	}
	up.phase, up.err = updateChecking, ""
	up.lastCheck = time.Now()
	up.mu.Unlock()
	up.onChange()
	current := ""
	if up.u != nil {
		current = up.u.Info.Version
	}
	phase := "check"
	fail := func(err error) {
		core.Log("event", "update-error", "phase", phase, "manual", manual, "error", err.Error())
		up.mu.Lock()
		up.err = err.Error()
		up.mu.Unlock()
		up.set(updateError)
		if manual {
			core.Notify(appName, "Update check failed: "+truncate(err.Error(), 120))
		}
	}
	if up.u == nil {
		fail(fmt.Errorf("can't read version.json"))
		return
	}

	res, err := up.u.Check(context.Background())
	if err != nil {
		fail(err)
		return
	}
	core.Log("event", "update-check", "manual", manual, "current", current, "latest", res.Version,
		"available", res.Available, "alreadyDownloaded", res.Ready)
	if !res.Available {
		up.set(updateIdle)
		if manual {
			core.Notify(appName, fmt.Sprintf("You're up to date (%s)", up.u.Info.Version))
		}
		return
	}
	up.mu.Lock()
	up.version = res.Version
	up.mu.Unlock()
	if !res.Ready {
		phase = "download"
		up.set(updateDownloading)
		started := time.Now()
		if err := up.u.Download(context.Background()); err != nil {
			fail(err)
			return
		}
		core.Log("event", "update-download", "version", res.Version, "seconds", int(time.Since(started).Seconds()))
	}
	up.set(updateReady)
	core.Notify(appName, fmt.Sprintf(`Version %s is ready. Choose "Install" in the panel.`, res.Version))
}

// install replaces the app with the downloaded version and restarts it.
func (up *Updates) install() {
	// Claim the install under the lock: duplicate clicks arrive on separate goroutines
	// and must not start two helpers against the same bundle.
	up.mu.Lock()
	if up.phase != updateReady {
		up.mu.Unlock()
		return
	}
	up.phase = updateInstalling
	version := up.version
	up.mu.Unlock()
	up.onChange()
	core.Log("event", "update-install", "from", up.u.Info.Version, "to", version)
	app.scheduler.Stop()
	if err := up.u.Apply(); err != nil {
		core.Log("event", "update-error", "phase", "install", "error", err.Error())
		up.mu.Lock()
		up.err = err.Error()
		up.mu.Unlock()
		up.set(updateError)
		core.Notify(appName, "Couldn't install the update: "+truncate(err.Error(), 120))
		app.scheduler.Run()
		return
	}
	quit() // the helper waits for this app to exit, then swaps it and relaunches
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
