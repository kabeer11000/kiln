package kiln

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultStoreHonorsKilnHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv(HomeEnv, home)
	s, err := DefaultStore()
	if err != nil || s.Dir != filepath.Join(home, "runs") {
		t.Fatalf("store %q, %v", s.Dir, err)
	}
}

func TestImportLegacy(t *testing.T) {
	legacy := Store{Dir: filepath.Join(t.TempDir(), ".kiln", "runs")}
	r := NewRun(Config{Name: "old", Image: "x"}, time.Now())
	if err := legacy.Save(r); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(legacy.LogPath(r.ID), []byte("log"), 0o644)
	os.MkdirAll(filepath.Join(legacy.ArtifactsDir(r.ID), "artifacts"), 0o755)
	os.WriteFile(filepath.Join(legacy.ArtifactsDir(r.ID), "artifacts", "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(legacy.Dir, "notes.txt"), []byte("not a run file"), 0o644)

	global := Store{Dir: filepath.Join(t.TempDir(), "runs")}
	n, err := global.ImportLegacy(legacy.Dir)
	if err != nil || n != 1 {
		t.Fatalf("imported %d, %v", n, err)
	}
	if got, err := global.Find(r.Name); err != nil || got.ID != r.ID {
		t.Fatalf("imported run not findable: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(global.ArtifactsDir(r.ID), "artifacts", "a.txt")); string(b) != "a" {
		t.Error("artifact folders must come along")
	}
	if _, err := os.Stat(filepath.Join(global.Dir, "notes.txt")); err == nil {
		t.Error("only run files are imported")
	}
	if n, _ := global.ImportLegacy(legacy.Dir); n != 0 {
		t.Errorf("second import must be a no-op, imported %d", n)
	}
	if _, err := os.Stat(legacy.LogPath(r.ID)); err != nil {
		t.Error("the old folder must be left untouched")
	}
}
