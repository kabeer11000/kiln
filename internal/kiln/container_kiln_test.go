package kiln

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The docker /kiln folder, without running docker: what the agent would see and write.
func TestContainerKilnFolder(t *testing.T) {
	t.Setenv(HomeEnv, t.TempDir())
	src := writeTree(t, map[string]string{"prompt.md": "hello"})
	in, err := PackInputs(src, []string{"prompt.md"}, nil, filepath.Join(t.TempDir(), "in.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	c := Container{Bin: "docker"}
	r := &Run{ID: "r_dk", Inputs: in}
	dir, err := c.prepare(r)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "inputs", "prompt.md")); string(b) != "hello" {
		t.Fatalf("inputs not unpacked: %q", b)
	}

	if err := c.Send(context.Background(), r, "stop please"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "inbox.jsonl")); !strings.Contains(string(b), `"msg":"stop please"`) {
		t.Errorf("inbox: %s", b)
	}

	// The agent writes two complete lines and one still in progress.
	os.WriteFile(filepath.Join(dir, "outbox.jsonl"), []byte("one\ntwo\npartial"), 0o644)
	os.WriteFile(filepath.Join(dir, "artifacts", "report.html"), []byte("<h1>ok</h1>"), 0o644)
	var buf bytes.Buffer
	if err := c.Monitor(context.Background(), "kiln-r_dk", false, &buf); err != nil {
		t.Fatal(err)
	}
	var events []string
	WatchMonitor(&buf, &Watcher{}, func(e string) { events = append(events, e) }, nil)
	if strings.Join(events, "|") != "agent: one|agent: two" {
		t.Errorf("outbox events %v (an unfinished line must wait)", events)
	}

	// Follow picks up the line once it is finished, and stops with the context.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.WriteFile(filepath.Join(dir, "outbox.jsonl"), []byte("one\ntwo\npartial done\n"), 0o644)
	}()
	buf.Reset()
	c.Monitor(ctx, "kiln-r_dk", true, &buf)
	if !strings.Contains(buf.String(), "partial done") {
		t.Errorf("follow missed the finished line: %s", buf.String())
	}

	data, err := c.Artifacts(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if n, err := ExtractArtifacts(data, out); err != nil || n != 3 {
		t.Fatalf("artifacts: %d files, %v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "artifacts", "report.html")); string(b) != "<h1>ok</h1>" {
		t.Errorf("report: %q", b)
	}
}
