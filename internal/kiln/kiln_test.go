package kiln

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kiln.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "name: exp-1\nimage: alpine\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backend != "docker" || c.Budget.MaxUSD != 5 || c.Budget.MaxHours != 1 || c.Budget.WarnAt != 0.8 ||
		c.Resources.CPU != 1 || c.Resources.MemoryGB != 2 {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct{ name, body, code, contains string }{
		{"empty", "", "invalid_config", "file is empty"},
		{"unknown field typo", "name: a\nimage: x\nbudgt: {}\n", "invalid_config", "budgt"},
		{"bad name", "name: My Exp\nimage: x\n", "invalid_config", "name:"},
		{"no image", "name: a\n", "invalid_config", "image: required"},
		{"bad backend", "name: a\nimage: x\nbackend: mars\n", "invalid_config", `unknown "mars"`},
		{"negative cap", "name: a\nimage: x\nbudget: {max_usd: -1}\n", "invalid_config", "max_usd"},
		{"warn out of range", "name: a\nimage: x\nbudget: {warn_at: 2}\n", "invalid_config", "warn_at"},
		{"bad env name", "name: a\nimage: x\nenv: {\"1BAD\": v}\n", "invalid_config", "1BAD"},
		{"secret in env", "name: a\nimage: x\nenv: {KEY: v}\nsecrets: [KEY]\n", "invalid_config", "also set in env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tt.body))
			ke, ok := err.(*Error)
			if !ok || ke.Code != tt.code || !strings.Contains(ke.Message, tt.contains) || ke.Fix == "" {
				t.Fatalf("got %#v", err)
			}
		})
	}
}

func TestLoadConfigMissing(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if ke, ok := err.(*Error); !ok || ke.Code != "config_not_found" {
		t.Fatalf("got %#v", err)
	}
}

func TestCheckSecrets(t *testing.T) {
	c := Config{Secrets: []string{"A", "B"}}
	env := map[string]string{"A": "1", "B": ""}
	err := c.CheckSecrets(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if ke, ok := err.(*Error); !ok || ke.Code != "missing_secret" || !strings.Contains(ke.Message, "B") {
		t.Fatalf("got %#v", err)
	}
}

func TestBudgetCheck(t *testing.T) {
	b := Budget{MaxUSD: 10, MaxHours: 2, WarnAt: 0.5}
	tests := []struct {
		elapsed time.Duration
		price   float64
		stop    State
		warn    bool
	}{
		{10 * time.Minute, 1, "", false},
		{30 * time.Minute, 10, "", true},            // $5 = 50% of dollar cap
		{time.Hour, 0, "", true},                    // 1h = 50% of hour cap
		{time.Hour, 10, BudgetExceeded, false},      // $10
		{2 * time.Hour, 0, TimedOut, false},         // 2h
		{2 * time.Hour, 100, BudgetExceeded, false}, // both: dollar cap reported
	}
	for _, tt := range tests {
		_, stop, warn := b.Check(tt.elapsed, tt.price)
		if stop != tt.stop || warn != tt.warn {
			t.Errorf("Check(%s, %g) = %q %v, want %q %v", tt.elapsed, tt.price, stop, warn, tt.stop, tt.warn)
		}
	}
	if got := b.MaxSpend(1); got != 2 {
		t.Errorf("MaxSpend(1) = %g, want 2 (hour cap binds)", got)
	}
	if got := b.MaxSpend(100); got != 10 {
		t.Errorf("MaxSpend(100) = %g, want 10 (dollar cap binds)", got)
	}
}

func TestStoreFind(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	now := time.Now()
	a := NewRun(Config{Name: "a"}, now)
	b := NewRun(Config{Name: "b"}, now.Add(time.Second))
	a.ID, b.ID = "r_aaa111", "r_aab222"
	for _, r := range []*Run{a, b} {
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	for ref, want := range map[string]string{"r_aaa111": a.ID, "r_aaa": a.ID, a.Name: a.ID, "r_aab": b.ID} {
		if r, err := s.Find(ref); err != nil || r.ID != want {
			t.Errorf("Find(%q) = %v, %v; want %s", ref, r, err, want)
		}
	}
	if _, err := s.Find("r_aa"); err == nil || err.(*Error).Code != "ambiguous_run" {
		t.Errorf("ambiguous prefix: %v", err)
	}
	if _, err := s.Find("zzz"); err == nil || err.(*Error).Code != "run_not_found" {
		t.Errorf("missing: %v", err)
	}
	runs, _ := s.List()
	if len(runs) != 2 || runs[0].ID != b.ID {
		t.Errorf("List not newest first: %v", runs)
	}
}

func TestDiff(t *testing.T) {
	now := time.Now()
	a := NewRun(Config{Name: "x", Image: "img:1", Env: map[string]string{"MODE": "fast"}}, now)
	b := NewRun(Config{Name: "x", Image: "img:2", Env: map[string]string{"MODE": "fast"}}, now)
	b.State, b.SpentUSD = Failed, 1.5
	var fields []string
	for _, c := range Diff(a, b, now) {
		fields = append(fields, c.Field)
	}
	want := []string{"config.image", "spent_usd", "state"}
	if !slices.Equal(fields, want) {
		t.Fatalf("diff fields %v, want %v", fields, want)
	}
}

func TestContainerRunArgs(t *testing.T) {
	r := &Run{ID: "r_1", Config: Config{
		Image: "alpine", Command: []string{"echo", "hi"},
		Env: map[string]string{"B": "2", "A": "1"}, Secrets: []string{"TOKEN"},
		Resources: Resources{CPU: 0.5, MemoryGB: 1.5},
	}}
	got := strings.Join(Container{Bin: "docker"}.runArgs(r), " ")
	want := "run -d --name kiln-r_1 --label kiln.run=r_1 --cpus 0.5 --memory 1536m -e A=1 -e B=2 -e TOKEN alpine echo hi"
	if got != want {
		t.Fatalf("\ngot  %s\nwant %s", got, want)
	}
}

func TestStaleUsesHeartbeat(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	r := &Run{ID: "r_hb", State: Running}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	later := r.UpdatedAt.Add(StaleAfter + time.Second)
	if !s.Stale(r, later) {
		t.Error("no save and no heartbeat for longer than StaleAfter: stale")
	}
	if err := s.Heartbeat(r.ID); err != nil {
		t.Fatal(err)
	}
	if s.Stale(r, time.Now().Add(StaleAfter/2)) {
		t.Error("a recent heartbeat means a live supervisor, even if the record is old")
	}
	r.State = Succeeded
	if s.Stale(r, later.Add(time.Hour)) {
		t.Error("finished runs are never stale")
	}
}
