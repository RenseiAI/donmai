package runner

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/codeintelcontract"
	"github.com/RenseiAI/donmai/prompt"
)

func nativePolicySelection() codeIntelDeliverySelection {
	return codeIntelDeliverySelection{Route: codeIntelDeliveryNative, Tools: []string{codeintelcontract.ToolGetRepoMap, codeintelcontract.ToolSearchSymbols}}
}

func TestTranslateSpecForCodeIntelDeliveryProjectsDefaultOrExplicitAuthority(t *testing.T) {
	t.Parallel()
	caps := agent.Capabilities{AcceptsAllowedToolsList: true, NeedsPermissionConfig: true}
	selection := nativePolicySelection()
	projected, err := translateSpecForCodeIntelDelivery(QueuedWork{}, caps, SpecInputs{Autonomous: true}, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := append(defaultAllowedTools(), codeintelcontract.ToolGetRepoMap, codeintelcontract.ToolSearchSymbols)
	if !reflect.DeepEqual(projected.AllowedTools, wantDefault) {
		t.Fatalf("default native allowlist=%v want=%v", projected.AllowedTools, wantDefault)
	}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{AllowedTools: []string{"Read", "Bash(git:*)"}}}
	explicit, err := translateSpecForCodeIntelDelivery(qw, caps, SpecInputs{Autonomous: true}, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(explicit.AllowedTools, qw.AllowedTools) {
		t.Fatalf("explicit allowlist widened: got=%v want=%v", explicit.AllowedTools, qw.AllowedTools)
	}
}

func TestTranslateSpecForCodeIntelDeliveryNormalizesAliasesAndRejectsTypos(t *testing.T) {
	t.Parallel()
	caps := agent.Capabilities{AcceptsAllowedToolsList: true}
	selection := nativePolicySelection()
	fq := "mcp__" + codeintelcontract.ServerName + "__" + codeintelcontract.ToolSearchSymbols
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{AllowedTools: []string{fq, "Read"}, DisallowedTools: []string{"Bash(env)", fq}}}
	spec, err := translateSpecForCodeIntelDelivery(qw, caps, SpecInputs{Autonomous: true}, selection, []string{"Read(/tmp/private)", fq})
	if err != nil {
		t.Fatal(err)
	}
	if spec.AllowedTools[0] != codeintelcontract.ToolSearchSymbols {
		t.Fatalf("allowed alias not normalized: %v", spec.AllowedTools)
	}
	wantTail := []string{"Bash(env)", codeintelcontract.ToolSearchSymbols, "Read(/tmp/private)", codeintelcontract.ToolSearchSymbols}
	if !reflect.DeepEqual(spec.DisallowedTools[len(spec.DisallowedTools)-len(wantTail):], wantTail) {
		t.Fatalf("disallow normalization/order=%v want tail=%v", spec.DisallowedTools, wantTail)
	}
	for _, raw := range []string{"mcp__other__" + codeintelcontract.ToolSearchSymbols, "af_code_unknown", "AF_CODE_SEARCH_SYMBOLS", " " + codeintelcontract.ToolSearchSymbols, codeintelcontract.ToolSearchSymbols + "(*)"} {
		bad := QueuedWork{QueuedWork: prompt.QueuedWork{AllowedTools: []string{raw}}}
		if _, err := translateSpecForCodeIntelDelivery(bad, caps, SpecInputs{Autonomous: true}, selection, nil); err == nil {
			t.Errorf("native policy pattern %q was accepted", raw)
		}
	}
}

func TestTranslateSpecForCodeIntelDeliveryPreservesLegacyMCPAndInteractive(t *testing.T) {
	t.Parallel()
	caps := agent.Capabilities{AcceptsAllowedToolsList: true}
	ci := &prompt.CodeIntelWork{Tools: []string{codeintelcontract.ToolGetRepoMap}}
	for _, test := range []struct {
		name      string
		qw        QueuedWork
		in        SpecInputs
		selection codeIntelDeliverySelection
	}{
		{name: "legacy", qw: QueuedWork{}, in: SpecInputs{Autonomous: true}, selection: codeIntelDeliverySelection{Route: codeIntelDeliveryLegacy}},
		{name: "mcp", qw: QueuedWork{QueuedWork: prompt.QueuedWork{CodeIntel: ci}}, in: SpecInputs{Autonomous: true}, selection: codeIntelDeliverySelection{Route: codeIntelDeliveryMCP}},
		{name: "interactive native remains fieldless", qw: QueuedWork{QueuedWork: prompt.QueuedWork{Mode: prompt.InteractiveRunMode, CodeIntel: ci}}, in: SpecInputs{}, selection: nativePolicySelection()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			legacyInput := test.in
			legacyInput.CodeIntelDeliveryRoute = test.selection.Route
			want := translateSpec(test.qw, caps, legacyInput)
			got, err := translateSpecForCodeIntelDelivery(test.qw, caps, test.in, test.selection, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("checked wrapper changed legacy Spec:\nwant=%+v\ngot=%+v", want, got)
			}
		})
	}
}

func TestTranslateSpecForCodeIntelDeliveryDetachesInputs(t *testing.T) {
	t.Parallel()
	fq := "mcp__" + codeintelcontract.ServerName + "__" + codeintelcontract.ToolSearchSymbols
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{AllowedTools: []string{fq}}}
	selection := nativePolicySelection()
	spec, err := translateSpecForCodeIntelDelivery(qw, agent.Capabilities{AcceptsAllowedToolsList: true}, SpecInputs{Autonomous: true}, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	qw.AllowedTools[0] = "mutated"
	selection.Tools[0] = "mutated"
	if spec.AllowedTools[0] != codeintelcontract.ToolSearchSymbols {
		t.Fatalf("Spec aliases queued work or selection: %v", spec.AllowedTools)
	}
	spec.AllowedTools[0] = "spec-mutated"
	if qw.AllowedTools[0] != "mutated" {
		t.Fatal("Spec mutation reached queued work")
	}
}

func TestRequirePreparedNativeCodeIntelPolicyUsesExactValueFreeComparison(t *testing.T) {
	t.Parallel()
	base := agent.Spec{AllowedTools: []string{"Read", codeintelcontract.ToolSearchSymbols}, DisallowedTools: []string{"Bash(env)"}, PermissionConfig: &agent.PermissionConfig{AllowPatterns: []string{"^af_code_"}, DisallowPatterns: []string{"unknown$"}, DefaultDecision: "deny"}}
	if err := requirePreparedNativeCodeIntelPolicy(base, base); err != nil {
		t.Fatalf("equal policy refused: %v", err)
	}
	mutations := []agent.Spec{
		{AllowedTools: []string{codeintelcontract.ToolSearchSymbols, "Read"}, DisallowedTools: base.DisallowedTools, PermissionConfig: base.PermissionConfig},
		{AllowedTools: base.AllowedTools, DisallowedTools: []string{"Bash(env)", "Bash(env)"}, PermissionConfig: base.PermissionConfig},
		{AllowedTools: base.AllowedTools, DisallowedTools: base.DisallowedTools, PermissionConfig: &agent.PermissionConfig{AllowPatterns: []string{"^af_code_"}, DisallowPatterns: []string{"unknown$"}, DefaultDecision: "allow"}},
	}
	for _, actual := range mutations {
		err := requirePreparedNativeCodeIntelPolicy(actual, base)
		var mismatch *nativeCodeIntelPolicyAuthorityMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("mismatch error=%T %v", err, err)
		}
		if strings.Contains(err.Error(), "Read") || strings.Contains(err.Error(), "Bash") || strings.Contains(err.Error(), "af_code") {
			t.Fatalf("mismatch error leaked policy value: %q", err)
		}
	}
	empty := base
	empty.AllowedTools = []string{}
	nilAllowed := base
	nilAllowed.AllowedTools = nil
	if err := requirePreparedNativeCodeIntelPolicy(empty, nilAllowed); err == nil {
		t.Fatal("nil and nonnil-empty policies compared equal")
	}
	snapshot := snapshotNativeCodeIntelPolicy(base)
	base.AllowedTools[0] = "mutated"
	base.PermissionConfig.AllowPatterns[0] = "mutated"
	if snapshot.AllowedTools[0] == "mutated" || snapshot.PermissionConfig.AllowPatterns[0] == "mutated" {
		t.Fatal("policy snapshot aliases source")
	}
}
