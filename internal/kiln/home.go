package kiln

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// HomeEnv overrides where kiln keeps run records. Default: <user config dir>/kiln
// (%AppData%\kiln on Windows, ~/Library/Application Support/kiln on macOS, ~/.config/kiln on Linux).
const HomeEnv = "KILN_HOME"

// DefaultStore is the one run store for this user, so a run is visible from any folder.
func DefaultStore() (Store, error) {
	home := os.Getenv(HomeEnv)
	if home == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return Store{}, &Error{Code: "no_home", Message: "cannot find a folder for run records: " + err.Error(),
				Fix: "set " + HomeEnv + " to a folder kiln can write to"}
		}
		home = filepath.Join(cfg, "kiln")
	}
	abs, err := filepath.Abs(filepath.Join(home, "runs"))
	if err != nil {
		return Store{}, err
	}
	return Store{Dir: abs}, nil
}

// ImportLegacy copies run files from a v1.0/v1.1 project-local store (./.kiln/runs) into s,
// skipping any already there. The old folder is left untouched. Returns how many runs it imported.
func (s Store) ImportLegacy(dir string) (int, error) {
	abs, err := filepath.Abs(dir)
	if err != nil || abs == s.Dir {
		return 0, err
	}
	entries, err := os.ReadDir(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return 0, err
	}
	runs := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "r_") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		dst := filepath.Join(s.Dir, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := copyTree(filepath.Join(abs, name), dst); err != nil {
			return runs, err
		}
		if strings.HasSuffix(name, ".json") {
			runs++
		}
	}
	return runs, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
