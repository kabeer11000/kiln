package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/kabeer11000/kiln/internal/kiln"
)

// cmdMCP serves the Model Context Protocol over stdio (newline-delimited JSON-RPC 2.0), so an
// agent (any MCP client) can start, watch and talk to runs as native tools. Each tool
// runs the same handler as the CLI command with --json.
func (a *app) cmdMCP(args []string) (int, error) {
	if _, err := a.parse("mcp", args, 0); err != nil {
		return 2, err
	}
	return 0, a.serveMCP(os.Stdin, os.Stdout)
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	argv        func(map[string]any) []string
	handler     func(*app, []string) (int, error)
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func obj(required []string, props map[string]any) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var (
	pRun  = map[string]any{"type": "string", "description": "Run id (r_...), unique id prefix, or run name"}
	pFile = map[string]any{"type": "string", "description": "Path to kiln.yaml (default: ./kiln.yaml of the server's folder). Absolute paths are safest."}
)

func withFile(argv []string, m map[string]any) []string {
	if f := str(m, "file"); f != "" {
		argv = append(argv, "-f", f)
	}
	return argv
}

func mcpTools() []mcpTool {
	runOnly := obj([]string{"run"}, map[string]any{"run": pRun})
	one := func(m map[string]any) []string { return []string{str(m, "run")} }
	return []mcpTool{
		{Name: "kiln_validate", Description: "Check a kiln.yaml and return it with defaults applied. Call kiln_schema first if unsure of fields.",
			InputSchema: obj(nil, map[string]any{"file": pFile}), argv: func(m map[string]any) []string { return withFile(nil, m) }, handler: (*app).cmdValidate},
		{Name: "kiln_plan", Description: "Price per hour and worst-case spend for a kiln.yaml, without starting anything.",
			InputSchema: obj(nil, map[string]any{"file": pFile}), argv: func(m map[string]any) []string { return withFile([]string{"--plan"}, m) }, handler: (*app).cmdRun},
		{Name: "kiln_run", Description: "Start a run in the background and return its id at once. Hard caps (budget.max_usd, budget.max_hours) always apply. Follow up with kiln_wait, kiln_top or kiln_status.",
			InputSchema: obj(nil, map[string]any{"file": pFile}), argv: func(m map[string]any) []string { return withFile([]string{"-d"}, m) }, handler: (*app).cmdRun},
		{Name: "kiln_list", Description: "All runs on this machine, newest first.",
			InputSchema: obj(nil, map[string]any{}), argv: func(map[string]any) []string { return nil }, handler: (*app).cmdList},
		{Name: "kiln_status", Description: "One run: state, spend so far, reason it ended, events.",
			InputSchema: runOnly, argv: one, handler: (*app).cmdStatus},
		{Name: "kiln_wait", Description: "Block until a run ends (or timeout_seconds passes, default 600) and return its final record. state is one of succeeded, failed, cancelled, budget_exceeded, timed_out.",
			InputSchema: obj([]string{"run"}, map[string]any{"run": pRun, "timeout_seconds": map[string]any{"type": "number", "description": "Default 600"}}),
			argv: func(m map[string]any) []string {
				t, _ := m["timeout_seconds"].(float64)
				if t <= 0 {
					t = 600
				}
				return []string{str(m, "run"), "--timeout", (time.Duration(t) * time.Second).String()}
			}, handler: (*app).cmdWait},
		{Name: "kiln_top", Description: "Inside a run (aws): every process with CPU% and memory, peak memory per process, memory per container, network, open connections, disk, artifact files, and events (agent messages, crashes).",
			InputSchema: runOnly, argv: one, handler: (*app).cmdTop},
		{Name: "kiln_logs", Description: "The run's container output (stdout and stderr). Long output is cut to the last part.",
			InputSchema: runOnly, argv: one, handler: (*app).cmdLogs},
		{Name: "kiln_send", Description: "Send a message to the running agent. It appears as a JSON line {\"t\", \"msg\"} in /kiln/inbox.jsonl inside the task within seconds.",
			InputSchema: obj([]string{"run", "message"}, map[string]any{"run": pRun, "message": map[string]any{"type": "string"}}),
			argv:        func(m map[string]any) []string { return []string{str(m, "run"), str(m, "message")} }, handler: (*app).cmdSend},
		{Name: "kiln_cancel", Description: "Stop a run. Spend stops within seconds.",
			InputSchema: runOnly, argv: one, handler: (*app).cmdCancel},
		{Name: "kiln_artifacts", Description: "Download what the run left in /kiln/artifacts (plus its inbox/outbox) and return the local folder.",
			InputSchema: obj([]string{"run"}, map[string]any{"run": pRun, "dir": map[string]any{"type": "string", "description": "Where to unpack (default: next to the run record)"}}),
			argv: func(m map[string]any) []string {
				argv := []string{str(m, "run")}
				if d := str(m, "dir"); d != "" {
					argv = append(argv, "-o", d)
				}
				return argv
			}, handler: (*app).cmdArtifacts},
		{Name: "kiln_rerun", Description: "Start a new run that reproduces an old one: same config, exact image digest, byte-identical inputs. Returns the new id.",
			InputSchema: runOnly, argv: func(m map[string]any) []string { return []string{"-d", str(m, "run")} }, handler: (*app).cmdRerun},
		{Name: "kiln_diff", Description: "What differs between two runs: config, image, inputs, outcome, cost.",
			InputSchema: obj([]string{"a", "b"}, map[string]any{"a": pRun, "b": pRun}),
			argv:        func(m map[string]any) []string { return []string{str(m, "a"), str(m, "b")} }, handler: (*app).cmdDiff},
		{Name: "kiln_schema", Description: "JSON Schema of kiln.yaml: every field, default and limit.",
			InputSchema: obj(nil, map[string]any{}), argv: func(map[string]any) []string { return nil }, handler: (*app).cmdSchema},
	}
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// maxToolOutput keeps tool results small enough for an agent's context. The tail is kept:
// for logs, the end is what matters.
const maxToolOutput = 60_000

func (a *app) serveMCP(in io.Reader, out io.Writer) error {
	tools := mcpTools()
	byName := map[string]mcpTool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	enc := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any) error {
		return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	replyErr := func(id json.RawMessage, code int, msg string) error {
		return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var m rpcMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			if err := replyErr(json.RawMessage("null"), -32700, "parse error"); err != nil {
				return err
			}
			continue
		}
		if m.ID == nil { // notification (e.g. notifications/initialized): no reply
			continue
		}
		var err error
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			err = reply(m.ID, map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "kiln", "version": version},
				"instructions": "kiln runs containers on AWS or locally with hard dollar and time caps. Typical flow: kiln_schema -> write kiln.yaml -> " +
					"kiln_plan -> kiln_run -> kiln_wait (or kiln_top to watch) -> kiln_artifacts. Inside the task: /kiln/inputs (files in), " +
					"/kiln/artifacts (files out), /kiln/outbox.jsonl (messages out), /kiln/inbox.jsonl (messages in).",
			})
		case "ping":
			err = reply(m.ID, map[string]any{})
		case "tools/list":
			err = reply(m.ID, map[string]any{"tools": tools})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if json.Unmarshal(m.Params, &p) != nil {
				err = replyErr(m.ID, -32602, "invalid params")
				break
			}
			t, ok := byName[p.Name]
			if !ok {
				err = replyErr(m.ID, -32602, "unknown tool "+p.Name)
				break
			}
			text, isErr := a.callTool(t, p.Arguments)
			err = reply(m.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr})
		default:
			err = replyErr(m.ID, -32601, "method not found: "+m.Method)
		}
		if err != nil {
			return err
		}
	}
	return sc.Err()
}

// callTool runs one CLI handler with --json and returns its output. Errors come back as the
// usual {"error": {code, message, fix}} so the agent sees how to fix them.
func (a *app) callTool(t mcpTool, args map[string]any) (string, bool) {
	if args == nil {
		args = map[string]any{}
	}
	var buf bytes.Buffer
	sub := &app{out: &buf, json: true, store: a.store, ctx: a.ctx}
	_, err := t.handler(sub, t.argv(args))
	if err != nil {
		buf.Reset()
		sub.fail(err, 1)
	}
	text := buf.String()
	if len(text) > maxToolOutput {
		text = fmt.Sprintf("[first %d bytes cut]\n", len(text)-maxToolOutput) + text[len(text)-maxToolOutput:]
	}
	return text, err != nil
}

func (a *app) cmdSchema(args []string) (int, error) {
	if _, err := a.parse("schema", args, 0); err != nil {
		return 2, err
	}
	fmt.Fprint(a.out, kiln.Schema)
	return 0, nil
}
