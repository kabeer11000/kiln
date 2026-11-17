package kiln

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// Container runs on a local docker or podman via their CLI (both share the same flags).
type Container struct{ Bin string }

func (c Container) Preflight(_ context.Context, cfg Config) error {
	if len(cfg.Inputs) > 0 {
		return &Error{Code: "not_supported", Message: "inputs: is not supported on the " + c.Bin + " backend yet", Fix: "use backend: aws, or COPY the files into your image"}
	}
	return cfg.CheckSecrets(os.LookupEnv)
}

func (c Container) Start(ctx context.Context, r *Run) (string, error) {
	name := "kiln-" + r.ID
	if _, err := c.exec(ctx, c.runArgs(r)...); err != nil {
		_ = c.Remove(context.Background(), name) // `run` can leave a created container behind
		return "", err
	}
	return name, nil
}

func (c Container) runArgs(r *Run) []string {
	cfg := r.Config
	args := []string{"run", "-d",
		"--name", "kiln-" + r.ID,
		"--label", "kiln.run=" + r.ID,
		"--cpus", strconv.FormatFloat(cfg.Resources.CPU, 'f', -1, 64),
		"--memory", fmt.Sprintf("%dm", int(cfg.Resources.MemoryGB*1024)),
	}
	for _, k := range slices.Sorted(maps.Keys(cfg.Env)) {
		args = append(args, "-e", k+"="+cfg.Env[k])
	}
	for _, s := range cfg.Secrets {
		args = append(args, "-e", s) // name only: value comes from our env, never hits the run record
	}
	args = append(args, cfg.Image)
	return append(args, cfg.Command...)
}

func (c Container) Status(ctx context.Context, id string) (Status, error) {
	out, err := c.exec(ctx, "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}} {{.Image}}", id)
	if err != nil {
		return Status{}, err
	}
	f := strings.Fields(out)
	if len(f) != 3 {
		return Status{}, fmt.Errorf("%s inspect: unexpected output %q", c.Bin, out)
	}
	st := Status{ImageDigest: f[2]}
	switch f[0] {
	case "created", "configured", "initialized", "running", "restarting", "paused":
		st.State = Running
	case "exited", "stopped", "dead":
		st.State, st.Reason = Failed, "exit code "+f[1]
		if f[1] == "0" {
			st.State, st.Reason = Succeeded, "exit code 0"
		}
	default:
		return Status{}, fmt.Errorf("%s inspect: unknown container status %q", c.Bin, f[0])
	}
	return st, nil
}

func (c Container) Logs(ctx context.Context, id string, follow bool, w io.Writer) error {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	cmd := exec.CommandContext(ctx, c.Bin, append(args, id)...)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// Stop gives the container 10s to exit cleanly, then kills it. The container (and its logs) stay.
func (c Container) Stop(ctx context.Context, id string) error {
	return ignoreGone(c.exec(ctx, "stop", "-t", "10", id))
}

func (c Container) Remove(ctx context.Context, id string) error {
	return ignoreGone(c.exec(ctx, "rm", "-f", id))
}

func ignoreGone(_ string, err error) error {
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no such container") || strings.Contains(msg, "no container with") {
			return nil
		}
	}
	return err
}

// PricePerHour is 0 locally: your own machine. Use budget.price_per_hour to test caps.
func (c Container) PricePerHour(Resources) float64 { return 0 }

func (c Container) exec(ctx context.Context, args ...string) (string, error) {
	return runCLI(ctx, fmt.Sprintf("check that %s is installed and running (`%s info`)", c.Bin, c.Bin), c.Bin, args...)
}
