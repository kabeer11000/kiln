package kiln

import (
	"bufio"
	"cmp"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// monitorScript runs in the kiln-monitor container next to the agent. See monitor.sh.
//
//go:embed monitor.sh
var monitorScript string

// clockTicks is USER_HZ on Linux: /proc/<pid>/stat CPU times are in 1/100 s.
const clockTicks = 100

// MonitorRecord is one line the monitor prints. Kind says which fields are set.
type MonitorRecord struct {
	Kind      string               `json:"kiln"` // start snapshot final conns stats outbox inbox uploaded upload_failed deadline
	T         int64                `json:"t"`
	Procs     []Proc               `json:"procs,omitempty"`
	Net       *NetCounters         `json:"net,omitempty"`
	Disk      *DiskUsage           `json:"disk,omitempty"`
	Artifacts *ArtifactsCount      `json:"artifacts,omitempty"`
	Files     []ArtifactFile       `json:"files,omitempty"`
	Conns     []RawConn            `json:"conns,omitempty"`
	Stats     map[string]*struct { // ECS task stats (Docker format); null for containers not running
		Name        string `json:"name"`
		MemoryStats struct {
			Usage    uint64 `json:"usage"`
			MaxUsage uint64 `json:"max_usage"`
		} `json:"memory_stats"`
	} `json:"stats,omitempty"`
	Line     string `json:"line,omitempty"`
	Lines    int    `json:"lines,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	Deadline int64  `json:"deadline,omitempty"`
	SHA      string `json:"sha,omitempty"`  // repo
	KB       int64  `json:"kb,omitempty"`   // repo checkout size
	Secs     int64  `json:"secs,omitempty"` // repo checkout time
}

type Proc struct {
	PID      int     `json:"pid"`
	PPID     int     `json:"ppid"`
	Name     string  `json:"name"`
	State    string  `json:"state"` // R S D Z T ...
	CPUTicks float64 `json:"cpu_ticks"`
	RSSKB    float64 `json:"rss_kb"`
	CPUPct   float64 `json:"cpu_pct"` // computed by kiln from consecutive snapshots
}

type NetCounters struct {
	RX float64 `json:"rx"`
	TX float64 `json:"tx"`
}

type DiskUsage struct {
	UsedKB  float64 `json:"used_kb"`
	TotalKB float64 `json:"total_kb"`
}

type ArtifactsCount struct {
	Files int     `json:"files"`
	KB    float64 `json:"kb"`
}

type ArtifactFile struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// RawConn is a /proc/net/tcp row: remote address in kernel hex, and the TCP state code.
type RawConn struct {
	R string `json:"r"`
	S string `json:"s"`
}

// View is the latest picture of a running task, built from the monitor's records.
type View struct {
	At        time.Time               `json:"at"`
	Procs     []Proc                  `json:"procs"`
	Net       *NetCounters            `json:"net,omitempty"`
	Disk      *DiskUsage              `json:"disk,omitempty"`
	Artifacts *ArtifactsCount         `json:"artifacts,omitempty"`
	Files     []ArtifactFile          `json:"files,omitempty"`
	Conns     map[string]int          `json:"conns,omitempty"`       // "ip:port state" -> count
	Memory    map[string]ContainerMem `json:"memory,omitempty"`      // per container, from ECS
	PeakRSSKB map[string]float64      `json:"peak_rss_kb,omitempty"` // biggest single process seen, by name
}

type ContainerMem struct {
	Bytes     uint64 `json:"bytes"`
	PeakBytes uint64 `json:"peak_bytes"`
}

// Watcher turns monitor records into a View plus human-readable events (crashes, agent messages).
type Watcher struct {
	View  View
	prev  map[int]Proc
	seen  map[int]int // readings each pid appeared in
	prevT int64
}

// Feed takes one log line. It returns the parsed record (nil if the line isn't one) and any events.
func (w *Watcher) Feed(line string) (*MonitorRecord, []string) {
	i := strings.IndexByte(line, '{') // `aws logs tail` prefixes a timestamp
	if i < 0 {
		return nil, nil
	}
	var rec MonitorRecord
	if json.Unmarshal([]byte(line[i:]), &rec) != nil || rec.Kind == "" {
		return nil, nil
	}
	var ev []string
	at := time.Unix(rec.T, 0)
	switch rec.Kind {
	case "start":
		ev = append(ev, fmt.Sprintf("monitor started; hard deadline %s", time.Unix(rec.Deadline, 0).Format("15:04:05")))
	case "snapshot", "final":
		ev = append(ev, w.snapshot(rec, at)...)
	case "conns":
		w.View.Conns = map[string]int{}
		for _, c := range rec.Conns {
			w.View.Conns[decodeAddr(c.R)+" "+tcpState(c.S)]++
		}
	case "stats":
		w.View.Memory = map[string]ContainerMem{}
		for _, s := range rec.Stats {
			if s == nil || s.Name == "" || strings.Contains(s.Name, "fargate-pause") {
				continue
			}
			w.View.Memory[strings.TrimPrefix(s.Name, "/")] = ContainerMem{s.MemoryStats.Usage, s.MemoryStats.MaxUsage}
		}
	case "outbox":
		ev = append(ev, "agent: "+rec.Line)
	case "inbox":
		ev = append(ev, fmt.Sprintf("inbox delivered to agent (%d message(s) total)", rec.Lines))
	case "uploaded":
		ev = append(ev, "artifacts uploaded ("+humanBytes(float64(rec.Bytes))+" compressed); get them with `kiln artifacts`")
	case "inputs":
		ev = append(ev, "inputs unpacked into /kiln/inputs ("+humanBytes(float64(rec.Bytes))+" compressed)")
	case "repo":
		ev = append(ev, fmt.Sprintf("repo checked out at /kiln/src (%s, %s in %ds)", rec.SHA[:min(12, len(rec.SHA))], humanBytes(float64(rec.KB)*1024), rec.Secs))
	case "repo_failed":
		ev = append(ev, "repo checkout failed; the agent was not started: "+rec.Line)
	case "inputs_failed":
		ev = append(ev, "inputs download failed; the agent was not started")
	case "upload_failed":
		ev = append(ev, "artifact upload failed")
	case "deadline":
		ev = append(ev, "hard deadline reached inside the task")
	}
	return &rec, ev
}

func (w *Watcher) snapshot(rec MonitorRecord, at time.Time) []string {
	var ev []string
	cur := map[int]Proc{}
	if w.seen == nil {
		w.seen, w.View.PeakRSSKB = map[int]int{}, map[string]float64{}
	}
	dt := float64(rec.T - w.prevT)
	for i, p := range rec.Procs {
		if old, ok := w.prev[p.PID]; ok && dt > 0 && old.Name == p.Name {
			rec.Procs[i].CPUPct = max(0, (p.CPUTicks-old.CPUTicks)/clockTicks/dt*100)
		}
		if p.State == "Z" {
			if old, ok := w.prev[p.PID]; !ok || old.State != "Z" {
				ev = append(ev, fmt.Sprintf("process %s (pid %d) exited and was not reaped (zombie)", p.Name, p.PID))
			}
		}
		cur[p.PID] = rec.Procs[i]
		w.seen[p.PID]++
		w.View.PeakRSSKB[p.Name] = max(w.View.PeakRSSKB[p.Name], p.RSSKB)
	}
	// The monitor lists the top processes by memory; only call one gone if it was small enough
	// to still be listed (the list wasn't full), and only if it lived through 2+ readings, so
	// short-lived helpers (sleep, grep in a loop) don't flood the events.
	for _, pid := range slices.Sorted(maps.Keys(w.prev)) {
		if _, ok := cur[pid]; !ok {
			if p := w.prev[pid]; len(rec.Procs) < 80 && w.seen[pid] >= 2 {
				ev = append(ev, fmt.Sprintf("process %s (pid %d) exited, was using %s", p.Name, pid, humanBytes(p.RSSKB*1024)))
			}
			delete(w.seen, pid)
		}
	}
	w.prev, w.prevT = cur, rec.T
	w.View.At, w.View.Procs, w.View.Net, w.View.Disk, w.View.Artifacts = at, rec.Procs, rec.Net, rec.Disk, rec.Artifacts
	if rec.Kind == "final" {
		w.View.Files = rec.Files
	}
	return ev
}

// WatchMonitor reads monitor log lines until EOF, calling onEvent for events and onRecord for each record.
func WatchMonitor(r io.Reader, w *Watcher, onEvent func(string), onRecord func(*MonitorRecord)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		rec, ev := w.Feed(sc.Text())
		if rec == nil {
			continue
		}
		for _, e := range ev {
			if onEvent != nil {
				onEvent(e)
			}
		}
		if onRecord != nil {
			onRecord(rec)
		}
	}
	return sc.Err()
}

// Render writes the view for humans: memory, network, disk, then the busiest processes.
func (v View) Render(w io.Writer, maxProcs int) {
	if v.At.IsZero() {
		fmt.Fprintln(w, "no readings yet (the monitor reports every few seconds once the task is running)")
		return
	}
	fmt.Fprintf(w, "--- %s\n", v.At.Format("15:04:05"))
	if len(v.Memory) > 0 {
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(v.Memory)) {
			m := v.Memory[k]
			parts = append(parts, fmt.Sprintf("%s %s (peak %s)", k, humanBytes(float64(m.Bytes)), humanBytes(float64(m.PeakBytes))))
		}
		fmt.Fprintf(w, "memory:    %s\n", strings.Join(parts, ", "))
	}
	if len(v.PeakRSSKB) > 0 {
		names := slices.SortedFunc(maps.Keys(v.PeakRSSKB), func(a, b string) int { return cmp.Compare(v.PeakRSSKB[b], v.PeakRSSKB[a]) })
		var parts []string
		for _, n := range names[:min(len(names), 5)] {
			parts = append(parts, n+" "+humanBytes(v.PeakRSSKB[n]*1024))
		}
		fmt.Fprintf(w, "peak/proc: %s\n", strings.Join(parts, ", "))
	}
	if v.Net != nil {
		fmt.Fprintf(w, "network:   in %s, out %s\n", humanBytes(v.Net.RX), humanBytes(v.Net.TX))
	}
	if v.Disk != nil {
		fmt.Fprintf(w, "disk:      %s of %s\n", humanBytes(v.Disk.UsedKB*1024), humanBytes(v.Disk.TotalKB*1024))
	}
	if v.Artifacts != nil {
		fmt.Fprintf(w, "artifacts: %d file(s), %s\n", v.Artifacts.Files, humanBytes(v.Artifacts.KB*1024))
	}
	if len(v.Conns) > 0 {
		keys := slices.Sorted(maps.Keys(v.Conns))
		shown := keys[:min(len(keys), 8)]
		var parts []string
		for _, k := range shown {
			s := k
			if n := v.Conns[k]; n > 1 {
				s += fmt.Sprintf(" x%d", n)
			}
			parts = append(parts, s)
		}
		more := ""
		if len(keys) > len(shown) {
			more = fmt.Sprintf(" (+%d more)", len(keys)-len(shown))
		}
		fmt.Fprintf(w, "conns:     %s%s\n", strings.Join(parts, ", "), more)
	}
	fmt.Fprintf(w, "%7s  %-16s %5s %6s  %s\n", "PID", "PROCESS", "STATE", "CPU%", "MEMORY")
	for _, p := range v.Procs[:min(len(v.Procs), maxProcs)] {
		fmt.Fprintf(w, "%7d  %-16s %5s %6.1f  %s\n", p.PID, p.Name, p.State, p.CPUPct, humanBytes(p.RSSKB*1024))
	}
	if len(v.Procs) > maxProcs {
		fmt.Fprintf(w, "  ... %d more processes\n", len(v.Procs)-maxProcs)
	}
	for _, f := range v.Files {
		fmt.Fprintf(w, "  artifact %s (%s)\n", f.Path, humanBytes(float64(f.Bytes)))
	}
}

// decodeAddr turns a /proc/net/tcp{,6} address ("0100007F:0050") into "127.0.0.1:80".
func decodeAddr(s string) string {
	h, p, ok := strings.Cut(s, ":")
	b, err := hex.DecodeString(h)
	port, perr := strconv.ParseUint(p, 16, 16)
	if !ok || err != nil || perr != nil || (len(b) != 4 && len(b) != 16) {
		return s
	}
	// The kernel prints each 32-bit word in host (little-endian) order.
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	ip := net.IP(b)
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))
}

func tcpState(s string) string {
	names := map[string]string{"01": "established", "02": "syn_sent", "03": "syn_recv", "04": "fin_wait1",
		"05": "fin_wait2", "06": "time_wait", "07": "close", "08": "close_wait", "09": "last_ack", "0B": "closing"}
	if n, ok := names[s]; ok {
		return n
	}
	return s
}

func humanBytes(b float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", b)
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}
