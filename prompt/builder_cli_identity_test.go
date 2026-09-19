package prompt

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runtime/statehome"
	"github.com/RenseiAI/donmai/templates"
)

func TestResolveCLIExecutableNameStrictGrammar(t *testing.T) {
	valid128 := "a" + strings.Repeat("-", 127)
	for _, tc := range []struct {
		in, want string
	}{
		{"", "donmai"}, {"sample-cli", "sample-cli"}, {"A_1.2-x", "A_1.2-x"}, {valid128, valid128},
	} {
		got, err := ResolveCLIExecutableName(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("ResolveCLIExecutableName(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"-sample", " sample", "sample ", "sample cli", "sample/cli", `sample\cli`, "sample;cli", "sample$cli", "é", "a" + strings.Repeat("-", 128)} {
		if got, err := ResolveCLIExecutableName(in); err == nil || got != "" || strings.Contains(err.Error(), in) {
			t.Fatalf("ResolveCLIExecutableName(%q) = %q, %v; want value-free refusal", in, got, err)
		}
	}
}

func TestBuilderCLIExecutableNameLegacyRaymondCompositionAndCopy(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)
	statehome.SetBrand("sample-local-a")
	work := QueuedWork{SessionID: "session-1", IssueIdentifier: "EX-1", Body: "exercise the command identity", WorkType: string(WorkTypeDevelopment)}

	registry, err := templates.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		base *Builder
	}{
		{"legacy", &Builder{SystemAppend: "system-marker", SkillAppend: "skill-marker"}},
		{"raymond", &Builder{SystemAppend: "system-marker", SkillAppend: "skill-marker", Registry: registry}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configured, err := tc.base.WithCLIExecutableName("sample-cli")
			if err != nil {
				t.Fatal(err)
			}
			// Initialize the lazy legacy cache before copying. Copy must retain
			// construction inputs without copying sync.Once or parsed state.
			if _, _, err := configured.Build(work); err != nil {
				t.Fatal(err)
			}
			copy := configured.Copy()
			composition, err := copy.BuildComposition(work)
			if err != nil {
				t.Fatal(err)
			}
			all := composition.SystemPrompt() + "\n" + composition.UserPrompt
			for _, want := range []string{"autonomous Sample-local-a agent", "`sample-cli linear`", "`sample-cli linear create-comment`", "system-marker", "skill-marker"} {
				if !strings.Contains(all, want) {
					t.Fatalf("composition missing %q: %s", want, all)
				}
			}
			if strings.Contains(all, "sample-local-a linear") || strings.Contains(all, "`donmai linear") {
				t.Fatalf("filesystem or default identity leaked into explicit commands: %s", all)
			}
		})
	}
}

func TestBuilderZeroCLIIsLiteralDonmaiUnderNamedFilesystemBrand(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)
	statehome.SetBrand("sample-local-b")
	system, user, err := NewBuilder().Build(QueuedWork{SessionID: "session-2", IssueIdentifier: "EX-2", Body: "default command", WorkType: string(WorkTypeDevelopment)})
	if err != nil {
		t.Fatal(err)
	}
	all := system + "\n" + user
	if !strings.Contains(all, "autonomous Sample-local-b agent") || !strings.Contains(all, "`donmai linear`") || strings.Contains(all, "sample-local-b linear") {
		t.Fatalf("display and executable identities were not separated: %s", all)
	}
}
