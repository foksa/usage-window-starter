package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func local(y int, mo time.Month, d, h, mi int) int64 {
	return time.Date(y, mo, d, h, mi, 0, 0, time.Local).UnixMilli()
}

// NOW is 2026-09-24 11:00 local time.
var NOW = local(2026, 9, 24, 11, 0)

func eqWindow(t *testing.T, got LimitWindow, active bool, used float64, resetsAt *int64) {
	t.Helper()
	if got.Active != active || got.UsedPct != used || (got.ResetsAt == nil) != (resetsAt == nil) ||
		(resetsAt != nil && *got.ResetsAt != *resetsAt) {
		g, _ := json.Marshal(got)
		t.Fatalf("window = %s, want active=%v used=%v resetsAt=%v", g, active, used, resetsAt)
	}
}

func TestClaudeIdleSession(t *testing.T) {
	s, err := ParseUsage(fixture(t, "claude-usage-idle.txt"), NOW)
	if err != nil {
		t.Fatal(err)
	}
	eqWindow(t, s.FiveHour, false, 0, nil)
	if s.Weekly.UsedPct != 27 || *s.Weekly.ResetsAt != local(2026, 9, 26, 10, 0) {
		t.Fatalf("weekly = %+v", s.Weekly)
	}
}

func TestClaudeActiveSession(t *testing.T) {
	s, err := ParseUsage(fixture(t, "claude-usage-active.txt"), NOW)
	if err != nil {
		t.Fatal(err)
	}
	eqWindow(t, s.FiveHour, true, 2, ms(local(2026, 9, 24, 16, 59)))
	if *s.Weekly.ResetsAt != local(2026, 9, 26, 9, 59) {
		t.Fatalf("weekly reset = %d", *s.Weekly.ResetsAt)
	}
}

func TestClaudeSameDayReset(t *testing.T) {
	out := "Current session: 34% used · resets 3:30pm (Europe/Belgrade)\nCurrent week (all models): 40% used · resets Sep 26 at 10am (Europe/Belgrade)"
	s, err := ParseUsage(out, NOW)
	if err != nil {
		t.Fatal(err)
	}
	eqWindow(t, s.FiveHour, true, 34, ms(local(2026, 9, 24, 15, 30)))
}

func TestResetTimeRollsToTomorrow(t *testing.T) {
	if got := *ParseResetTime("2am", NOW); got != local(2026, 9, 25, 2, 0) {
		t.Fatal(got)
	}
}

func TestResetTime12AndRelative(t *testing.T) {
	if got := *ParseResetTime("12pm", NOW); got != local(2026, 9, 24, 12, 0) {
		t.Fatal("12pm", got)
	}
	if got := *ParseResetTime("12am", NOW); got != local(2026, 9, 25, 0, 0) {
		t.Fatal("12am", got)
	}
	if got := *ParseResetTime("in 2h 13m", NOW); got != NOW+133*60_000 {
		t.Fatal("relative", got)
	}
}

func TestUnparseableResetStillActive(t *testing.T) {
	s, err := ParseUsage("Current session: 5% used · resets soonish", NOW)
	if err != nil {
		t.Fatal(err)
	}
	if !s.FiveHour.Active || s.FiveHour.ResetsAt != nil {
		t.Fatalf("%+v", s.FiveHour)
	}
}

func TestUnknownFormatFails(t *testing.T) {
	if _, err := ParseUsage("Something totally different", NOW); err == nil {
		t.Fatal("expected an error")
	}
}

func codexFixture(t *testing.T) RateLimitsResult {
	var raw RateLimitsResult
	if err := json.Unmarshal([]byte(fixture(t, "codex-ratelimits.json")), &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func withPrimary(raw RateLimitsResult, w *rpcWindow) RateLimitsResult {
	return RateLimitsResult{RateLimits: &rateLimits{Primary: w, Secondary: raw.RateLimits.Secondary}}
}

func fiveHourWindow(used float64, resetsAtMs int64) *rpcWindow {
	d := 300.0
	sec := (resetsAtMs + 500) / 1000
	return &rpcWindow{UsedPercent: used, WindowDurationMins: &d, ResetsAt: &sec}
}

func TestCodexActiveBeforeReset(t *testing.T) {
	now := int64(1790254054_000 - 3600_000)
	s, _ := ParseRateLimits(codexFixture(t), now)
	eqWindow(t, s.FiveHour, true, 0, ms(1790254054_000))
	if s.Weekly.UsedPct != 62 {
		t.Fatal(s.Weekly.UsedPct)
	}
}

func TestCodexIdleAfterReset(t *testing.T) {
	s, _ := ParseRateLimits(codexFixture(t), 1790254054_000+1000)
	if s.FiveHour.Active {
		t.Fatal("expected idle")
	}
}

func TestCodexHypotheticalWindowIsIdle(t *testing.T) {
	now := int64(1790254054_000)
	s, _ := ParseRateLimits(withPrimary(codexFixture(t), fiveHourWindow(0, now+300*60_000)), now)
	if s.FiveHour.Active {
		t.Fatal("0% with reset exactly one window from now is Codex's 'no window' report")
	}
}

func TestCodexFixedResetBehindIsReal(t *testing.T) {
	now := int64(1790254054_000)
	s, _ := ParseRateLimits(withPrimary(codexFixture(t), fiveHourWindow(0, now+300*60_000-90_000)), now)
	if !s.FiveHour.Active {
		t.Fatal("0% with a fixed reset that has fallen behind is a real window")
	}
}

func TestCodexUsageAboveZeroIsReal(t *testing.T) {
	now := int64(1790254054_000)
	s, _ := ParseRateLimits(withPrimary(codexFixture(t), fiveHourWindow(1, now+300*60_000)), now)
	if !s.FiveHour.Active {
		t.Fatal("usage above 0% is always a real window")
	}
}

func TestCodexNullPrimaryIsIdle(t *testing.T) {
	s, _ := ParseRateLimits(withPrimary(codexFixture(t), nil), NOW)
	if s.FiveHour.Active {
		t.Fatal("expected idle")
	}
}

func TestShouldStart(t *testing.T) {
	idle := Snapshot{Weekly: &LimitWindow{Active: true, UsedPct: 50}}
	cfg := Config{AutoStart: true}
	activeSnap := idle
	activeSnap.FiveHour = LimitWindow{Active: true, UsedPct: 3, ResetsAt: ms(NOW + 1)}
	exhausted := idle
	exhausted.Weekly = &LimitWindow{Active: true, UsedPct: 100}
	paused := cfg
	paused.AutoStart = false
	afternoon := cfg
	afternoon.ActiveHours = &ActiveHours{Start: "13:00", End: "23:00"}

	cases := []struct {
		name string
		snap Snapshot
		st   ProviderState
		cfg  Config
		want Decision
	}{
		{"idle → start", idle, ProviderState{}, cfg, Decision{true, ReasonIdle}},
		{"active → wait", activeSnap, ProviderState{}, cfg, Decision{false, ReasonActive}},
		{"started recently → wait", idle, ProviderState{LastStartAt: NOW - 5*60_000}, cfg, Decision{false, ReasonStartedRecently}},
		{"started long ago → start", idle, ProviderState{LastStartAt: NOW - 6*3600_000}, cfg, Decision{true, ReasonIdle}},
		{"weekly exhausted → wait", exhausted, ProviderState{}, cfg, Decision{false, ReasonWeeklyExhausted}},
		{"auto-start paused → wait", idle, ProviderState{}, paused, Decision{false, ReasonPaused}},
		{"outside active hours → wait", idle, ProviderState{}, afternoon, Decision{false, ReasonOutsideHours}},
	}
	for _, c := range cases {
		if got := ShouldStart(c.snap, &c.st, NOW, c.cfg); got != c.want {
			t.Errorf("%s: got %+v", c.name, got)
		}
	}
}

func TestInActiveHoursAcrossMidnight(t *testing.T) {
	ah := &ActiveHours{Start: "22:00", End: "06:00"}
	if !InActiveHours(ah, local(2026, 9, 24, 23, 0)) || !InActiveHours(ah, local(2026, 9, 24, 3, 0)) || InActiveHours(ah, NOW) {
		t.Fatal("wrong result for a range across midnight")
	}
}

func TestActiveHoursBegan(t *testing.T) {
	day := &ActiveHours{Start: "06:00", End: "23:59"}
	if got, ok := ActiveHoursBegan(day, local(2026, 9, 26, 9, 0)); !ok || got != local(2026, 9, 26, 6, 0) {
		t.Fatal("today's start", got)
	}
	if _, ok := ActiveHoursBegan(day, local(2026, 9, 26, 3, 0)); ok {
		t.Fatal("outside should be false")
	}
	night := &ActiveHours{Start: "22:00", End: "02:00"}
	if got, ok := ActiveHoursBegan(night, local(2026, 9, 26, 1, 0)); !ok || got != local(2026, 9, 25, 22, 0) {
		t.Fatal("yesterday's start", got)
	}
	if _, ok := ActiveHoursBegan(nil, time.Now().UnixMilli()); ok {
		t.Fatal("no active hours should be false")
	}
}
