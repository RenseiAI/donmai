package codex

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/RenseiAI/donmai/agent"
)

// TestTOMLBasicString_RoundTripsEveryControlCharacter proves tomlBasicString
// emits a TOML basic string that is valid under TOML 1.0 for every input the
// launch can carry: each rune 0x00-0x1f and DEL (0x7f), plus quotes,
// backslashes, newlines, non-ASCII text and U+2028, decodes back to the exact
// input through a real TOML parser. The CLI this launch targets falls back to
// the raw literal when a `--config` value fails to parse as TOML, so an
// encoding that is merely JSON-valid (the prior shape: `\a` and `\v` escapes,
// raw DEL) would deliver the quoted literal instead of the intended text.
// RED proof: restore the json.Marshal encoder and the DEL case fails to
// decode.
func TestTOMLBasicString_RoundTripsEveryControlCharacter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
	}{
		{name: "empty"},
		{name: "plain", input: "hello world"},
		{name: "quotes and backslashes", input: `say "hi" and \\ escape`},
		{name: "newlines", input: "line one\nline two\r\nline three\r"},
		{name: "tabs and form feed", input: "a\tb\fc"},
		{name: "backspace", input: "a\bb"},
		{name: "non-ascii", input: "雪 snow"},
		{name: "line separator U+2028", input: "a b"},
		{name: "paragraph separator U+2029", input: "a b"},
		{name: "lone replacement char", input: "a�b"},
		{name: "del", input: "a\x7fb"},
		{name: "bel", input: "a\ab"},
		{name: "vertical tab", input: "a\vb"},
		{name: "escape", input: "a\x1bb"},
		{name: "ansi color", input: "\x1b[31mred\x1b[0m"},
		{name: "nul", input: "a\x00b"},
	}
	for r := rune(0x00); r <= 0x1f; r++ {
		cases = append(cases, struct {
			name  string
			input string
		}{name: "control", input: "a" + string(r) + "b"})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := decodeTOMLBasicString(t, tomlBasicString(tt.input)); got != tt.input {
				t.Fatalf("round trip = %q, want %q (encoded %q)", got, tt.input, tomlBasicString(tt.input))
			}
		})
	}
}

// TestTOMLBasicString_UsesTOMLEscapes pins the exact escape vocabulary: the
// short forms where TOML defines them, `\uXXXX` for every other C0 control
// and DEL, and literal passthrough for everything else.
func TestTOMLBasicString_UsesTOMLEscapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{input: `"`, want: `"\""`},
		{input: `\`, want: `"\\"`},
		{input: "\b", want: `"\b"`},
		{input: "\t", want: `"\t"`},
		{input: "\n", want: `"\n"`},
		{input: "\f", want: `"\f"`},
		{input: "\r", want: `"\r"`},
		{input: "\a", want: `"\u0007"`},
		{input: "\v", want: `"\u000B"`},
		{input: "\x00", want: `"\u0000"`},
		{input: "\x1b", want: `"\u001B"`},
		{input: "\x7f", want: `"\u007F"`},
		{input: "雪", want: `"雪"`},
		{input: " ", want: `" "`},
		{input: "plain", want: `"plain"`},
	}
	for _, tt := range tests {
		if got := tomlBasicString(tt.input); got != tt.want {
			t.Errorf("tomlBasicString(%q) = %q, want %q", tt.input, got, tt.want)
		}
		if decoded := decodeTOMLBasicString(t, tomlBasicString(tt.input)); decoded != tt.input {
			t.Errorf("round trip of %q = %q", tt.input, decoded)
		}
	}
}

// TestBuildInteractiveLaunch_DeveloperInstructionsRoundTrip proves a launch
// context carrying control characters and terminal escapes arrives byte-exact:
// the `developer_instructions=` argument decodes through real TOML back to
// the exact SystemPromptAppend. RED proof: restore strconv.Quote for
// developer_instructions and the `\a` / `\v` escapes fail to decode.
func TestBuildInteractiveLaunch_DeveloperInstructionsRoundTrip(t *testing.T) {
	t.Parallel()
	const appendText = "role intent\nline two\aring\x0b[31mred\x1b[0m tail with \"quotes\" and \\ backslash 雪"
	launch, err := buildInteractiveLaunchEnv(agent.Spec{
		Cwd:                t.TempDir(),
		SystemPromptAppend: appendText,
	}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("buildInteractiveLaunchEnv: %v", err)
	}
	var override string
	for i := 0; i+1 < len(launch.argv); i++ {
		if launch.argv[i] == "--config" && strings.HasPrefix(launch.argv[i+1], "developer_instructions=") {
			override = launch.argv[i+1]
		}
	}
	if override == "" {
		t.Fatalf("argv carries no developer_instructions override: %q", launch.argv)
	}
	var decoded struct {
		Instructions string `toml:"developer_instructions"`
	}
	if err := toml.Unmarshal([]byte(override), &decoded); err != nil {
		t.Fatalf("developer_instructions override is not semantic TOML: %v\n%s", err, override)
	}
	if decoded.Instructions != appendText {
		t.Fatalf("decoded developer instructions = %q, want %q", decoded.Instructions, appendText)
	}
}

func decodeTOMLBasicString(t *testing.T, encoded string) string {
	t.Helper()
	var decoded struct {
		Value string `toml:"k"`
	}
	if err := toml.Unmarshal([]byte("k="+encoded), &decoded); err != nil {
		t.Fatalf("tomlBasicString output is not semantic TOML: %v\n%s", err, encoded)
	}
	return decoded.Value
}
