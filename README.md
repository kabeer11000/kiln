# kiln

Run long agent tasks, test suites, experiments and CI jobs in cloud containers, straight from a git
repo, with a hard cost cap on every run. Nothing builds or runs on your machine.

```yaml
# kiln.yaml
name: test-suite
image: public.ecr.aws/docker/library/node:22      # any stock image: nothing to build
repo: {url: https://github.com/acme/app.git, ref: main}
command: ["sh", "-c", "npm ci && npm test | tee /kiln/artifacts/report.txt"]
budget: {max_usd: 1, max_hours: 2}                 # hard caps, always
```

```
$ kiln run -d
started test-suite/brave-otter (r_laaq2isy) in the background
$ kiln wait r_laaq2isy
r_laaq2isy: succeeded (exit code 0), spent $0.0021, ran 3m12s
$ kiln artifacts r_laaq2isy
```

- **Hard caps.** A dollar cap and a time cap on every run, enforced outside your code. Shutdown time
  is billed, so kiln stops early enough that the total stays within the cap. Inside AWS a second
  deadline stops the task even if your laptop is gone.
- **Straight from git.** `repo:` checks out the exact commit inside the task. Stock images, no
  Docker on your machine, big repos never pass through your laptop.
- **See inside.** Every process with CPU and memory, peaks, network, connections, disk, crashes, live.
- **Talk to it.** Messages out of the task (`/kiln/outbox.jsonl`) and into it (`kiln send`).
- **Every run is a record.** Config, image digest, commit, inputs hash, outcome and cost.
  `kiln rerun` reproduces a run exactly; `kiln diff` shows what changed.
- **Built for agents.** `kiln mcp` exposes everything as MCP tools, `--json` on every command,
  errors that say how to fix them, a JSON Schema for kiln.yaml. See [AGENTS.md](AGENTS.md).
- **Cheap.** No kiln server. The AWS setup costs $0 while idle; a small Spot task is about $0.01/hour.

## Install

Download a binary from [Releases](https://github.com/kabeer11000/kiln/releases) and put it on your
PATH, or:

```bash
go install github.com/kabeer11000/kiln/cmd/kiln@latest
```

You need the AWS CLI, logged in, for the aws backend; git, for `repo:`. Docker only if you choose
`backend: docker`.

## Set up AWS (once per team account)

```bash
cd deploy/aws
terraform init
terraform apply -var alert_email=you@example.com
```

Everything it creates costs $0 while idle: an ECS cluster, an outbound-only security group in the
default VPC, IAM roles, a log group, an S3 bucket for inputs, artifacts and messages (objects expire
after 30 days), an S3-only signing key, an SSM parameter kiln reads its settings from, and a budget
alert. No NAT gateway, no load balancer. Give team members the policy from
`terraform output users_policy_arn`.

Upgrading from an older kiln: run `terraform apply` again.

## Your first run

```bash
kiln init            # writes a commented kiln.yaml for this folder
kiln run --plan      # price, caps, worst-case spend; starts nothing
kiln run             # start and follow (Ctrl-C cancels), or `kiln run -d` to return at once
```

## Getting code and files in

| How | kiln.yaml | Lands at | Best for |
|---|---|---|---|
| Git repo | `repo: {url, ref, token}` | `/kiln/src` (the default working folder) | Projects. Pinned to the exact commit; cloned inside the task |
| Local files | `inputs: [./data, prompt.md]` or `["."]` | `/kiln/inputs` | Uncommitted work, prompts, small data (up to 512 MB) |
| Image | `image:` | wherever the image puts it | Toolchains. Use stock images (node, python, golang) |

- **Private repos:** `repo.token: GITHUB_TOKEN` names a secret holding a read-only token (on AWS, in
  SSM; see Secrets). It's sent as an HTTP header, never stored in the checkout. The task can read it,
  so scope it to the one repo.
- **`inputs: ["."]`** sends the whole folder, contents at the top of `/kiln/inputs`. `.git` and
  `.kiln` are always skipped; add `exclude: [node_modules, "*.log", dist]` for the rest.
- The agent starts only after the repo and inputs are in place. Their commit and SHA-256 go in the run
  record.

## Getting results out

- Anything written to **`/kiln/artifacts`** is uploaded when the task stops: `kiln artifacts <run>`.
- Lines appended to **`/kiln/outbox.jsonl`** (progress, results) show up live in `kiln run`,
  `kiln top` and the API.
- **`kiln logs <run>`** is the container's output.

**Playwright:** write reports and traces into `/kiln/artifacts`, e.g.
`npx playwright test --output /kiln/artifacts/test-results` with
`PLAYWRIGHT_HTML_REPORT=/kiln/artifacts/report`. Traces include every network request the browser
made. Then `kiln artifacts <run>` and `npx playwright show-report`.

## Watching and talking to a run

```
$ kiln top r_lkjn4kb6 -f
--- 18:02:40
memory:    agent 61.2 MB (peak 121.0 MB), kiln-monitor 8.0 MB (peak 46.6 MB)
peak/proc: chrome 540.2 MB, node 210.0 MB, sh 54.0 MB
network:   in 8.7 MB, out 269.4 KB
disk:      515.4 MB of 20.5 GB
conns:     104.18.27.120:443 established x3
    PID  PROCESS          STATE   CPU%  MEMORY
     41  chrome               S   38.0  540.2 MB
event: agent: {"progress":"14/20 files"}
event: process chrome (pid 77) exited, was using 312.0 MB
```

`kiln send <run> "wrap up and write the report"` puts `{"t": ..., "msg": "..."}` in
`/kiln/inbox.jsonl` inside the task within seconds.

On AWS a small `kiln-monitor` container runs next to yours, sharing its process list, network and
`/kiln` folder. Readings every 5 seconds go to CloudWatch with the task's logs (about 3 MB per hour,
a fraction of a cent). On docker you get `/kiln` (files and messages) but no process readings yet.

## Start, stop, reproduce

```bash
kiln run -d                 # start in the background, print the run id
kiln wait <run>             # block until it ends; exit code is the outcome (--timeout 10m)
kiln cancel <run>           # stop it
kiln rerun <run> -d         # same config, exact image digest, same commit, identical inputs
kiln diff <old> <new>       # what changed: config, image, commit, inputs, outcome, cost
kiln list                   # every run on this machine (--here: started from this folder)
```

Runs are stored per user, not per project (`kiln version` shows where; set `KILN_HOME` to move it),
so any folder or agent sees every run. Runs from older versions' `.kiln/runs` folders are imported
automatically the first time kiln runs there.

## How caps are enforced

1. **kiln** tracks spend and stops the task when a cap is reached. Stopping takes up to a minute
   on AWS and that time is billed, so kiln stops that much early. The final spend is recorded from
   AWS's own start and stop times. `kiln run --plan` warns when caps are too small to be met at all.
2. **Inside AWS**, the monitor stops the task at the hard deadline, even if no kiln process is left
   (laptop asleep, `run -d` killed). `kiln cancel` or `kiln status` then records how it ended.
3. **An AWS budget alert** emails you as a last line of defense.

Verified on AWS: a $1.50 cap stopped a run at $1.46 by AWS's billed time; with kiln killed, the
in-AWS deadline stopped the task by itself.

## Secrets

Secrets live in SSM Parameter Store, never in kiln.yaml, run records or task definitions:

```bash
aws ssm put-parameter --type SecureString --name /kiln/kiln/secrets/LLM_API_KEY --value "..."
```

```yaml
secrets: [LLM_API_KEY]      # injected as environment variables
```

On Git Bash for Windows, prefix with `MSYS_NO_PATHCONV=1` so `/kiln/...` isn't turned into a path.
On docker, secrets come from your environment.

## For AI agents

Add kiln to your MCP client (the server is the command `kiln mcp` on stdio):

```json
{"mcpServers": {"kiln": {"command": "kiln", "args": ["mcp"]}}}
```

Tools: `kiln_schema`, `kiln_validate`, `kiln_plan`, `kiln_run`, `kiln_wait`, `kiln_status`,
`kiln_top`, `kiln_logs`, `kiln_send`, `kiln_cancel`, `kiln_artifacts`, `kiln_rerun`, `kiln_diff`,
`kiln_list`. Without MCP, every command takes `--json`. [AGENTS.md](AGENTS.md) is the one page an
agent needs.

## HTTP API

`kiln serve` runs a local API on 127.0.0.1:7777 over the same code as the CLI.

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

POST needs `Content-Type: application/json`. Only localhost host names are accepted, so a web page
in your browser can't drive it.

## Commands

| Command | What |
|---|---|
| `kiln init [--backend aws\|docker]` | Write a commented kiln.yaml |
| `kiln validate` | Check config, show it with defaults |
| `kiln schema` | JSON Schema for kiln.yaml |
| `kiln run [--plan] [-d]` | Start a run (`-d`: in the background) |
| `kiln wait <run> [--timeout 10m]` | Block until a run ends |
| `kiln status <run>` | State, spend, reason, commit, folder |
| `kiln top <run> [-f]` | Processes, CPU, memory, network, connections, disk, crashes, messages |
| `kiln logs <run> [-f]` | Container output |
| `kiln send <run> <message>` | Message the running task |
| `kiln artifacts <run> [-o dir]` | Download `/kiln/artifacts` |
| `kiln cancel <run>` | Stop a run |
| `kiln rerun <run> [-d]` | Reproduce a run exactly |
| `kiln diff <a> <b>` | Compare two runs |
| `kiln list [--here]` | Runs, newest first |
| `kiln mcp` | MCP server on stdio |
| `kiln serve` | Local HTTP API |

`<run>` is a run id, an id prefix, or a run name. Add `--json` to any command.

Exit codes: 0 succeeded, 1 kiln error, 2 usage error, 3 failed, 4 budget exceeded, 5 timed out, 6 cancelled.

Editors: the first line `kiln init` writes points YAML tooling at the schema for completion. Full
reference and design: [SPEC.md](SPEC.md).

## Development

```bash
go test ./...
```
