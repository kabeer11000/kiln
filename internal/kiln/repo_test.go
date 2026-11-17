package kiln

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo makes a local repo with two commits on main, a lightweight tag and an annotated tag.
func gitRepo(t *testing.T) (url, first, second string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "safe.directory=*", "-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "app.txt"), []byte("v1"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "one")
	first = run("rev-parse", "HEAD")
	run("tag", "-a", "v1", "-m", "release v1")
	os.WriteFile(filepath.Join(dir, "app.txt"), []byte("v2"), 0o644)
	run("commit", "-q", "-am", "two")
	second = run("rev-parse", "HEAD")
	return "file://" + filepath.ToSlash(dir), first, second
}

func TestResolveRepo(t *testing.T) {
	url, first, second := gitRepo(t)
	ctx := context.Background()
	for ref, want := range map[string]string{"": second, "main": second, "v1": first, first: first} {
		info, err := ResolveRepo(ctx, &RepoConfig{URL: url, Ref: ref})
		if err != nil || info.SHA != want {
			t.Errorf("ref %q: sha %v, err %v; want %s", ref, info, err, want)
		}
	}
	if _, err := ResolveRepo(ctx, &RepoConfig{URL: url, Ref: "nope"}); err == nil || !strings.Contains(err.Error(), "no branch or tag nope") {
		t.Errorf("missing ref: %v", err)
	}
	if _, err := ResolveRepo(ctx, &RepoConfig{URL: url + "-missing"}); err == nil {
		t.Error("unreachable repo must fail before anything starts")
	}
	if info, err := ResolveRepo(ctx, nil); info != nil || err != nil {
		t.Error("no repo: nothing to resolve")
	}
}

func TestCloneLocalChecksOutExactCommit(t *testing.T) {
	url, first, _ := gitRepo(t)
	dir := filepath.Join(t.TempDir(), "src")
	if err := cloneLocal(context.Background(), &RepoConfig{URL: url}, &RepoInfo{URL: url, SHA: first}, dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "app.txt")); string(b) != "v1" {
		t.Errorf("checked out %q, want the first commit's v1", b)
	}
}

func TestRepoConfig(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "name: a\nimage: node:22\nrepo: {url: https://github.com/a/b.git, token: GH_TOKEN}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workdir != "/kiln/src" {
		t.Errorf("workdir defaults to the checkout: %q", c.Workdir)
	}
	for yaml, want := range map[string]string{
		"repo: {ref: main}\n":                                   "repo.url: required",
		"repo: {url: https://x/y.git, token: ghp_abc-123}\n":    "the NAME of a secret",
		"repo: {url: git@github.com:a/b.git, token: GH}\n":      "only works with https",
		"repo: {url: https://x/y.git}\nworkdir: relative/dir\n": "workdir: must be an absolute path",
	} {
		if _, err := LoadConfig(writeConfig(t, "name: a\nimage: x\n"+yaml)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", strings.TrimSpace(yaml), err, want)
		}
	}
}

func TestTaskDefinitionRepo(t *testing.T) {
	r := &Run{ID: "r_1", Repo: &RepoInfo{URL: "https://github.com/a/b.git", Ref: "main", SHA: strings.Repeat("a", 40)},
		Config: Config{Name: "x", Image: "node:22", Workdir: "/kiln/src", Repo: &RepoConfig{URL: "https://github.com/a/b.git", Token: "GH_TOKEN"},
			Budget: Budget{MaxUSD: 1, MaxHours: 1}, Resources: Resources{CPU: 0.25, MemoryGB: 0.5}}}
	td, err := (&AWS{Region: "us-east-1"}).taskDefinition(r, &awsInfra{SecretsPrefix: "/kiln/kiln/secrets/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defs := td["containerDefinitions"].([]any)
	agent, _ := json.Marshal(defs[0])
	mon, _ := json.Marshal(defs[1])
	for _, want := range []string{`"workingDirectory":"/kiln/src"`, `{"name":"KILN_REPO_SHA","value":"` + strings.Repeat("a", 40) + `"}`} {
		if !strings.Contains(string(agent), want) {
			t.Errorf("agent missing %s", want)
		}
	}
	for _, want := range []string{`{"name":"KILN_REPO_URL","value":"https://github.com/a/b.git"}`,
		`{"name":"KILN_REPO_TOKEN","valueFrom":"/kiln/kiln/secrets/GH_TOKEN"}`} {
		if !strings.Contains(string(mon), want) {
			t.Errorf("monitor missing %s", want)
		}
	}
	if strings.Contains(string(agent), "KILN_REPO_TOKEN") {
		t.Error("the token is injected into the monitor only")
	}
}
