package core

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type ActiveHours struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type ClaudeConfig struct {
	Enabled bool   `json:"enabled"`
	Bin     string `json:"bin"`
	Model   string `json:"model"`
}

type CodexConfig struct {
	Enabled         bool   `json:"enabled"`
	Bin             string `json:"bin"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
}

type Config struct {
	// AutoStart is the master switch for sending start messages; checks keep running when off.
	AutoStart   bool `json:"autoStart"`
	IntervalMin int  `json:"intervalMin"`
	// ActiveHours is e.g. {08:00, 24:00}; nil = always.
	ActiveHours *ActiveHours `json:"activeHours"`
	// RefreshOnOpenSec: opening the panel checks providers whose last check is older
	// than this; 0 = never.
	RefreshOnOpenSec int `json:"refreshOnOpenSec"`
	// TrayShowTimes shows time left in the menu bar next to the icon.
	TrayShowTimes bool         `json:"trayShowTimes"`
	Claude        ClaudeConfig `json:"claude"`
	Codex         CodexConfig  `json:"codex"`
}

// Enabled reports whether provider p is turned on.
func (c Config) Enabled(p Provider) bool {
	if p == Claude {
		return c.Claude.Enabled
	}
	return c.Codex.Enabled
}

// Model is provider p's configured model.
func (c Config) Model(p Provider) string {
	if p == Claude {
		return c.Claude.Model
	}
	return c.Codex.Model
}

func findBin(name string, candidates ...string) string {
	for _, p := range candidates {
		if exists(p) {
			return p
		}
	}
	return name
}

func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		AutoStart:        true,
		IntervalMin:      10,
		RefreshOnOpenSec: 60,
		TrayShowTimes:    true,
		Claude: ClaudeConfig{
			Enabled: true,
			Bin:     findBin("claude", filepath.Join(home, ".local/bin/claude"), "/opt/homebrew/bin/claude", "/usr/local/bin/claude"),
			Model:   "haiku",
		},
		Codex: CodexConfig{
			Enabled:         true,
			Bin:             findBin("codex", "/opt/homebrew/bin/codex", "/usr/local/bin/codex", filepath.Join(home, ".local/bin/codex")),
			Model:           "gpt-5.6-luna",
			ReasoningEffort: "low",
		},
	}
}

var hhmm = regexp.MustCompile(`^([01]?\d|2[0-4]):[0-5]\d$`)

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func str(v any, fallback string) string {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return fallback
}

func boolean(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

// number accepts JSON numbers and numeric strings, like JS Number().
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func clampRound(v any, lo, hi, fallback int) int {
	n, ok := number(v)
	if !ok {
		return fallback
	}
	return int(math.Min(float64(hi), math.Max(float64(lo), math.Round(n))))
}

// SanitizeConfig keeps every valid field from a user config and falls back to the
// defaults field by field.
func SanitizeConfig(raw any, d Config) Config {
	u := obj(raw)
	claude, codex := obj(u["claude"]), obj(u["codex"])
	var ah *ActiveHours
	if a := obj(u["activeHours"]); a != nil {
		s, sok := a["start"].(string)
		e, eok := a["end"].(string)
		if sok && eok && hhmm.MatchString(s) && hhmm.MatchString(e) {
			ah = &ActiveHours{Start: s, End: e}
		}
	}
	return Config{
		AutoStart:        boolean(u["autoStart"], d.AutoStart),
		IntervalMin:      clampRound(u["intervalMin"], 1, 120, d.IntervalMin),
		ActiveHours:      ah,
		RefreshOnOpenSec: clampRound(u["refreshOnOpenSec"], 0, 3600, d.RefreshOnOpenSec),
		TrayShowTimes:    boolean(u["trayShowTimes"], d.TrayShowTimes),
		Claude: ClaudeConfig{
			Enabled: boolean(claude["enabled"], d.Claude.Enabled),
			Bin:     str(claude["bin"], d.Claude.Bin),
			Model:   str(claude["model"], d.Claude.Model),
		},
		Codex: CodexConfig{
			Enabled:         boolean(codex["enabled"], d.Codex.Enabled),
			Bin:             str(codex["bin"], d.Codex.Bin),
			Model:           str(codex["model"], d.Codex.Model),
			ReasoningEffort: str(codex["reasoningEffort"], d.Codex.ReasoningEffort),
		},
	}
}

// roundTrip turns a typed config into the generic shape SanitizeConfig reads.
func roundTrip(c Config) any {
	data, _ := json.Marshal(c)
	var raw any
	_ = json.Unmarshal(data, &raw)
	return raw
}

// loadConfig reads config.json, creating it with the defaults when missing.
func loadConfig() Config {
	_ = os.MkdirAll(HomeDir, 0o755)
	data, err := os.ReadFile(ConfigFile)
	if os.IsNotExist(err) {
		d := DefaultConfig()
		_ = saveConfig(d)
		return d
	}
	var raw any
	if err != nil || json.Unmarshal(data, &raw) != nil {
		// Keep the damaged file for recovery and carry on with defaults.
		backup := fmt.Sprintf("%s.broken-%d", ConfigFile, time.Now().UnixMilli())
		_ = os.Rename(ConfigFile, backup)
		Log("event", "config-recovered", "backup", backup)
		// The user may have paused or disabled providers, so don't send anything until
		// they look at Settings again.
		recovered := DefaultConfig()
		recovered.AutoStart = false
		_ = saveConfig(recovered)
		Notify("Usage Window Starter",
			"Settings file was damaged. Auto-start is paused until you check Settings. The old file was saved as "+backup)
		return recovered
	}
	return SanitizeConfig(raw, DefaultConfig())
}

func saveConfig(c Config) error {
	data, _ := json.MarshalIndent(c, "", "  ")
	return writeFileAtomic(ConfigFile, append(data, '\n'))
}
