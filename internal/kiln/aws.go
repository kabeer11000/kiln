package kiln

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Fargate prices, us-east-1, Linux x86 (2026). Spot floats; this is a conservative upper estimate
// so the dollar cap errs on the side of stopping early.
const (
	fargateVCPUHour     = 0.04048
	fargateGBHour       = 0.004445
	fargateSpotVCPUHour = 0.0130
	fargateSpotGBHour   = 0.00142
	publicIPv4Hour      = 0.005

	// awsStopReserve is how long stopping a task takes, all billed: the agent's 10s to exit,
	// the monitor's final reading and upload, and ECS tearing the task down (measured 16-50s).
	awsStopReserve = 60 * time.Second
	// deadlineGrace lets the supervisor stop the task first; the in-task deadline is the backstop.
	deadlineGrace = 15
	monitorImage  = "public.ecr.aws/docker/library/alpine:3.20"
	agentName     = "agent"
	monitorName   = "kiln-monitor"
	deadlineName  = "kiln-deadline" // v1.0 tasks; still recognised when reconciling old runs
	logPrefix     = "kiln"
	// urlSlack keeps a run's presigned URLs valid a while past its hard deadline, for the final upload.
	urlSlack = time.Hour
)

// AWS runs tasks on ECS Fargate. Infra (cluster, subnets, roles) is created once by
// deploy/aws and published to SSM at /kiln/<stack>/config, so kiln.yaml stays small.
type AWS struct {
	Region string
	Stack  string
	infra  *awsInfra
	signer *awsKey
}

type awsInfra struct {
	Cluster          string   `json:"cluster"`
	Subnets          []string `json:"subnets"`
	SecurityGroups   []string `json:"security_groups"`
	ExecutionRoleARN string   `json:"execution_role_arn"`
	LogGroup         string   `json:"log_group"`
	SecretsPrefix    string   `json:"secrets_prefix"`
	Bucket           string   `json:"bucket"`           // v1.1+: artifacts and inbox
	SignerParameter  string   `json:"signer_parameter"` // v1.1+: SSM name of the URL-signing key
}

func (a *AWS) cli(ctx context.Context, args ...string) (string, error) {
	return runCLI(ctx, "check `aws sts get-caller-identity` works and `terraform apply` ran in deploy/aws",
		"aws", append(args, "--region", a.Region, "--output", "json")...)
}

func (a *AWS) load(ctx context.Context) (*awsInfra, error) {
	if a.infra != nil {
		return a.infra, nil
	}
	name := "/kiln/" + a.Stack + "/config"
	out, err := a.cli(ctx, "ssm", "get-parameter", "--name", name, "--query", "Parameter.Value")
	if err != nil {
		return nil, &Error{Code: "aws_not_set_up", Message: "kiln stack " + a.Stack + " not found in " + a.Region + ": " + err.Error(),
			Fix: "run `terraform apply` in deploy/aws (see README), or set aws.stack / aws.region in kiln.yaml"}
	}
	var raw string
	var inf awsInfra
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &inf); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	a.infra = &inf
	return a.infra, nil
}

func (a *AWS) Preflight(ctx context.Context, c Config) error {
	if _, _, err := fargateSize(c.Resources.CPU, c.Resources.MemoryGB); err != nil {
		return err
	}
	inf, err := a.load(ctx)
	if err != nil {
		return err
	}
	var missing []string
	names := slices.Clone(c.Secrets)
	if c.Repo != nil && c.Repo.Token != "" {
		names = append(names, c.Repo.Token)
	}
	for chunk := range slices.Chunk(names, 10) { // GetParameters takes at most 10 names
		args := []string{"ssm", "get-parameters", "--query", "InvalidParameters", "--names"}
		for _, s := range chunk {
			args = append(args, inf.SecretsPrefix+s)
		}
		out, err := a.cli(ctx, args...)
		if err != nil {
			return err
		}
		var invalid []string
		if err := json.Unmarshal([]byte(out), &invalid); err != nil {
			return err
		}
		missing = append(missing, invalid...)
	}
	if len(missing) > 0 {
		return &Error{Code: "missing_secret", Message: "not in SSM Parameter Store: " + strings.Join(missing, ", "),
			Fix: fmt.Sprintf("aws ssm put-parameter --region %s --type SecureString --name %s --value ...", a.Region, missing[0])}
	}
	return nil
}

func (a *AWS) Start(ctx context.Context, r *Run) (string, error) {
	inf, err := a.load(ctx)
	if err != nil {
		return "", err
	}
	if err := a.uploadInputs(ctx, r); err != nil {
		return "", err
	}
	urls, err := a.runURLs(ctx, r, time.Now())
	if err != nil {
		return "", err
	}
	td, err := a.taskDefinition(r, inf, urls)
	if err != nil {
		return "", err
	}
	tdJSON, _ := json.Marshal(td)
	out, err := a.cli(ctx, "ecs", "register-task-definition", "--cli-input-json", string(tdJSON),
		"--query", "taskDefinition.taskDefinitionArn")
	if err != nil {
		return "", err
	}
	var tdARN string
	if err := json.Unmarshal([]byte(out), &tdARN); err != nil {
		return "", err
	}

	provider := "FARGATE"
	if r.Config.Resources.Spot {
		provider = "FARGATE_SPOT"
	}
	network, _ := json.Marshal(map[string]any{"awsvpcConfiguration": map[string]any{
		"subnets": inf.Subnets, "securityGroups": inf.SecurityGroups, "assignPublicIp": "ENABLED"}})
	out, err = a.cli(ctx, "ecs", "run-task",
		"--cluster", inf.Cluster,
		"--task-definition", tdARN,
		"--capacity-provider-strategy", "capacityProvider="+provider+",weight=1",
		"--network-configuration", string(network),
		"--started-by", "kiln",
		"--enable-ecs-managed-tags",
		"--tags", "key=project,value=kiln", "key=kiln-run,value="+r.ID, "key=kiln-stack,value="+a.Stack)
	if err != nil {
		a.deregister(tdARN)
		return "", err
	}
	var res struct {
		Tasks    []struct{ TaskArn string }
		Failures []struct{ Arn, Reason, Detail string }
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		a.deregister(tdARN)
		return "", err
	}
	if len(res.Tasks) == 0 {
		a.deregister(tdARN)
		reason := "no task started"
		if len(res.Failures) > 0 {
			reason = strings.TrimSpace(res.Failures[0].Reason + " " + res.Failures[0].Detail)
		}
		return "", &Error{Code: "aws_run_failed", Message: "ECS run-task: " + reason, Fix: "check Fargate quotas and capacity in " + a.Region}
	}
	return res.Tasks[0].TaskArn, nil
}

// runURLs are presigned URLs for this run's objects only: the monitor polls the inbox and
// uploads artifacts with them, so the task needs no AWS credentials. Empty on pre-v1.1 stacks.
func (a *AWS) runURLs(ctx context.Context, r *Run, now time.Time) (map[string]string, error) {
	inf, err := a.load(ctx)
	if err != nil || inf.Bucket == "" {
		return map[string]string{}, err
	}
	k, err := a.loadSigner(ctx)
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(deadlineSeconds(r.Config.Budget, r.PricePerHour))*time.Second + urlSlack
	urls := map[string]string{
		"KILN_INBOX_URL":  presignS3(*k, a.Region, inf.Bucket, runKey(r.ID, "inbox.jsonl"), "GET", now, ttl),
		"KILN_UPLOAD_URL": presignS3(*k, a.Region, inf.Bucket, runKey(r.ID, "artifacts.tgz"), "PUT", now, ttl),
	}
	if r.Inputs != nil {
		urls["KILN_INPUTS_URL"] = presignS3(*k, a.Region, inf.Bucket, runKey(r.ID, "inputs.tgz"), "GET", now, ttl)
	}
	return urls, nil
}

func runKey(id, name string) string { return "runs/" + id + "/" + name }

// uploadInputs puts the packed inputs where the monitor will fetch them, and checks they are
// byte-identical to what the run record says (so a rerun can't silently use different files).
func (a *AWS) uploadInputs(ctx context.Context, r *Run) error {
	if r.Inputs == nil {
		return nil
	}
	data, err := os.ReadFile(r.Inputs.Local)
	if err != nil {
		return &Error{Code: "inputs_missing", Message: "packed inputs " + r.Inputs.Local + " are gone: " + err.Error(),
			Fix: "run `kiln run` again to repack them from the source files"}
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != r.Inputs.SHA256 {
		return &Error{Code: "inputs_changed", Message: r.Inputs.Local + " no longer matches the recorded sha256"}
	}
	inf, k, err := a.bucketAndSigner(ctx)
	if err != nil {
		return err
	}
	_, err = httpDo(ctx, "PUT", presignS3(*k, a.Region, inf.Bucket, runKey(r.ID, "inputs.tgz"), "PUT", time.Now(), 15*time.Minute), data)
	return err
}

func (a *AWS) loadSigner(ctx context.Context) (*awsKey, error) {
	if a.signer != nil {
		return a.signer, nil
	}
	inf, err := a.load(ctx)
	if err != nil {
		return nil, err
	}
	if inf.SignerParameter == "" {
		return nil, &Error{Code: "aws_stack_outdated", Message: "kiln stack " + a.Stack + " has no artifact bucket",
			Fix: "run `terraform apply` in deploy/aws to upgrade the stack to v1.1"}
	}
	out, err := a.cli(ctx, "ssm", "get-parameter", "--name", inf.SignerParameter, "--with-decryption", "--query", "Parameter.Value")
	if err != nil {
		return nil, err
	}
	var raw string
	var k awsKey
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &k); err != nil || k.ID == "" || k.Secret == "" {
		return nil, fmt.Errorf("%s: not a signing key", inf.SignerParameter)
	}
	a.signer = &k
	return a.signer, nil
}

func (a *AWS) taskDefinition(r *Run, inf *awsInfra, urls map[string]string) (map[string]any, error) {
	c := r.Config
	cpu, mem, err := fargateSize(c.Resources.CPU, c.Resources.MemoryGB)
	if err != nil {
		return nil, err
	}
	env := []map[string]string{{"name": "KILN_RUN_ID", "value": r.ID}, {"name": "KILN_DIR", "value": "/kiln"}}
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		env = append(env, map[string]string{"name": k, "value": c.Env[k]})
	}
	secrets := []map[string]string{}
	for _, s := range c.Secrets {
		secrets = append(secrets, map[string]string{"name": s, "valueFrom": inf.SecretsPrefix + s})
	}
	logs := map[string]any{"logDriver": "awslogs", "options": map[string]string{
		"awslogs-group": inf.LogGroup, "awslogs-region": a.Region, "awslogs-stream-prefix": logPrefix}}
	mount := []map[string]any{{"sourceVolume": "kiln", "containerPath": "/kiln"}}
	agent := map[string]any{
		"name": agentName, "image": c.Image, "essential": true,
		"environment": env, "secrets": secrets, "mountPoints": mount,
		"stopTimeout":      10, // seconds between SIGTERM and SIGKILL; ECS default is 30, all billed
		"logConfiguration": logs,
		// HEALTHY: the monitor has created /kiln and unpacked any inputs (it touches /kiln/.ready).
		"dependsOn": []map[string]string{{"containerName": monitorName, "condition": "HEALTHY"}},
	}
	if len(c.Command) > 0 {
		agent["command"] = c.Command
	}
	if c.Workdir != "" {
		agent["workingDirectory"] = c.Workdir
	}
	// The monitor is essential: when it exits at the hard deadline, ECS stops the whole task.
	// This enforces the caps inside AWS even if the machine running `kiln run` goes away.
	// It shares the task's process namespace (pidMode task) so it sees every agent process.
	monEnv := []map[string]string{{"name": "KILN_DEADLINE", "value": strconv.Itoa(deadlineSeconds(c.Budget, r.PricePerHour))}}
	for _, k := range slices.Sorted(maps.Keys(urls)) {
		monEnv = append(monEnv, map[string]string{"name": k, "value": urls[k]})
	}
	monSecrets := []map[string]string{}
	if r.Repo != nil {
		monEnv = append(monEnv, map[string]string{"name": "KILN_REPO_URL", "value": r.Repo.URL},
			map[string]string{"name": "KILN_REPO_SHA", "value": r.Repo.SHA})
		env = append(env, map[string]string{"name": "KILN_REPO_SHA", "value": r.Repo.SHA})
		agent["environment"] = env
		if c.Repo != nil && c.Repo.Token != "" {
			monSecrets = append(monSecrets, map[string]string{"name": "KILN_REPO_TOKEN", "valueFrom": inf.SecretsPrefix + c.Repo.Token})
		}
	}
	monitor := map[string]any{
		"name": monitorName, "image": monitorImage, "essential": true, "secrets": monSecrets,
		"stopTimeout": 30,                                                                // time for the final reading and the artifact upload
		"command":     []string{"sh", "-c", strings.ReplaceAll(monitorScript, "\r", "")}, // CRLF breaks sh
		"environment": monEnv, "mountPoints": mount, "logConfiguration": logs,
		"healthCheck": map[string]any{"command": []string{"CMD-SHELL", "test -f /kiln/.ready"},
			"interval": 5, "timeout": 2, "retries": 3, "startPeriod": 300},
	}
	return map[string]any{
		"family":                  "kiln-" + a.Stack + "-" + c.Name,
		"requiresCompatibilities": []string{"FARGATE"},
		"networkMode":             "awsvpc",
		"cpu":                     strconv.Itoa(cpu),
		"memory":                  strconv.Itoa(mem),
		"executionRoleArn":        inf.ExecutionRoleARN,
		"runtimePlatform":         map[string]string{"cpuArchitecture": "X86_64", "operatingSystemFamily": "LINUX"},
		"pidMode":                 "task",
		"volumes":                 []map[string]string{{"name": "kiln"}},
		"containerDefinitions":    []any{agent, monitor},
		"tags":                    []map[string]string{{"key": "project", "value": "kiln"}, {"key": "kiln-run", "value": r.ID}},
	}, nil
}

// deadlineSeconds is the in-task hard limit: the hour cap, or the time the dollar cap is
// reached at this price, whichever is sooner, minus the billed shutdown time, plus a little
// grace so the supervisor (if alive) acts first.
func deadlineSeconds(b Budget, pricePerHour float64) int {
	secs := b.MaxHours * 3600
	if pricePerHour > 0 {
		secs = min(secs, b.MaxUSD/pricePerHour*3600)
	}
	return max(int(math.Ceil(secs-awsStopReserve.Seconds()))+deadlineGrace, deadlineGrace)
}

func (a *AWS) StopReserve() time.Duration { return awsStopReserve }

type ecsTask struct {
	LastStatus        string `json:"lastStatus"`
	PullStartedAt     string `json:"pullStartedAt"` // Fargate billing starts here
	CreatedAt         string `json:"createdAt"`
	StoppedAt         string `json:"stoppedAt"`
	StoppedReason     string `json:"stoppedReason"`
	TaskDefinitionArn string `json:"taskDefinitionArn"`
	Containers        []struct {
		Name        string `json:"name"`
		ExitCode    *int   `json:"exitCode"`
		Reason      string `json:"reason"`
		ImageDigest string `json:"imageDigest"`
	} `json:"containers"`
}

func (a *AWS) describe(ctx context.Context, handle string) (ecsTask, error) {
	inf, err := a.load(ctx)
	if err != nil {
		return ecsTask{}, err
	}
	out, err := a.cli(ctx, "ecs", "describe-tasks", "--cluster", inf.Cluster, "--tasks", handle)
	if err != nil {
		return ecsTask{}, err
	}
	var res struct {
		Tasks    []ecsTask
		Failures []struct{ Reason string }
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return ecsTask{}, err
	}
	if len(res.Tasks) == 0 {
		return ecsTask{}, errTaskGone
	}
	return res.Tasks[0], nil
}

var errTaskGone = errors.New("ECS task not found")

func (a *AWS) Status(ctx context.Context, handle string) (Status, error) {
	t, err := a.describe(ctx, handle)
	if err != nil {
		return Status{}, err
	}
	return t.status(), nil
}

func (t ecsTask) status() Status {
	var st Status
	st.BilledFrom = awsTime(t.PullStartedAt)
	if st.BilledFrom.IsZero() {
		st.BilledFrom = awsTime(t.CreatedAt)
	}
	st.BilledTo = awsTime(t.StoppedAt)
	agentExit, deadlineExit := -1, -1
	var agentReason string
	for _, c := range t.Containers {
		switch {
		case c.Name == agentName:
			st.ImageDigest, agentReason = c.ImageDigest, c.Reason
			if c.ExitCode != nil {
				agentExit = *c.ExitCode
			}
		case (c.Name == monitorName || c.Name == deadlineName) && c.ExitCode != nil:
			deadlineExit = *c.ExitCode
		}
	}
	switch {
	case t.LastStatus != "STOPPED":
		st.State = Running
	case agentExit == 0:
		st.State, st.Reason = Succeeded, "exit code 0"
	case deadlineExit == 0:
		st.State, st.Reason = TimedOut, "hard deadline reached (enforced inside AWS)"
	default:
		parts := []string{t.StoppedReason}
		if agentReason != "" {
			parts = append(parts, agentReason)
		}
		if agentExit >= 0 {
			parts = append(parts, "exit code "+strconv.Itoa(agentExit))
		}
		st.State, st.Reason = Failed, strings.Join(parts, "; ")
	}
	return st
}

func (a *AWS) Logs(ctx context.Context, handle string, follow bool, w io.Writer) error {
	return a.logs(ctx, agentName, handle, follow, w)
}

// Monitor streams the kiln-monitor's JSON records (see MonitorRecord).
func (a *AWS) Monitor(ctx context.Context, handle string, follow bool, w io.Writer) error {
	return a.logs(ctx, monitorName, handle, follow, w)
}

func (a *AWS) logs(ctx context.Context, container, handle string, follow bool, w io.Writer) error {
	inf, err := a.load(ctx)
	if err != nil {
		return err
	}
	stream := logPrefix + "/" + container + "/" + handle[strings.LastIndex(handle, "/")+1:]
	if follow {
		return a.followLogs(ctx, inf.LogGroup, stream, w)
	}
	out, err := a.cli(ctx, "logs", "filter-log-events", "--log-group-name", inf.LogGroup,
		"--log-stream-names", stream, "--query", "events[].message")
	if err != nil {
		return err
	}
	var lines []string
	if err := json.Unmarshal([]byte(out), &lines); err != nil {
		return err
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(w, l); err != nil {
			return err
		}
	}
	return nil
}

// followLogs polls the stream with a forward token until ctx ends. (`aws logs tail --follow`
// is not used: the CLI block-buffers when writing to a pipe, so nothing arrives until it exits.)
// The stream doesn't exist until the container starts; that and transient errors are retried.
func (a *AWS) followLogs(ctx context.Context, group, stream string, w io.Writer) error {
	token := ""
	for {
		args := []string{"logs", "get-log-events", "--log-group-name", group, "--log-stream-name", stream,
			"--start-from-head", "--no-paginate", "--query", "{token: nextForwardToken, lines: events[].message}"}
		if token != "" {
			args = append(args, "--next-token", token)
		}
		out, err := a.cli(ctx, args...)
		var page struct {
			Token string   `json:"token"`
			Lines []string `json:"lines"`
		}
		if err == nil && json.Unmarshal([]byte(out), &page) == nil {
			for _, l := range page.Lines {
				if _, err := fmt.Fprintln(w, l); err != nil {
					return err
				}
			}
			if page.Token != "" {
				token = page.Token
			}
			if len(page.Lines) > 0 {
				continue // more may be waiting
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *AWS) Stop(ctx context.Context, handle string) error {
	inf, err := a.load(ctx)
	if err != nil {
		return err
	}
	t, err := a.describe(ctx, handle)
	if errors.Is(err, errTaskGone) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.LastStatus == "STOPPED" {
		return nil
	}
	if _, err := a.cli(ctx, "ecs", "stop-task", "--cluster", inf.Cluster, "--task", handle, "--reason", "stopped by kiln"); err != nil {
		return err
	}
	// Wait until it's really stopped: the monitor's final reading and artifact upload happen
	// during shutdown, and billing runs until then, so the recorded spend stays honest.
	for {
		if t, err := a.describe(ctx, handle); err != nil || t.LastStatus == "STOPPED" {
			return nil // stop-task succeeded; failing to watch it finish isn't a stop failure
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

// Send appends a message to the run's inbox. The monitor copies it to /kiln/inbox.jsonl in
// the task within a few seconds. Each line is {"t": unix seconds, "msg": text}.
// ponytail: read-modify-write; two senders at the same instant can drop one message.
func (a *AWS) Send(ctx context.Context, r *Run, msg string) error {
	inf, k, err := a.bucketAndSigner(ctx)
	if err != nil {
		return err
	}
	key := runKey(r.ID, "inbox.jsonl")
	now := time.Now()
	cur, err := httpDo(ctx, "GET", presignS3(*k, a.Region, inf.Bucket, key, "GET", now, 5*time.Minute), nil)
	var he *httpError
	if errors.As(err, &he) && (he.Code == 404 || he.Code == 403) { // 403: no inbox yet (the signer can't list)
		cur, err = nil, nil
	}
	if err != nil {
		return err
	}
	line, _ := json.Marshal(map[string]any{"t": now.Unix(), "msg": msg})
	body := append(cur, append(line, '\n')...)
	_, err = httpDo(ctx, "PUT", presignS3(*k, a.Region, inf.Bucket, key, "PUT", now, 5*time.Minute), body)
	return err
}

// Artifacts downloads the archive the monitor uploaded when the task stopped.
func (a *AWS) Artifacts(ctx context.Context, r *Run) ([]byte, error) {
	inf, k, err := a.bucketAndSigner(ctx)
	if err != nil {
		return nil, err
	}
	data, err := httpDo(ctx, "GET", presignS3(*k, a.Region, inf.Bucket, runKey(r.ID, "artifacts.tgz"), "GET", time.Now(), 5*time.Minute), nil)
	var he *httpError
	if errors.As(err, &he) && (he.Code == 404 || he.Code == 403) {
		return nil, &Error{Code: "no_artifacts", Message: "no artifacts uploaded for " + r.ID,
			Fix: "artifacts are uploaded when the task stops; put files in /kiln/artifacts inside the task"}
	}
	return data, err
}

func (a *AWS) bucketAndSigner(ctx context.Context) (*awsInfra, *awsKey, error) {
	k, err := a.loadSigner(ctx)
	if err != nil {
		return nil, nil, err
	}
	return a.infra, k, nil
}

// Remove deregisters the per-run task definition. Logs stay in CloudWatch for the retention period.
func (a *AWS) Remove(ctx context.Context, handle string) error {
	t, err := a.describe(ctx, handle)
	if errors.Is(err, errTaskGone) {
		return nil
	}
	if err != nil {
		return err
	}
	a.deregister(t.TaskDefinitionArn)
	return nil
}

// deregister cleans up the per-run task definition revision. Best effort: a leftover
// revision costs nothing.
func (a *AWS) deregister(arn string) {
	if arn != "" {
		_, _ = a.cli(context.Background(), "ecs", "deregister-task-definition", "--task-definition", arn)
	}
}

func (a *AWS) PricePerHour(r Resources) float64 {
	cpu, mem, err := fargateSize(r.CPU, r.MemoryGB)
	if err != nil {
		return math.Inf(1) // unreachable after Preflight; fail safe: any spend exceeds the cap
	}
	vcpu, gb := float64(cpu)/1024, float64(mem)/1024
	if r.Spot {
		return vcpu*fargateSpotVCPUHour + gb*fargateSpotGBHour + publicIPv4Hour
	}
	return vcpu*fargateVCPUHour + gb*fargateGBHour + publicIPv4Hour
}

// fargateSize rounds a request up to the smallest valid Fargate CPU (units) / memory (MiB) pair.
func fargateSize(cpu, memGB float64) (int, int, error) {
	tiers := []struct{ cpu, minMem, maxMem, step int }{
		{256, 512, 2048, 512}, // 0.5, 1 or 2 GB
		{512, 1024, 4096, 1024},
		{1024, 2048, 8192, 1024},
		{2048, 4096, 16384, 1024},
		{4096, 8192, 30720, 1024},
		{8192, 16384, 61440, 4096},
		{16384, 32768, 122880, 8192},
	}
	wantCPU, wantMem := int(math.Ceil(cpu*1024)), int(math.Ceil(memGB*1024))
	for _, t := range tiers {
		if t.cpu < wantCPU || t.maxMem < wantMem {
			continue
		}
		mem := t.minMem
		for mem < wantMem {
			mem += t.step
		}
		if t.cpu == 256 && mem == 1536 {
			mem = 2048 // 0.25 vCPU allows 0.5, 1 or 2 GB only
		}
		return t.cpu, mem, nil
	}
	return 0, 0, &Error{Code: "invalid_resources", Message: fmt.Sprintf("no Fargate size fits cpu %g, memory_gb %g", cpu, memGB),
		Fix: "Fargate allows up to 16 vCPU / 120 GB"}
}

// awsTime parses the CLI's timestamps ("2026-11-17T18:58:53.337000+05:00"); zero if absent.
func awsTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
