package codex

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
)

// Access-program identifiers reported by ProbeAccessPrograms.
const (
	AccessProgramDaybreak = "daybreak"
	AccessProgramTierBlue = "blue"
	AccessProgramTierRed  = "red"
)

// AccessProgramState is the probe's verdict for one group of models.
type AccessProgramState string

// AccessProgramState values.
const (
	AccessProgramEnabled    AccessProgramState = "enabled"
	AccessProgramNotEnabled AccessProgramState = "notEnabled"
	AccessProgramUnknown    AccessProgramState = "unknown"
)

// AccessProgramGroup lists the allowlisted models that share one program
// state and tier.
type AccessProgramGroup struct {
	Program string             `json:"program"`
	State   AccessProgramState `json:"state"`
	Tier    string             `json:"tier,omitempty"`
	Models  []string           `json:"models"`
}

var (
	errAccessProgramScan  = errors.New("codex: access program scan did not complete")
	builtInModelAllowlist = map[string]bool{"gpt-6.2-sol": true, "gpt-6.2-astra": true, "gpt-6-astra": true, "gpt-6.1-sol": true, "gpt-6.1-terra": true, "gpt-6-sol": true, "gpt-6-luna": true, "gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true, "gpt-daybreak-blue-latest": true, "gpt-daybreak-red-latest": true, "gpt-5.5": true, "codex-auto-review": true}
)

// ProbeAccessPrograms scans the app-server model catalog (model/list) and
// groups allowlisted models by their access-program state. Any incomplete or
// malformed scan returns errAccessProgramScan and no groups.
func (p *Provider) ProbeAccessPrograms(ctx context.Context) ([]AccessProgramGroup, error) {
	if p == nil || ctx.Err() != nil {
		return nil, errAccessProgramScan
	}
	if err := p.ensureProbeStarted(ctx); err != nil {
		return nil, errAccessProgramScan
	}
	p.startMu.Lock()
	cl, rt := p.client, p.opts.RPCTimeout
	p.startMu.Unlock()
	seen, obs := map[string]bool{}, map[string]*modelObservation{}
	var cur string
	for pg := 0; pg < 64; pg++ {
		if ctx.Err() != nil {
			return nil, errAccessProgramScan
		}
		pm := map[string]any{"modelProvider": "openai", "includeHidden": true}
		if cur != "" {
			pm["cursor"] = cur
		}
		raw, err := cl.Request(ctx, "model/list", pm, rt)
		if err != nil {
			return nil, errAccessProgramScan
		}
		es, nx, ok := parseModelListPage(raw)
		if !ok {
			return nil, errAccessProgramScan
		}
		for _, e := range es {
			if e == nil || e.m == "" {
				return nil, errAccessProgramScan
			}
			if !builtInModelAllowlist[e.m] {
				continue
			}
			o := obs[e.m]
			if o == nil {
				o = &modelObservation{}
				obs[e.m] = o
			}
			o.add(e.a, e.cy, e.uf)
		}
		if nx == nil {
			break
		}
		if *nx == "" || seen[*nx] || pg+1 >= 64 {
			return nil, errAccessProgramScan
		}
		seen[*nx] = true
		cur = *nx
	}
	if ctx.Err() != nil {
		return nil, errAccessProgramScan
	}
	return groupAccessObservations(obs), nil
}

type modelObservation struct {
	s, c, u bool
	a       string
	cy      []string
}

func (o *modelObservation) add(a string, c []string, u bool) {
	if u {
		o.u = true
	}
	n := append([]string(nil), c...)
	sort.Strings(n)
	if !o.s {
		o.s, o.a, o.cy = true, a, n
		return
	}
	if o.a != a || !slices.Equal(o.cy, n) {
		o.c = true
	}
}

type modelEntry struct {
	m, a string
	cy   []string
	uf   bool
}

func parseModelListPage(raw json.RawMessage) ([]*modelEntry, *string, bool) {
	var top map[string]any
	if json.Unmarshal(raw, &top) != nil {
		return nil, nil, false
	}
	d, ok := top["data"].([]any)
	if !ok {
		return nil, nil, false
	}
	var nx *string
	if v, ok := top["nextCursor"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return nil, nil, false
		}
		nx = &s
	}
	out := make([]*modelEntry, 0, len(d))
	for _, it := range d {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, nil, false
		}
		ms, ok := m["model"].(string)
		if !ok {
			return nil, nil, false
		}
		e := &modelEntry{m: ms, a: "default"}
		ap, has := m["availableAccessPrograms"]
		if !has || ap == nil {
			e.a = ""
			out = append(out, e)
			continue
		}
		am, ok := ap.(map[string]any)
		if !ok {
			return nil, nil, false
		}
		if av, ok := am["daybreakAvailability"]; ok && av != nil {
			s, ok := av.(string)
			if !ok {
				e.uf = true
			} else {
				switch s {
				case "available", "enrollmentRequired", "unavailableForModel", "default":
					e.a = s
				default:
					e.uf = true
				}
			}
		}
		if cy, ok := am["cyber"]; ok && cy != nil {
			arr, ok := cy.([]any)
			if !ok {
				e.uf = true
			} else {
				se := map[string]bool{}
				for _, c := range arr {
					s, ok := c.(string)
					if !ok {
						e.uf = true
						continue
					}
					switch s {
					case "standard", "daybreakBlue", "daybreakRed":
						se[s] = true
					default:
						e.uf = true
					}
				}
				for s := range se {
					e.cy = append(e.cy, s)
				}
				sort.Strings(e.cy)
			}
		}
		out = append(out, e)
	}
	return out, nx, true
}

func groupAccessObservations(obs map[string]*modelObservation) []AccessProgramGroup {
	var bl, rd, ne, un []string
	for m, o := range obs {
		if o == nil || !o.s || o.a == "" {
			if o != nil && o.s && (o.u || o.c) {
				un = append(un, m)
			}
			continue
		}
		if o.u || o.c {
			un = append(un, m)
			continue
		}
		b := slices.Contains(o.cy, "daybreakBlue")
		r := slices.Contains(o.cy, "daybreakRed")
		switch o.a {
		case "unavailableForModel":
			un = append(un, m)
		case "enrollmentRequired":
			if b || r {
				un = append(un, m)
			} else {
				ne = append(ne, m)
			}
		case "available":
			if !b && !r {
				un = append(un, m)
			} else {
				if b {
					bl = append(bl, m)
				}
				if r {
					rd = append(rd, m)
				}
			}
		default:
			if b {
				bl = append(bl, m)
			}
			if r {
				rd = append(rd, m)
			}
		}
	}
	var g []AccessProgramGroup
	add := func(s AccessProgramState, t string, ms []string) {
		if len(ms) == 0 {
			return
		}
		sort.Strings(ms)
		g = append(g, AccessProgramGroup{Program: AccessProgramDaybreak, State: s, Tier: t, Models: ms})
	}
	add(AccessProgramEnabled, AccessProgramTierBlue, bl)
	add(AccessProgramEnabled, AccessProgramTierRed, rd)
	add(AccessProgramNotEnabled, "", ne)
	add(AccessProgramUnknown, "", un)
	sort.Slice(g, func(i, j int) bool {
		if g[i].State != g[j].State {
			return g[i].State < g[j].State
		}
		return g[i].Tier < g[j].Tier
	})
	return g
}
