package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kabeer11000/kiln/internal/kiln"
)

func TestMCP(t *testing.T) {
	a := &app{store: kiln.Store{Dir: t.TempDir()}}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"kiln_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"kiln_status","arguments":{"run":"r_nope"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"kiln_schema"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"nope"}`,
		`not json`,
	}, "\n")
	var out bytes.Buffer
	if err := a.serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var resp []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		resp = append(resp, m)
	}
	if len(resp) != 7 { // 6 requests with ids + 1 parse error; the notification gets no reply
		t.Fatalf("got %d replies: %v", len(resp), resp)
	}
	result := func(i int) map[string]any { r, _ := resp[i]["result"].(map[string]any); return r }
	text := func(i int) string {
		c := result(i)["content"].([]any)[0].(map[string]any)
		return c["text"].(string)
	}
	if v := result(0)["protocolVersion"]; v != "2025-06-18" {
		t.Errorf("initialize: %v", resp[0])
	}
	tools := result(1)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		tm := tl.(map[string]any)
		names[tm["name"].(string)] = true
		if tm["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("%s: inputSchema must be an object schema", tm["name"])
		}
	}
	for _, want := range []string{"kiln_run", "kiln_wait", "kiln_top", "kiln_send", "kiln_cancel", "kiln_artifacts", "kiln_rerun", "kiln_plan"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
	if strings.TrimSpace(text(2)) != "[]" || result(2)["isError"] != false {
		t.Errorf("kiln_list on an empty store: %q", text(2))
	}
	if result(3)["isError"] != true || !strings.Contains(text(3), `"run_not_found"`) || !strings.Contains(text(3), `"fix"`) {
		t.Errorf("errors must come back as tool errors with a fix: %v", resp[3])
	}
	if !strings.Contains(text(4), `"$schema"`) {
		t.Errorf("kiln_schema: %.100s", text(4))
	}
	if resp[5]["error"].(map[string]any)["code"].(float64) != -32601 || resp[6]["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Errorf("unknown method / parse error: %v %v", resp[5], resp[6])
	}
}
