package core

import "sync"

// Store owns the config and the per-provider state for the life of the process:
// both are read from disk once, served from memory, and written back when they
// change. The files are its persistence, not a channel between parts of the app,
// so while the app runs it is their only writer and edits made to them by hand or
// by the CLI are overwritten.
type Store struct {
	mu    sync.Mutex
	cfg   Config
	state State
}

// OpenStore loads config.json (creating it with the defaults) and state.json.
func OpenStore() *Store {
	return &Store{cfg: loadConfig(), state: loadState()}
}

func (s *Store) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// SetConfig sanitizes c, saves it and makes it current.
func (s *Store) SetConfig(c Config) error {
	_, err := s.UpdateConfig(func(cfg *Config) { *cfg = c })
	return err
}

// UpdateConfig applies change to the current config as one step, so concurrent
// updates don't undo each other, and returns the saved result. On a failed save the
// current config stays as it was.
func (s *Store) UpdateConfig(change func(*Config)) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	change(&c)
	c = SanitizeConfig(roundTrip(c), DefaultConfig())
	if err := saveConfig(c); err != nil {
		return s.cfg, err
	}
	s.cfg = c
	return c, nil
}

// Provider returns a copy of p's state (zero when it has none yet).
func (s *Store) Provider(p Provider) ProviderState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps := s.state[p]; ps != nil {
		return *ps
	}
	return ProviderState{}
}

// State returns a copy of every provider's state.
func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := make(State, len(s.state))
	for p, ps := range s.state {
		c := *ps
		st[p] = &c
	}
	return st
}

// SetProvider makes ps p's current state and saves it. The in-memory state is
// updated even when saving fails: it's still the truth for this run.
func (s *Store) SetProvider(p Provider, ps ProviderState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[p] = &ps
	return saveState(s.state)
}
