// Command kiln runs long-running agent tasks in containers with hard cost caps.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
  run [-f file] [--plan] [-d]
                            start and supervise a run (--plan: show cost, don't start;
                            -d: start in the background and return the run id)
  rerun <run> [-d]          run again with the same image digest and identical inputs
  wait <run> [--timeout 10m]
                            block until a run ends; exit code is its outcome
  schema                    JSON Schema for kiln.yaml (editors, agents)
  mcp                       MCP server on stdio: every command as a tool for AI agents
  list                      all runs, newest first
  status <run>              one run (id, id prefix, or name)
  logs <run> [-f]           run logs
  cancel <run>              stop a run
  diff <a> <b>              what changed between two runs
  top <run> [-f] [-n N]     live view inside a run: processes, CPU, memory, network,
                            connections, disk, crashes, agent messages (aws)
  send <run> <message>      send a message to the running agent (/kiln/inbox.jsonl) (aws)
  artifacts <run> [-o dir]  download files the agent left in /kiln/artifacts (aws)
  serve [--addr host:port]  local HTTP API for all of the above (default 127.0.0.1:7777)
  version                   print the kiln version

every command accepts --json for machine-readable output.
`

// version is set at release build time: -ldflags "-X main.version=v1.0.0".
var version = "dev"

type app struct {
	out   io.Writer
	json  bool
	store kiln.Store
	ctx   context.Context // nil = background; the API sets the request's context
}

func (a *app) context() context.Context {
	if a.ctx == nil {
		return context.Background()
	}
	return a.ctx
}

func main() {
	args, asJSON := extractJSON(os.Args[1:])
	a := &app{out: os.Stdout, json: asJSON}
	st, err := kiln.DefaultStore()
	if err != nil {
		os.Exit(a.fail(err, 1))
	}
	a.store = st
	// v1.0/v1.1 kept runs in ./.kiln/runs; bring them into the shared store once.
	if n, err := st.ImportLegacy(filepath.Join(".kiln", "runs")); err == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "kiln: imported %d run(s) from .kiln/runs into %s\n", n, st.Dir)
	}
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
		"top": a.cmdTop, "send": a.cmdSend, "artifacts": a.cmdArtifacts, "serve": a.cmdServe,
		"rerun": a.cmdRerun, "wait": a.cmdWait, "_supervise": a.cmdSupervise,
		"schema": a.cmdSchema, "mcp": a.cmdMCP,
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Fprint(a.out, usage)
		return 0
	}
	if cmd == "version" || cmd == "--version" {
		a.emit(map[string]string{"version": version, "runs_dir": a.store.Dir}, func() {
			fmt.Fprintf(a.out, "kiln %s (runs in %s)\n", version, a.store.Dir)
		})
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
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	backend := fs.String("backend", "aws", "aws | docker | podman")
	if _, err := a.parseFS(fs, args, 0); err != nil {
		return 2, err
	}
	if _, err := os.Stat(kiln.DefaultFile); err == nil {
		return 1, &kiln.Error{Code: "config_exists", Message: kiln.DefaultFile + " already exists", Fix: "edit it, or delete it and run `kiln init` again"}
	}
	extra := ""
	switch *backend {
	case "aws":
		extra = starterAWS
	case "docker", "podman":
	default:
		return 2, &kiln.Error{Code: "usage", Message: "init: unknown backend " + *backend, Fix: "use --backend aws, docker or podman"}
	}
	cwd, _ := os.Getwd()
	if err := os.WriteFile(kiln.DefaultFile, []byte(fmt.Sprintf(starterConfig, runName(filepath.Base(cwd)), *backend, extra)), 0o644); err != nil {
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
	detach := fs.Bool("d", false, "start in the background and return the run id")
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
	r.Dir, _ = os.Getwd()
	if r.Repo, err = kiln.ResolveRepo(a.context(), c.Repo); err != nil {
		return 1, err
	}

	// Starting and stopping alone take about twice the stop reserve; a cap below that can't be met.
	var warnings []string
	if reserve := kiln.StopReserveOf(b); reserve > 0 && c.Budget.CapDuration(r.PricePerHour) < 2*reserve {
		warnings = append(warnings, fmt.Sprintf("the caps allow %s of running, but starting and stopping a task alone take about %s; the run will stop right away and may cost more than max_usd",
			c.Budget.CapDuration(r.PricePerHour).Round(time.Second), 2*reserve))
	}
	if *plan {
		p := map[string]any{
			"name": c.Name, "backend": c.Backend, "image": c.Image, "price_per_hour": r.PricePerHour,
			"max_usd": c.Budget.MaxUSD, "max_hours": c.Budget.MaxHours, "max_spend_usd": c.Budget.MaxSpend(r.PricePerHour),
			"repo": r.Repo, "warnings": orEmpty(warnings),
		}
		a.emit(p, func() {
			fmt.Fprintf(a.out, "plan: %s on %s (%s)\n  price:     $%.4f/h\n  caps:      $%g or %gh, whichever first\n  max spend: $%.4f\n",
				c.Name, c.Backend, c.Image, r.PricePerHour, c.Budget.MaxUSD, c.Budget.MaxHours, c.Budget.MaxSpend(r.PricePerHour))
			if r.Repo != nil {
				fmt.Fprintf(a.out, "  repo:      %s @ %s (%s), checked out at %s\n", r.Repo.URL, r.Repo.SHA[:12], r.Repo.Ref, c.Workdir)
			}
			fmt.Fprintln(a.out, "  caps include shutdown time, which is billed: kiln stops early enough to stay within them")
			for _, w := range warnings {
				fmt.Fprintln(a.out, "  warning:  ", w)
			}
		})
		return 0, nil
	}
	for _, w := range warnings {
		r.Events = append(r.Events, kiln.Event{At: time.Now(), Message: "warning: " + w})
	}
	if len(c.Inputs) > 0 {
		r.Inputs, err = kiln.PackInputs(filepath.Dir(*file), c.Inputs, c.Exclude, a.store.InputsPath(r.ID))
		if err != nil {
			return 1, err
		}
	}
	return a.start(r, b, *detach)
}

// start supervises r in this process, or with detach hands it to a background kiln process
// (output in .kiln/runs/<id>.out) and returns at once. Caps hold either way: on aws the
// in-task deadline stops the task even if no kiln process is left.
func (a *app) start(r *kiln.Run, b kiln.Backend, detach bool) (int, error) {
	if detach {
		if err := a.store.Save(r); err != nil {
			return 1, err
		}
		if err := spawnSupervisor(a.store, r.ID); err != nil {
			return 1, err
		}
		a.emit(map[string]any{"id": r.ID, "name": r.Name, "state": r.State, "detached": true}, func() {
			fmt.Fprintf(a.out, "started %s (%s) in the background\n  watch: kiln top %s -f\n  wait:  kiln wait %s\n  stop:  kiln cancel %s\n",
				r.Name, r.ID, r.ID, r.ID, r.ID)
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
	err := sup.Run(ctx, r)
	a.emit(r, func() {
		fmt.Fprintf(a.out, "%s: %s, spent $%.4f, ran %s\n", r.ID, r.State, r.SpentUSD, r.Duration(time.Now()).Round(time.Second))
	})
	return r.State.ExitCode(), err
}

// cmdSupervise is the background half of `run -d`: it supervises a run that was already recorded.
func (a *app) cmdSupervise(args []string) (int, error) {
	r, err := a.findArg("_supervise", args)
	if err != nil {
		return 1, err
	}
	if r.State != kiln.Queued {
		return 1, &kiln.Error{Code: "already_started", Message: "run " + r.ID + " is " + string(r.State)}
	}
	b, err := kiln.BackendFor(r.Config)
	if err != nil {
		return 1, err
	}
	return a.start(r, b, false)
}

// cmdRerun starts a new run that reproduces an old one: same config, the exact image digest
// the old run used (not the tag, which may have moved), and byte-identical inputs.
func (a *app) cmdRerun(args []string) (int, error) {
	fs := flag.NewFlagSet("rerun", flag.ContinueOnError)
	detach := fs.Bool("d", false, "start in the background and return the run id")
	pos, err := a.parseFS(fs, args, 1)
	if err != nil {
		return 2, err
	}
	old, err := a.store.Find(pos[0])
	if err != nil {
		return 1, err
	}
	c := old.Config
	c.Image = kiln.PinImage(c.Image, old.ImageDigest)
	b, err := kiln.BackendFor(c)
	if err != nil {
		return 1, err
	}
	if err := b.Preflight(a.context(), c); err != nil {
		return 1, err
	}
	r := kiln.NewRun(c, time.Now())
	r.PricePerHour = kiln.PriceFor(c, b)
	r.GitSHA, r.GitDirty, r.Inputs, r.RerunOf, r.Dir, r.Repo = old.GitSHA, old.GitDirty, old.Inputs, old.ID, old.Dir, old.Repo
	if r.Inputs != nil {
		in := *r.Inputs // the archive lives with the old run in the store
		in.Local = a.store.InputsPath(old.ID)
		r.Inputs = &in
	}
	return a.start(r, b, *detach)
}

// cmdWait blocks until a run ends and exits with its state's exit code.
func (a *app) cmdWait(args []string) (int, error) {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 0, "give up after this long (e.g. 10m); 0 = until the run ends")
	pos, err := a.parseFS(fs, args, 1)
	if err != nil {
		return 2, err
	}
	r, err := a.store.Find(pos[0])
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(a.context(), os.Interrupt)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	for !r.State.Terminal() {
		if a.store.Stale(r, time.Now()) {
			return 1, &kiln.Error{Code: "supervisor_gone", Message: "no kiln process is supervising " + r.ID,
				Fix: "run `kiln cancel " + r.ID + "` to stop it and record how it ended"}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 1, &kiln.Error{Code: "still_running", Message: fmt.Sprintf("%s is still %s after %s", r.ID, r.State, *timeout),
					Fix: "wait again, check progress with `kiln top " + r.ID + "`, or `kiln cancel " + r.ID + "`"}
			}
			return 1, &kiln.Error{Code: "interrupted", Message: "stopped waiting; the run continues"}
		case <-time.After(2 * time.Second):
		}
		if r, err = a.store.Load(r.ID); err != nil {
			return 1, err
		}
	}
	a.emit(r, func() {
		fmt.Fprintf(a.out, "%s: %s (%s), spent $%.4f, ran %s\n", r.ID, r.State, r.Reason, r.SpentUSD, r.Duration(time.Now()).Round(time.Second))
	})
	return r.State.ExitCode(), nil
}

func (a *app) cmdList(args []string) (int, error) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	here := fs.Bool("here", false, "only runs started from this folder")
	if _, err := a.parseFS(fs, args, 0); err != nil {
		return 2, err
	}
	all, err := a.store.List()
	if err != nil {
		return 1, err
	}
	runs := all
	if *here {
		cwd, _ := os.Getwd()
		runs = nil
		for _, r := range all {
			if r.Dir == cwd {
				runs = append(runs, r)
			}
		}
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
	stale := a.store.Stale(r, time.Now())
	cancelling := !r.State.Terminal() && a.store.CancelRequested(r.ID)
	a.emit(map[string]any{"run": r, "stale": stale, "cancel_requested": cancelling}, func() {
		state := string(r.State)
		if cancelling {
			state += " (cancel requested; stopping)"
		}
		fmt.Fprintf(a.out, "%s  %s\n  state:   %s\n  spent:   $%.4f of $%g (at $%.4f/h)\n  ran:     %s of %gh\n",
			r.ID, r.Name, state, r.SpentUSD, r.Config.Budget.MaxUSD, r.PricePerHour,
			r.Duration(time.Now()).Round(time.Second), r.Config.Budget.MaxHours)
		if r.Dir != "" {
			fmt.Fprintf(a.out, "  from:    %s\n", r.Dir)
		}
		if r.Repo != nil {
			fmt.Fprintf(a.out, "  repo:    %s @ %s (%s)\n", r.Repo.URL, r.Repo.SHA[:min(12, len(r.Repo.SHA))], r.Repo.Ref)
		}
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
	a.emit(map[string]any{"id": r.ID, "state": r.State, "cancel_requested": !direct}, func() {
		if direct {
			fmt.Fprintf(a.out, "%s %s: %s\n", r.ID, r.State, r.Reason)
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

func (a *app) observer(r *kiln.Run) (kiln.Observer, error) {
	b, err := kiln.BackendFor(r.Config)
	if err != nil {
		return nil, err
	}
	return kiln.ObserverFor(b, r.Config.Backend)
}

func (a *app) cmdTop(args []string) (int, error) {
	fs := flag.NewFlagSet("top", flag.ContinueOnError)
	follow := fs.Bool("f", false, "keep watching until the run ends")
	n := fs.Int("n", 15, "processes to show")
	pos, err := a.parseFS(fs, args, 1)
	if err != nil {
		return 2, err
	}
	r, err := a.store.Find(pos[0])
	if err != nil {
		return 1, err
	}
	o, err := a.observer(r)
	if err != nil {
		return 1, err
	}
	var src io.Reader
	ctx, stop := signal.NotifyContext(a.context(), os.Interrupt)
	defer stop()
	if saved, err := os.ReadFile(a.store.MonitorPath(r.ID)); err == nil && r.State.Terminal() {
		src = bytes.NewReader(saved)
	} else if *follow && !r.State.Terminal() {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(o.Monitor(ctx, r.Handle, true, pw)) }()
		go func() { // the log stream never ends by itself: stop when the run does
			for ctx.Err() == nil {
				time.Sleep(3 * time.Second)
				if cur, err := a.store.Load(r.ID); err == nil && cur.State.Terminal() {
					stop()
				}
			}
		}()
		src = pr
	} else {
		var buf bytes.Buffer
		if err := o.Monitor(ctx, r.Handle, false, &buf); err != nil {
			return 1, err
		}
		src = &buf
	}

	w := &kiln.Watcher{}
	var events []string
	onEvent := func(e string) {
		events = append(events, e)
		if *follow && !a.json {
			fmt.Fprintln(a.out, "event:", e)
		}
	}
	onRecord := func(rec *kiln.MonitorRecord) {
		if !*follow {
			return
		}
		if a.json { // one JSON object per line, CPU% filled in, as it happens
			_ = json.NewEncoder(a.out).Encode(rec)
		} else if rec.Kind == "snapshot" || rec.Kind == "final" {
			w.View.Render(a.out, *n)
		}
	}
	if *follow && a.json {
		onEvent = func(e string) { _ = json.NewEncoder(a.out).Encode(map[string]string{"kiln": "event", "message": e}) }
	}
	err = kiln.WatchMonitor(src, w, onEvent, onRecord)
	if ctx.Err() != nil {
		err = nil // Ctrl-C, client gone, or the run ended
	}
	if *follow || err != nil {
		if cur, lerr := a.store.Load(r.ID); err == nil && lerr == nil && cur.State.Terminal() && !a.json {
			fmt.Fprintf(a.out, "run %s ended: %s (%s)\n", cur.ID, cur.State, cur.Reason)
		}
		return 0, err
	}
	a.emit(map[string]any{"id": r.ID, "state": r.State, "view": w.View, "events": orEmpty(events)}, func() {
		fmt.Fprintf(a.out, "%s  %s  %s\n", r.ID, r.Name, r.State)
		for _, e := range events {
			fmt.Fprintln(a.out, "event:", e)
		}
		w.View.Render(a.out, *n)
	})
	return 0, nil
}

func (a *app) cmdSend(args []string) (int, error) {
	pos, err := a.parse("send", args, 2)
	if err != nil {
		return 2, err
	}
	r, err := a.store.Find(pos[0])
	if err != nil {
		return 1, err
	}
	if r.State.Terminal() {
		return 1, &kiln.Error{Code: "already_finished", Message: "run " + r.ID + " already " + string(r.State)}
	}
	o, err := a.observer(r)
	if err != nil {
		return 1, err
	}
	if err := o.Send(a.context(), r, pos[1]); err != nil {
		return 1, err
	}
	a.emit(map[string]any{"id": r.ID, "sent": pos[1]}, func() {
		fmt.Fprintf(a.out, "sent to %s; the agent sees it in /kiln/inbox.jsonl within a few seconds\n", r.ID)
	})
	return 0, nil
}

func (a *app) cmdArtifacts(args []string) (int, error) {
	fs := flag.NewFlagSet("artifacts", flag.ContinueOnError)
	dir := fs.String("o", "", "directory to unpack into (default .kiln/runs/<id>-artifacts)")
	pos, err := a.parseFS(fs, args, 1)
	if err != nil {
		return 2, err
	}
	r, err := a.store.Find(pos[0])
	if err != nil {
		return 1, err
	}
	o, err := a.observer(r)
	if err != nil {
		return 1, err
	}
	data, err := o.Artifacts(a.context(), r)
	if err != nil {
		return 1, err
	}
	if *dir == "" {
		*dir = a.store.ArtifactsDir(r.ID)
	}
	n, err := kiln.ExtractArtifacts(data, *dir)
	if err != nil {
		return 1, err
	}
	a.emit(map[string]any{"id": r.ID, "dir": *dir, "files": n}, func() {
		fmt.Fprintf(a.out, "%d file(s) in %s (artifacts/ plus the agent's outbox.jsonl and inbox.jsonl)\n", n, *dir)
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

// starterConfig is what `kiln init` writes. %[1]s name, %[2]s backend, %[3]s backend-specific lines.
const starterConfig = `# yaml-language-server: $schema=https://raw.githubusercontent.com/kabeer11000/kiln/main/internal/kiln/kiln.schema.json
# kiln run config. Every field: ` + "`kiln schema`" + `. Check it: ` + "`kiln validate`" + `, cost: ` + "`kiln run --plan`" + `.
name: %[1]s
backend: %[2]s              # aws | docker | podman
image: public.ecr.aws/docker/library/alpine:3.20
command: ["sh", "-c", "echo hello from kiln; ls /kiln/inputs; echo done > /kiln/artifacts/result.txt"]

# Files in: copied to /kiln/inputs before the command starts ("." = this whole folder).
inputs: ["."]
exclude: [node_modules, "*.log"]
# Files out: anything written to /kiln/artifacts. Get it with ` + "`kiln artifacts <run>`" + `.

budget:
  max_usd: 1               # hard cap: the run is stopped when reached
  max_hours: 1             # hard wall-clock cap
  warn_at: 0.8             # warn at 80%% of either cap
  # price_per_hour: 0.05   # override the price (e.g. to try the cap on docker, which is free)

resources:
  cpu: 1
  memory_gb: 2
%[3]s
# env:
#   MODE: fast
# secrets: [LLM_API_KEY]   # docker: from your environment; aws: from SSM. Never stored
`

const starterAWS = `  spot: true               # ~70% cheaper; may be interrupted

aws:
  region: us-east-1
  stack: kiln
`

// runName turns a folder name into a valid run name: lowercase letters, digits, dashes.
func runName(folder string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(folder) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	if name == "" {
		return "my-run"
	}
	return name
}
