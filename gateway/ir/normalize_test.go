package ir

import "testing"

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
	explicit := 999
	if got := (ThinkingSpec{Level: ThinkingLow, BudgetTokens: &explicit}).BudgetForBudgetUpstream(); got != 999 {
		t.Errorf("explicit budget = %d, want 999", got)
	}
}

func TestLevelFromEffort(t *testing.T) {
	cases := map[string]ThinkingLevel{
		"minimal": ThinkingMinimal, "low": ThinkingLow, "medium": ThinkingMedium,
		"high": ThinkingHigh, "": ThinkingOff, "bogus": ThinkingOff,
	}
	for in, want := range cases {
		if got := LevelFromEffort(in); got != want {
			t.Errorf("LevelFromEffort(%q) = %q, want %q", in, got, want)
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
