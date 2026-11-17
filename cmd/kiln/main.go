// Command kiln runs long-running agent tasks in containers with hard cost caps.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kabeer11000/kiln/internal/kiln"
)

const usage = `kiln: run agent tasks in containers with hard cost caps

usage: kiln <command> [flags] [--json]

commands:
  init                      write a starter kiln.yaml
  validate [-f file]        check config, print it with defaults
  run [-f file] [--plan]    start and supervise a run (--plan: show cost, don't start)
  list                      all runs, newest first
  status <run>              one run (id, id prefix, or name)
  logs <run> [-f]           run logs
  cancel <run>              stop a run
  diff <a> <b>              what changed between two runs

every command accepts --json for machine-readable output.
`

type app struct {
	out   io.Writer
	json  bool
	store kiln.Store
}

func main() {
	args, asJSON := extractJSON(os.Args[1:])
	a := &app{out: os.Stdout, json: asJSON, store: kiln.Store{Dir: ".kiln/runs"}}
	os.Exit(a.main(args))
}

func (a *app) main(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.out, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	handlers := map[string]func([]string) (int, error){
		"init": a.cmdInit, "validate": a.cmdValidate, "run": a.cmdRun, "list": a.cmdList,
		"status": a.cmdStatus, "logs": a.cmdLogs, "cancel": a.cmdCancel, "diff": a.cmdDiff,
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Fprint(a.out, usage)
		return 0
	}
	h, ok := handlers[cmd]
	if !ok {
		return a.fail(&kiln.Error{Code: "unknown_command", Message: "unknown command " + cmd, Fix: "run `kiln help`"}, 2)
	}
	code, err := h(rest)
	if err != nil {
		var ke *kiln.Error
		if errors.As(err, &ke) && ke.Code == "usage" {
			code = 2
		}
		return a.fail(err, max(code, 1))
	}
	return code
}

func (a *app) cmdInit(args []string) (int, error) {
	if _, err := a.parse("init", args, 0); err != nil {
		return 2, err
	}
	if _, err := os.Stat(kiln.DefaultFile); err == nil {
		return 1, &kiln.Error{Code: "config_exists", Message: kiln.DefaultFile + " already exists", Fix: "edit it, or delete it and run `kiln init` again"}
	}
	if err := os.WriteFile(kiln.DefaultFile, []byte(starterConfig), 0o644); err != nil {
		return 1, err
	}
	a.emit(map[string]string{"created": kiln.DefaultFile}, func() {
		fmt.Fprintf(a.out, "created %s; edit it, then `kiln run --plan`\n", kiln.DefaultFile)
	})
	return 0, nil
}

func (a *app) cmdValidate(args []string) (int, error) {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	file := fs.String("f", kiln.DefaultFile, "config file")
	if _, err := a.parseFS(fs, args, 0); err != nil {
		return 2, err
	}
	c, err := kiln.LoadConfig(*file)
	if err != nil {
		return 1, err
	}
	a.emit(c, func() {
		fmt.Fprintf(a.out, "ok: %s is valid (%s on %s, caps $%g / %gh)\n", *file, c.Name, c.Backend, c.Budget.MaxUSD, c.Budget.MaxHours)
	})
	return 0, nil
}

func (a *app) cmdRun(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	file := fs.String("f", kiln.DefaultFile, "config file")
	plan := fs.Bool("plan", false, "show price and max spend without starting")
	if _, err := a.parseFS(fs, args, 0); err != nil {
		return 2, err
	}
	c, err := kiln.LoadConfig(*file)
	if err != nil {
		return 1, err
	}
	b, err := kiln.BackendFor(c)
	if err != nil {
		return 1, err
	}
	if err := b.Preflight(context.Background(), c); err != nil {
		return 1, err
	}
	r := kiln.NewRun(c, time.Now())
	r.PricePerHour = kiln.PriceFor(c, b)
	r.GitSHA, r.GitDirty = gitInfo()

	if *plan {
		p := map[string]any{
			"name": c.Name, "backend": c.Backend, "image": c.Image, "price_per_hour": r.PricePerHour,
			"max_usd": c.Budget.MaxUSD, "max_hours": c.Budget.MaxHours, "max_spend_usd": c.Budget.MaxSpend(r.PricePerHour),
		}
		a.emit(p, func() {
			fmt.Fprintf(a.out, "plan: %s on %s (%s)\n  price:     $%.4f/h\n  caps:      $%g or %gh, whichever first\n  max spend: $%.4f\n",
				c.Name, c.Backend, c.Image, r.PricePerHour, c.Budget.MaxUSD, c.Budget.MaxHours, c.Budget.MaxSpend(r.PricePerHour))
		})
		return 0, nil
	}

	sup := kiln.NewSupervisor(b, a.store)
	if !a.json {
		fmt.Fprintf(a.out, "run %s (%s)\n", r.Name, r.ID)
		sup.OnEvent = func(_ *kiln.Run, e kiln.Event) {
			fmt.Fprintf(a.out, "[%s] %s\n", e.At.Format("15:04:05"), e.Message)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err = sup.Run(ctx, r)
	a.emit(r, func() {
		fmt.Fprintf(a.out, "%s: %s, spent $%.4f, ran %s\n", r.ID, r.State, r.SpentUSD, r.Duration(time.Now()).Round(time.Second))
	})
	return r.State.ExitCode(), err
}

func (a *app) cmdList(args []string) (int, error) {
	if _, err := a.parse("list", args, 0); err != nil {
		return 2, err
	}
	runs, err := a.store.List()
	if err != nil {
		return 1, err
	}
	a.emit(orEmpty(runs), func() {
		tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tSTATE\tSPENT\tDURATION\tCREATED")
		for _, r := range runs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t$%.4f\t%s\t%s\n", r.ID, r.Name, r.State, r.SpentUSD,
				r.Duration(time.Now()).Round(time.Second), r.CreatedAt.Format("2006-01-02 15:04"))
		}
		tw.Flush()
	})
	return 0, nil
}

func (a *app) cmdStatus(args []string) (int, error) {
	r, err := a.findArg("status", args)
	if err != nil {
		return 1, err
	}
	stale := !r.State.Terminal() && time.Since(r.UpdatedAt) > kiln.StaleAfter
	a.emit(map[string]any{"run": r, "stale": stale}, func() {
		fmt.Fprintf(a.out, "%s  %s\n  state:   %s\n  spent:   $%.4f of $%g (at $%.4f/h)\n  ran:     %s of %gh\n",
			r.ID, r.Name, r.State, r.SpentUSD, r.Config.Budget.MaxUSD, r.PricePerHour,
			r.Duration(time.Now()).Round(time.Second), r.Config.Budget.MaxHours)
		if r.Reason != "" {
			fmt.Fprintf(a.out, "  reason:  %s\n", r.Reason)
		}
		if r.GitSHA != "" {
			fmt.Fprintf(a.out, "  git:     %s (dirty: %t)\n", r.GitSHA, r.GitDirty)
		}
		if stale {
			fmt.Fprintln(a.out, "  warning: supervisor is not responding; `kiln cancel` will stop the container")
		}
	})
	return 0, nil
}

func (a *app) cmdLogs(args []string) (int, error) {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow")
	pos, err := a.parseFS(fs, args, 1)
	if err != nil {
		return 1, err
	}
	r, err := a.store.Find(pos[0])
	if err != nil {
		return 1, err
	}
	if r.State.Terminal() {
		f, err := os.Open(a.store.LogPath(r.ID))
		if err != nil {
			return 1, &kiln.Error{Code: "no_logs", Message: "no saved logs for " + r.ID}
		}
		defer f.Close()
		_, err = io.Copy(a.out, f)
		return 0, err
	}
	b, err := kiln.BackendFor(r.Config)
	if err != nil {
		return 1, err
	}
	return 0, b.Logs(context.Background(), r.Handle, *follow, a.out)
}

func (a *app) cmdCancel(args []string) (int, error) {
	r, err := a.findArg("cancel", args)
	if err != nil {
		return 1, err
	}
	b, err := kiln.BackendFor(r.Config)
	if err != nil {
		return 1, err
	}
	direct, err := kiln.Cancel(context.Background(), a.store, b, r, time.Now())
	if err != nil {
		return 1, err
	}
	a.emit(map[string]any{"id": r.ID, "cancelled": direct, "cancel_requested": !direct}, func() {
		if direct {
			fmt.Fprintf(a.out, "%s cancelled (supervisor was gone; container stopped)\n", r.ID)
		} else {
			fmt.Fprintf(a.out, "%s cancel requested; its supervisor stops it within a few seconds\n", r.ID)
		}
	})
	return 0, nil
}

func (a *app) cmdDiff(args []string) (int, error) {
	pos, err := a.parse("diff", args, 2)
	if err != nil {
		return 1, err
	}
	ra, err := a.store.Find(pos[0])
	if err != nil {
		return 1, err
	}
	rb, err := a.store.Find(pos[1])
	if err != nil {
		return 1, err
	}
	changes := kiln.Diff(ra, rb, time.Now())
	a.emit(orEmpty(changes), func() {
		if len(changes) == 0 {
			fmt.Fprintln(a.out, "no differences")
		}
		for _, c := range changes {
			fmt.Fprintf(a.out, "%s: %v -> %v\n", c.Field, c.A, c.B)
		}
	})
	return 0, nil
}

// --- helpers ---

func (a *app) findArg(cmd string, args []string) (*kiln.Run, error) {
	pos, err := a.parse(cmd, args, 1)
	if err != nil {
		return nil, err
	}
	return a.store.Find(pos[0])
}

func (a *app) parse(cmd string, args []string, nargs int) ([]string, error) {
	return a.parseFS(flag.NewFlagSet(cmd, flag.ContinueOnError), args, nargs)
}

// parseFS parses flags anywhere among the arguments (Go's flag stops at the first
// positional) and returns exactly nargs positionals.
func (a *app) parseFS(fs *flag.FlagSet, args []string, nargs int) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, &kiln.Error{Code: "usage", Message: fs.Name() + ": " + err.Error(), Fix: "run `kiln help`"}
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != nargs {
		return nil, &kiln.Error{Code: "usage", Message: fmt.Sprintf("%s: expected %d argument(s), got %d", fs.Name(), nargs, len(pos)), Fix: "run `kiln help`"}
	}
	return pos, nil
}

func (a *app) emit(v any, human func()) {
	if !a.json {
		human()
		return
	}
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (a *app) fail(err error, code int) int {
	var ke *kiln.Error
	if !errors.As(err, &ke) {
		ke = &kiln.Error{Code: "error", Message: err.Error()}
	}
	a.emit(map[string]any{"error": ke}, func() {
		fmt.Fprintln(a.out, "error:", err)
		if ke.Fix != "" {
			fmt.Fprintln(a.out, "fix:  ", ke.Fix)
		}
	})
	return code
}

// extractJSON pulls --json from anywhere in args so it works before or after the command.
func extractJSON(args []string) ([]string, bool) {
	var out []string
	found := false
	for _, arg := range args {
		if arg == "--json" || arg == "-json" {
			found = true
			continue
		}
		out = append(out, arg)
	}
	return out, found
}

// orEmpty makes JSON print [] instead of null for no results.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func gitInfo() (sha string, dirty bool) {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "", false
	}
	st, _ := exec.Command("git", "status", "--porcelain").Output()
	return strings.TrimSpace(string(out)), len(strings.TrimSpace(string(st))) > 0
}

const starterConfig = `# kiln run config. Reference: SPEC.md
name: my-experiment
backend: docker            # docker | podman | aws
image: alpine:3.20
command: ["sh", "-c", "echo hello from kiln; sleep 5"]

budget:
  max_usd: 5               # hard cap: the run is killed when reached
  max_hours: 1             # hard wall-clock cap
  warn_at: 0.8             # warn at 80% of either cap
  # price_per_hour: 0.05   # override the backend price (useful to test caps locally)

resources:
  cpu: 1
  memory_gb: 2

# env:
#   MODE: fast
# secrets: [LLM_API_KEY]   # docker: read from your env; aws: from SSM. Never stored
`
