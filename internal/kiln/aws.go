package kiln

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// Fargate prices, us-east-1, Linux x86 (2026). Spot floats; this is a conservative upper estimate
// so the dollar cap errs on the side of stopping early.
const (
	fargateVCPUHour     = 0.04048
	fargateGBHour       = 0.004445
	fargateSpotVCPUHour = 0.0130
	fargateSpotGBHour   = 0.00142
	publicIPv4Hour      = 0.005

	// deadlineGrace lets the supervisor stop the task first; the in-task deadline is the backstop.
	deadlineGrace = 60
	deadlineImage = "public.ecr.aws/docker/library/busybox:stable"
	agentName     = "agent"
	deadlineName  = "kiln-deadline"
	logPrefix     = "kiln"
)

// AWS runs tasks on ECS Fargate. Infra (cluster, subnets, roles) is created once by
// deploy/aws and published to SSM at /kiln/<stack>/config, so kiln.yaml stays small.
type AWS struct {
	Region string
	Stack  string
	infra  *awsInfra
}

type awsInfra struct {
	Cluster          string   `json:"cluster"`
	Subnets          []string `json:"subnets"`
	SecurityGroups   []string `json:"security_groups"`
	ExecutionRoleARN string   `json:"execution_role_arn"`
	LogGroup         string   `json:"log_group"`
	SecretsPrefix    string   `json:"secrets_prefix"`
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
	for chunk := range slices.Chunk(c.Secrets, 10) { // GetParameters takes at most 10 names
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
	td, err := a.taskDefinition(r, inf)
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

func (a *AWS) taskDefinition(r *Run, inf *awsInfra) (map[string]any, error) {
	c := r.Config
	cpu, mem, err := fargateSize(c.Resources.CPU, c.Resources.MemoryGB)
	if err != nil {
		return nil, err
	}
	env := []map[string]string{{"name": "KILN_RUN_ID", "value": r.ID}}
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		env = append(env, map[string]string{"name": k, "value": c.Env[k]})
	}
	secrets := []map[string]string{}
	for _, s := range c.Secrets {
		secrets = append(secrets, map[string]string{"name": s, "valueFrom": inf.SecretsPrefix + s})
	}
	agent := map[string]any{
		"name": agentName, "image": c.Image, "essential": true,
		"environment": env, "secrets": secrets,
		"stopTimeout": 10, // seconds between SIGTERM and SIGKILL; ECS default is 30, all billed
		"logConfiguration": map[string]any{"logDriver": "awslogs", "options": map[string]string{
			"awslogs-group": inf.LogGroup, "awslogs-region": a.Region, "awslogs-stream-prefix": logPrefix}},
	}
	if len(c.Command) > 0 {
		agent["command"] = c.Command
	}
	// The deadline container is essential: when its sleep ends, ECS stops the whole task.
	// This enforces the caps inside AWS even if the machine running `kiln run` goes away.
	deadline := map[string]any{
		"name": deadlineName, "image": deadlineImage, "essential": true, "stopTimeout": 2,
		"command": []string{"sleep", strconv.Itoa(deadlineSeconds(c.Budget, r.PricePerHour))},
	}
	return map[string]any{
		"family":                  "kiln-" + a.Stack + "-" + c.Name,
		"requiresCompatibilities": []string{"FARGATE"},
		"networkMode":             "awsvpc",
		"cpu":                     strconv.Itoa(cpu),
		"memory":                  strconv.Itoa(mem),
		"executionRoleArn":        inf.ExecutionRoleARN,
		"runtimePlatform":         map[string]string{"cpuArchitecture": "X86_64", "operatingSystemFamily": "LINUX"},
		"containerDefinitions":    []any{agent, deadline},
		"tags":                    []map[string]string{{"key": "project", "value": "kiln"}, {"key": "kiln-run", "value": r.ID}},
	}, nil
}

// deadlineSeconds is the hard wall-clock limit: the hour cap, or the time the dollar cap is
// reached at this price, whichever is sooner, plus grace for the supervisor to act first.
func deadlineSeconds(b Budget, pricePerHour float64) int {
	secs := b.MaxHours * 3600
	if pricePerHour > 0 {
		secs = min(secs, b.MaxUSD/pricePerHour*3600)
	}
	return int(math.Ceil(secs)) + deadlineGrace
}

type ecsTask struct {
	LastStatus        string `json:"lastStatus"`
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
	agentExit, deadlineExit := -1, -1
	var agentReason string
	for _, c := range t.Containers {
		switch {
		case c.Name == agentName:
			st.ImageDigest, agentReason = c.ImageDigest, c.Reason
			if c.ExitCode != nil {
				agentExit = *c.ExitCode
			}
		case c.Name == deadlineName && c.ExitCode != nil:
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
	inf, err := a.load(ctx)
	if err != nil {
		return err
	}
	stream := logPrefix + "/" + agentName + "/" + handle[strings.LastIndex(handle, "/")+1:]
	if follow {
		cmd := exec.CommandContext(ctx, "aws", "logs", "tail", inf.LogGroup, "--log-stream-names", stream,
			"--follow", "--format", "short", "--region", a.Region)
		cmd.Stdout, cmd.Stderr = w, w
		return cmd.Run()
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
	if t.LastStatus != "STOPPED" {
		if _, err := a.cli(ctx, "ecs", "stop-task", "--cluster", inf.Cluster, "--task", handle, "--reason", "stopped by kiln"); err != nil {
			return err
		}
	}
	return nil
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
