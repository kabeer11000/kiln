package kiln

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// AWS's published example: "Authenticating Requests: Using Query Parameters (AWS Signature Version 4)".
func TestPresignS3MatchesAWSExample(t *testing.T) {
	k := awsKey{ID: "AKIAIOSFODNN7EXAMPLE", Secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	u := presignS3(k, "us-east-1", "examplebucket", "test.txt", "GET", now, 24*time.Hour)
	want := "https://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if u != want {
		t.Errorf("got  %s\nwant %s", u, want)
	}
	if u := presignS3(k, "eu-west-1", "b", "runs/r_1/x.tgz", "PUT", now, 30*24*time.Hour); !strings.Contains(u, "X-Amz-Expires=604800") ||
		!strings.HasPrefix(u, "https://b.s3.eu-west-1.amazonaws.com/runs/r_1/x.tgz?") {
		t.Errorf("expiry must cap at 7 days and keep '/' in the path: %s", u)
	}
}

func TestDecodeAddr(t *testing.T) {
	for in, want := range map[string]string{
		"0100007F:0050":                         "127.0.0.1:80",
		"5DB8D822:01BB":                         "34.216.184.93:443",
		"0000000000000000FFFF00000100007F:1F90": "127.0.0.1:8080", // v4-mapped v6
		"B80D0120000000000000000001000000:0016": "[2001:db8::1]:22",
		"junk":                                  "junk",
	} {
		if got := decodeAddr(in); got != want {
			t.Errorf("decodeAddr(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestWatcher(t *testing.T) {
	w := &Watcher{}
	lines := []string{
		`not json`,
		`2026-11-17T12:00:00 {"kiln":"start","t":100,"deadline":200}`,
		`{"kiln":"snapshot","t":100,"procs":[{"pid":7,"name":"chrome","state":"S","cpu_ticks":100,"rss_kb":2048},{"pid":9,"name":"node","state":"S","cpu_ticks":0,"rss_kb":1024}],"net":{"rx":10,"tx":20}}`,
		`{"kiln":"snapshot","t":105,"procs":[{"pid":7,"name":"chrome","state":"S","cpu_ticks":350,"rss_kb":4096},{"pid":11,"name":"sh","state":"Z","cpu_ticks":0,"rss_kb":0}]}`,
		`{"kiln":"conns","t":105,"conns":[{"r":"5DB8D822:01BB","s":"01"},{"r":"5DB8D822:01BB","s":"01"}]}`,
		`{"kiln":"stats","t":105,"stats":{"abc":{"name":"/agent","memory_stats":{"usage":1048576,"max_usage":3145728}},"p":{"name":"aws-fargate-pause"},"gone":null}}`,
		`{"kiln":"snapshot","t":110,"procs":[{"pid":7,"name":"chrome","state":"S","cpu_ticks":350,"rss_kb":4096}]}`,
		`{"kiln":"outbox","t":106,"line":"migrated 14/20 files"}`,
		`{"kiln":"uploaded","t":107,"bytes":2048}`,
	}
	var events []string
	maxCPU := 0.0
	err := WatchMonitor(strings.NewReader(strings.Join(lines, "\n")), w, func(e string) { events = append(events, e) },
		func(r *MonitorRecord) {
			for _, p := range r.Procs {
				maxCPU = max(maxCPU, p.CPUPct)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if maxCPU != 50 { // chrome: 250 ticks over 5 s = 2.5 s of CPU = 50%
		t.Errorf("cpu%% = %v, want 50", maxCPU)
	}
	all := strings.Join(events, "\n")
	for _, want := range []string{"monitor started",
		"process sh (pid 11) exited and was not reaped (zombie)", "agent: migrated 14/20 files", "artifacts uploaded (2.0 KB"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing event %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "node") { // seen in one reading only: too short-lived to report
		t.Errorf("short-lived process reported:\n%s", all)
	}
	if w.View.PeakRSSKB["chrome"] != 4096 || len(w.View.Memory) != 1 {
		t.Errorf("peaks %v, memory %v", w.View.PeakRSSKB, w.View.Memory)
	}
	if w.View.Conns["34.216.184.93:443 established"] != 2 || w.View.Memory["agent"] != (ContainerMem{1048576, 3145728}) {
		t.Errorf("view: %+v", w.View)
	}
	var out bytes.Buffer
	w.View.Render(&out, 10)
	for _, want := range []string{"agent 1.0 MB (peak 3.0 MB)", "34.216.184.93:443 established x2", "peak/proc: chrome 4.0 MB"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("render missing %q:\n%s", want, out.String())
		}
	}
}

func tgz(t *testing.T, entries ...tar.Header) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		body := "data:" + h.Name
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractArtifacts(t *testing.T) {
	dir := t.TempDir()
	data := tgz(t,
		tar.Header{Name: "artifacts/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "artifacts/report/index.html", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "outbox.jsonl", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "artifacts/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	)
	n, err := ExtractArtifacts(data, dir)
	if err != nil || n != 2 {
		t.Fatalf("extract: %d files, %v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "artifacts", "report", "index.html")); string(b) != "data:artifacts/report/index.html" {
		t.Errorf("content %q", b)
	}
	if _, err := os.Lstat(filepath.Join(dir, "artifacts", "link")); err == nil {
		t.Error("symlinks must not be extracted")
	}

	for _, evil := range []string{"../escape.txt", "/abs.txt", "a/../../escape.txt"} {
		if _, err := ExtractArtifacts(tgz(t, tar.Header{Name: evil, Typeflag: tar.TypeReg}), t.TempDir()); err == nil {
			t.Errorf("%s: path escape must be refused", evil)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(dir))
	if slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == "escape.txt" }) {
		t.Error("file escaped the target dir")
	}
}
