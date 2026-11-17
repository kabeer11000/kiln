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
backend: aws                   # aws | docker | podman
image: ghcr.io/acme/agent:1.4
command: ["./run-agent.sh"]

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
inputs: [./data, prompt.md]    # aws: unpacked to /kiln/inputs before the agent starts; sha256 recorded

aws:                           # only with backend: aws
  region: us-east-1            # default us-east-1
  stack: kiln                  # default kiln. Which deploy/aws install to use
```

Unknown fields are rejected, so typos fail fast. Setting a cap to 0 means "use the default".

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
| `kiln init` | Write a starter kiln.yaml |
| `kiln validate [-f file]` | Check config, print it with defaults applied |
| `kiln run [-f file] [--plan]` | Start and supervise a run. `--plan` prints price and max spend without starting |
| `kiln list` | All runs, newest first |
| `kiln status <run>` | One run. `<run>` is an id, id prefix, or name |
| `kiln logs <run> [-f]` | Logs (saved logs once finished) |
| `kiln cancel <run>` | Stop a run |
| `kiln diff <a> <b>` | What changed between two runs: config, image, inputs, git, outcome, cost |
| `kiln run -d` | Start in the background (a detached `kiln _supervise <id>` process), print the id |
| `kiln rerun <run> [-d]` | New run with the old config, image pinned to the old digest, same inputs archive |
| `kiln wait <run>` | Block until terminal; exit code per the table above |
| `kiln top <run> [-f] [-n N]` | Monitor view (aws). `--json -f`: one record per line |
| `kiln send <run> <msg>` | Append `{"t","msg"}` to the run's inbox (aws) |
| `kiln artifacts <run> [-o dir]` | Download and unpack the artifacts archive (aws) |
| `kiln serve [--addr]` | Local HTTP API over the same handlers (see README) |

## Cost enforcement

Three layers, outermost last:

1. **Supervisor** (the `kiln run` process). Computes `spent = elapsed_hours x price_per_hour`. It wakes
   exactly when a cap is due and checks caps before every backend call, and every backend call is cut
   off at the cap, so a slow cloud API never adds overshoot. On a cap it stops the task first, then
   saves logs, then cleans up.
2. **In-task deadline** (aws). Every task carries a tiny essential `kiln-deadline` container that sleeps
   until the earlier of the two caps (+60s grace). When it exits, ECS stops the whole task. This holds
   even if the machine running `kiln run` dies.
3. **Budget alert** (aws). An AWS Budget on the `project=kiln` tag emails at 80% / forecast 100%.
   Alerts only: AWS Budgets cannot stop spend.

The final recorded spend includes the time it took to stop the task.

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
- Before the agent starts it unpacks inputs and touches `/kiln/.ready`; the agent `dependsOn`
  the monitor being HEALTHY.

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
