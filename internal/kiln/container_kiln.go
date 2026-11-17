package kiln

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// On docker/podman, /kiln is a bind mount of a folder next to the run record, so the agent sees
// the same layout as on aws: /kiln/inputs, /kiln/artifacts, /kiln/inbox.jsonl, /kiln/outbox.jsonl.
// ponytail: messages and files only; no process/memory readings locally yet (use `docker stats`).

func (c Container) hostDir(id string) (string, error) {
	s, err := DefaultStore()
	if err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, id+"-kiln"), nil
}

// prepare creates the run's /kiln folder and unpacks the inputs into it.
func (c Container) prepare(r *Run) (string, error) {
	dir, err := c.hostDir(r.ID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o777); err != nil {
		return "", err
	}
	for _, f := range []string{"inbox.jsonl", "outbox.jsonl"} {
		fh, err := os.OpenFile(filepath.Join(dir, f), os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return "", err
		}
		fh.Close()
	}
	if r.Repo != nil {
		if err := cloneLocal(context.Background(), r.Config.Repo, r.Repo, filepath.Join(dir, "src")); err != nil {
			return "", err
		}
	}
	if r.Inputs != nil {
		data, err := os.ReadFile(r.Inputs.Local)
		if err != nil {
			return "", &Error{Code: "inputs_missing", Message: "packed inputs " + r.Inputs.Local + " are gone: " + err.Error(),
				Fix: "run `kiln run` again to repack them from the source files"}
		}
		if _, err := ExtractArtifacts(data, filepath.Join(dir, "inputs")); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// Monitor reports the agent's outbox lines as monitor records (follow: as they are written).
func (c Container) Monitor(ctx context.Context, handle string, follow bool, w io.Writer) error {
	dir, err := c.hostDir(trimKilnPrefix(handle))
	if err != nil {
		return err
	}
	sent := 0
	for {
		data, err := os.ReadFile(filepath.Join(dir, "outbox.jsonl"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		n := 0
		for sc.Scan() {
			// Only complete lines: a line still being written has no trailing newline yet.
			if n++; n <= sent || (n == bytes.Count(data, []byte("\n"))+1) {
				continue
			}
			rec, _ := json.Marshal(map[string]any{"kiln": "outbox", "t": time.Now().Unix(), "line": sc.Text()})
			if _, err := fmt.Fprintf(w, "%s\n", rec); err != nil {
				return err
			}
			sent = n
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Send appends to the run's /kiln/inbox.jsonl directly: it is a local file.
func (c Container) Send(_ context.Context, r *Run, msg string) error {
	dir, err := c.hostDir(r.ID)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "inbox.jsonl"), os.O_APPEND|os.O_WRONLY, 0o666)
	if err != nil {
		return &Error{Code: "no_inbox", Message: "run " + r.ID + " has no /kiln folder: " + err.Error()}
	}
	defer f.Close()
	line, _ := json.Marshal(map[string]any{"t": time.Now().Unix(), "msg": msg})
	_, err = f.Write(append(line, '\n'))
	return err
}

// Artifacts packs the run's /kiln/artifacts and message files the same way the aws monitor does.
func (c Container) Artifacts(_ context.Context, r *Run) ([]byte, error) {
	dir, err := c.hostDir(r.ID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, &Error{Code: "no_artifacts", Message: "no /kiln folder for " + r.ID}
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"artifacts", "outbox.jsonl", "inbox.jsonl"} {
		src := filepath.Join(dir, name)
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			hdr := &tar.Header{Name: filepath.ToSlash(rel), ModTime: time.Unix(0, 0)}
			switch {
			case d.IsDir():
				hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, hdr.Name+"/", 0o755
				return tw.WriteHeader(hdr)
			case d.Type().IsRegular():
				return addFile(tw, p, hdr)
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func trimKilnPrefix(handle string) string {
	if len(handle) > 5 && handle[:5] == "kiln-" {
		return handle[5:]
	}
	return handle
}
