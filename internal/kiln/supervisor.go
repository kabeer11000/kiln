package kiln

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// StaleAfter: a running record not updated for this long has no live supervisor.
const StaleAfter = 30 * time.Second

const statusTimeout = 30 * time.Second

// Supervisor starts a run, enforces its caps, and always stops the container at the end.
// The same loop becomes the in-task sidecar on cloud backends.
type Supervisor struct {
	Backend Backend
	Store   Store
	Tick    time.Duration
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration)
	OnEvent func(*Run, Event)
}

func NewSupervisor(b Backend, s Store) *Supervisor {
	return &Supervisor{Backend: b, Store: s, Tick: 5 * time.Second, Now: time.Now, Sleep: sleepCtx}
}

// Run blocks until the run reaches a terminal state. Cancelling ctx cancels the run.
// The returned error is about kiln itself (e.g. the container could not be stopped);
// the run's outcome is in r.State.
func (s *Supervisor) Run(ctx context.Context, r *Run) error {
	s.transition(r, Provisioning, "starting on "+r.Config.Backend)
	if err := s.Store.Save(r); err != nil {
		return err
	}
	h, err := s.Backend.Start(ctx, r)
	if err != nil {
		return s.finish(r, Failed, "start failed: "+err.Error())
	}
	r.Handle = h
	started := s.Now()
	r.StartedAt = &started
	s.transition(r, Running, "running")

	b := r.Config.Budget
	warned, statusErrs := false, 0
	for {
		if err := s.Store.Save(r); err != nil {
			// Can't record progress, so we can't enforce safely: stop spending.
			return errors.Join(err, s.Backend.Stop(context.Background(), r.Handle))
		}
		// Wake at the next tick, or exactly when a cap is reached, whichever is sooner.
		s.Sleep(ctx, min(s.Tick, b.Remaining(s.Now().Sub(started), r.PricePerHour)))
		if ctx.Err() != nil {
			return s.finish(r, Cancelled, "interrupted")
		}
		if s.Store.CancelRequested(r.ID) {
			return s.finish(r, Cancelled, "cancel requested")
		}

		// Caps are checked before the backend call, so a slow backend never adds overshoot.
		elapsed := s.Now().Sub(started)
		spent, stop, warn := b.Check(elapsed, r.PricePerHour)
		r.SpentUSD = spent
		switch stop {
		case BudgetExceeded:
			return s.finish(r, stop, fmt.Sprintf("spent %s of %s cap", usd(spent), usd(b.MaxUSD)))
		case TimedOut:
			return s.finish(r, stop, fmt.Sprintf("ran %s, cap is %gh", elapsed.Round(time.Second), b.MaxHours))
		}

		// The call may not outlive the cap: a slow backend is cut off, and the next loop enforces it.
		sctx, cancel := context.WithTimeout(ctx, min(statusTimeout, b.Remaining(elapsed, r.PricePerHour)))
		st, err := s.Backend.Status(sctx, r.Handle)
		cancel()
		if err != nil {
			if statusErrs++; statusErrs >= 3 {
				return s.finish(r, Failed, "lost contact with backend: "+err.Error())
			}
			continue
		}
		statusErrs = 0
		if st.ImageDigest != "" {
			r.ImageDigest = st.ImageDigest
		}
		if st.State.Terminal() {
			return s.finish(r, st.State, st.Reason)
		}
		if warn && !warned {
			warned = true
			s.event(r, fmt.Sprintf("warning: past %.0f%% of a cap (spent %s of %s, ran %s of %gh)",
				b.WarnAt*100, usd(spent), usd(b.MaxUSD), elapsed.Round(time.Second), b.MaxHours))
		}
	}
}

// usd formats money with enough precision for tiny test runs.
func usd(v float64) string {
	if v < 1 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func (s *Supervisor) finish(r *Run, st State, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var errs []error
	// Stop first: every second until then is billed. Logs and cleanup can wait.
	if r.Handle != "" {
		if err := s.Backend.Stop(ctx, r.Handle); err != nil {
			errs = append(errs, &Error{Code: "stop_failed", Message: "could not stop " + r.Handle + ": " + err.Error(),
				Fix: "it may still be running and costing money; stop it manually"})
		}
	}
	now := s.Now()
	r.EndedAt, r.Reason = &now, reason
	r.SpentUSD = r.Duration(now).Hours() * r.PricePerHour // true final spend, including stop time
	if r.Handle != "" {
		// Not fatal: the outcome still gets recorded.
		if err := s.saveLogs(ctx, r); err != nil {
			s.event(r, "could not save logs: "+err.Error())
		}
		if err := s.Backend.Remove(ctx, r.Handle); err != nil {
			s.event(r, "could not clean up "+r.Handle+": "+err.Error())
		}
	}
	s.transition(r, st, reason)
	errs = append(errs, s.Store.Save(r))
	return errors.Join(errs...)
}

func (s *Supervisor) saveLogs(ctx context.Context, r *Run) error {
	f, err := os.Create(s.Store.LogPath(r.ID))
	if err != nil {
		return err
	}
	defer f.Close()
	return s.Backend.Logs(ctx, r.Handle, false, f)
}

func (s *Supervisor) transition(r *Run, st State, msg string) {
	r.State = st
	s.emit(r, Event{At: s.Now(), State: st, Message: msg})
}

func (s *Supervisor) event(r *Run, msg string) { s.emit(r, Event{At: s.Now(), Message: msg}) }

func (s *Supervisor) emit(r *Run, e Event) {
	r.Events = append(r.Events, e)
	if s.OnEvent != nil {
		s.OnEvent(r, e)
	}
}

// Cancel stops a run. With a live supervisor it asks it to stop. If the supervisor is gone
// (record is stale) it stops the task directly so nothing keeps costing money; if the task
// already ended on its own (e.g. the in-task deadline fired), it records that outcome instead.
func Cancel(ctx context.Context, s Store, b Backend, r *Run, now time.Time) (direct bool, err error) {
	if r.State.Terminal() {
		return false, &Error{Code: "already_finished", Message: "run " + r.ID + " already " + string(r.State)}
	}
	if now.Sub(r.UpdatedAt) < StaleAfter {
		return false, s.RequestCancel(r.ID)
	}
	state, reason := Cancelled, "cancelled (supervisor was not running)"
	if r.Handle != "" {
		if st, err := b.Status(ctx, r.Handle); err == nil && st.State.Terminal() {
			state, reason = st.State, st.Reason+" (recorded after supervisor exit)"
		} else if err := b.Stop(ctx, r.Handle); err != nil {
			return true, err
		}
		_ = b.Remove(ctx, r.Handle) // best effort: the spend has already ended
	}
	// ponytail: a run that crashed mid-Start has no handle; its container (kiln-<id>) needs a manual rm.
	r.State, r.Reason, r.EndedAt = state, reason, &now
	r.Events = append(r.Events, Event{At: now, State: state, Message: reason})
	return true, s.Save(r)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
