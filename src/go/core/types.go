package core

type Provider string

const (
	Claude Provider = "claude"
	Codex  Provider = "codex"
)

var Providers = []Provider{Claude, Codex}

var Label = map[Provider]string{Claude: "Claude", Codex: "Codex"}

// LimitWindow is one usage window (5h or weekly).
type LimitWindow struct {
	// Active is true while a window is running (it has a reset time in the future).
	Active  bool    `json:"active"`
	UsedPct float64 `json:"usedPct"`
	// ResetsAt is epoch ms, nil when idle or unknown.
	ResetsAt *int64 `json:"resetsAt"`
}

type Snapshot struct {
	FiveHour LimitWindow  `json:"fiveHour"`
	Weekly   *LimitWindow `json:"weekly"`
}

type ProviderState struct {
	LastCheckAt         int64     `json:"lastCheckAt,omitempty"`
	LastSnapshot        *Snapshot `json:"lastSnapshot,omitempty"`
	LastStartAt         int64     `json:"lastStartAt,omitempty"`
	LastDecision        Reason    `json:"lastDecision,omitempty"`
	LastError           string    `json:"lastError,omitempty"`
	ConsecutiveFailures int       `json:"consecutiveFailures,omitempty"`
	FailureNotified     bool      `json:"failureNotified,omitempty"`
}

type State map[Provider]*ProviderState

func ms(v int64) *int64 { return &v }
