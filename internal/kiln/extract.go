package kiln

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ExtractArtifacts unpacks a gzipped tar into dir. The archive comes from inside the task (agent
// code), so it is untrusted: os.Root refuses any path that escapes dir, and only regular files
// and directories are written (no links, devices, or modes beyond rw).
func ExtractArtifacts(data []byte, dir string) (files int, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("artifacts: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, fmt.Errorf("artifacts: %w", err)
		}
		name := filepath.FromSlash(h.Name)
		if !filepath.IsLocal(name) {
			return files, fmt.Errorf("artifacts: refusing path %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return files, err
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return files, err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return files, err
			}
			files++
		}
	}
}
