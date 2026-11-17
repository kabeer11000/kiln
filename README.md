# kiln

Run long-running AI agent tasks, experiments and CI jobs in containers, with a hard cost cap on every run.

```
$ kiln run
run refactor-auth/brave-otter (r_laaq2isy)
[17:14:43] starting on aws
[17:14:48] running
[17:15:05] warning: past 80% of a cap (spent $0.1700 of $0.2000, ran 17s of 0.1h)
[17:15:22] spent $0.2000 of $0.2000 cap
r_laaq2isy: budget_exceeded, spent $0.2023, ran 22s
```

- **Hard caps.** Every run has a dollar cap and a time cap. On AWS the cap is also enforced inside
  the task itself, so it holds even if your laptop dies.
- **Every run is a record.** Config, image digest, git commit, outcome and cost. `kiln diff a b` shows
  what changed.
- **Cheap.** No kiln server. On AWS the whole setup costs $0 while idle; a small Spot task is about $0.01/hour.
- **For people and agents.** Readable output by default, `--json` on every command, errors that say how to fix them.

## Install

Download a binary from [Releases](../../releases), or:

```bash
go install github.com/kabeer11000/kiln/cmd/kiln@latest
```

## Quick start (local)

Needs Docker or Podman.

```bash
kiln init            # writes kiln.yaml
kiln run --plan      # price and max spend, starts nothing
kiln run
kiln list
```

## AWS

### One-time setup (per team account)

```bash
cd deploy/aws
terraform init
terraform apply -var alert_email=you@example.com
```

This creates only things that cost nothing while idle: an ECS cluster, an outbound-only security group
in the default VPC, IAM roles, a log group, a budget alert, and an SSM parameter that kiln reads its
settings from. No NAT gateway, no load balancer.

Give team members the policy from `terraform output users_policy_arn`.

Upgrading from v1.0: run `terraform apply` again. v1.1 adds an S3 bucket for inputs, artifacts and
messages (objects expire after 30 days) and an S3-only key kiln uses to sign per-run upload links.
Tasks still get no AWS credentials.

To get budget alerts for kiln spend only, activate the `project` tag under
Billing > Cost allocation tags (AWS shows it about a day after the first run).

### Run

```yaml
# kiln.yaml
name: nightly-eval
backend: aws
image: public.ecr.aws/docker/library/alpine:3.20
command: ["sh", "-c", "echo hello; sleep 30"]
resources: {cpu: 0.25, memory_gb: 0.5, spot: true}
budget: {max_usd: 1, max_hours: 2}
secrets: [LLM_API_KEY]
```

Secrets live in SSM Parameter Store, never in kiln.yaml, run records or task definitions:

```bash
aws ssm put-parameter --type SecureString --name /kiln/kiln/secrets/LLM_API_KEY --value "..."
```

On Git Bash for Windows, prefix with `MSYS_NO_PATHCONV=1` so the `/kiln/...` name isn't turned into a path.

### How caps are enforced

1. `kiln run` tracks spend and stops the task when a cap is reached. Overshoot is near zero: caps are
   checked on a schedule aligned to the cap, and slow AWS calls are cut off at the cap.
2. Each task carries a tiny `kiln-deadline` container. When it exits at the cap, AWS stops the task,
   even if `kiln run` is gone.
3. The AWS budget alert emails you as a last line of defense.

## Inside a run (aws)

Next to your container, every task runs a small `kiln-monitor` container. It shares the task's
process list, network and a `/kiln` folder with your agent:

| Path | Who writes | What |
|---|---|---|
| `/kiln/inputs/` | kiln, before your agent starts | Files listed under `inputs:` in kiln.yaml |
| `/kiln/artifacts/` | your agent | Anything here is uploaded when the task stops. `kiln artifacts <run>` downloads it |
| `/kiln/outbox.jsonl` | your agent | One line per message (progress, results). Shows up live in `kiln run`, `kiln top` and the API |
| `/kiln/inbox.jsonl` | kiln | Messages sent with `kiln send`, one JSON line each: `{"t": 1763390000, "msg": "..."}` |

What you can see while it runs, without touching the agent:

```
$ kiln top r_lkjn4kb6 -f
--- 18:02:40
memory:    agent 61.2 MB (peak 121.0 MB), kiln-monitor 8.0 MB (peak 46.6 MB)
peak/proc: chrome 540.2 MB, node 210.0 MB, sh 54.0 MB
network:   in 8.7 MB, out 269.4 KB
disk:      515.4 MB of 20.5 GB
artifacts: 2 file(s), 16.0 KB
conns:     104.18.27.120:443 established x3, 1.1.1.1:80 established
    PID  PROCESS          STATE   CPU%  MEMORY
     41  chrome               S   38.0  540.2 MB
event: agent: {"progress":"tick 10"}
event: process chrome (pid 77) exited, was using 312.0 MB
```

- Every process with its CPU and memory, plus the peak per process name.
- Memory per container (current and peak), network totals, open TCP connections, disk.
- Crashes: processes that disappear, zombies, out-of-memory kills (in the run's reason).
- `--json` gives one JSON record per line, for agents.

Readings are taken every 5 seconds and stored in CloudWatch with the task's logs (about 3 MB per
hour of run time, a fraction of a cent). The full set is saved to `.kiln/runs/<id>.monitor.jsonl`
when the run ends.

**Playwright:** write reports and traces into `/kiln/artifacts`, e.g.
`npx playwright test --output /kiln/artifacts/test-results --reporter html` with
`PLAYWRIGHT_HTML_REPORT=/kiln/artifacts/report`. Traces include every network request the browser
made. Then `kiln artifacts <run>` and `npx playwright show-report`.

### Files in, files out

```yaml
inputs: [./data, prompt.md]   # relative to kiln.yaml; unpacked to /kiln/inputs/data, /kiln/inputs/prompt.md
```

kiln packs the inputs, records their SHA-256 in the run, and the agent starts only after they
are unpacked. Up to 512 MB; put bigger data in the image. Files come out through `/kiln/artifacts`.

### Start, stop, reproduce

```bash
kiln run -d                 # start in the background, print the run id, return
kiln wait <run>             # block until it ends; exit code is the outcome
kiln cancel <run>           # stop it
kiln rerun <run> -d         # run it again: same image digest (not the tag), byte-identical inputs
kiln diff <old> <new>       # what changed: config, image, inputs, outcome, cost
```

A background run keeps its caps even if your machine sleeps: the monitor stops the task inside
AWS at the hard deadline.

## API

`kiln serve` runs a local HTTP API (127.0.0.1:7777). It calls the same code as the CLI with
`--json`, so the two always agree.

| Method | Path | Same as |
|---|---|---|
| GET | `/v1/runs` | `kiln list` |
| POST | `/v1/runs` body `{"file": "kiln.yaml"}` | `kiln run -d` |
| GET | `/v1/runs/{run}` | `kiln status` |
| GET | `/v1/runs/{run}/logs` | `kiln logs` |
| GET | `/v1/runs/{run}/top[?follow=1]` | `kiln top` (follow streams JSON lines) |
| GET | `/v1/runs/{run}/wait` | `kiln wait` |
| GET | `/v1/runs/{run}/diff/{other}` | `kiln diff` |
| POST | `/v1/runs/{run}/send` body `{"message": "..."}` | `kiln send` |
| POST | `/v1/runs/{run}/cancel` | `kiln cancel` |
| POST | `/v1/runs/{run}/rerun` | `kiln rerun -d` |
| POST | `/v1/runs/{run}/artifacts` | `kiln artifacts` |

POST requests need `Content-Type: application/json`. Only localhost host names are accepted, so a
web page in your browser can't drive it.

## Commands

| Command | What |
|---|---|
| `kiln init` | Write a starter kiln.yaml |
| `kiln validate` | Check config, show it with defaults |
| `kiln run [--plan] [-d]` | Start and supervise a run (Ctrl-C cancels it). `-d`: in the background |
| `kiln rerun <run> [-d]` | Run again with the same image digest and inputs |
| `kiln wait <run>` | Block until a run ends |
| `kiln top <run> [-f]` | Processes, CPU, memory, network, connections, disk, crashes, messages |
| `kiln send <run> <message>` | Message the running agent |
| `kiln artifacts <run> [-o dir]` | Download `/kiln/artifacts` |
| `kiln serve` | Local HTTP API |
| `kiln list` | All runs |
| `kiln status <run>` | One run: state, spend, reason, git commit |
| `kiln logs <run> [-f]` | Logs |
| `kiln cancel <run>` | Stop a run |
| `kiln diff <a> <b>` | Compare two runs |

`<run>` is a run id, an id prefix, or a run name. Add `--json` to any command.

Exit codes: 0 succeeded, 1 kiln error, 2 usage error, 3 failed, 4 budget exceeded, 5 timed out, 6 cancelled.

See [SPEC.md](SPEC.md) for the full config reference and design.

## Development

```bash
go test ./...
```
