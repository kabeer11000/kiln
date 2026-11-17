package kiln

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Store keeps run records as JSON files in one directory.
// ponytail: local dir only; team-shared store moves to S3/GCS/Blob with the cloud backends.
type Store struct{ Dir string }

func (s Store) path(id, ext string) string { return filepath.Join(s.Dir, id+ext) }

func (s Store) LogPath(id string) string { return s.path(id, ".log") }

// InputsPath is the packed inputs archive. Kept so `kiln rerun` sends byte-identical files.
func (s Store) InputsPath(id string) string { return s.path(id, ".inputs.tgz") }

// OutPath is the output of a background (`run -d`) supervisor.
func (s Store) OutPath(id string) string { return s.path(id, ".out") }

// MonitorPath holds the monitor's JSON records (procs, memory, network, messages), saved at the end.
func (s Store) MonitorPath(id string) string { return s.path(id, ".monitor.jsonl") }

// ArtifactsDir is where `kiln artifacts` unpacks a run's files.
func (s Store) ArtifactsDir(id string) string { return s.path(id, "-artifacts") }

// Save writes atomically so concurrent readers (`kiln status`) never see a partial file.
func (s Store) Save(r *Run) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	r.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(r.ID, ".json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(r.ID, ".json"))
}

func (s Store) Load(id string) (*Run, error) {
	data, err := os.ReadFile(s.path(id, ".json"))
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	return &r, nil
}

// List returns all runs, newest first.
func (s Store) List() ([]*Run, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []*Run
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		r, err := s.Load(id)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	slices.SortFunc(runs, func(a, b *Run) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return runs, nil
}

// Find resolves an exact id, an exact name, or a unique id prefix.
func (s Store) Find(ref string) (*Run, error) {
	if r, err := s.Load(ref); err == nil {
		return r, nil
	}
	runs, err := s.List()
	if err != nil {
		return nil, err
	}
	var hits []*Run
	for _, r := range runs {
		if r.Name == ref || strings.HasPrefix(r.ID, ref) {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 0:
		return nil, &Error{Code: "run_not_found", Message: "no run matches " + ref + " in " + s.Dir,
			Fix: "run `kiln list` to see runs; runs from another machine or another " + HomeEnv + " are not here"}
	case 1:
		return hits[0], nil
	}
	return nil, &Error{Code: "ambiguous_run", Message: fmt.Sprintf("%d runs match %s", len(hits), ref), Fix: "use the full run id"}
}

// Heartbeat marks the run's supervisor as alive. It is a separate file so the supervisor can
// prove it is alive while it's busy (e.g. waiting for a task to stop) without rewriting the record.
func (s Store) Heartbeat(id string) error {
	p := s.path(id, ".alive")
	now := time.Now()
	if err := os.Chtimes(p, now, now); err == nil {
		return nil
	}
	return os.WriteFile(p, nil, 0o644)
}

// Stale reports whether nothing has supervised a not-yet-finished run for StaleAfter.
func (s Store) Stale(r *Run, now time.Time) bool {
	if r.State.Terminal() {
		return false
	}
	last := r.UpdatedAt
	if fi, err := os.Stat(s.path(r.ID, ".alive")); err == nil && fi.ModTime().After(last) {
		last = fi.ModTime()
	}
	return now.Sub(last) > StaleAfter
}

func (s Store) RequestCancel(id string) error {
	return os.WriteFile(s.path(id, ".cancel"), nil, 0o644)
}

func (s Store) CancelRequested(id string) bool {
	_, err := os.Stat(s.path(id, ".cancel"))
	return err == nil
}
