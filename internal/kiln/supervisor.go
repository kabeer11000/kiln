package kiln

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	live    *liveMonitor
}

func NewSupervisor(b Backend, s Store) *Supervisor {
	return &Supervisor{Backend: b, Store: s, Tick: 5 * time.Second, Now: time.Now, Sleep: sleepCtx}
}

// Run blocks until the run reaches a terminal state. Cancelling ctx cancels the run.
// The returned error is about kiln itself (e.g. the container could not be stopped);
// the run's outcome is in r.State.
func (s *Supervisor) Run(ctx context.Context, r *Run) error {
	defer s.heartbeat(r.ID)()
	s.transition(r, Provisioning, "starting on "+r.Config.Backend)
	if err := s.Store.Save(r); err != nil {
		return err
	}
	if s.Store.CancelRequested(r.ID) { // cancelled while queued (e.g. right after `run -d`)
		return s.finish(r, Cancelled, "cancel requested before start")
	}
	h, err := s.Backend.Start(ctx, r)
	if err != nil {
		return s.finish(r, Failed, "start failed: "+err.Error())
	}
	r.Handle = h
	started := s.Now()
	r.StartedAt = &started
	s.transition(r, Running, "running")

	s.live = s.watch(ctx, r)
	defer s.live.stop()

	b := r.Config.Budget
	reserve := StopReserveOf(s.Backend)
	warned, statusErrs := false, 0
	for {
		s.live.drain(s, r)
		if s.Store.CancelRequested(r.ID) { // also catches a cancel that arrived during Start
			return s.finish(r, Cancelled, "cancel requested")
		}
		if err := s.Store.Save(r); err != nil {
			// Can't record progress, so we can't enforce safely: stop spending.
			return errors.Join(err, s.Backend.Stop(context.Background(), r.Handle))
		}
		// Wake at the next tick, or exactly when a cap is reached, whichever is sooner.
		s.Sleep(ctx, min(s.Tick, b.Remaining(s.Now().Sub(started)+reserve, r.PricePerHour)))
		if ctx.Err() != nil {
			return s.finish(r, Cancelled, "interrupted")
		}
		if s.Store.CancelRequested(r.ID) {
			return s.finish(r, Cancelled, "cancel requested")
		}

		// Caps are checked before the backend call, so a slow backend never adds overshoot.
		// Decisions count the shutdown time too (reserve): it is billed, so stopping that much
		// early keeps the final total within the cap.
		elapsed := s.Now().Sub(started)
		_, stop, warn := b.Check(elapsed+reserve, r.PricePerHour)
		r.SpentUSD = elapsed.Hours() * r.PricePerHour
		early := ""
		if reserve > 0 {
			early = fmt.Sprintf("; stopping %s early because shutdown is billed too", reserve)
		}
		switch stop {
		case BudgetExceeded:
			return s.finish(r, stop, fmt.Sprintf("reached the %s cap (spent %s so far%s)", usd(b.MaxUSD), usd(r.SpentUSD), early))
		case TimedOut:
			return s.finish(r, stop, fmt.Sprintf("reached the %gh cap (ran %s%s)", b.MaxHours, elapsed.Round(time.Second), early))
		}

		// The call may not outlive the cap: a slow backend is cut off, and the next loop enforces it.
		sctx, cancel := context.WithTimeout(ctx, min(statusTimeout, b.Remaining(elapsed+reserve, r.PricePerHour)))
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
				b.WarnAt*100, usd(r.SpentUSD), usd(b.MaxUSD), elapsed.Round(time.Second), b.MaxHours))
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
	r.SpentUSD = r.Duration(now).Hours() * r.PricePerHour // final spend including stop time, as kiln saw it
	if r.Handle != "" {
		// Better: the provider's own billed period, which isn't inflated by slow API calls here.
		if st, err := s.Backend.Status(ctx, r.Handle); err == nil && !st.BilledFrom.IsZero() && st.BilledTo.After(st.BilledFrom) {
			r.SpentUSD = st.BilledTo.Sub(st.BilledFrom).Hours() * r.PricePerHour
		}
	}
	if r.Handle != "" {
		// Not fatal: the outcome still gets recorded.
		if err := s.saveLogs(ctx, r); err != nil {
			s.event(r, "could not save logs: "+err.Error())
		}
		if err := s.saveMonitor(ctx, r); err != nil {
			s.event(r, "could not save monitor readings: "+err.Error())
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

// liveMonitor follows the monitor stream in the background. Events reach the run record only
// through drain, on the supervisor's goroutine, so the record is never written concurrently.
type liveMonitor struct {
	events chan string
	lines  map[string]int // record lines processed, so a reconnect doesn't reprocess them
	// sent counts each event already added to the record. The final replay of the full stream
	// adds only events not in here. Comparing events, not lines, matters: the live stream can
	// skip a record (CloudWatch's forward token is time-based, so a late-indexed record is
	// missed), and then it notices e.g. a process exit one reading later than the replay does.
	sent   map[string]int
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *Supervisor) watch(ctx context.Context, r *Run) *liveMonitor {
	ctx, cancel := context.WithCancel(ctx)
	l := &liveMonitor{events: make(chan string, 256), lines: map[string]int{}, sent: map[string]int{}, cancel: cancel, done: make(chan struct{})}
	o, ok := s.Backend.(Observer)
	if !ok {
		close(l.done)
		return l
	}
	go func() {
		defer close(l.done)
		w := &Watcher{}
		for ctx.Err() == nil {
			pr, pw := io.Pipe()
			go func() { pw.CloseWithError(o.Monitor(ctx, r.Handle, true, pw)) }()
			pass := map[string]int{} // a retry re-reads from the start; skip what was processed
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 1<<20), 1<<24)
			for sc.Scan() {
				line := sc.Text()
				if pass[line]++; pass[line] <= l.lines[line] {
					continue
				}
				l.lines[line]++
				_, evs := w.Feed(line)
				for _, e := range evs {
					select {
					case l.events <- e:
					default: // never block the stream on a slow supervisor
					}
				}
			}
			pr.Close()
			sleepCtx(ctx, 5*time.Second) // real time, not s.Sleep: tests fake the clock
		}
	}()
	return l
}

func (l *liveMonitor) drain(s *Supervisor, r *Run) {
	for {
		select {
		case e := <-l.events:
			l.sent[e]++
			s.event(r, e)
		default:
			return
		}
	}
}

func (l *liveMonitor) stop() {
	l.cancel()
	<-l.done
}

// saveMonitor stores every monitor record next to the run, then replays them so events the
// live stream missed (the final reading, the artifact upload) still reach the record.
func (s *Supervisor) saveMonitor(ctx context.Context, r *Run) error {
	o, ok := s.Backend.(Observer)
	if !ok || s.live == nil {
		return nil
	}
	s.live.stop()
	s.live.drain(s, r)
	var buf bytes.Buffer
	if err := o.Monitor(ctx, r.Handle, false, &buf); err != nil {
		return err
	}
	if err := os.WriteFile(s.Store.MonitorPath(r.ID), buf.Bytes(), 0o644); err != nil {
		return err
	}
	return WatchMonitor(&buf, &Watcher{}, func(e string) {
		if s.live.sent[e] > 0 {
			s.live.sent[e]--
			return
		}
		s.event(r, e)
	}, nil)
}

// heartbeat touches the run's .alive file every few seconds until the returned stop is called.
func (s *Supervisor) heartbeat(id string) (stop func()) {
	_ = s.Store.Heartbeat(id)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = s.Store.Heartbeat(id)
			}
		}
	}()
	return func() { close(done) }
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
	if !s.Stale(r, now) {
		return false, s.RequestCancel(r.ID)
	}
	state, reason := Cancelled, "cancelled (supervisor was not running)"
	var billed Status
	if r.Handle != "" {
		if st, err := b.Status(ctx, r.Handle); err == nil && st.State.Terminal() {
			state, reason, billed = st.State, st.Reason+" (recorded after supervisor exit)", st
		} else if err := b.Stop(ctx, r.Handle); err != nil {
			return true, err
		} else if st, err := b.Status(ctx, r.Handle); err == nil {
			billed = st
		}
		_ = b.Remove(ctx, r.Handle) // best effort: the spend has already ended
	}
	// ponytail: a run that crashed mid-Start has no handle; its container (kiln-<id>) needs a manual rm.
	r.State, r.Reason, r.EndedAt = state, reason, &now
	r.SpentUSD = r.Duration(now).Hours() * r.PricePerHour // without provider times: the latest it can have stopped
	if !billed.BilledFrom.IsZero() && billed.BilledTo.After(billed.BilledFrom) {
		r.SpentUSD = billed.BilledTo.Sub(billed.BilledFrom).Hours() * r.PricePerHour
		end := billed.BilledTo
		r.EndedAt = &end
	}
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
