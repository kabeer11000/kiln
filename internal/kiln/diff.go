package kiln

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"
)

type Change struct {
	Field string `json:"field"`
	A     any    `json:"a"`
	B     any    `json:"b"`
}

// Diff lists what differs between two runs: inputs (config, image, git) and outcome.
func Diff(a, b *Run, now time.Time) []Change {
	fa, fb := flatten(diffView(a, now)), flatten(diffView(b, now))
	var out []Change
	for _, k := range slices.Sorted(maps.Keys(merge(fa, fb))) {
		if fmt.Sprint(fa[k]) != fmt.Sprint(fb[k]) {
			out = append(out, Change{Field: k, A: fa[k], B: fb[k]})
		}
	}
	return out
}

func diffView(r *Run, now time.Time) map[string]any {
	v := map[string]any{
		"config": r.Config, "image_digest": r.ImageDigest, "git_sha": r.GitSHA, "git_dirty": r.GitDirty,
		"state": r.State, "reason": r.Reason, "spent_usd": fmt.Sprintf("%.4f", r.SpentUSD),
		"duration": r.Duration(now).Round(time.Second).String(),
	}
	if r.Inputs != nil {
		v["inputs_sha256"] = r.Inputs.SHA256
	}
	// Round-trip through JSON so nested structs become maps with their json field names.
	data, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}

func flatten(m map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if mm, ok := v.(map[string]any); ok && len(mm) > 0 {
			for k, vv := range mm {
				if prefix != "" {
					k = prefix + "." + k
				}
				walk(k, vv)
			}
			return
		}
		out[prefix] = v
	}
	walk("", m)
	return out
}

func merge(a, b map[string]any) map[string]any {
	m := maps.Clone(a)
	maps.Copy(m, b)
	return m
}
