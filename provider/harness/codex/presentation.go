package codex

import (
	"encoding/json"
	"sort"
	"strings"
)

// fileUpdateChange is one entry of an app-server fileChange item. `kind` is a
// tagged object (`{"type":"update","move_path":…}`) on current app-server
// versions and a bare string on older ones; both decode.
type fileUpdateChange struct {
	Path string          `json:"path"`
	Kind json.RawMessage `json:"kind"`
	Diff string          `json:"diff"`
}

// kind returns the change kind and the rename target, if any.
func (c fileUpdateChange) kind() (kind, movePath string) {
	var s string
	if json.Unmarshal(c.Kind, &s) == nil {
		return s, ""
	}
	var obj struct {
		Type     string `json:"type"`
		MovePath string `json:"move_path"`
	}
	if json.Unmarshal(c.Kind, &obj) == nil {
		return obj.Type, obj.MovePath
	}
	return "", ""
}

// fileChangeContent renders the change list as one "kind: path" line per
// file ("update: a -> b" for a rename), then each update's unified diff
// under a ---/+++ header so renderers can detect and tint it.
func fileChangeContent(changes []fileUpdateChange) string {
	var lines, diffs []string
	for _, ch := range changes {
		kind, move := ch.kind()
		if kind == "" {
			kind = "change"
		}
		line := kind + ": " + ch.Path
		if move != "" {
			line += " -> " + move
		}
		lines = append(lines, line)
		if kind == "update" && strings.HasPrefix(strings.TrimSpace(ch.Diff), "@@") {
			target := ch.Path
			if move != "" {
				target = move
			}
			diffs = append(diffs, "--- "+ch.Path+"\n+++ "+target+"\n"+strings.TrimRight(ch.Diff, "\n"))
		}
	}
	out := strings.Join(lines, "\n")
	if len(diffs) > 0 {
		out += "\n\n" + strings.Join(diffs, "\n")
	}
	return out
}

// moderationBlocks lists the classifiers that blocked the turn, as
// "stage/classifier" (for example "prompt/cyber_security"), sorted. The
// metadata maps stage → classifier → {is_blocked | outputs[].is_blocked}.
func moderationBlocks(metadata json.RawMessage) []string {
	var stages map[string]map[string]json.RawMessage
	if json.Unmarshal(metadata, &stages) != nil {
		return nil
	}
	var out []string
	for stage, classifiers := range stages {
		for name, raw := range classifiers {
			var c struct {
				IsBlocked bool `json:"is_blocked"`
				Outputs   []struct {
					IsBlocked bool `json:"is_blocked"`
				} `json:"outputs"`
			}
			if json.Unmarshal(raw, &c) != nil {
				continue
			}
			blocked := c.IsBlocked
			for _, o := range c.Outputs {
				blocked = blocked || o.IsBlocked
			}
			if blocked {
				out = append(out, stage+"/"+name)
			}
		}
	}
	sort.Strings(out)
	return out
}
