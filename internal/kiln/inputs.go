package kiln

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// maxInputBytes caps the packed inputs. Bigger data belongs in the image or object storage.
// ponytail: uploaded in one in-memory PUT; switch to multipart upload if teams need GBs.
const maxInputBytes = 512 << 20

// Inputs records the files sent into a run. The archive is deterministic (sorted, zeroed
// times and owners), so the same files always give the same SHA-256.
type Inputs struct {
	Paths  []string `json:"paths"`
	Files  int      `json:"files"`
	Bytes  int64    `json:"bytes"`  // compressed
	SHA256 string   `json:"sha256"` // of the archive
	Local  string   `json:"local"`  // archive kept next to the run record, reused by `kiln rerun`
}

// PackInputs tars and gzips paths (relative to baseDir) into dest. Each path lands under its
// own base name: ./data/x.csv -> /kiln/inputs/data/x.csv, prompt.md -> /kiln/inputs/prompt.md.
func PackInputs(baseDir string, paths []string, dest string) (*Inputs, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(dest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	counter := &countWriter{w: io.MultiWriter(f, h)}
	gz := gzip.NewWriter(counter)
	tw := tar.NewWriter(gz)
	in := &Inputs{Paths: paths, Local: dest}
	seen := map[string]string{}
	for _, p := range paths {
		src := p
		if !filepath.IsAbs(src) {
			src = filepath.Join(baseDir, p)
		}
		root := filepath.Base(filepath.Clean(src))
		if prev, dup := seen[root]; dup {
			return nil, &Error{Code: "invalid_inputs", Message: fmt.Sprintf("inputs %s and %s both unpack to /kiln/inputs/%s", prev, p, root),
				Fix: "rename one, or put both in a folder and list the folder"}
		}
		seen[root] = p
		err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(src, path)
			name := filepath.ToSlash(filepath.Join(root, rel))
			hdr := &tar.Header{Name: name, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
			switch {
			case d.IsDir():
				hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, name+"/", 0o755
				return tw.WriteHeader(hdr)
			case d.Type().IsRegular():
				info, err := d.Info()
				if err != nil {
					return err
				}
				hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, 0o644, info.Size()
				if info.Mode()&0o111 != 0 {
					hdr.Mode = 0o755
				}
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				src, err := os.Open(path)
				if err != nil {
					return err
				}
				defer src.Close()
				if _, err := io.Copy(tw, src); err != nil {
					return err
				}
				in.Files++
				if counter.n > maxInputBytes {
					return &Error{Code: "inputs_too_large", Message: fmt.Sprintf("inputs are over %d MB", maxInputBytes>>20),
						Fix: "bake large data into the image, or have the agent download it"}
				}
			}
			return nil // symlinks and special files are skipped
		})
		if err != nil {
			if os.IsNotExist(err) {
				return nil, &Error{Code: "invalid_inputs", Message: "input not found: " + p, Fix: "paths in inputs: are relative to kiln.yaml"}
			}
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	in.Bytes, in.SHA256 = counter.n, hex.EncodeToString(h.Sum(nil))
	return in, f.Close()
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
