package kiln

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
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

// alwaysExcluded are never packed: version control, and kiln's own run folder (packing it
// while writing the archive into it is what made `inputs: ["."]` fail in v1.1).
var alwaysExcluded = []string{".git", ".kiln"}

// PackInputs tars and gzips paths (relative to baseDir) into dest. Each path lands under its
// own base name: ./data/x.csv -> /kiln/inputs/data/x.csv, prompt.md -> /kiln/inputs/prompt.md.
// "." is special: the folder's contents go straight into /kiln/inputs, and it must be the only
// entry. exclude holds glob patterns matched against each file or folder's name and against its
// path inside the input (e.g. "node_modules", "*.log", "build/cache").
func PackInputs(baseDir string, paths, exclude []string, dest string) (*Inputs, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	for _, pat := range exclude {
		if _, err := path.Match(pat, ""); err != nil {
			return nil, &Error{Code: "invalid_inputs", Message: "bad exclude pattern " + pat + ": " + err.Error()}
		}
	}
	destAbs, _ := filepath.Abs(dest)
	f, err := os.Create(dest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	counter := &countWriter{w: io.MultiWriter(f, h)}
	gz := gzip.NewWriter(counter)
	tw := tar.NewWriter(gz)
	in := &Inputs{Paths: paths, Local: destAbs}
	seen := map[string]string{}
	for _, p := range paths {
		src := p
		if !filepath.IsAbs(src) {
			src = filepath.Join(baseDir, p)
		}
		root := filepath.Base(filepath.Clean(src))
		if filepath.Clean(p) == "." {
			if len(paths) > 1 {
				return nil, &Error{Code: "invalid_inputs", Message: `inputs: "." must be the only entry (it already includes everything)`,
					Fix: "use exclude: to leave things out"}
			}
			root = ""
		}
		if prev, dup := seen[root]; dup {
			return nil, &Error{Code: "invalid_inputs", Message: fmt.Sprintf("inputs %s and %s both unpack to /kiln/inputs/%s", prev, p, root),
				Fix: "rename one, or put both in a folder and list the folder"}
		}
		seen[root] = p
		err := filepath.WalkDir(src, func(fp string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(src, fp)
			rel = filepath.ToSlash(rel)
			if abs, _ := filepath.Abs(fp); abs == destAbs {
				return nil // the archive being written
			}
			if rel != "." && excluded(d.Name(), rel, exclude) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			name := path.Join(root, rel)
			if name == "." || name == "" {
				return nil // "." itself: its contents go to the top
			}
			hdr := &tar.Header{Name: name, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
			switch {
			case d.IsDir():
				hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, name+"/", 0o755
				return tw.WriteHeader(hdr)
			case d.Type().IsRegular():
				if err := addFile(tw, fp, hdr); err != nil {
					return err
				}
				in.Files++
				if counter.n > maxInputBytes {
					return &Error{Code: "inputs_too_large", Message: fmt.Sprintf("inputs are over %d MB (at %s)", maxInputBytes>>20, name),
						Fix: "add big folders to exclude: (e.g. node_modules), bake data into the image, or have the agent download it"}
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

func excluded(name, rel string, patterns []string) bool {
	for _, pat := range append(alwaysExcluded, patterns...) {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
		if ok, _ := path.Match(pat, rel); ok {
			return true
		}
	}
	return false
}

// addFile copies exactly the size the header announces. A file that changes while it is
// packed (a log being written, or a build output) gets a clear error instead of tar's
// "write too long".
func addFile(tw *tar.Writer, fp string, hdr *tar.Header) error {
	src, err := os.Open(fp)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
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
	changed := &Error{Code: "inputs_changed_while_packing", Message: "input file " + fp + " changed while it was being packed",
		Fix: "stop whatever is writing it, or add it to exclude:"}
	if _, err := io.CopyN(tw, src, info.Size()); err != nil {
		if errors.Is(err, io.EOF) {
			return changed
		}
		return err
	}
	if n, _ := src.Read(make([]byte, 1)); n > 0 {
		return changed
	}
	return nil
}
