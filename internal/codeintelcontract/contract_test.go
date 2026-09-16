package codeintelcontract

import (
	"encoding/json"
	"testing"
)

func TestDescriptorsAreClosedUniqueAndCloned(t *testing.T) {
	values := Descriptors()
	if len(values) != 6 {
		t.Fatalf("descriptor count = %d, want 6", len(values))
	}
	seen := map[string]bool{}
	for _, value := range values {
		if value.Name == "" || value.Description == "" || !json.Valid(value.InputSchema) || seen[value.Name] {
			t.Fatalf("malformed descriptor: %+v", value)
		}
		seen[value.Name] = true
	}
	values[0].InputSchema[0] = 'x'
	if !json.Valid(Descriptors()[0].InputSchema) {
		t.Fatal("descriptor schema aliases caller memory")
	}
}

func TestNormalizePolicyIdentity(t *testing.T) {
	t.Parallel()
	fq := "mcp__" + ServerName + "__" + ToolSearchSymbols
	for _, test := range []struct {
		name, raw, canonical string
		related, wantErr     bool
	}{
		{name: "canonical", raw: ToolSearchSymbols, canonical: ToolSearchSymbols, related: true},
		{name: "fq", raw: fq, canonical: ToolSearchSymbols, related: true},
		{name: "wildcard", raw: "*"},
		{name: "builtin", raw: "Read"},
		{name: "foreign", raw: "mcp__tracker__read"},
		{name: "unknown native", raw: "af_code_unknown", wantErr: true},
		{name: "wrong server", raw: "mcp__other__" + ToolSearchSymbols, wantErr: true},
		{name: "unknown fq member", raw: "mcp__" + ServerName + "__af_code_unknown", wantErr: true},
		{name: "case drift", raw: "AF_CODE_SEARCH_SYMBOLS", wantErr: true},
		{name: "whitespace", raw: " " + ToolSearchSymbols, wantErr: true},
		{name: "constraint", raw: ToolSearchSymbols + "(*)", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizePolicyIdentity(test.raw)
			if (err != nil) != test.wantErr {
				t.Fatalf("NormalizePolicyIdentity(%q) err=%v wantErr=%v", test.raw, err, test.wantErr)
			}
			if err == nil && (got.Canonical != test.canonical || got.Related != test.related) {
				t.Fatalf("NormalizePolicyIdentity(%q)=%+v want canonical=%q related=%v", test.raw, got, test.canonical, test.related)
			}
		})
	}
}
