# kiln spec

Run long-running AI agent tasks, experiments and CI jobs in containers on any cloud.
Every run is a named, versioned record with a hard cost cap. Built for teams running
tens of runs at once.

## Principles

1. **Hard caps, always.** Every run has a dollar cap and a wall-clock cap. There is no "unlimited".
2. **Enforcement lives outside the agent.** The agent runs arbitrary code; anything it can kill is not a cap.
3. **Near-zero overhead.** No always-on kiln service. Idle cost of kiln itself is $0.
4. **One interface for humans and agents.** Every command is readable by default and `--json` for machines.
5. **Run = commit.** A run pins its config, image digest and git SHA so it can be reproduced and diffed.

## kiln.yaml

```yaml
name: refactor-auth            # lowercase, digits, dashes
backend: aws                   # aws (default) | docker | podman
image: public.ecr.aws/docker/library/node:22
command: ["sh", "-c", "npm ci && npm test"]
workdir: /kiln/src             # default /kiln/src with repo:, else the image's own

repo:                          # checked out at /kiln/src before the command starts
  url: https://github.com/acme/app.git
  ref: main                    # branch, tag or commit; pinned to the commit at run start
  token: GITHUB_TOKEN          # private https repos: a secret NAME (sent as an HTTP header)

budget:
  max_usd: 5                   # default 5. Hard cap
  max_hours: 1                 # default 1. Hard wall-clock cap
  warn_at: 0.8                 # default 0.8. Warn at this fraction of either cap
  price_per_hour: 0.05         # optional override of the backend price

resources:
  cpu: 1                       # default 1   (aws: rounded up to a valid Fargate size)
  memory_gb: 2                 # default 2
  spot: true                   # aws: Fargate Spot, ~70% cheaper, can be interrupted

env: {MODE: fast}              # stored in the run record
secrets: [LLM_API_KEY]         # names only. docker: from your env. aws: from SSM. Never stored
inputs: [./data, prompt.md]    # unpacked to /kiln/inputs before the command starts; sha256 recorded
                               # ["."] = whole folder at the top; .git and .kiln always skipped
exclude: [node_modules, "*.log"] # glob patterns on names and paths, left out of inputs

aws:                           # only with backend: aws
  region: us-east-1            # default us-east-1
  stack: kiln                  # default kiln. Which deploy/aws install to use
```

Unknown fields are rejected, so typos fail fast. Setting a cap to 0 means "use the default".
`kiln schema` prints the JSON Schema (`internal/kiln/kiln.schema.json`); a test keeps it in sync
with the config struct.

## Run store

One store per user: `$KILN_HOME/runs`, default `<user config dir>/kiln/runs`. Each run has
`<id>.json` (the record), `.log`, `.monitor.jsonl`, `.inputs.tgz`, `.out` (background supervisor),
`.alive` (heartbeat), `.cancel` (request), `-artifacts/`, and on docker `-kiln/` (the `/kiln` mount).
The record keeps `dir`, the folder the run was started from (`kiln list --here`). v1.0/v1.1
project-local `.kiln/runs` folders are copied in on first use; the originals are left alone.

## Run lifecycle

```
queued -> provisioning -> running -> succeeded | failed | cancelled | budget_exceeded | timed_out
```

| State | Exit code |
|---|---|
| succeeded | 0 |
| (kiln error) | 1 |
| (usage error) | 2 |
| failed | 3 |
| budget_exceeded | 4 |
| timed_out | 5 |
| cancelled | 6 |

## CLI

Every command accepts `--json`. Errors are `{"error": {"code", "message", "fix"}}`.

| Command | What |
|---|---|
| `kiln init [--backend]` | Write a commented kiln.yaml named after the folder |
| `kiln schema` | JSON Schema for kiln.yaml |
| `kiln mcp` | MCP server on stdio (newline-delimited JSON-RPC); one tool per command, same handlers |
| `kiln validate [-f file]` | Check config, print it with defaults applied |
| `kiln run [-f file] [--plan]` | Start and supervise a run. `--plan` prints price and max spend without starting |
| `kiln list [--here]` | All runs, newest first |
| `kiln status <run>` | One run. `<run>` is an id, id prefix, or name |
| `kiln logs <run> [-f]` | Logs (saved logs once finished) |
| `kiln cancel <run>` | Stop a run |
| `kiln diff <a> <b>` | What changed between two runs: config, image, inputs, git, outcome, cost |
| `kiln run -d` | Start in the background (a detached `kiln _supervise <id>` process), print the id |
| `kiln rerun <run> [-d]` | New run with the old config, image pinned to the old digest, same inputs archive |
| `kiln wait <run> [--timeout d]` | Block until terminal; exit code per the table above. Timeout: error `still_running` |
| `kiln top <run> [-f] [-n N]` | Monitor view. `--json -f`: one record per line. docker: messages only |
| `kiln send <run> <msg>` | Append `{"t","msg"}` to the run's inbox |
| `kiln artifacts <run> [-o dir]` | Download and unpack the artifacts archive |
| `kiln serve [--addr]` | Local HTTP API over the same handlers (see README) |

## Cost enforcement

Three layers, outermost last:

1. **Supervisor** (`kiln run`, or the background `kiln _supervise` of `run -d`). Spend so far is
   `elapsed_hours x price_per_hour`. Stopping is billed too, so decisions use
   `elapsed + stop_reserve` (aws 60s: the agent's 10s to exit, the monitor's final upload, ECS
   teardown, measured 16-50s; docker 10s). It wakes exactly when a cap is due, checks caps before
   every backend call, and cuts every backend call off at the cap, so a slow cloud API never adds
   overshoot. On a cap it stops the task first, then saves logs, then cleans up.
2. **In-task deadline** (aws). The essential `kiln-monitor` container exits 0 at
   `cap - stop_reserve + 15s`; ECS then stops the whole task. This holds even if no kiln process is
   left. The supervisor reports it as `timed_out`; with no supervisor, `kiln cancel` records it.
3. **Budget alert** (aws). An AWS Budget on the `project=kiln` tag emails at 80% / forecast 100%.
   Alerts only: AWS Budgets cannot stop spend.

The final recorded spend uses the provider's billed period when it has one (aws: `pullStartedAt` to
`stoppedAt`), so it matches the bill rather than kiln's view through slow API calls. `--plan` warns
when the caps allow less running time than two stop reserves: such a run is stopped at once and
startup plus shutdown can exceed `max_usd`.

Verified live: a $1.50 cap at $36/h stopped at $1.4623 billed; with the supervisor killed, the
monitor ended the task inside AWS and `kiln cancel` recorded `timed_out`.

## Architecture

```
kiln CLI ──writes──> run store (.kiln/runs/*.json, *.log)
   │
   └── supervisor loop ──> Backend ──> docker / podman / ECS Fargate
```

### Backend interface

```go
type Backend interface {
    Preflight(ctx, Config) error           // reachable? secrets exist?
    Start(ctx, *Run) (handle, error)
    Status(ctx, handle) (Status, error)    // state, image digest, reason
    Logs(ctx, handle, follow, io.Writer) error
    Stop(ctx, handle) error                // ends spend; logs stay readable
    Remove(ctx, handle) error              // cleanup
    PricePerHour(Resources) float64        // feeds the cost cap
}
```

| Backend | Platform | Status |
|---|---|---|
| aws | ECS Fargate / Fargate Spot | v1.0 |
| docker / podman | local | v1.0 |
| k8s | Any Kubernetes cluster (Jobs + `activeDeadlineSeconds`) | planned |
| gcp | Cloud Run Jobs | planned |
| azure | Container Apps Jobs | planned |

### AWS layout (deploy/aws)

All idle-$0: ECS cluster (Fargate + Fargate Spot), outbound-only security group in the default VPC
(public IP, so no NAT gateway), task execution role, log group, SSM parameter `/kiln/<stack>/config`
that kiln reads, IAM policy for team members, optional budget alert.

Tasks get no IAM role: the agent container has no AWS permissions. Secrets live in SSM Parameter
Store under `/kiln/<stack>/secrets/` and are injected by ECS at container start.

### Monitor (aws)

Each task has two containers: `agent` and `kiln-monitor` (alpine, a ~5 KB shell script, essential).
`pidMode: task` and a shared `/kiln` volume let the monitor see every agent process and file.

- Every 5 s it prints JSON records to its log stream: processes (pid, name, state, CPU ticks, RSS),
  network counters, TCP connections, disk, artifact counts; ECS per-container memory every 30 s.
- It forwards new `/kiln/outbox.jsonl` lines, and polls the run's inbox object for `kiln send`.
- It exits 0 at the hard deadline (ECS then stops the task: the in-task cap). On SIGTERM it
  takes a final reading and uploads `/kiln/artifacts` + inbox/outbox, then exits 143.
- Before the agent starts it clones the repo at the pinned commit into `/kiln/src` (shallow fetch
  by SHA, full fetch as fallback; token as an HTTP header, from SSM into the monitor only), unpacks
  inputs, and touches `/kiln/.ready`; the agent `dependsOn` the monitor being HEALTHY (health check
  start period 300 s). A failed checkout or download exits 1, so the run fails without starting.

### docker / podman

`/kiln` is a bind mount of `<store>/<id>-kiln`. kiln clones the repo there with the local git and
unpacks inputs before `docker run`; `kiln send` appends to its `inbox.jsonl`; `kiln top` shows the
outbox; `kiln artifacts` packs `artifacts/` and the message files. No process readings yet.

The task has no AWS credentials. kiln hands the monitor presigned S3 URLs for this run's objects
only (`runs/<id>/inbox.jsonl` GET, `artifacts.tgz` PUT, `inputs.tgz` GET), signed with an S3-only
key from SSM (`/kiln/<stack>/signer`), valid until the hard deadline plus an hour (max 7 days).

The supervisor follows the monitor stream live (polling CloudWatch with a forward token), turns
records into run events (agent messages, process exits, uploads), and at the end saves every
record to `.kiln/runs/<id>.monitor.jsonl`, emitting events for any record the live stream missed.

A supervisor proves it is alive by touching `.kiln/runs/<id>.alive` every 5 s; a run with neither a
recent save nor a recent heartbeat is stale, and `kiln cancel` then stops the task directly.

## Planned

- Run records in S3 for team-shared history; supervisor inside the task instead of a local background process
- LLM proxy in the sidecar for token-level caps (the agent never sees real API keys)
- Team caps (daily/monthly), concurrency limits, `kiln spend`
- Monitor for docker/podman and the other backends
- Full per-request network log (a proxy in the task), `matrix:` sweeps
- `kiln mcp`, JSON Schema for kiln.yaml, GitHub Action
- k8s, GCP, Azure backends
