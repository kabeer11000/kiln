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

## Commands

| Command | What |
|---|---|
| `kiln init` | Write a starter kiln.yaml |
| `kiln validate` | Check config, show it with defaults |
| `kiln run [--plan]` | Start and supervise a run (Ctrl-C cancels it) |
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
