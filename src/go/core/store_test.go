package core

import (
	"os"
	"testing"
)

func TestStoreSavesAndReopens(t *testing.T) {
	store := setup(t)
	cfg := store.Config()
	cfg.IntervalMin = 500 // out of range: sanitized to the max
	if err := store.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if got := store.Config().IntervalMin; got != 120 {
		t.Fatal("interval in memory =", got)
	}
	_ = store.SetProvider(Codex, ProviderState{LastStartAt: 42})

	again := OpenStore()
	if again.Config().IntervalMin != 120 || again.Provider(Codex).LastStartAt != 42 {
		t.Fatalf("reopened: %+v %+v", again.Config(), again.Provider(Codex))
	}
}

func TestStoreKeepsConfigWhenSaveFails(t *testing.T) {
	store := setup(t)
	if err := os.Chmod(HomeDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(HomeDir, 0o755) })
	if _, err := store.UpdateConfig(func(c *Config) { c.AutoStart = false }); err == nil {
		t.Fatal("save into a read-only folder succeeded")
	}
	if !store.Config().AutoStart {
		t.Fatal("unsaved change became current")
	}
}

func TestStoreHandsOutCopies(t *testing.T) {
	store := setup(t)
	_ = store.SetProvider(Codex, ProviderState{LastDecision: "a"})
	store.State()[Codex].LastDecision = "b"
	if store.Provider(Codex).LastDecision != "a" {
		t.Fatal("caller changed the stored state")
	}
}
