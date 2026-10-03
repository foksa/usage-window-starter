package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var (
	idleSnap   = Snapshot{}
	activeSnap = Snapshot{FiveHour: LimitWindow{Active: true, UsedPct: 1, ResetsAt: ms(time.Now().UnixMilli() + 5*3600_000)}}
)

// setup gives each test a throwaway home (never the real one) with Claude disabled,
// short timings, and fakes restored afterwards.
func setup(t *testing.T) {
	t.Helper()
	SetHome(t.TempDir())
	if strings.Contains(HomeDir, "/.usage-window-starter") {
		t.Fatal("tests must never use the real home")
	}
	cfg := DefaultConfig()
	cfg.Claude.Enabled = false
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	Notifier = func(string, string) {}
	savedTiming, savedCheck, savedStart := Timing, Check, Start
	Check = map[Provider]func(Config) (Snapshot, error){Claude: CheckClaude, Codex: CheckCodex}
	Start = map[Provider]func(Config) (string, error){Claude: StartClaude, Codex: StartCodex}
	Timing.ConfirmDelay = 30 * time.Millisecond
	Timing.ResetGrace = map[Provider]time.Duration{Claude: 20 * time.Millisecond, Codex: 20 * time.Millisecond}
	Timing.ResetRetry = 20 * time.Millisecond
	t.Cleanup(func() { Timing, Check, Start = savedTiming, savedCheck, savedStart })
}

func TestDamagedConfigIsMovedAside(t *testing.T) {
	setup(t)
	if err := os.WriteFile(ConfigFile, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := LoadConfig()
	if cfg.AutoStart {
		t.Fatal("recovered config should be paused")
	}
	if LoadConfig().AutoStart {
		t.Fatal("the recovered file should stay paused")
	}
	entries, _ := os.ReadDir(HomeDir)
	found := false
	for _, e := range entries {
		found = found || strings.HasPrefix(e.Name(), "config.json.broken-")
	}
	if !found {
		t.Fatal("no backup of the damaged file")
	}
}

func TestInvalidFieldsFallBackOneByOne(t *testing.T) {
	d := DefaultConfig()
	cfg := SanitizeConfig(map[string]any{
		"autoStart":        false,
		"intervalMin":      "abc",
		"activeHours":      map[string]any{"start": "25:99", "end": "08:00"},
		"codex":            map[string]any{"model": "", "reasoningEffort": "high", "enabled": "yes"},
		"refreshOnOpenSec": -5.0,
		"trayShowTimes":    "no",
	}, d)
	want := d.Codex
	want.ReasoningEffort = "high"
	if cfg.AutoStart || cfg.IntervalMin != d.IntervalMin || cfg.ActiveHours != nil || cfg.Codex != want ||
		cfg.RefreshOnOpenSec != 0 || cfg.TrayShowTimes != d.TrayShowTimes {
		t.Fatalf("%+v", cfg)
	}
}

func TestSavesLeaveNoTempFiles(t *testing.T) {
	setup(t)
	_ = SaveConfig(DefaultConfig())
	entries, _ := os.ReadDir(HomeDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatal("temp file left behind:", e.Name())
		}
	}
}

func TestStartIsOnDiskBeforeConfirmDelayEnds(t *testing.T) {
	setup(t)
	var checks int32
	var seenOnDisk int64
	Check[Codex] = func(Config) (Snapshot, error) {
		if atomic.AddInt32(&checks, 1) == 2 { // runs after the delay
			if st := LoadState()[Codex]; st != nil {
				seenOnDisk = st.LastStartAt
			}
			return activeSnap, nil
		}
		return idleSnap, nil
	}
	Start[Codex] = func(Config) (string, error) { return "ok", nil }
	NewScheduler(nil).RunDue(true, nowMs())
	if seenOnDisk == 0 {
		t.Fatal("start wasn't persisted before the confirmation check")
	}
}

func TestTestMessageCountsAsStart(t *testing.T) {
	setup(t)
	var starts int32
	Check[Codex] = func(Config) (Snapshot, error) { return idleSnap, nil } // Codex can't tell a fresh session from idle yet
	Start[Codex] = func(Config) (string, error) { atomic.AddInt32(&starts, 1); return "ok", nil }
	s := NewScheduler(nil)
	if _, err := s.TestModel(Codex, LoadConfig()); err != nil {
		t.Fatal(err)
	}
	s.RunDue(true, nowMs())
	if starts != 1 || LoadState()[Codex].LastDecision != "started recently" {
		t.Fatalf("starts=%d decision=%q", starts, LoadState()[Codex].LastDecision)
	}
}

func TestTestMessageDuringActiveSessionDoesNotPostpone(t *testing.T) {
	setup(t)
	endsSoon := Snapshot{FiveHour: LimitWindow{Active: true, UsedPct: 40, ResetsAt: ms(nowMs() + 60_000)}}
	Check[Codex] = func(Config) (Snapshot, error) { return endsSoon, nil }
	Start[Codex] = func(Config) (string, error) { return "ok", nil }
	s := NewScheduler(nil)
	s.RunDue(true, nowMs()) // records the active snapshot
	_, _ = s.TestModel(Codex, LoadConfig())
	if LoadState()[Codex].LastStartAt != 0 {
		t.Fatal("a test during an active session was counted as a start")
	}
}

func TestTestMessageDuringCheckIsRefused(t *testing.T) {
	setup(t)
	release := make(chan struct{})
	Check[Codex] = func(Config) (Snapshot, error) { <-release; return activeSnap, nil }
	var starts int32
	Start[Codex] = func(Config) (string, error) { atomic.AddInt32(&starts, 1); return "ok", nil }
	s := NewScheduler(nil)
	done := make(chan struct{})
	go func() { s.RunDue(true, nowMs()); close(done) }()
	for !s.IsBusy(Codex) {
		time.Sleep(time.Millisecond)
	}
	if _, err := s.TestModel(Codex, LoadConfig()); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatal("expected a busy error, got", err)
	}
	close(release)
	<-done
	if starts != 0 {
		t.Fatal("a test message was sent in parallel")
	}
}

func TestNoChecksOutsideActiveHoursAndDueWhenTheyBegin(t *testing.T) {
	setup(t)
	cfg := LoadConfig()
	cfg.IntervalMin = 30
	cfg.ActiveHours = &ActiveHours{Start: "06:00", End: "23:59"}
	_ = SaveConfig(cfg)
	var checks int32
	Check[Codex] = func(Config) (Snapshot, error) { atomic.AddInt32(&checks, 1); return activeSnap, nil }
	s := NewScheduler(nil)
	SaveProviderState(Codex, &ProviderState{LastCheckAt: local(2026, 9, 25, 23, 50)})
	s.RunDue(false, local(2026, 9, 26, 3, 0)) // interval is up, but it's night
	if checks != 0 {
		t.Fatal("checked at night")
	}
	SaveProviderState(Codex, &ProviderState{LastCheckAt: local(2026, 9, 26, 5, 50)}) // e.g. a forced check
	s.RunDue(false, local(2026, 9, 26, 6, 0))                                        // interval not up, but hours just began
	if checks != 1 {
		t.Fatal("not checked when active hours began")
	}
	s.RunDue(true, local(2026, 9, 26, 3, 0)) // "Check now" still works at night
	if checks != 2 {
		t.Fatal("forced check didn't run at night")
	}
}

func TestRefreshChecksOnlyStaleProviders(t *testing.T) {
	setup(t)
	cfg := LoadConfig()
	cfg.Claude.Enabled = true
	_ = SaveConfig(cfg)
	var claudeChecks, codexChecks int32
	Check[Claude] = func(Config) (Snapshot, error) { atomic.AddInt32(&claudeChecks, 1); return activeSnap, nil }
	Check[Codex] = func(Config) (Snapshot, error) { atomic.AddInt32(&codexChecks, 1); return activeSnap, nil }
	now := nowMs()
	SaveProviderState(Claude, &ProviderState{LastCheckAt: now - 30_000})
	SaveProviderState(Codex, &ProviderState{LastCheckAt: now - 90_000})
	NewScheduler(nil).Refresh(time.Minute, now)
	if claudeChecks != 0 || codexChecks != 1 {
		t.Fatalf("claude=%d codex=%d", claudeChecks, codexChecks)
	}
}

func TestStateIsSavedWhenCheckFails(t *testing.T) {
	setup(t)
	Check[Codex] = func(Config) (Snapshot, error) { return Snapshot{}, os.ErrInvalid }
	NewScheduler(nil).RunDue(true, nowMs())
	if st := LoadState()[Codex]; st == nil || st.LastError != os.ErrInvalid.Error() {
		t.Fatalf("%+v", st)
	}
}

func TestAppServerWithoutPingDir(t *testing.T) {
	setup(t)
	_ = os.RemoveAll(PingDir)
	// A fake app-server that reads one line and exits without answering. With a
	// missing cwd the spawn itself would fail instead.
	fake := filepath.Join(HomeDir, "fake-codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nread line\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out any
	err := AppServerRequest(fake, "account/rateLimits/read", nil, 5*time.Second, &out)
	if err == nil || !strings.Contains(err.Error(), "exited without answering") {
		t.Fatal("got", err)
	}
}

func TestResetCheckStartsNextSession(t *testing.T) {
	setup(t)
	var checks, starts int32
	resetsAt := nowMs() + 80
	Check[Codex] = func(Config) (Snapshot, error) {
		atomic.AddInt32(&checks, 1)
		if atomic.LoadInt32(&starts) > 0 {
			return activeSnap, nil // after our start
		}
		if nowMs() < resetsAt {
			return Snapshot{FiveHour: LimitWindow{Active: true, UsedPct: 50, ResetsAt: ms(resetsAt)}}, nil
		}
		return idleSnap, nil
	}
	Start[Codex] = func(Config) (string, error) { atomic.AddInt32(&starts, 1); return "ok", nil }
	s := NewScheduler(nil)
	s.Run() // interval is 10 min, so only the reset timer can trigger the second check
	time.Sleep(400 * time.Millisecond)
	s.Stop()
	if starts != 1 || checks < 3 { // initial, at reset, confirm
		t.Fatalf("starts=%d checks=%d", starts, checks)
	}
}

func TestResetCheckRetriesWhileOldSessionReported(t *testing.T) {
	setup(t)
	var calls, starts int32
	resetsAt := nowMs() + 40
	Check[Codex] = func(Config) (Snapshot, error) {
		n := atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&starts) > 0 {
			return activeSnap, nil
		}
		// Server lags: keeps reporting the old session for the first checks after the reset.
		if n <= 3 {
			return Snapshot{FiveHour: LimitWindow{Active: true, UsedPct: 50, ResetsAt: ms(resetsAt)}}, nil
		}
		return idleSnap, nil
	}
	Start[Codex] = func(Config) (string, error) { atomic.AddInt32(&starts, 1); return "ok", nil }
	s := NewScheduler(nil)
	s.Run()
	time.Sleep(500 * time.Millisecond)
	s.Stop()
	if starts != 1 {
		t.Fatal("starts =", starts)
	}
}

func TestWeeklyResetCheckStartsNextSession(t *testing.T) {
	setup(t)
	var checks, starts int32
	resetsAt := nowMs() + 80
	Check[Codex] = func(Config) (Snapshot, error) {
		atomic.AddInt32(&checks, 1)
		if atomic.LoadInt32(&starts) > 0 {
			return activeSnap, nil
		}
		if nowMs() < resetsAt {
			return Snapshot{Weekly: &LimitWindow{Active: true, UsedPct: 100, ResetsAt: ms(resetsAt)}}, nil
		}
		return idleSnap, nil
	}
	Start[Codex] = func(Config) (string, error) { atomic.AddInt32(&starts, 1); return "ok", nil }
	s := NewScheduler(nil)
	s.Run() // interval is 10 min, so only the reset timer can trigger the second check
	time.Sleep(400 * time.Millisecond)
	s.Stop()
	if starts != 1 || checks < 3 { // initial, at weekly reset, confirm
		t.Fatalf("starts=%d checks=%d", starts, checks)
	}
}

func TestRestartReArmsResetCheck(t *testing.T) {
	setup(t)
	resetsAt := nowMs() + 60
	// Checked just before the restart, so the regular interval isn't due for 10 minutes.
	SaveProviderState(Codex, &ProviderState{
		LastCheckAt:  nowMs(),
		LastSnapshot: &Snapshot{FiveHour: LimitWindow{Active: true, UsedPct: 50, ResetsAt: ms(resetsAt)}},
	})
	var starts int32
	Check[Codex] = func(Config) (Snapshot, error) {
		if atomic.LoadInt32(&starts) > 0 || nowMs() < resetsAt {
			return activeSnap, nil
		}
		return idleSnap, nil
	}
	Start[Codex] = func(Config) (string, error) { atomic.AddInt32(&starts, 1); return "ok", nil }
	s := NewScheduler(nil)
	s.Run()
	time.Sleep(400 * time.Millisecond)
	s.Stop()
	if starts != 1 {
		t.Fatal("starts =", starts)
	}
}

func TestOneOffCommandsLeaveNoTimers(t *testing.T) {
	setup(t)
	var checks int32
	Check[Codex] = func(Config) (Snapshot, error) {
		atomic.AddInt32(&checks, 1)
		return Snapshot{FiveHour: LimitWindow{Active: true, UsedPct: 5, ResetsAt: ms(nowMs() + 50)}}, nil
	}
	NewScheduler(nil).RunDue(true, nowMs()) // not running: no reset timer
	time.Sleep(200 * time.Millisecond)
	if checks != 1 {
		t.Fatal("checks =", checks)
	}
}

func TestBareCommandNamesResolveOnTheCliPath(t *testing.T) {
	setup(t)
	dir := filepath.Join(HomeDir, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-cli")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A GUI app's PATH doesn't include the CLI's folder.
	t.Setenv("PATH", "/usr/bin:/bin")
	if got := ResolveBin("fake-cli"); got != "fake-cli" {
		t.Fatal("found a command that isn't on the CLI path:", got)
	}
	t.Setenv("PATH", "/usr/bin:/bin:"+dir)
	if got := ResolveBin("fake-cli"); got != fake {
		t.Fatal(got)
	}
	if r := Run([]string{"sh", "-c", "echo ok"}, 5*time.Second); r.Code != 0 || strings.TrimSpace(r.Stdout) != "ok" {
		t.Fatalf("%+v", r)
	}
}

func TestActivityShowsStartingDuringStartAndConfirmation(t *testing.T) {
	setup(t)
	var seen []string
	s := NewScheduler(nil)
	var checks int32
	Check[Codex] = func(Config) (Snapshot, error) {
		if atomic.AddInt32(&checks, 1) == 1 {
			seen = append(seen, s.Activity(Codex))
			return idleSnap, nil
		}
		seen = append(seen, s.Activity(Codex)) // the confirmation check
		return activeSnap, nil
	}
	Start[Codex] = func(Config) (string, error) { seen = append(seen, s.Activity(Codex)); return "ok", nil }
	s.RunDue(true, nowMs())
	want := []string{"checking", "starting", "starting"}
	if strings.Join(seen, ",") != strings.Join(want, ",") || s.Activity(Codex) != "" {
		t.Fatalf("activity = %v, after = %q", seen, s.Activity(Codex))
	}
}

func TestInstanceLockAllowsOneCopy(t *testing.T) {
	setup(t)
	first, _, err := AcquireInstanceLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, pid, err := AcquireInstanceLock(); !errors.Is(err, ErrAlreadyRunning) || pid != os.Getpid() {
		t.Fatalf("second copy: pid=%d err=%v", pid, err)
	}
	first.Close() // quitting (or crashing) releases it
	again, _, err := AcquireInstanceLock()
	if err != nil {
		t.Fatal("lock not released:", err)
	}
	again.Close()
}

func TestOldLoginAgentsAreRemoved(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if RemoveLoginAgents() {
		t.Fatal("reported agents that don't exist")
	}
	_ = os.MkdirAll(agentsDir(), 0o755)
	for _, p := range loginAgents() {
		_ = os.WriteFile(p, []byte("<plist/>"), 0o644)
	}
	if !RemoveLoginAgents() {
		t.Fatal("agents not found")
	}
	for _, p := range loginAgents() {
		if exists(p) {
			t.Fatal("left behind:", p)
		}
	}
}
