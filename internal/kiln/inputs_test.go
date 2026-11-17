package kiln

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPackInputs(t *testing.T) {
	src := writeTree(t, map[string]string{"data/a.csv": "1,2", "data/sub/b.txt": "b", "prompt.md": "do it"})
	out := t.TempDir()
	in, err := PackInputs(src, []string{"data", "prompt.md"}, nil, filepath.Join(out, "1.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	if in.Files != 3 || in.SHA256 == "" || in.Bytes == 0 {
		t.Fatalf("inputs: %+v", in)
	}

	// Deterministic: same files, later mtimes -> same hash. That is what makes runs comparable.
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(src, "prompt.md"), later, later)
	again, err := PackInputs(src, []string{"data", "prompt.md"}, nil, filepath.Join(out, "2.tgz"))
	if err != nil || again.SHA256 != in.SHA256 {
		t.Errorf("same files must give the same sha256: %v vs %v (%v)", in.SHA256, again.SHA256, err)
	}
	os.WriteFile(filepath.Join(src, "prompt.md"), []byte("do something else"), 0o644)
	if changed, _ := PackInputs(src, []string{"data", "prompt.md"}, nil, filepath.Join(out, "3.tgz")); changed.SHA256 == in.SHA256 {
		t.Error("changed content must change the sha256")
	}

	// Round trip through the same extractor kiln uses for artifacts.
	data, _ := os.ReadFile(in.Local)
	dest := t.TempDir()
	if n, err := ExtractArtifacts(data, dest); err != nil || n != 3 {
		t.Fatalf("extract: %d %v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "data", "sub", "b.txt")); string(b) != "b" {
		t.Errorf("data/sub/b.txt = %q", b)
	}
}

func TestPackInputsErrors(t *testing.T) {
	src := writeTree(t, map[string]string{"a/x": "1", "b/x/y": "2"})
	out := filepath.Join(t.TempDir(), "i.tgz")
	if _, err := PackInputs(src, []string{"missing"}, nil, out); err == nil || !strings.Contains(err.Error(), "input not found") {
		t.Errorf("missing path: %v", err)
	}
	if _, err := PackInputs(src, []string{"a/x", "b/x"}, nil, out); err == nil || !strings.Contains(err.Error(), "both unpack") {
		t.Errorf("two inputs with the same base name must be refused: %v", err)
	}
}

func TestPinImage(t *testing.T) {
	d := "sha256:abc"
	for in, want := range map[string]string{
		"alpine:3.20":                    "alpine@sha256:abc",
		"alpine":                         "alpine@sha256:abc",
		"registry:5000/team/agent:1.4":   "registry:5000/team/agent@sha256:abc",
		"registry:5000/team/agent":       "registry:5000/team/agent@sha256:abc",
		"ghcr.io/a/b@sha256:already":     "ghcr.io/a/b@sha256:already",
		"public.ecr.aws/docker/x:stable": "public.ecr.aws/docker/x@sha256:abc",
	} {
		if got := PinImage(in, d); got != want {
			t.Errorf("PinImage(%s) = %s, want %s", in, got, want)
		}
	}
	if got := PinImage("alpine:3.20", ""); got != "alpine:3.20" {
		t.Errorf("no digest known: keep the image as is, got %s", got)
	}
}

func TestTaskDefinitionInputsGate(t *testing.T) {
	r := &Run{ID: "r_1", Config: Config{Name: "x", Image: "x", Budget: Budget{MaxUSD: 1, MaxHours: 1},
		Resources: Resources{CPU: 0.25, MemoryGB: 0.5}}}
	td, err := (&AWS{Region: "us-east-1"}).taskDefinition(r, &awsInfra{}, map[string]string{"KILN_INPUTS_URL": "https://in"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(td)
	s := string(data)
	for _, want := range []string{`"condition":"HEALTHY"`, `test -f /kiln/.ready`, `{"name":"KILN_INPUTS_URL","value":"https://in"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// The v1.1 bug: inputs: ["."] packed .kiln/ while writing the archive into it.
func TestPackInputsDotExcludesKilnAndGit(t *testing.T) {
	src := writeTree(t, map[string]string{
		"main.go": "package main", "src/app.ts": "x", ".git/HEAD": "ref", ".kiln/runs/old.json": "{}",
		"node_modules/pkg/index.js": "big", "debug.log": "noise", "src/keep.log.txt": "kept",
	})
	dest := filepath.Join(src, ".kiln", "runs", "r_new.inputs.tgz") // archive inside the packed folder
	in, err := PackInputs(src, []string{"."}, []string{"node_modules", "*.log"}, dest)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dest)
	out := t.TempDir()
	if _, err := ExtractArtifacts(data, out); err != nil {
		t.Fatal(err)
	}
	var got []string
	filepath.WalkDir(out, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(out, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	want := "main.go,src/app.ts,src/keep.log.txt"
	if strings.Join(got, ",") != want || in.Files != 3 {
		t.Errorf(`"." packed %v (%d files), want %s at the top of /kiln/inputs`, got, in.Files, want)
	}
	if _, err := PackInputs(src, []string{".", "main.go"}, nil, dest); err == nil {
		t.Error(`"." combined with other inputs must be refused`)
	}
	if _, err := PackInputs(src, []string{"."}, []string{"[bad"}, dest); err == nil {
		t.Error("a malformed exclude pattern must be refused")
	}
}
