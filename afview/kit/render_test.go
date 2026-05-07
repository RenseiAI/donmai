package kit_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/afview/kit"
)

func sampleKits() []afclient.Kit {
	return []afclient.Kit{
		{
			ID:                 "spring/java",
			Name:               "Spring Java",
			Version:            "1.0.0",
			Scope:              afclient.KitScopeProject,
			Status:             afclient.KitStatusActive,
			Source:             afclient.KitSourceLocal,
			Trust:              afclient.KitTrustUnsigned,
			ProvidesCommands:   true,
			ProvidesPrompts:    true,
			ProvidesMCPServers: true,
		},
		{
			ID:      "next-js",
			Name:    "Next.js",
			Version: "2.0.0",
			Scope:   afclient.KitScopeOrg,
			Status:  afclient.KitStatusDisabled,
			Source:  afclient.KitSourceBundled,
			Trust:   afclient.KitTrustSignedVerified,
		},
	}
}

func sampleManifest() *afclient.KitManifest {
	return &afclient.KitManifest{
		Kit: afclient.Kit{
			ID:                 "spring/java",
			Name:               "Spring Java",
			Description:        "Maven/Gradle Spring Boot",
			Version:            "1.0.0",
			Scope:              afclient.KitScopeProject,
			Status:             afclient.KitStatusActive,
			Source:             afclient.KitSourceLocal,
			Trust:              afclient.KitTrustUnsigned,
			Author:             "Spring Team",
			AuthorID:           "did:web:spring.io",
			License:            "Apache-2.0",
			Homepage:           "https://spring.io",
			DetectFiles:        []string{"pom.xml", "build.gradle"},
			DetectExec:         "bin/detect",
			ProvidesCommands:   true,
			ProvidesMCPServers: true,
			ProvidesSkills:     true,
		},
		SupportedOS:     []string{"linux", "macos"},
		SupportedArch:   []string{"x86_64"},
		ConflictsWith:   []string{"maven-only"},
		ComposesWith:    []string{"docker-compose"},
		Order:           "framework",
		DetectToolchain: map[string]string{"java": "17"},
		Commands:        map[string]string{"build": "./mvnw compile", "test": "./mvnw test"},
		MCPServerNames:  []string{"spring-context"},
		SkillFiles:      []string{"skills/spring-test-debugging/SKILL.md"},
	}
}

func sampleSources() []afclient.KitRegistrySource {
	return []afclient.KitRegistrySource{
		{Name: "local", Kind: "local", URL: "~/.rensei/kits", Enabled: true, Priority: 1},
		{Name: "tessl", Kind: "tessl", URL: "https://registry.tessl.io", Enabled: false, Priority: 4},
	}
}

func TestRenderList_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	if err := kit.RenderList(&buf, sampleKits(), true); err != nil {
		t.Fatalf("RenderList: %v", err)
	}
	got := buf.String()
	wantContains := []string{"spring/java", "next-js", "1.0.0", "2.0.0", "ID", "VERSION", "SCOPE", "STATUS", "SOURCE", "active", "disabled", "local", "bundled"}
	for _, w := range wantContains {
		if !strings.Contains(got, w) {
			t.Errorf("RenderList: missing %q\n--- output ---\n%s", w, got)
		}
	}
}

func TestRenderList_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := kit.RenderList(&buf, nil, true); err != nil {
		t.Fatalf("RenderList: %v", err)
	}
	if !strings.Contains(buf.String(), "No kits installed") {
		t.Errorf("empty list should say 'No kits installed', got: %s", buf.String())
	}
}

func TestPlainList_NoANSI(t *testing.T) {
	var buf bytes.Buffer
	if err := kit.PlainList(&buf, sampleKits()); err != nil {
		t.Fatalf("PlainList: %v", err)
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("PlainList contains ANSI escapes:\n%s", buf.String())
	}
}

func TestPlainList_MatchesNoColor(t *testing.T) {
	var p, r bytes.Buffer
	_ = kit.PlainList(&p, sampleKits())
	_ = kit.RenderList(&r, sampleKits(), true)
	if p.String() != r.String() {
		t.Errorf("PlainList diverges from RenderList(noColor=true)\n--- plain ---\n%s\n--- render ---\n%s", p.String(), r.String())
	}
}

func TestRenderShow_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	if err := kit.RenderShow(&buf, sampleManifest(), true); err != nil {
		t.Fatalf("RenderShow: %v", err)
	}
	got := buf.String()
	wantContains := []string{
		"spring/java", "Spring Java", "Maven/Gradle Spring Boot",
		"Apache-2.0", "did:web:spring.io",
		"Detect", "pom.xml", "build.gradle", "bin/detect",
		"Toolchain", "java", "17",
		"Commands", "./mvnw compile", "./mvnw test",
		"Provides", "commands", "MCP servers", "skills",
		"Composition", "framework", "docker-compose", "maven-only",
		"unsigned",
	}
	for _, w := range wantContains {
		if !strings.Contains(got, w) {
			t.Errorf("RenderShow: missing %q\n--- output ---\n%s", w, got)
		}
	}
}

func TestRenderShow_DeterministicCommandOrder(t *testing.T) {
	// Two runs of RenderShow must produce byte-identical output despite
	// map iteration randomness.
	m := sampleManifest()
	var a, b bytes.Buffer
	_ = kit.RenderShow(&a, m, true)
	_ = kit.RenderShow(&b, m, true)
	if a.String() != b.String() {
		t.Errorf("RenderShow output is non-deterministic across runs\n--- a ---\n%s\n--- b ---\n%s", a.String(), b.String())
	}
}

func TestPlainShow(t *testing.T) {
	var buf bytes.Buffer
	if err := kit.PlainShow(&buf, sampleManifest()); err != nil {
		t.Fatalf("PlainShow: %v", err)
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("PlainShow contains ANSI escapes")
	}
}

func TestRenderInstall(t *testing.T) {
	var buf bytes.Buffer
	res := &afclient.KitInstallResult{
		Kit:     afclient.Kit{ID: "spring/java", Version: "1.0.0", Status: afclient.KitStatusActive, Source: afclient.KitSourceLocal},
		Message: "kit spring/java@1.0.0 installed",
	}
	if err := kit.RenderInstall(&buf, res, true); err != nil {
		t.Fatalf("RenderInstall: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "spring/java") || !strings.Contains(got, "installed") {
		t.Errorf("RenderInstall: unexpected output\n%s", got)
	}
}

func TestRenderInstall_FallbackMessage(t *testing.T) {
	var buf bytes.Buffer
	res := &afclient.KitInstallResult{
		Kit: afclient.Kit{ID: "x", Version: "1", Status: afclient.KitStatusActive, Source: afclient.KitSourceLocal},
	}
	if err := kit.RenderInstall(&buf, res, true); err != nil {
		t.Fatalf("RenderInstall: %v", err)
	}
	if !strings.Contains(buf.String(), "kit x@1 installed") {
		t.Errorf("RenderInstall fallback: missing default message, got: %s", buf.String())
	}
}

func TestRenderToggle(t *testing.T) {
	var buf bytes.Buffer
	k := &afclient.Kit{ID: "spring/java", Status: afclient.KitStatusActive}
	if err := kit.RenderToggle(&buf, k, true, true); err != nil {
		t.Fatalf("RenderToggle: %v", err)
	}
	if !strings.Contains(buf.String(), "kit spring/java enabled") {
		t.Errorf("RenderToggle (enabled): unexpected output\n%s", buf.String())
	}

	buf.Reset()
	k.Status = afclient.KitStatusDisabled
	_ = kit.RenderToggle(&buf, k, false, true)
	if !strings.Contains(buf.String(), "kit spring/java disabled") {
		t.Errorf("RenderToggle (disabled): unexpected output\n%s", buf.String())
	}
}

func TestRenderVerifySignature(t *testing.T) {
	var buf bytes.Buffer
	res := &afclient.KitSignatureResult{
		KitID:    "spring/java",
		Trust:    afclient.KitTrustUnsigned,
		SignerID: "did:web:spring.io",
		SignedAt: "2026-05-07T10:00:00Z",
		Details:  "Wave 9 caveat: signing partially implemented.",
	}
	if err := kit.RenderVerifySignature(&buf, res, true); err != nil {
		t.Fatalf("RenderVerifySignature: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"spring/java", "[unsigned]", "did:web:spring.io", "2026-05-07", "Wave 9 caveat"} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderVerifySignature: missing %q\n%s", want, got)
		}
	}
}

func TestRenderSources(t *testing.T) {
	var buf bytes.Buffer
	if err := kit.RenderSources(&buf, sampleSources(), true); err != nil {
		t.Fatalf("RenderSources: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"NAME", "KIND", "STATUS", "URL", "local", "tessl", "enabled", "disabled", "registry.tessl.io"} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderSources: missing %q\n%s", want, got)
		}
	}
}

func TestRenderSources_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := kit.RenderSources(&buf, nil, true); err != nil {
		t.Fatalf("RenderSources: %v", err)
	}
	if !strings.Contains(buf.String(), "No kit sources") {
		t.Errorf("empty sources: want 'No kit sources', got: %s", buf.String())
	}
}

func TestPlainSources_NoANSI(t *testing.T) {
	var buf bytes.Buffer
	if err := kit.PlainSources(&buf, sampleSources()); err != nil {
		t.Fatalf("PlainSources: %v", err)
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("PlainSources contains ANSI escapes")
	}
}

func TestTrustSymbol_NoColor(t *testing.T) {
	tests := []struct {
		trust afclient.KitTrustState
		want  string
	}{
		{afclient.KitTrustSignedVerified, "[verified]"},
		{afclient.KitTrustSignedUnverified, "[signed/unverified]"},
		{afclient.KitTrustUnsigned, "[unsigned]"},
	}
	for _, c := range tests {
		got := kit.TrustSymbol(c.trust, true)
		if got != c.want {
			t.Errorf("TrustSymbol(%q, noColor=true) = %q, want %q", c.trust, got, c.want)
		}
	}
}

func TestTrustSymbol_Color(t *testing.T) {
	tests := []struct {
		trust afclient.KitTrustState
		want  string
	}{
		{afclient.KitTrustSignedVerified, "✅"},
		{afclient.KitTrustSignedUnverified, "⚠"},
		{afclient.KitTrustUnsigned, "🔓"},
	}
	for _, c := range tests {
		got := kit.TrustSymbol(c.trust, false)
		if got != c.want {
			t.Errorf("TrustSymbol(%q, noColor=false) = %q, want %q", c.trust, got, c.want)
		}
	}
}

func TestNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if kit.NoColorEnv() {
		t.Error("NoColorEnv: want false with empty NO_COLOR")
	}
	t.Setenv("NO_COLOR", "1")
	if !kit.NoColorEnv() {
		t.Error("NoColorEnv: want true with NO_COLOR=1")
	}
}

// TestRenderShow_AnsiEscapesPresentInColorMode pins the contract that
// non-noColor mode emits ANSI escapes (so the colour path is exercised).
func TestRenderShow_AnsiEscapesPresentInColorMode(t *testing.T) {
	var buf bytes.Buffer
	if err := kit.RenderShow(&buf, sampleManifest(), false); err != nil {
		t.Fatalf("RenderShow: %v", err)
	}
	if !strings.Contains(buf.String(), "\033[") {
		t.Errorf("RenderShow color mode: want ANSI escapes")
	}
}

// TestRenderShow_OmitsEmptyOptionalSections ensures that a sparse
// manifest doesn't print empty section headers.
func TestRenderShow_OmitsEmptyOptionalSections(t *testing.T) {
	m := &afclient.KitManifest{Kit: afclient.Kit{ID: "minimal", Name: "Minimal", Version: "1", Scope: afclient.KitScopeProject, Status: afclient.KitStatusActive, Source: afclient.KitSourceLocal, Trust: afclient.KitTrustUnsigned}}
	var buf bytes.Buffer
	_ = kit.RenderShow(&buf, m, true)
	got := buf.String()
	for _, absent := range []string{"Detect", "Commands", "Provides", "Composition"} {
		if strings.Contains(got, absent) {
			t.Errorf("minimal manifest: unexpected section %q in output:\n%s", absent, got)
		}
	}
}
