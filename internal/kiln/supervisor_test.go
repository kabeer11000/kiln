package kiln

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// fake is a backend whose container exits after exitAfter (0 = never) with exitState.
type fake struct {
	clock      *time.Time
	exitAfter  time.Duration
	exitState  State
	startErr   error
	statusErr  error
	started    time.Time
	stops      int
	removes    int
	onStatus   func(n int)
	statusCall int
	slow       time.Duration // each Status call takes this long (fake clock), cut off at ctx deadline
}

func (f *fake) Preflight(context.Context, Config) error { return nil }

func (f *fake) Start(context.Context, *Run) (string, error) {
	if f.startErr != nil {
		return "", f.startErr
	}
	f.started = *f.clock
	return "c1", nil
}

func (f *fake) Status(ctx context.Context, _ string) (Status, error) {
	f.statusCall++
	if f.slow > 0 {
		d := f.slow
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d {
			*f.clock = f.clock.Add(time.Until(dl))
			return Status{}, context.DeadlineExceeded
		}
		*f.clock = f.clock.Add(d)
	}
	if f.onStatus != nil {
		f.onStatus(f.statusCall)
	}
	if f.statusErr != nil {
		return Status{}, f.statusErr
	}
	if f.exitAfter > 0 && f.clock.Sub(f.started) >= f.exitAfter {
		return Status{State: f.exitState, ImageDigest: "sha256:abc", Reason: "exited"}, nil
	}
	return Status{State: Running, ImageDigest: "sha256:abc"}, nil
}

func (f *fake) Logs(_ context.Context, _ string, _ bool, w io.Writer) error {
	_, err := io.WriteString(w, "hello\n")
	return err
}

func (f *fake) Stop(context.Context, string) error { f.stops++; return nil }

func (f *fake) Remove(context.Context, string) error { f.removes++; return nil }

func (f *fake) PricePerHour(Resources) float64 { return 0 }

func setup(t *testing.T, f *fake, budget Budget, price float64) (*Supervisor, *Run) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.clock = &now
	sup := &Supervisor{
		Backend: f, Store: Store{Dir: t.TempDir()}, Tick: 5 * time.Second,
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) { now = now.Add(d) },
	}
	r := NewRun(Config{Name: "t", Backend: "docker", Image: "x", Budget: budget}, now)
	r.PricePerHour = price
	return sup, r
}

var roomy = Budget{MaxUSD: 100, MaxHours: 10, WarnAt: 0.8}

func TestSupervisorOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		fake   fake
		budget Budget
		price  float64
		want   State
		reason string
	}{
		{"succeeds", fake{exitAfter: 30 * time.Second, exitState: Succeeded}, roomy, 1, Succeeded, "exited"},
		{"fails", fake{exitAfter: 30 * time.Second, exitState: Failed}, roomy, 1, Failed, "exited"},
		{"dollar cap", fake{}, Budget{MaxUSD: 1, MaxHours: 10, WarnAt: 0.8}, 10, BudgetExceeded, "spent $1.00 of $1.00 cap"},
		{"hour cap", fake{}, Budget{MaxUSD: 100, MaxHours: 0.5, WarnAt: 0.8}, 0, TimedOut, "ran 30m0s, cap is 0.5h"},
		{"start error", fake{startErr: errors.New("no image")}, roomy, 1, Failed, "start failed: no image"},
		{"lost backend", fake{statusErr: errors.New("gone")}, roomy, 1, Failed, "lost contact with backend: gone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.fake
			sup, r := setup(t, &f, tt.budget, tt.price)
			if err := sup.Run(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			if r.State != tt.want || r.Reason != tt.reason {
				t.Fatalf("got %s %q, want %s %q", r.State, r.Reason, tt.want, tt.reason)
			}
			saved, err := sup.Store.Load(r.ID)
			if err != nil || saved.State != tt.want || saved.EndedAt == nil {
				t.Fatalf("saved record wrong: %+v, %v", saved, err)
			}
			if tt.fake.startErr == nil && (f.stops != 1 || f.removes != 1) {
				t.Fatalf("stopped %d, removed %d times, want 1 each", f.stops, f.removes)
			}
			if tt.fake.startErr == nil && tt.fake.statusErr == nil && r.ImageDigest != "sha256:abc" {
				t.Fatalf("image digest not recorded: %q", r.ImageDigest)
			}
		})
	}
}

func TestCapsHaveNoOvershoot(t *testing.T) {
	// Tick does not divide the cap evenly: the supervisor must still stop at the cap, not the next tick.
	f := &fake{}
	sup, r := setup(t, f, Budget{MaxUSD: 1, MaxHours: 10, WarnAt: 0.8}, 7)
	sup.Tick = 13 * time.Second
	_ = sup.Run(context.Background(), r)
	if r.State != BudgetExceeded || r.SpentUSD < 1 || r.SpentUSD > 1.001 {
		t.Fatalf("state %s, spent %v, want budget_exceeded within [1, 1.001]", r.State, r.SpentUSD)
	}

	f = &fake{}
	sup, r = setup(t, f, Budget{MaxUSD: 100, MaxHours: 0.006, WarnAt: 0.8}, 0)
	sup.Tick = 13 * time.Second
	_ = sup.Run(context.Background(), r)
	if d := r.Duration(time.Time{}); r.State != TimedOut || d < 21600*time.Millisecond || d > 21700*time.Millisecond {
		t.Fatalf("state %s, ran %s, want timed_out at ~21.6s", r.State, d)
	}
}

func TestSlowBackendDoesNotDelayCap(t *testing.T) {
	// Each status call takes 10s (like the AWS CLI on a slow machine). The cap must still hold.
	f := &fake{}
	sup, r := setup(t, f, Budget{MaxUSD: 100, MaxHours: 0.006, WarnAt: 0.8}, 0)
	f.slow = 10 * time.Second
	_ = sup.Run(context.Background(), r)
	if r.State != TimedOut || r.Reason != "ran 22s, cap is 0.006h" {
		t.Fatalf("state %s %q", r.State, r.Reason)
	}
}

func TestWarnsOnceBeforeCap(t *testing.T) {
	f := &fake{}
	sup, r := setup(t, f, Budget{MaxUSD: 1, MaxHours: 10, WarnAt: 0.5}, 10)
	_ = sup.Run(context.Background(), r)
	warns := 0
	for _, e := range r.Events {
		if strings.HasPrefix(e.Message, "warning:") {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("got %d warnings, want 1: %+v", warns, r.Events)
	}
}

func TestCancelRequestStopsRun(t *testing.T) {
	f := &fake{}
	sup, r := setup(t, f, roomy, 1)
	f.onStatus = func(n int) {
		if n == 2 {
			_ = sup.Store.RequestCancel(r.ID)
		}
	}
	_ = sup.Run(context.Background(), r)
	if r.State != Cancelled || f.stops != 1 {
		t.Fatalf("state %s, stops %d", r.State, f.stops)
	}
}

func TestContextCancelStopsRun(t *testing.T) {
	f := &fake{}
	sup, r := setup(t, f, roomy, 1)
	ctx, cancel := context.WithCancel(context.Background())
	f.onStatus = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	_ = sup.Run(ctx, r)
	if r.State != Cancelled || r.Reason != "interrupted" || f.stops != 1 {
		t.Fatalf("state %s %q, stops %d", r.State, r.Reason, f.stops)
	}
}

func TestLogsSavedOnFinish(t *testing.T) {
	f := &fake{exitAfter: time.Second, exitState: Succeeded}
	sup, r := setup(t, f, roomy, 1)
	_ = sup.Run(context.Background(), r)
	data, err := os.ReadFile(sup.Store.LogPath(r.ID))
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("logs %q, %v", data, err)
	}
}

func TestCancelStaleRunStopsContainerDirectly(t *testing.T) {
	f := &fake{}
	s := Store{Dir: t.TempDir()}
	r := NewRun(Config{Name: "t", Image: "x", Budget: roomy}, time.Now())
	r.State, r.Handle = Running, "c1"
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}

	// Fresh record: live supervisor, so only a request is written.
	direct, err := Cancel(context.Background(), s, f, r, time.Now())
	if err != nil || direct || !s.CancelRequested(r.ID) || f.stops != 0 {
		t.Fatalf("fresh: direct=%v err=%v stops=%d", direct, err, f.stops)
	}

	// Stale record: supervisor gone, so kiln stops the container itself.
	direct, err = Cancel(context.Background(), s, f, r, time.Now().Add(time.Minute))
	if err != nil || !direct || f.stops != 1 || r.State != Cancelled {
		t.Fatalf("stale: direct=%v err=%v stops=%d state=%s", direct, err, f.stops, r.State)
	}

	if _, err := Cancel(context.Background(), s, f, r, time.Now()); err == nil {
		t.Fatal("cancelling a finished run should fail")
	}
}

func TestCancelStaleRunRecordsOutcomeIfTaskAlreadyEnded(t *testing.T) {
	// Supervisor died, then the in-task deadline stopped the task. Record what really happened.
	now := time.Now()
	f := &fake{clock: &now, exitAfter: time.Nanosecond, exitState: TimedOut}
	s := Store{Dir: t.TempDir()}
	r := NewRun(Config{Name: "t", Image: "x", Budget: roomy}, now)
	r.State, r.Handle = Running, "c1"
	_ = s.Save(r)
	now = now.Add(time.Minute)
	direct, err := Cancel(context.Background(), s, f, r, now)
	if err != nil || !direct || r.State != TimedOut || f.stops != 0 || f.removes != 1 {
		t.Fatalf("direct=%v err=%v state=%s stops=%d removes=%d", direct, err, r.State, f.stops, f.removes)
	}
}

// observer adds a monitor stream to the fake: live records while running, the full set after.
type observer struct {
	*fake
	missed, live, final string // missed: in the stream, but the live follow never saw it
}

func (o *observer) Monitor(ctx context.Context, _ string, follow bool, w io.Writer) error {
	if !follow {
		_, err := io.WriteString(w, o.missed+o.live+o.final)
		return err
	}
	if _, err := io.WriteString(w, o.live); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (o *observer) Send(context.Context, *Run, string) error { return nil }

func (o *observer) Artifacts(context.Context, *Run) ([]byte, error) { return nil, nil }

func TestSupervisorRecordsMonitorEvents(t *testing.T) {
	f := &fake{exitAfter: 20 * time.Second, exitState: Succeeded}
	sup, r := setup(t, f, Budget{MaxUSD: 1, MaxHours: 1, WarnAt: 0.8}, 0)
	o := &observer{fake: f, missed: `{"kiln":"start","t":0,"deadline":9}` + "\n",
		live:  `{"kiln":"outbox","t":1,"line":"step 1"}` + "\n",
		final: `{"kiln":"outbox","t":2,"line":"done"}` + "\n" + `{"kiln":"uploaded","t":3,"bytes":10}` + "\n",
	}
	sup.Backend = o
	if err := sup.Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, e := range r.Events {
		msgs = append(msgs, e.Message)
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"monitor started", "agent: step 1", "agent: done", "artifacts uploaded"} {
		if strings.Count(all, want) != 1 {
			t.Errorf("want %q exactly once in events:\n%s", want, all)
		}
	}
	if b, err := os.ReadFile(sup.Store.MonitorPath(r.ID)); err != nil || !strings.Contains(string(b), `"uploaded"`) {
		t.Errorf("monitor records not saved: %v %s", err, b)
	}
}
