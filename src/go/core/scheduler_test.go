package core

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestNextActiveStart(t *testing.T) {
	ah := &ActiveHours{Start: "06:00", End: "23:59"}
	if got, _ := nextActiveStart(ah, local(2026, 9, 26, 3, 0)); got != local(2026, 9, 26, 6, 0) {
		t.Fatal("before start:", time.UnixMilli(got))
	}
	if got, _ := nextActiveStart(ah, local(2026, 9, 26, 6, 0)); got != local(2026, 9, 27, 6, 0) {
		t.Fatal("at start:", time.UnixMilli(got))
	}
	if _, ok := nextActiveStart(nil, local(2026, 9, 26, 3, 0)); ok {
		t.Fatal("no active hours have no start")
	}
}

func TestNapSleepsUntilTheNextDeadline(t *testing.T) {
	store := setup(t)
	s := NewScheduler(store, nil)
	cfg := store.Config()
	cfg.IntervalMin = 30
	cfg.ActiveHours = &ActiveHours{Start: "06:00", End: "23:59"}
	_ = store.SetConfig(cfg)
	cfg = store.Config()

	// At night: until active hours begin, capped at maxNap.
	if got := s.nap(cfg, local(2026, 9, 26, 5, 59)+30_000); got != 30*time.Second {
		t.Fatal("night:", got)
	}
	if got := s.nap(cfg, local(2026, 9, 26, 3, 0)); got != maxNap {
		t.Fatal("long night:", got)
	}
	// In hours: until the provider's interval is up.
	now := local(2026, 9, 26, 12, 0)
	_ = store.SetProvider(Codex, ProviderState{LastCheckAt: now - 29*60_000 - 50_000})
	if got := s.nap(cfg, now); got != 10*time.Second {
		t.Fatal("interval:", got)
	}
	// A busy provider is left out: it replans when it's done.
	s.claim(Codex)
	if got := s.nap(cfg, now); got != maxNap {
		t.Fatal("busy:", got)
	}
	// A planned reset check comes first.
	s.resetAt[Codex] = now + 5_000
	if got := s.nap(cfg, now); got != 5*time.Second {
		t.Fatal("reset:", got)
	}
}

// Stopping (quit, installing an update) doesn't wait out the confirmation delay.
func TestStopCutsConfirmationShort(t *testing.T) {
	store := setup(t)
	Timing.ConfirmDelay = time.Hour
	var checks int32
	Check[Codex] = func(Config) (Snapshot, error) { atomic.AddInt32(&checks, 1); return idleSnap, nil }
	Start[Codex] = func(Config) (string, error) { return "ok", nil }
	s := NewScheduler(store, nil)
	s.Run()
	for store.Provider(Codex).LastStartAt == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	s.Stop()
	deadline := time.Now().Add(time.Second)
	for s.IsBusy(Codex) {
		if time.Now().After(deadline) {
			t.Fatal("still waiting for the confirmation after Stop")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if checks != 1 {
		t.Fatal("confirmation check ran after Stop:", checks)
	}
}
