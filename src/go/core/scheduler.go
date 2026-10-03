package core

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// startCooldownMs: don't send another start message within this long of the last one.
const startCooldownMs = 30 * 60_000
const failureNotifyAfter = 3
const maxResetRetries = 5

// Timing is overridable by tests.
var Timing = struct {
	// ConfirmDelay is the wait before confirming a start: right after a start, Codex's
	// new window is indistinguishable from its "no window" report (see IdleToleranceMs).
	ConfirmDelay time.Duration
	// ResetGrace is how long after a reported reset time to check again. Codex reports
	// exact seconds; Claude's /usage shows minutes only ("4:59pm" for a 17:00 reset),
	// so it gets more room: checking early would send the start message into the old
	// session.
	ResetGrace map[Provider]time.Duration
	// ResetRetry spaces retries when a session is still reported as running after its
	// reset time.
	ResetRetry time.Duration
}{
	ConfirmDelay: 90 * time.Second,
	ResetGrace:   map[Provider]time.Duration{Claude: 90 * time.Second, Codex: 30 * time.Second},
	ResetRetry:   60 * time.Second,
}

// Check and Start are swappable by tests.
var (
	Check = map[Provider]func(Config) (Snapshot, error){Claude: CheckClaude, Codex: CheckCodex}
	Start = map[Provider]func(Config) (string, error){Claude: StartClaude, Codex: StartCodex}
)

func minutesOfDay(hhmm string) int {
	h, m, _ := strings.Cut(hhmm, ":")
	hi, _ := strconv.Atoi(h)
	mi, _ := strconv.Atoi(m)
	return hi*60 + mi
}

// InActiveHours supports ranges past midnight; nil means always.
func InActiveHours(ah *ActiveHours, now int64) bool {
	if ah == nil {
		return true
	}
	t := time.UnixMilli(now)
	cur := t.Hour()*60 + t.Minute()
	s, e := minutesOfDay(ah.Start), minutesOfDay(ah.End)
	if s <= e {
		return cur >= s && cur < e
	}
	return cur >= s || cur < e
}

// ActiveHoursBegan returns when the active-hours period containing now began, or
// false when now is outside it (or there are no active hours).
func ActiveHoursBegan(ah *ActiveHours, now int64) (int64, bool) {
	if ah == nil || !InActiveHours(ah, now) {
		return 0, false
	}
	s := minutesOfDay(ah.Start)
	t := time.UnixMilli(now)
	d := time.Date(t.Year(), t.Month(), t.Day(), s/60, s%60, 0, 0, time.Local)
	if d.UnixMilli() > now {
		d = d.AddDate(0, 0, -1) // began yesterday (range past midnight)
	}
	return d.UnixMilli(), true
}

type Decision struct {
	Start  bool
	Reason string
}

func ShouldStart(snap Snapshot, st *ProviderState, now int64, cfg Config) Decision {
	switch {
	case snap.FiveHour.Active:
		return Decision{false, "5h session active"}
	case !cfg.AutoStart:
		return Decision{false, "auto-start paused"}
	case snap.Weekly != nil && snap.Weekly.UsedPct >= 100:
		return Decision{false, "weekly limit exhausted"}
	case !InActiveHours(cfg.ActiveHours, now):
		return Decision{false, "outside active hours"}
	case st.LastStartAt != 0 && now-st.LastStartAt < startCooldownMs:
		return Decision{false, "started recently"}
	}
	return Decision{true, "5h session idle"}
}

// SessionActive reports whether a snapshot shows a 5h session that's still running
// (unknown counts as not).
func SessionActive(snap *Snapshot, now int64) bool {
	if snap == nil || !snap.FiveHour.Active {
		return false
	}
	return snap.FiveHour.ResetsAt == nil || *snap.FiveHour.ResetsAt > now
}

func nowMs() int64 { return time.Now().UnixMilli() }

// TickProvider checks one provider and, if its session is idle, starts a new one.
// Mutates st. persist is called as soon as a start succeeds, so the cooldown survives
// a quit or crash during the confirmation delay. starting, if set, is called when it
// sends the start message; the start and its confirmation take a couple of minutes.
func TickProvider(p Provider, cfg Config, st *ProviderState, now int64, persist func(*ProviderState), starting func()) {
	st.LastCheckAt = now
	phase := "check"
	err := func() error {
		snap, err := Check[p](cfg)
		if err != nil {
			return err
		}
		st.LastSnapshot = &snap
		d := ShouldStart(snap, st, now, cfg)
		st.LastDecision = d.Reason
		Log("provider", p, "event", "check", "fiveHour", snap.FiveHour, "weekly", snap.Weekly, "decision", d.Reason)
		if !d.Start {
			return nil
		}

		phase = "start"
		if starting != nil {
			starting()
		}
		reply, err := Start[p](cfg)
		if err != nil {
			return err
		}
		st.LastStartAt = nowMs()
		if persist != nil {
			persist(st)
		}
		phase = "check"
		time.Sleep(Timing.ConfirmDelay)
		after, err := Check[p](cfg)
		if err != nil {
			return err
		}
		st.LastSnapshot = &after
		st.LastDecision = "start sent, session not reported yet"
		msg := fmt.Sprintf("%s start message sent, but no active session reported yet", Label[p])
		if after.FiveHour.Active {
			st.LastDecision = "5h session active"
			msg = fmt.Sprintf("%s 5h session started — resets %s", Label[p], FmtTime(after.FiveHour.ResetsAt))
		}
		Log("provider", p, "event", "start", "model", cfg.Model(p), "reply", reply, "fiveHour", after.FiveHour)
		Notify("Usage Window Starter", msg)
		return nil
	}()
	if err == nil {
		st.ConsecutiveFailures = 0
		st.FailureNotified = false
		st.LastError = ""
		return
	}
	msg := err.Error()
	st.ConsecutiveFailures++
	st.LastError = msg
	Log("provider", p, "event", "error", "phase", phase, "error", msg, "consecutive", st.ConsecutiveFailures)
	// A failed start (e.g. a model that's no longer offered) needs attention right away.
	if (phase == "start" || st.ConsecutiveFailures >= failureNotifyAfter) && !st.FailureNotified {
		st.FailureNotified = true
		if phase == "start" {
			Notify("Usage Window Starter", fmt.Sprintf(`%s couldn't start a 5h session with model "%s": %s`, Label[p], cfg.Model(p), truncate(msg, 100)))
		} else {
			Notify("Usage Window Starter", fmt.Sprintf("%s limit check failing: %s", Label[p], truncate(msg, 120)))
		}
	}
}

// Scheduler runs checks on the configured interval. It wakes every minute and
// compares wall-clock time, so checks catch up right after the Mac wakes from sleep.
type Scheduler struct {
	OnChange func()

	mu sync.Mutex
	// activity is "checking" or "starting" while a provider is busy.
	activity     map[Provider]string
	stop         chan struct{}
	resetTimers  map[Provider]*time.Timer
	resetRetries map[Provider]int
}

func NewScheduler(onChange func()) *Scheduler {
	if onChange == nil {
		onChange = func() {}
	}
	return &Scheduler{
		OnChange:     onChange,
		activity:     map[Provider]string{},
		resetTimers:  map[Provider]*time.Timer{},
		resetRetries: map[Provider]int{},
	}
}

func (s *Scheduler) Run() {
	s.mu.Lock()
	if s.stop != nil {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.stop = stop
	s.mu.Unlock()

	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.RunDue(false, nowMs())
			}
		}
	}()
	// The first RunDue may skip providers checked recently (e.g. right after a
	// restart), so re-arm reset checks from the saved snapshots.
	cfg := LoadConfig()
	state := LoadState()
	for _, p := range Providers {
		if cfg.Enabled(p) && state[p] != nil {
			s.planResetCheck(p, state[p].LastSnapshot)
		}
	}
	go s.RunDue(false, nowMs())
}

func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	for p, t := range s.resetTimers {
		t.Stop()
		delete(s.resetTimers, p)
	}
}

func (s *Scheduler) IsBusy(p Provider) bool { return s.Activity(p) != "" }

// Activity is what provider p is doing: "checking", "starting", or "" when idle.
func (s *Scheduler) Activity(p Provider) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activity[p]
}

func (s *Scheduler) setActivity(p Provider, a string) {
	s.mu.Lock()
	s.activity[p] = a
	s.mu.Unlock()
	s.OnChange()
}

// tick runs TickProvider with state persistence and activity reporting.
func (s *Scheduler) tick(p Provider, cfg Config, st *ProviderState, now int64) {
	TickProvider(p, cfg, st, now, func(st *ProviderState) { SaveProviderState(p, st) },
		func() { s.setActivity(p, "starting") })
}

func (s *Scheduler) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop != nil
}

// RunDue checks every enabled provider whose interval has elapsed (or all of them
// with force). Nothing runs outside active hours unless forced. When they begin,
// every provider not checked since is due at once, instead of each waiting out its
// own interval.
func (s *Scheduler) RunDue(force bool, now int64) {
	cfg := LoadConfig()
	if !force && !InActiveHours(cfg.ActiveHours, now) {
		return
	}
	began, inHours := ActiveHoursBegan(cfg.ActiveHours, now)
	s.runWhere(cfg, now, func(last int64) bool {
		return force || now-last >= int64(cfg.IntervalMin)*60_000 || (inHours && last < began)
	})
}

// Refresh checks every enabled provider not checked within maxAge, in active hours only.
func (s *Scheduler) Refresh(maxAge time.Duration, now int64) {
	cfg := LoadConfig()
	if !InActiveHours(cfg.ActiveHours, now) {
		return
	}
	s.runWhere(cfg, now, func(last int64) bool { return now-last >= maxAge.Milliseconds() })
}

func (s *Scheduler) runWhere(cfg Config, now int64, isDue func(lastCheckAt int64) bool) {
	state := LoadState()
	var wg sync.WaitGroup
	for _, p := range Providers {
		var last int64
		if state[p] != nil {
			last = state[p].LastCheckAt
		}
		if !cfg.Enabled(p) || s.IsBusy(p) || !isDue(last) {
			continue
		}
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			s.withProvider(p, func(st *ProviderState) {
				s.tick(p, cfg, st, now)
			})
		}(p)
	}
	wg.Wait()
}

// StartNow starts a session right now if it's idle, ignoring pause, cooldown and
// active hours.
func (s *Scheduler) StartNow(p Provider) {
	cfg := LoadConfig()
	cfg.AutoStart = true
	cfg.ActiveHours = nil
	s.withProvider(p, func(st *ProviderState) {
		st.LastStartAt = 0
		st.FailureNotified = false
		s.tick(p, cfg, st, nowMs())
	})
}

// TestModel sends one test message with an unsaved config. It shares the provider
// lock with checks and records a start, because a test message starts a session when
// none is running.
func (s *Scheduler) TestModel(p Provider, cfg Config) (string, error) {
	var reply string
	var err error
	ok := s.withProvider(p, func(st *ProviderState) {
		// Only a session that looked idle can have been started by the test. Recording
		// it during an active session would push the next auto-start back by the cooldown.
		wasIdle := !SessionActive(st.LastSnapshot, nowMs())
		reply, err = Start[p](cfg)
		if err != nil {
			return
		}
		if wasIdle {
			st.LastStartAt = nowMs()
		}
		Log("provider", p, "event", "test", "model", cfg.Model(p), "reply", reply, "countedAsStart", wasIdle)
	})
	if !ok {
		doing := "a check"
		if s.Activity(p) == "starting" {
			doing = "starting a session"
		}
		return "", fmt.Errorf("%s is busy with %s, try again in a moment", Label[p], doing)
	}
	return reply, err
}

// withProvider serializes work per provider and persists its state without
// clobbering the other provider. Returns false when p is already busy.
func (s *Scheduler) withProvider(p Provider, fn func(st *ProviderState)) bool {
	s.mu.Lock()
	if s.activity[p] != "" {
		s.mu.Unlock()
		return false
	}
	s.activity[p] = "checking"
	s.mu.Unlock()
	s.OnChange()

	st := LoadState()[p]
	if st == nil {
		st = &ProviderState{}
	}
	defer func() {
		SaveProviderState(p, st)
		s.planResetCheck(p, st.LastSnapshot)
		s.mu.Lock()
		delete(s.activity, p)
		s.mu.Unlock()
		s.OnChange()
	}()
	fn(st)
	return true
}

// blockingReset is the reset time that stands between now and the next start: the
// running 5h session's, or the exhausted weekly limit's. Nil when there is none.
func blockingReset(snap *Snapshot) *int64 {
	switch {
	case snap == nil:
		return nil
	case snap.FiveHour.Active:
		return snap.FiveHour.ResetsAt
	case snap.Weekly != nil && snap.Weekly.UsedPct >= 100:
		return snap.Weekly.ResetsAt
	}
	return nil
}

// planResetCheck: besides the regular interval, check once right after the running
// session's (or the exhausted weekly limit's) reset time, so the next session starts
// within seconds instead of up to an interval later. Only while the scheduler is
// running, so one-off CLI commands exit normally.
func (s *Scheduler) planResetCheck(p Provider, snap *Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop == nil {
		return
	}
	if t := s.resetTimers[p]; t != nil {
		t.Stop()
		delete(s.resetTimers, p)
	}
	resetsAt := blockingReset(snap)
	if resetsAt == nil {
		delete(s.resetRetries, p)
		return
	}
	delay := time.Duration(*resetsAt-nowMs())*time.Millisecond + Timing.ResetGrace[p]
	if delay <= 0 {
		// Past its reset time but still reported as running (or exhausted): ask again
		// shortly, a few times, then leave it to the regular interval.
		n := s.resetRetries[p] + 1
		if n > maxResetRetries {
			return
		}
		s.resetRetries[p] = n
		delay = Timing.ResetRetry
	} else {
		delete(s.resetRetries, p)
	}
	s.resetTimers[p] = time.AfterFunc(delay, func() { s.checkAtReset(p) })
}

func (s *Scheduler) checkAtReset(p Provider) {
	s.mu.Lock()
	delete(s.resetTimers, p)
	retry := s.resetRetries[p]
	s.mu.Unlock()
	cfg := LoadConfig()
	// Outside active hours the first regular check once they begin takes over.
	if !cfg.Enabled(p) || !InActiveHours(cfg.ActiveHours, nowMs()) {
		return
	}
	Log("provider", p, "event", "reset-check", "retry", retry)
	// If a check is already running, its own completion plans the next reset check.
	s.withProvider(p, func(st *ProviderState) {
		s.tick(p, cfg, st, nowMs())
	})
}
