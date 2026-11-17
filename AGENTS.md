# kiln for AI agents

kiln runs a command in a cloud container (AWS Fargate) with hard dollar and time caps, starting
from a git repo and a stock image. You never need Docker or an image build on the local machine.

## Fastest path

With MCP (server command: `kiln mcp`, stdio): `kiln_schema` → write kiln.yaml → `kiln_plan` →
`kiln_run` → `kiln_wait` → `kiln_artifacts`.

Without MCP, the same with the CLI and `--json`:

```bash
kiln schema                          # every kiln.yaml field, default and limit
kiln validate --json                 # check before spending anything
kiln run --plan --json               # price, caps, worst case, warnings
kiln run -d --json                   # -> {"id": "r_...", ...}; returns at once
kiln wait r_... --timeout 10m --json # final record; exit code = outcome
kiln artifacts r_... --json          # -> {"dir": "...", "files": N}
```

## A good kiln.yaml

```yaml
name: fix-flaky-tests                               # lowercase, digits, dashes
image: public.ecr.aws/docker/library/node:22        # stock image; prefer public.ecr.aws (no rate limits)
repo:
  url: https://github.com/acme/app.git
  ref: my-branch                                    # pinned to its commit when the run starts
  token: GITHUB_TOKEN                               # only for private repos: a secret NAME
command: ["sh", "-c", "npm ci && npm test 2>&1 | tee /kiln/artifacts/test.txt"]
budget: {max_usd: 1, max_hours: 1}                  # hard; the run is stopped at either
resources: {cpu: 2, memory_gb: 4, spot: true}
```

- The command runs in `/kiln/src` (the checkout) when `repo:` is set.
- Local files that aren't committed: `inputs: ["."]` (+ `exclude: [node_modules]`) → `/kiln/inputs`.
- Stock images on public ECR: `public.ecr.aws/docker/library/{node,python,golang,rust,alpine}:<tag>`.
  For browsers: `mcr.microsoft.com/playwright:<version>`.
- Shell pipelines hide exit codes: use `set -o pipefail` (bash) or don't pipe the command whose
  result matters, or the run reports `succeeded` when your tests failed.

## Inside the task

| Path | Use |
|---|---|
| `/kiln/src` | The repo checkout (`$KILN_REPO_SHA` is the commit) |
| `/kiln/inputs` | Files from `inputs:` |
| `/kiln/artifacts` | Write results here; downloaded with `kiln artifacts` |
| `/kiln/outbox.jsonl` | Append a line to report progress; shows live in `kiln top` / `kiln_top` |
| `/kiln/inbox.jsonl` | Messages from `kiln send`: lines of `{"t": unix, "msg": "..."}`. Poll it |

Also set: `KILN_RUN_ID`, `KILN_DIR=/kiln`.

## Outcomes

`kiln wait` / `kiln status` → `run.state`:

| state | exit code | meaning |
|---|---|---|
| succeeded | 0 | command exited 0 |
| failed | 3 | non-zero exit, image pull error, repo checkout failed... see `run.reason` |
| budget_exceeded | 4 | dollar cap reached (by design, not a crash) |
| timed_out | 5 | time cap reached |
| cancelled | 6 | `kiln cancel` |

Errors from kiln itself are `{"error": {"code", "message", "fix"}}`: do what `fix` says.

## Watching and steering

- `kiln top <run> --json`: processes with CPU% and memory, peak memory per process, container
  memory, network, connections, disk, artifact files, and events (agent messages, process exits).
- `kiln send <run> "message"`: the task sees it in `/kiln/inbox.jsonl` within seconds.
- `kiln cancel <run>`: stops it; spend stops within a minute.
- `kiln rerun <run> -d`: exact reproduction (same image digest, commit, inputs).
- `kiln diff <a> <b>`: what changed between two runs.

Runs are visible from any folder on the machine (`kiln list`).

## Costs

`kiln run --plan` shows `price_per_hour` and `max_spend_usd` before anything starts. A 1 vCPU / 2 GB
Spot task is about $0.02/hour. Starting and stopping take about a minute and are billed; caps
smaller than that can't be met and `--plan` warns about it.
