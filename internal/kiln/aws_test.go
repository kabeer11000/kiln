package kiln

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestFargateSize(t *testing.T) {
	tests := []struct {
		cpu, mem       float64
		wantCPU, wantM int
	}{
		{0.25, 0.5, 256, 512},
		{0.25, 1.5, 256, 2048}, // 1.5 GB not allowed at 0.25 vCPU
		{0.1, 0.1, 256, 512},
		{1, 2, 1024, 2048},
		{1, 3, 1024, 3072},
		{1, 9, 2048, 9216}, // memory forces the next CPU tier
		{3, 4, 4096, 8192},
		{16, 120, 16384, 122880},
	}
	for _, tt := range tests {
		cpu, mem, err := fargateSize(tt.cpu, tt.mem)
		if err != nil || cpu != tt.wantCPU || mem != tt.wantM {
			t.Errorf("fargateSize(%g, %g) = %d, %d, %v; want %d, %d", tt.cpu, tt.mem, cpu, mem, err, tt.wantCPU, tt.wantM)
		}
	}
	if _, _, err := fargateSize(32, 1); err == nil {
		t.Error("32 vCPU should not fit")
	}
}

func TestFargatePrice(t *testing.T) {
	a := &AWS{}
	got := a.PricePerHour(Resources{CPU: 1, MemoryGB: 2})
	want := fargateVCPUHour + 2*fargateGBHour + publicIPv4Hour
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("on-demand = %v, want %v", got, want)
	}
	if spot := a.PricePerHour(Resources{CPU: 1, MemoryGB: 2, Spot: true}); spot >= got {
		t.Errorf("spot %v should be cheaper than on-demand %v", spot, got)
	}
	if p := a.PricePerHour(Resources{CPU: 99}); !math.IsInf(p, 1) {
		t.Errorf("invalid size should price as +Inf (fail safe), got %v", p)
	}
}

func TestDeadlineSeconds(t *testing.T) {
	// Hour cap binds: 0.5h.
	if got := deadlineSeconds(Budget{MaxUSD: 100, MaxHours: 0.5}, 1); got != 1800+deadlineGrace {
		t.Errorf("hour cap: %d", got)
	}
	// Dollar cap binds: $1 at $10/h = 6 min.
	if got := deadlineSeconds(Budget{MaxUSD: 1, MaxHours: 10}, 10); got != 360+deadlineGrace {
		t.Errorf("dollar cap: %d", got)
	}
	// Free: only the hour cap applies.
	if got := deadlineSeconds(Budget{MaxUSD: 1, MaxHours: 1}, 0); got != 3600+deadlineGrace {
		t.Errorf("free: %d", got)
	}
}

func TestTaskDefinition(t *testing.T) {
	a := &AWS{Region: "us-east-1", Stack: "kiln"}
	inf := &awsInfra{ExecutionRoleARN: "arn:role", LogGroup: "/kiln/kiln", SecretsPrefix: "/kiln/kiln/secrets/"}
	r := &Run{ID: "r_1", PricePerHour: 1, Config: Config{
		Name: "exp", Image: "alpine", Command: []string{"echo", "hi"},
		Env: map[string]string{"MODE": "fast"}, Secrets: []string{"TOKEN"},
		Budget: Budget{MaxUSD: 5, MaxHours: 1}, Resources: Resources{CPU: 0.25, MemoryGB: 0.5},
	}}
	td, err := a.taskDefinition(r, inf, map[string]string{"KILN_UPLOAD_URL": "https://up"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(td)
	s := string(data)
	for _, want := range []string{
		`"family":"kiln-kiln-exp"`, `"cpu":"256"`, `"memory":"512"`,
		`"command":["echo","hi"]`, `{"name":"MODE","value":"fast"}`, `{"name":"KILN_RUN_ID","value":"r_1"}`,
		`{"name":"TOKEN","valueFrom":"/kiln/kiln/secrets/TOKEN"}`,
		`"name":"kiln-monitor"`, `{"name":"KILN_DEADLINE","value":"3660"}`, `"essential":true`,
		`{"name":"KILN_UPLOAD_URL","value":"https://up"}`, `"pidMode":"task"`, `"volumes":[{"name":"kiln"}]`,
		`"containerPath":"/kiln"`, `"dependsOn":[{"condition":"HEALTHY","containerName":"kiln-monitor"}]`,
		`{"name":"KILN_DIR","value":"/kiln"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("task definition missing %s\n%s", want, s)
		}
	}
	if strings.Contains(s, "secret-value") {
		t.Error("secret values must never be in the task definition")
	}
}

func TestECSStatus(t *testing.T) {
	task := func(js string) ecsTask {
		var tk ecsTask
		if err := json.Unmarshal([]byte(js), &tk); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	tests := []struct {
		name, js string
		want     State
		reason   string
	}{
		{"pending", `{"lastStatus":"PROVISIONING","containers":[{"name":"agent"}]}`, Running, ""},
		{"succeeded", `{"lastStatus":"STOPPED","containers":[{"name":"agent","exitCode":0,"imageDigest":"sha256:x"},{"name":"kiln-deadline","exitCode":137}]}`, Succeeded, "exit code 0"},
		{"agent failed", `{"lastStatus":"STOPPED","stoppedReason":"Essential container in task exited","containers":[{"name":"agent","exitCode":2}]}`, Failed, "Essential container in task exited; exit code 2"},
		{"deadline hit", `{"lastStatus":"STOPPED","containers":[{"name":"agent","exitCode":137},{"name":"kiln-deadline","exitCode":0}]}`, TimedOut, "hard deadline reached (enforced inside AWS)"},
		{"monitor deadline", `{"lastStatus":"STOPPED","containers":[{"name":"agent","exitCode":137},{"name":"kiln-monitor","exitCode":0}]}`, TimedOut, "hard deadline reached (enforced inside AWS)"},
		{"monitor stopped normally", `{"lastStatus":"STOPPED","containers":[{"name":"agent","exitCode":0},{"name":"kiln-monitor","exitCode":143}]}`, Succeeded, "exit code 0"},
		{"pull failed", `{"lastStatus":"STOPPED","stoppedReason":"CannotPullContainerError","containers":[{"name":"agent","reason":"image not found"}]}`, Failed, "CannotPullContainerError; image not found"},
	}
	for _, tt := range tests {
		st := task(tt.js).status()
		if st.State != tt.want || st.Reason != tt.reason {
			t.Errorf("%s: got %s %q, want %s %q", tt.name, st.State, st.Reason, tt.want, tt.reason)
		}
	}
	if d := task(tests[1].js).status().ImageDigest; d != "sha256:x" {
		t.Errorf("digest %q", d)
	}
}

func TestAWSConfigDefaults(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "name: a\nimage: x\nbackend: aws\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.AWS == nil || c.AWS.Region != "us-east-1" || c.AWS.Stack != "kiln" {
		t.Fatalf("aws defaults: %+v", c.AWS)
	}
	if _, err := LoadConfig(writeConfig(t, "name: a\nimage: x\naws: {stack: s}\n")); err == nil {
		t.Fatal("aws block with docker backend should be rejected")
	}
}

func TestMonitorCommandHasNoCR(t *testing.T) {
	td, err := (&AWS{Region: "us-east-1"}).taskDefinition(&Run{ID: "r", Config: Config{Name: "x", Image: "x",
		Budget: Budget{MaxUSD: 1, MaxHours: 1}, Resources: Resources{CPU: 0.25, MemoryGB: 0.5}}}, &awsInfra{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mon := td["containerDefinitions"].([]any)[1].(map[string]any)
	if cmd := mon["command"].([]string); strings.ContainsRune(cmd[2], 13) {
		t.Error("monitor script has carriage returns; sh in the task will fail")
	}
}
