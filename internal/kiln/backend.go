package kiln

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Error is the one error shape for humans and agents: what went wrong and how to fix it.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Status is what a backend knows about a running task.
type Status struct {
	State       State  // Running, Succeeded, Failed or TimedOut
	ImageDigest string // once known
	Reason      string // why it stopped, if it did
}

// Backend runs one task somewhere. Everything else in kiln is backend-agnostic.
type Backend interface {
	// Preflight checks the backend is reachable and every secret exists, before anything is started.
	Preflight(ctx context.Context, c Config) error
	Start(ctx context.Context, r *Run) (handle string, err error)
	Status(ctx context.Context, handle string) (Status, error)
	Logs(ctx context.Context, handle string, follow bool, w io.Writer) error
	// Stop ends the task, and with it the spend. Logs must stay readable. Must succeed if already stopped or gone.
	Stop(ctx context.Context, handle string) error
	// Remove deletes whatever Stop left behind. Must succeed if already gone.
	Remove(ctx context.Context, handle string) error
	PricePerHour(Resources) float64
}

// Observer is a backend that runs the kiln monitor next to the agent: live process, memory,
// network and disk readings, messages in both directions, and artifacts. AWS only for now.
type Observer interface {
	// Monitor writes the monitor's JSON records, one per line (see MonitorRecord).
	Monitor(ctx context.Context, handle string, follow bool, w io.Writer) error
	// Send delivers a message to the agent's /kiln/inbox.jsonl.
	Send(ctx context.Context, r *Run, msg string) error
	// Artifacts returns the gzipped tar of /kiln/artifacts and the message logs, uploaded at stop.
	Artifacts(ctx context.Context, r *Run) ([]byte, error)
}

// ObserverFor returns b's Observer, or an error naming the backends that have one.
func ObserverFor(b Backend, backend string) (Observer, error) {
	if o, ok := b.(Observer); ok {
		return o, nil
	}
	return nil, &Error{Code: "not_supported", Message: "live monitoring, messages and artifacts are not available on the " + backend + " backend yet",
		Fix: "use backend: aws"}
}

var backends = map[string]func(Config) Backend{
	"docker": func(Config) Backend { return Container{Bin: "docker"} },
	"podman": func(Config) Backend { return Container{Bin: "podman"} },
	"aws":    func(c Config) Backend { return &AWS{Region: c.AWS.Region, Stack: c.AWS.Stack} },
}

func BackendFor(c Config) (Backend, error) {
	f, ok := backends[c.Backend]
	if !ok {
		return nil, &Error{Code: "unsupported_backend", Message: "backend " + c.Backend + " is not supported", Fix: "use backend: aws, docker or podman"}
	}
	return f(c), nil
}

// PriceFor is the hourly price used for the cost cap: the config override, else the backend's price.
func PriceFor(c Config, b Backend) float64 {
	if c.Budget.PricePerHour != nil {
		return *c.Budget.PricePerHour
	}
	return b.PricePerHour(c.Resources)
}

// runCLI runs a backend's command-line tool and turns failures into an *Error with a fix hint.
func runCLI(ctx context.Context, fix, bin string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", &Error{Code: "backend_error", Message: fmt.Sprintf("%s %s: %s", bin, strings.Join(args[:min(2, len(args))], " "), msg), Fix: fix}
	}
	return stdout.String(), nil
}
