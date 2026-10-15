package ir

import (
	"strings"
	"testing"
)

func TestThinkingSpec_Off(t *testing.T) {
	if !(ThinkingSpec{}).IsOff() {
		t.Error("zero ThinkingSpec should be off")
	}
	if !(ThinkingSpec{Level: ThinkingOff}).IsOff() {
		t.Error("explicit off should be off")
	}
	if (ThinkingSpec{Level: ThinkingHigh}).IsOff() {
		t.Error("high should not be off")
	}
}

func TestEffortForOpenAI(t *testing.T) {
	cases := map[ThinkingLevel]string{
		ThinkingOff:     "",
		ThinkingMinimal: "minimal",
		ThinkingLow:     "low",
		ThinkingMedium:  "medium",
		ThinkingHigh:    "high",
		ThinkingMax:     "high", // OpenAI has no "max"; clamp
	}
	for level, want := range cases {
		if got := (ThinkingSpec{Level: level}).EffortForOpenAI(); got != want {
			t.Errorf("EffortForOpenAI(%q) = %q, want %q", level, got, want)
		}
	}
}

func TestBudgetForBudgetUpstream(t *testing.T) {
	if got := (ThinkingSpec{}).BudgetForBudgetUpstream(); got != 0 {
		t.Errorf("off budget = %d, want 0", got)
	}
	if got := (ThinkingSpec{Level: ThinkingHigh}).BudgetForBudgetUpstream(); got != 16384 {
		t.Errorf("high budget = %d, want 16384", got)
	}
	if got := (ThinkingSpec{Level: ThinkingMax}).BudgetForBudgetUpstream(); got != 32768 {
		t.Errorf("max budget = %d, want 32768", got)
	}
	explicit := 999
	if got := (ThinkingSpec{Level: ThinkingLow, BudgetTokens: &explicit}).BudgetForBudgetUpstream(); got != 999 {
		t.Errorf("explicit budget = %d, want 999", got)
	}
}

func TestLevelFromEffort(t *testing.T) {
	cases := map[string]ThinkingLevel{
		"minimal": ThinkingMinimal, "low": ThinkingLow, "medium": ThinkingMedium,
		"high": ThinkingHigh, "xhigh": ThinkingMax, "max": ThinkingMax,
		"": ThinkingOff, "off": ThinkingOff, "none": ThinkingOff,
	}
	for in, want := range cases {
		got, err := LevelFromEffort(in)
		if err != nil {
			t.Errorf("LevelFromEffort(%q) error = %v, want nil", in, err)
			continue
		}
		if got != want {
			t.Errorf("LevelFromEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLevelFromEffort_UnknownNamesValue(t *testing.T) {
	_, err := LevelFromEffort("bogus")
	if err == nil {
		t.Fatal("LevelFromEffort(bogus) = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q should name the rejected value", err.Error())
	}
}

func TestDocumentedEfforts_CoversLevelFromEffort(t *testing.T) {
	for _, effort := range DocumentedEfforts() {
		if _, err := LevelFromEffort(effort); err != nil {
			t.Errorf("DocumentedEfforts %q rejected: %v", effort, err)
		}
	}
}

func TestLevelFromBudget(t *testing.T) {
	cases := []struct {
		budget int
		want   ThinkingLevel
	}{
		{0, ThinkingOff},
		{-5, ThinkingOff},
		{500, ThinkingMinimal},
		{4096, ThinkingLow},
		{8000, ThinkingMedium},
		{16000, ThinkingHigh},
		{100000, ThinkingMax},
	}
	for _, c := range cases {
		if got := LevelFromBudget(c.budget); got != c.want {
			t.Errorf("LevelFromBudget(%d) = %q, want %q", c.budget, got, c.want)
		}
	}
}
