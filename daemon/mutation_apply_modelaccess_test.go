package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runner/access"
	"gopkg.in/yaml.v3"
)

// readYamlModelAccess reads the persisted daemon.yaml and returns its
// ModelAccess block (nil when absent). Used to verify the atomic write of
// the modelAccess.set / modelAccess.clear mutations actually landed on disk.
func readYamlModelAccess(t *testing.T, path string) *access.ModelAccessConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	var parsed Config
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}
	return parsed.ModelAccess
}

// anthropicHostSessionPolicy is a small reusable AccessPolicy: allow
// host-session, deny byok on anthropic.
func anthropicHostSessionPolicy() access.AccessPolicy {
	return access.AccessPolicy{
		Matrix: map[string]map[string]access.AccessCostCell{
			"anthropic": {
				string(agent.AuthHostSession): {Allowed: true, Host: "oauth-cli"},
				string(agent.AuthBYOK):        {Allowed: false},
			},
		},
	}
}

func TestApplyPendingMutations_ModelAccessSet_Default(t *testing.T) {
	t.Parallel()
	d, path := newTestDaemonWithProjects(t, nil)

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{
			ID: "dmut_ma_set_default",
			Op: "modelAccess.set",
			Params: mustParams(t, map[string]any{
				"policy": anthropicHostSessionPolicy(),
			}),
		},
	})
	if len(applied) != 1 || applied[0] != "dmut_ma_set_default" {
		t.Fatalf("applied = %v, want [dmut_ma_set_default]", applied)
	}
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}

	// In-memory.
	if d.config.ModelAccess == nil {
		t.Fatalf("in-memory ModelAccess is nil after set")
	}
	cell := d.config.ModelAccess.Default.Matrix["anthropic"][string(agent.AuthHostSession)]
	if !cell.Allowed || cell.Host != "oauth-cli" {
		t.Errorf("Default anthropic host-session cell = %+v, want {allowed,oauth-cli}", cell)
	}

	// On-disk (atomic write).
	got := readYamlModelAccess(t, path)
	if got == nil {
		t.Fatalf("yaml ModelAccess is nil after set")
	}
	if denied := got.Default.Matrix["anthropic"][string(agent.AuthBYOK)]; denied.Allowed {
		t.Errorf("Default anthropic byok cell = %+v, want allowed:false persisted", denied)
	}
}

func TestApplyPendingMutations_ModelAccessSet_Workload(t *testing.T) {
	t.Parallel()
	d, path := newTestDaemonWithProjects(t, nil)

	kgPolicy := access.AccessPolicy{
		AuthOrder: []access.AuthMode{agent.AuthBYOK, agent.AuthMetered},
		Matrix: map[string]map[string]access.AccessCostCell{
			"anthropic": {
				string(agent.AuthBYOK):        {Allowed: true, Host: "vertex", Model: "claude-haiku"},
				string(agent.AuthHostSession): {Allowed: false},
			},
		},
	}

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{
			ID: "dmut_ma_set_kg",
			Op: "modelAccess.set",
			Params: mustParams(t, map[string]any{
				"workload": "kg-extraction",
				"policy":   kgPolicy,
			}),
		},
	})
	if len(applied) != 1 || len(failures) != 0 {
		t.Fatalf("applied/failures = %v/%v, want [dmut_ma_set_kg]/[]", applied, failures)
	}

	got := readYamlModelAccess(t, path)
	if got == nil || got.Workloads == nil {
		t.Fatalf("yaml ModelAccess.Workloads nil after workload set: %+v", got)
	}
	pol, ok := got.Workloads["kg-extraction"]
	if !ok {
		t.Fatalf("workload kg-extraction absent after set")
	}
	cell := pol.Matrix["anthropic"][string(agent.AuthBYOK)]
	if !cell.Allowed || cell.Host != "vertex" || cell.Model != "claude-haiku" {
		t.Errorf("kg byok cell = %+v, want {allowed,vertex,claude-haiku}", cell)
	}
	if len(pol.AuthOrder) != 2 || pol.AuthOrder[0] != agent.AuthBYOK {
		t.Errorf("kg authOrder = %v, want [byok metered]", pol.AuthOrder)
	}
	// Default block stays untouched — the workload set must not clobber the
	// default narrowing baseline. (The YAML decoder may materialize an empty
	// matrix map; the load-bearing property is that it carries NO rules.)
	if len(got.Default.Matrix) != 0 || len(got.Default.AuthOrder) != 0 {
		t.Errorf("Default policy = %+v, want empty (untouched by workload set)", got.Default)
	}
}

func TestApplyPendingMutations_ModelAccessSet_Overwrite(t *testing.T) {
	t.Parallel()
	d, _ := newTestDaemonWithProjects(t, nil)

	first := mustParams(t, map[string]any{"policy": anthropicHostSessionPolicy()})
	if _, f := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "set1", Op: "modelAccess.set", Params: first},
	}); len(f) != 0 {
		t.Fatalf("first set failed: %v", f)
	}

	// Overwrite the default with a different (allow-byok) policy — idempotent
	// overwrite, last writer wins.
	second := mustParams(t, map[string]any{
		"policy": access.AccessPolicy{
			Matrix: map[string]map[string]access.AccessCostCell{
				"anthropic": {string(agent.AuthBYOK): {Allowed: true}},
			},
		},
	})
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "set2", Op: "modelAccess.set", Params: second},
	})
	if len(applied) != 1 || len(failures) != 0 {
		t.Fatalf("overwrite applied/failures = %v/%v", applied, failures)
	}
	if _, hasHostSession := d.config.ModelAccess.Default.Matrix["anthropic"][string(agent.AuthHostSession)]; hasHostSession {
		t.Errorf("overwrite kept stale host-session cell; want full replacement")
	}
	if cell := d.config.ModelAccess.Default.Matrix["anthropic"][string(agent.AuthBYOK)]; !cell.Allowed {
		t.Errorf("overwrite byok cell = %+v, want allowed:true", cell)
	}
}

func TestApplyPendingMutations_ModelAccessClear_Workload(t *testing.T) {
	t.Parallel()
	d, path := newTestDaemonWithProjects(t, nil)
	d.config.ModelAccess = &access.ModelAccessConfig{
		Default: anthropicHostSessionPolicy(),
		Workloads: map[string]access.AccessPolicy{
			"kg-extraction": {Matrix: map[string]map[string]access.AccessCostCell{
				"anthropic": {string(agent.AuthBYOK): {Allowed: true}},
			}},
		},
	}
	if err := WriteConfig(path, d.config); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "clr_kg", Op: "modelAccess.clear", Params: mustParams(t, map[string]string{"workload": "kg-extraction"})},
	})
	if len(applied) != 1 || len(failures) != 0 {
		t.Fatalf("clear applied/failures = %v/%v", applied, failures)
	}

	got := readYamlModelAccess(t, path)
	if got == nil {
		t.Fatalf("ModelAccess block wholly removed by workload clear; want default retained")
	}
	if _, ok := got.Workloads["kg-extraction"]; ok {
		t.Errorf("kg-extraction still present after clear")
	}
	// Default must survive a workload-scoped clear.
	if cell := got.Default.Matrix["anthropic"][string(agent.AuthHostSession)]; !cell.Allowed {
		t.Errorf("Default lost on workload clear: %+v", cell)
	}
}

func TestApplyPendingMutations_ModelAccessClear_WholeBlock(t *testing.T) {
	t.Parallel()
	d, path := newTestDaemonWithProjects(t, nil)
	d.config.ModelAccess = &access.ModelAccessConfig{Default: anthropicHostSessionPolicy()}
	if err := WriteConfig(path, d.config); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}

	// Empty workload => clear the whole block (revert to nil-block identity).
	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "clr_all", Op: "modelAccess.clear", Params: mustParams(t, map[string]string{"workload": ""})},
	})
	if len(applied) != 1 || len(failures) != 0 {
		t.Fatalf("whole-block clear applied/failures = %v/%v", applied, failures)
	}
	if d.config.ModelAccess != nil {
		t.Errorf("in-memory ModelAccess = %+v, want nil after whole-block clear", d.config.ModelAccess)
	}
	if got := readYamlModelAccess(t, path); got != nil {
		t.Errorf("yaml ModelAccess = %+v, want nil after whole-block clear", got)
	}
}

func TestApplyPendingMutations_ModelAccessClear_Absent(t *testing.T) {
	t.Parallel()
	// Clear-of-absent on a daemon that never had a ModelAccess block: no-op
	// success (idempotent), mirroring project.remove-not-present.
	d, _ := newTestDaemonWithProjects(t, nil)

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "clr_ghost", Op: "modelAccess.clear", Params: mustParams(t, map[string]string{"workload": "never-existed"})},
	})
	if len(applied) != 1 || applied[0] != "clr_ghost" {
		t.Fatalf("applied = %v, want [clr_ghost] (no-op success)", applied)
	}
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none on clear-of-absent", failures)
	}
	if d.config.ModelAccess != nil {
		t.Errorf("clear-of-absent created a block: %+v", d.config.ModelAccess)
	}

	// Also: clear a missing workload when a block exists but lacks it.
	d.config.ModelAccess = &access.ModelAccessConfig{Default: anthropicHostSessionPolicy()}
	applied, failures = d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "clr_ghost2", Op: "modelAccess.clear", Params: mustParams(t, map[string]string{"workload": "ghost"})},
	})
	if len(applied) != 1 || len(failures) != 0 {
		t.Fatalf("clear-of-absent-workload applied/failures = %v/%v", applied, failures)
	}
	if d.config.ModelAccess == nil {
		t.Errorf("clear-of-absent-workload removed the existing default block")
	}
}

func TestApplyPendingMutations_ModelAccessSet_BadParams(t *testing.T) {
	t.Parallel()
	d, _ := newTestDaemonWithProjects(t, nil)

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "bad_json", Op: "modelAccess.set", Params: json.RawMessage(`{"policy": "not-an-object"}`)},
	})
	if len(applied) != 0 {
		t.Errorf("applied = %v, want none on bad params", applied)
	}
	if len(failures) != 1 || failures[0].ID != "bad_json" {
		t.Fatalf("failures = %v, want one for bad_json", failures)
	}
	if !strings.Contains(failures[0].Error, "decode params") {
		t.Errorf("failure.Error = %q, want decode-params message", failures[0].Error)
	}
}

func TestApplyPendingMutations_ModelAccess_UnknownOp(t *testing.T) {
	t.Parallel()
	// A modelAccess op the daemon doesn't recognise keeps the existing
	// fail-as-failed default (older daemon vs newer platform).
	d, _ := newTestDaemonWithProjects(t, nil)

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "ma_future", Op: "modelAccess.merge", Params: json.RawMessage(`{}`)},
	})
	if len(applied) != 0 {
		t.Errorf("applied = %v, want none for unknown op", applied)
	}
	if len(failures) != 1 || failures[0].ID != "ma_future" {
		t.Fatalf("failures = %v, want one for ma_future", failures)
	}
	if !strings.Contains(failures[0].Error, "unsupported mutation op") {
		t.Errorf("failure.Error = %q, want unsupported-op message", failures[0].Error)
	}
}

func TestApplyPendingMutations_ModelAccessSet_UnknownWorkloadRejected(t *testing.T) {
	t.Parallel()
	// A workload key outside the shared vocabulary (e.g. a typo'd
	// "develop-ment") can never match at enforcement time — the strict block
	// would be stored but silently fall back to the ceiling. The mutation must
	// fail (NACK) and leave both memory and disk untouched.
	d, path := newTestDaemonWithProjects(t, nil)

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{
			ID: "set_typo",
			Op: "modelAccess.set",
			Params: mustParams(t, map[string]any{
				"workload": "develop-ment",
				"policy":   anthropicHostSessionPolicy(),
			}),
		},
	})
	if len(applied) != 0 {
		t.Errorf("applied = %v, want none for unknown workload", applied)
	}
	if len(failures) != 1 || failures[0].ID != "set_typo" {
		t.Fatalf("failures = %v, want one for set_typo", failures)
	}
	if !strings.Contains(failures[0].Error, `unknown workload "develop-ment"`) {
		t.Errorf("failure.Error = %q, want unknown-workload message", failures[0].Error)
	}
	// The error must teach the fix: list the valid vocabulary.
	if !strings.Contains(failures[0].Error, "development") {
		t.Errorf("failure.Error = %q, want the known-keys list included", failures[0].Error)
	}
	if d.config.ModelAccess != nil {
		t.Errorf("in-memory ModelAccess = %+v, want nil after rejected set", d.config.ModelAccess)
	}
	if got := readYamlModelAccess(t, path); got != nil {
		t.Errorf("yaml ModelAccess = %+v, want nothing persisted after rejected set", got)
	}
}

func TestApplyPendingMutations_ModelAccessSet_WorkloadVocabulary(t *testing.T) {
	t.Parallel()
	// Pin the breadth of the accepted vocabulary: agent work types, the
	// non-agent batch work types, and the mode-derived interview workload.
	keys := []string{"development", "qa", "code-survival-scan", "kg-extraction", "interview"}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			d, path := newTestDaemonWithProjects(t, nil)
			applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
				{
					ID: "set_" + key,
					Op: "modelAccess.set",
					Params: mustParams(t, map[string]any{
						"workload": key,
						"policy":   anthropicHostSessionPolicy(),
					}),
				},
			})
			if len(applied) != 1 || len(failures) != 0 {
				t.Fatalf("applied/failures = %v/%v, want [set_%s]/[]", applied, failures, key)
			}
			got := readYamlModelAccess(t, path)
			if got == nil || got.Workloads == nil {
				t.Fatalf("yaml ModelAccess.Workloads nil after set: %+v", got)
			}
			if _, ok := got.Workloads[key]; !ok {
				t.Errorf("workload %q absent after accepted set", key)
			}
		})
	}
}
