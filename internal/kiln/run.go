package kiln

import (
	"crypto/rand"
	"encoding/base32"
	mrand "math/rand/v2"
	"strings"
	"time"
)

type State string

const (
	Queued         State = "queued"
	Provisioning   State = "provisioning"
	Running        State = "running"
	Succeeded      State = "succeeded"
	Failed         State = "failed"
	Cancelled      State = "cancelled"
	BudgetExceeded State = "budget_exceeded"
	TimedOut       State = "timed_out"
)

func (s State) Terminal() bool {
	switch s {
	case Succeeded, Failed, Cancelled, BudgetExceeded, TimedOut:
		return true
	}
	return false
}

// ExitCode maps a final state to the CLI exit code documented in SPEC.md.
func (s State) ExitCode() int {
	switch s {
	case Succeeded:
		return 0
	case Failed:
		return 3
	case BudgetExceeded:
		return 4
	case TimedOut:
		return 5
	case Cancelled:
		return 6
	}
	return 1
}

type Event struct {
	At      time.Time `json:"at"`
	State   State     `json:"state,omitempty"`
	Message string    `json:"message"`
}

type Run struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	State        State      `json:"state"`
	Reason       string     `json:"reason,omitempty"`
	Config       Config     `json:"config"`
	ImageDigest  string     `json:"image_digest,omitempty"`
	GitSHA       string     `json:"git_sha,omitempty"`
	GitDirty     bool       `json:"git_dirty,omitempty"`
	Handle       string     `json:"handle,omitempty"`
	PricePerHour float64    `json:"price_per_hour"`
	SpentUSD     float64    `json:"spent_usd"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
	Events       []Event    `json:"events"`
}

func NewRun(c Config, now time.Time) *Run {
	return &Run{
		ID:        newID(),
		Name:      c.Name + "/" + pick(adjectives) + "-" + pick(nouns),
		State:     Queued,
		Config:    c,
		CreatedAt: now,
		Events:    []Event{{At: now, State: Queued, Message: "queued"}},
	}
}

// Duration is time spent running so far (or in total once ended).
func (r *Run) Duration(now time.Time) time.Duration {
	if r.StartedAt == nil {
		return 0
	}
	if r.EndedAt != nil {
		now = *r.EndedAt
	}
	return now.Sub(*r.StartedAt)
}

func newID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return "r_" + strings.ToLower(base32.StdEncoding.EncodeToString(b))
}

func pick(words []string) string { return words[mrand.IntN(len(words))] }

var (
	adjectives = []string{"amber", "bold", "brave", "bright", "calm", "clever", "eager", "fierce", "gentle", "keen", "lucky", "noble", "quiet", "rapid", "solid", "swift"}
	nouns      = []string{"badger", "cedar", "comet", "ember", "falcon", "flint", "harbor", "heron", "lantern", "lynx", "maple", "meadow", "otter", "quartz", "raven", "willow"}
)
