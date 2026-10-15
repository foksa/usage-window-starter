package core

import (
	"context"
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

// Reason says why a check did or didn't start a session. The value is what
// state.json and the log record.
type Reason string

const (
	ReasonIdle             Reason = "5h session idle"
	ReasonActive           Reason = "5h session active"
	ReasonPaused           Reason = "auto-start paused"
	ReasonWeeklyExhausted  Reason = "weekly limit exhausted"
	ReasonOutsideHours     Reason = "outside active hours"
	ReasonStartedRecently  Reason = "started recently"
	ReasonStartUnconfirmed Reason = "start sent, session not reported yet"
)

type Decision struct {
	Start  bool
	Reason Reason
}

// Activity is what a provider is busy with; "" is idle. The value is what the
// panel gets.
type Activity string

const (
	Idle     Activity = ""
	Checking Activity = "checking"
	Starting Activity = "starting"
)

func ShouldStart(snap Snapshot, st *ProviderState, now int64, cfg Config) Decision {
	switch {
	case snap.FiveHour.Active:
		return Decision{false, ReasonActive}
	case !cfg.AutoStart:
		return Decision{false, ReasonPaused}
	case snap.Weekly != nil && snap.Weekly.UsedPct >= 100:
		return Decision{false, ReasonWeeklyExhausted}
	case !InActiveHours(cfg.ActiveHours, now):
		return Decision{false, ReasonOutsideHours}
	case st.LastStartAt != 0 && now-st.LastStartAt < startCooldownMs:
		return Decision{false, ReasonStartedRecently}
	}
	return Decision{true, ReasonIdle}
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
// Cancelling ctx (the app stopping) skips the confirmation.
func TickProvider(ctx context.Context, p Provider, cfg Config, st *ProviderState, now int64, persist func(*ProviderState), starting func()) {
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
		st.LastDecision = ReasonStartUnconfirmed
		select {
		case <-time.After(Timing.ConfirmDelay):
		case <-ctx.Done():
			Log("provider", p, "event", "start", "model", cfg.Model(p), "reply", reply, "confirmed", false)
			return nil
		}
		after, err := Check[p](cfg)
		if err != nil {
			return err
		}
		st.LastSnapshot = &after
		msg := fmt.Sprintf("%s start message sent, but no active session reported yet", Label[p])
		if after.FiveHour.Active {
			st.LastDecision = ReasonActive
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

// maxNap bounds how long the scheduler sleeps between looks at the clock. Go timers
// run on a clock that stops while the Mac sleeps, so a deadline that passed during
// sleep is only noticed when the timer fires; this keeps that within a minute of
// waking.
const maxNap = time.Minute

// Scheduler decides when each provider is checked: on the configured interval, as
// soon as active hours begin, and right after a reported reset time. While running,
// one goroutine owns the timing. It sleeps until the earliest of those deadlines (or
// maxNap), runs what's due, and works out the next one. Checks themselves run in their
// own goroutines, one at a time per provider.
type Scheduler struct {
	OnChange func()
	store    *Store
	// replan wakes the loop to work out its next deadline again.
	replan chan struct{}

	mu sync.Mutex
	// activity is set while a provider is busy.
	activity map[Provider]Activity
	// ctx and cancel are set while running; cancel stops the loop and any
	// confirmation wait.
	ctx    context.Context
	cancel context.CancelFunc
	// resetAt is when to check each provider again because of a reset time.
	resetAt      map[Provider]int64
	resetRetries map[Provider]int
}

func NewScheduler(store *Store, onChange func()) *Scheduler {
	if onChange == nil {
		onChange = func() {}
	}
	return &Scheduler{
		OnChange:     onChange,
		store:        store,
		replan:       make(chan struct{}, 1),
		activity:     map[Provider]Activity{},
		resetAt:      map[Provider]int64{},
		resetRetries: map[Provider]int{},
	}
}

func (s *Scheduler) Run() {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel = ctx, cancel
	s.mu.Unlock()
	// The first round may skip providers checked recently (e.g. right after a
	// restart), so plan reset checks from the saved snapshots.
	cfg := s.store.Config()
	for _, p := range Providers {
		if cfg.Enabled(p) {
			s.planResetCheck(p, s.store.Provider(p).LastSnapshot)
		}
	}
	go s.loop(ctx)
}

func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.ctx, s.cancel = nil, nil
	}
	clear(s.resetAt)
	clear(s.resetRetries)
}

// runCtx is the context work runs under: the running scheduler's, or one that's never
// cancelled for one-off use (the CLI).
func (s *Scheduler) runCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *Scheduler) poke() {
	select {
	case s.replan <- struct{}{}:
	default: // a replan is already pending
	}
}

func (s *Scheduler) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.replan:
		}
		now := nowMs()
		cfg := s.store.Config()
		s.runDueResets(cfg, now)
		if InActiveHours(cfg.ActiveHours, now) {
			s.dispatch(cfg, now, s.isDue(cfg, now, false))
		}
		timer.Reset(s.nap(cfg, now))
	}
}

// nap is how long the loop can sleep: until the next reset check, the next regular
// check of a provider that isn't busy (a busy one replans when it's done), or the
// start of active hours.
func (s *Scheduler) nap(cfg Config, now int64) time.Duration {
	next := now + maxNap.Milliseconds()
	consider := func(t int64) { next = min(next, t) }
	s.mu.Lock()
	for _, t := range s.resetAt {
		consider(t)
	}
	s.mu.Unlock()
	if InActiveHours(cfg.ActiveHours, now) {
		for _, p := range Providers {
			if cfg.Enabled(p) && !s.IsBusy(p) {
				consider(s.store.Provider(p).LastCheckAt + int64(cfg.IntervalMin)*60_000)
			}
		}
	} else if start, ok := nextActiveStart(cfg.ActiveHours, now); ok {
		consider(start)
	}
	return max(time.Duration(next-now)*time.Millisecond, 0)
}

// nextActiveStart is when active hours next begin after now (false when there are
// none, i.e. always active).
func nextActiveStart(ah *ActiveHours, now int64) (int64, bool) {
	if ah == nil {
		return 0, false
	}
	s := minutesOfDay(ah.Start)
	t := time.UnixMilli(now)
	d := time.Date(t.Year(), t.Month(), t.Day(), s/60, s%60, 0, 0, time.Local)
	if d.UnixMilli() <= now {
		d = d.AddDate(0, 0, 1)
	}
	return d.UnixMilli(), true
}

func (s *Scheduler) IsBusy(p Provider) bool { return s.Activity(p) != Idle }

// Activity is what provider p is busy with, or Idle.
func (s *Scheduler) Activity(p Provider) Activity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activity[p]
}

func (s *Scheduler) setActivity(p Provider, a Activity) {
	s.mu.Lock()
	s.activity[p] = a
	s.mu.Unlock()
	s.OnChange()
}

// tick runs TickProvider with state persistence and activity reporting.
func (s *Scheduler) tick(p Provider, cfg Config, st *ProviderState, now int64) {
	TickProvider(s.runCtx(), p, cfg, st, now, func(st *ProviderState) { _ = s.store.SetProvider(p, *st) },
		func() { s.setActivity(p, Starting) })
}

// isDue reports, from a provider's last check, whether it's due: its interval has
// elapsed, active hours began since, or force.
func (s *Scheduler) isDue(cfg Config, now int64, force bool) func(lastCheckAt int64) bool {
	began, inHours := ActiveHoursBegan(cfg.ActiveHours, now)
	return func(last int64) bool {
		return force || now-last >= int64(cfg.IntervalMin)*60_000 || (inHours && last < began)
	}
}

// RunDue checks every enabled provider that's due (or all of them with force) and
// waits for them. Nothing runs outside active hours unless forced. When they begin,
// every provider not checked since is due at once, instead of each waiting out its
// own interval.
func (s *Scheduler) RunDue(force bool, now int64) {
	cfg := s.store.Config()
	if !force && !InActiveHours(cfg.ActiveHours, now) {
		return
	}
	s.dispatch(cfg, now, s.isDue(cfg, now, force)).Wait()
}

// Refresh checks every enabled provider not checked within maxAge, in active hours only.
func (s *Scheduler) Refresh(maxAge time.Duration, now int64) {
	cfg := s.store.Config()
	if !InActiveHours(cfg.ActiveHours, now) {
		return
	}
	s.dispatch(cfg, now, func(last int64) bool { return now-last >= maxAge.Milliseconds() }).Wait()
}

// dispatch claims every enabled, due provider that isn't busy and checks each in its
// own goroutine. The returned WaitGroup is done when they've all finished.
func (s *Scheduler) dispatch(cfg Config, now int64, isDue func(lastCheckAt int64) bool) *sync.WaitGroup {
	var wg sync.WaitGroup
	for _, p := range Providers {
		if !cfg.Enabled(p) || !isDue(s.store.Provider(p).LastCheckAt) || !s.claim(p) {
			continue
		}
		wg.Go(func() {
			s.runClaimed(p, func(st *ProviderState) { s.tick(p, cfg, st, now) })
		})
	}
	return &wg
}

// StartNow starts a session right now if it's idle, ignoring pause, cooldown and
// active hours, and reports how it went.
func (s *Scheduler) StartNow(p Provider) StartResult {
	cfg := s.store.Config()
	cfg.AutoStart = true
	cfg.ActiveHours = nil
	ok := s.withProvider(p, func(st *ProviderState) {
		st.LastStartAt = 0
		st.FailureNotified = false
		s.tick(p, cfg, st, nowMs())
	})
	if !ok {
		return StartResult{Error: fmt.Sprintf("%s is busy with %s, try again in a moment", Label[p], s.Activity(p))}
	}
	st := s.store.Provider(p)
	r := StartResult{Error: st.LastError}
	if st.LastSnapshot != nil {
		w := st.LastSnapshot.FiveHour
		r.FiveHour = &w
	}
	return r
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
		if s.Activity(p) == Starting {
			doing = "starting a session"
		}
		return "", fmt.Errorf("%s is busy with %s, try again in a moment", Label[p], doing)
	}
	return reply, err
}

// withProvider runs fn on p's state, one piece of work per provider at a time.
// Returns false when p is already busy.
func (s *Scheduler) withProvider(p Provider, fn func(st *ProviderState)) bool {
	if !s.claim(p) {
		return false
	}
	s.runClaimed(p, fn)
	return true
}

// claim marks p busy, or returns false when it already is.
func (s *Scheduler) claim(p Provider) bool {
	s.mu.Lock()
	if s.activity[p] != Idle {
		s.mu.Unlock()
		return false
	}
	s.activity[p] = Checking
	s.mu.Unlock()
	s.OnChange()
	return true
}

// runClaimed runs fn on p's state, which claim has marked busy, then saves the
// state, plans the reset check, frees p and has the loop replan.
func (s *Scheduler) runClaimed(p Provider, fn func(st *ProviderState)) {
	st := s.store.Provider(p)
	defer func() {
		_ = s.store.SetProvider(p, st)
		s.planResetCheck(p, st.LastSnapshot)
		s.mu.Lock()
		delete(s.activity, p)
		s.mu.Unlock()
		s.OnChange()
		s.poke()
	}()
	fn(&st)
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
	if s.cancel == nil {
		return
	}
	delete(s.resetAt, p)
	resetsAt := blockingReset(snap)
	if resetsAt == nil {
		delete(s.resetRetries, p)
		return
	}
	now := nowMs()
	at := *resetsAt + Timing.ResetGrace[p].Milliseconds()
	if at <= now {
		// Past its reset time but still reported as running (or exhausted): ask again
		// shortly, a few times, then leave it to the regular interval.
		n := s.resetRetries[p] + 1
		if n > maxResetRetries {
			return
		}
		s.resetRetries[p] = n
		at = now + Timing.ResetRetry.Milliseconds()
	} else {
		delete(s.resetRetries, p)
	}
	s.resetAt[p] = at
	s.poke()
}

// runDueResets starts the reset checks whose time has come. One for a provider
// that's busy is dropped: that check's own completion plans the next one.
func (s *Scheduler) runDueResets(cfg Config, now int64) {
	s.mu.Lock()
	due := map[Provider]int{}
	for p, at := range s.resetAt {
		if at <= now {
			due[p] = s.resetRetries[p]
			delete(s.resetAt, p)
		}
	}
	s.mu.Unlock()
	for p, retry := range due {
		// Outside active hours the first regular check once they begin takes over.
		if !cfg.Enabled(p) || !InActiveHours(cfg.ActiveHours, now) || !s.claim(p) {
			continue
		}
		go s.runClaimed(p, func(st *ProviderState) {
			Log("provider", p, "event", "reset-check", "retry", retry)
			s.tick(p, cfg, st, nowMs())
		})
	}
}
