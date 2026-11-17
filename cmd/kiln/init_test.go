package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kabeer11000/kiln/internal/kiln"
)

func TestInitWritesValidConfig(t *testing.T) {
	for _, backend := range []string{"aws", "docker"} {
		dir := filepath.Join(t.TempDir(), "My Project_2")
		os.MkdirAll(dir, 0o755)
		t.Chdir(dir)
		a := &app{out: &bytes.Buffer{}, store: kiln.Store{Dir: t.TempDir()}}
		if code, err := a.cmdInit([]string{"--backend", backend}); err != nil || code != 0 {
			t.Fatalf("%s: init: %d %v", backend, code, err)
		}
		c, err := kiln.LoadConfig(kiln.DefaultFile)
		if err != nil {
			t.Fatalf("%s: starter config is invalid: %v", backend, err)
		}
		if c.Name != "my-project-2" || c.Backend != backend || c.Budget.WarnAt != 0.8 {
			t.Errorf("%s: %+v", backend, c)
		}
		if backend == "aws" && (c.AWS == nil || !c.Resources.Spot) {
			t.Errorf("aws starter should set aws: and spot: %+v", c)
		}
		if _, err := kiln.PackInputs(".", c.Inputs, c.Exclude, filepath.Join(t.TempDir(), "i.tgz")); err != nil {
			t.Errorf("%s: starter inputs must pack: %v", backend, err)
		}
	}
}

func TestRunName(t *testing.T) {
	for in, want := range map[string]string{"talos": "talos", "My Project_2": "my-project-2", "---": "my-run", "ÄÖ x": "x"} {
		if got := runName(in); got != want {
			t.Errorf("runName(%q) = %q, want %q", in, got, want)
		}
	}
}
