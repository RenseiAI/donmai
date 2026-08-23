package workarea

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryDeclarationWireIsClosed(t *testing.T) {
	raw := []byte(`{"protocol":"session-root-v1","repositories":[{"source":{"repository":"https://example.test/acme/repo.git","ref":"main","future":true},"role":"primary","authority":"mutable"}]}`)
	var declaration RepositoryDeclarationV1
	if err := json.Unmarshal(raw, &declaration); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Unmarshal error = %v, want unknown-field refusal", err)
	}
}

func testDeclaration() RepositoryDeclarationV1 {
	return RepositoryDeclarationV1{
		Protocol: ProtocolSessionRootV1,
		Repositories: []DeclaredRepositoryV1{
			{Source: RepositorySource{Repository: "https://example.test/acme/primary.git", Ref: "main"}, Role: RepositoryRolePrimary, Authority: RepositoryMutable},
			{Source: RepositorySource{Repository: "https://user:fixture-secret@example.test/acme/corpus.git", Ref: "v1"}, Role: RepositoryRoleContext, Authority: RepositoryReadOnly},
		},
	}
}

func TestRepositoryDeclarationNormalizeAndExplicitSelection(t *testing.T) {
	declaration := testDeclaration()
	normalized, err := declaration.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if normalized.Selected.Name != "primary" {
		t.Fatalf("default selected repository = %q, want primary", normalized.Selected.Name)
	}

	declaration.Select = &RepositoryFilter{Kind: RepositoryFilterNamed, Name: "corpus"}
	normalized, err = declaration.Normalize()
	if err != nil {
		t.Fatalf("Normalize named context: %v", err)
	}
	if normalized.Selected.Name != "corpus" || normalized.Selected.Authority != RepositoryReadOnly {
		t.Fatalf("named selection = %#v, want read-only corpus", normalized.Selected)
	}
}

func TestRepositoryDeclarationExplicitPinNeverFallsBackToPrimary(t *testing.T) {
	declaration := testDeclaration()
	declaration.Select = &RepositoryFilter{Kind: RepositoryFilterNamed, Name: "missing"}
	_, err := declaration.Normalize()
	var contractErr *RepositoryContractError
	if !errors.As(err, &contractErr) {
		t.Fatalf("Normalize error = %v, want RepositoryContractError", err)
	}
	if contractErr.Reason != ReasonRepositoryUndeclared || contractErr.RuleID != RuleFilterDeclared {
		t.Fatalf("error = %#v, want undeclared/%s", contractErr, RuleFilterDeclared)
	}
}

func TestRepositoryDeclarationRoleSelectionRequiresOne(t *testing.T) {
	declaration := testDeclaration()
	declaration.Repositories = append(declaration.Repositories,
		DeclaredRepositoryV1{Source: RepositorySource{Repository: "https://example.test/acme/other.git"}, Role: RepositoryRoleContext, Authority: RepositoryReadOnly})
	declaration.Select = &RepositoryFilter{Kind: RepositoryFilterRole, Role: RepositoryRoleContext}
	_, err := declaration.Normalize()
	var contractErr *RepositoryContractError
	if !errors.As(err, &contractErr) || contractErr.Reason != ReasonRepositoryFilterAmbiguous || contractErr.RuleID != RuleFilterSingle {
		t.Fatalf("Normalize error = %#v, want ambiguous/%s", err, RuleFilterSingle)
	}
}

func TestRepositoryDeclarationRejectsCollidingCanonicalLeaves(t *testing.T) {
	declaration := testDeclaration()
	declaration.Repositories[1].Source.Repository = "ssh://example.test/other/primary.git"
	_, err := declaration.Normalize()
	var contractErr *RepositoryContractError
	if !errors.As(err, &contractErr) || contractErr.Reason != ReasonRepositoryLeafCollision || contractErr.RuleID != RuleLeafUnique {
		t.Fatalf("Normalize error = %#v, want collision/%s", err, RuleLeafUnique)
	}
	if contractErr.Repository == "" || contractErr.OtherRepository == "" {
		t.Fatalf("collision error did not name both entries: %#v", contractErr)
	}
}

func TestRepositoryDeclarationRejectsCaseFoldCollision(t *testing.T) {
	declaration := testDeclaration()
	declaration.Repositories[0].Name = "Repo"
	declaration.Repositories[1].Name = "repo"
	_, err := declaration.Normalize()
	var contractErr *RepositoryContractError
	if !errors.As(err, &contractErr) || contractErr.Reason != ReasonRepositoryLeafCollision {
		t.Fatalf("Normalize error = %#v, want case-fold collision", err)
	}
}

func TestRepositoryLeafRefusesInsteadOfSanitizing(t *testing.T) {
	for _, source := range []string{
		"https://example.test/acme/.workarea.git",
		"https://example.test/acme/repo name.git",
		"https://example.test/acme/..git",
	} {
		t.Run(source, func(t *testing.T) {
			if leaf, err := RepositoryLeaf(source); err == nil {
				t.Fatalf("RepositoryLeaf = %q, want refusal", leaf)
			}
		})
	}
}

func TestExecutorCapabilitiesRequireExactProtocolAndReadOnlyBoundary(t *testing.T) {
	normalized, err := testDeclaration().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		caps ExecutorWorkareaCapabilities
		want RepositoryReasonCode
	}{
		{name: "absent protocol", caps: ExecutorWorkareaCapabilities{}, want: ReasonProtocolUnsupported},
		{name: "protocol without enforcement", caps: ExecutorWorkareaCapabilities{MultiRepositoryWorkareaProtocols: []Protocol{ProtocolSessionRootV1}}, want: ReasonAuthorityEnforcementMissing},
		{name: "exact", caps: ExecutorWorkareaCapabilities{MultiRepositoryWorkareaProtocols: []Protocol{ProtocolSessionRootV1}, RepositoryAuthorityEnforcement: RepositoryAuthorityIsolatedReadOnlyV1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.caps.ValidateFor(normalized)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateFor: %v", err)
				}
				return
			}
			var contractErr *RepositoryContractError
			if !errors.As(err, &contractErr) || contractErr.Reason != tc.want {
				t.Fatalf("ValidateFor error = %#v, want %s", err, tc.want)
			}
		})
	}
}

func TestDeclarationRecordIsAtomicModeBoundAndSecretFree(t *testing.T) {
	normalized, err := testDeclaration().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	root := RootPath(filepath.Join(t.TempDir(), "session"))
	if err := os.MkdirAll(root.String(), 0o700); err != nil {
		t.Fatal(err)
	}
	record := NewDeclarationRecord("session", "wa_fixture", normalized, map[string]string{"primary": "abc123", "corpus": "def456"})
	if err := WriteDeclaration(context.Background(), root, record); err != nil {
		t.Fatalf("WriteDeclaration: %v", err)
	}
	body, err := os.ReadFile(DeclarationPath(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"fixture-secret", "https://", "example.test"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("declaration contains forbidden source material %q: %s", forbidden, body)
		}
	}
	info, err := os.Stat(DeclarationPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("declaration mode = %#o, want 0600", got)
	}
	loaded, err := ReadDeclaration(root)
	if err != nil {
		t.Fatalf("ReadDeclaration: %v", err)
	}
	if loaded.SelectedRepository != "primary" || len(loaded.Repositories) != 2 {
		t.Fatalf("loaded record = %#v", loaded)
	}
	entries, err := os.ReadDir(filepath.Join(root.String(), DeclarationDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != DeclarationFileName {
		t.Fatalf("metadata entries = %v, want only declaration.json", entries)
	}
}

func TestDiscoverLayoutReadsDeclarationAndRetainsLegacyFlat(t *testing.T) {
	parent := t.TempDir()
	normalized, err := testDeclaration().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	nested, err := NewLayout(parent, "nested-session", normalized.Selected.Leaf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested.Repository.String(), 0o700); err != nil {
		t.Fatal(err)
	}
	record := NewDeclarationRecord("nested-session", "wa_nested", normalized, nil)
	if err := WriteDeclaration(context.Background(), nested.Root, record); err != nil {
		t.Fatal(err)
	}
	discovered, found, err := DiscoverLayout(parent, "nested-session", "ignored")
	if err != nil || !found || discovered != nested {
		t.Fatalf("nested DiscoverLayout = (%#v, %v, %v), want (%#v, true, nil)", discovered, found, err, nested)
	}

	flat, err := FlatLayout(parent, "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(flat.Root.String(), ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	discovered, found, err = DiscoverLayout(parent, "legacy-session", "primary")
	if err != nil || !found || discovered != flat || discovered.Root != RootPath(discovered.Repository) {
		t.Fatalf("flat DiscoverLayout = (%#v, %v, %v), want (%#v, true, nil)", discovered, found, err, flat)
	}
}

func TestDeclarationRootedIORefusesSymlinkAndSwap(t *testing.T) {
	normalized, err := testDeclaration().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	record := NewDeclarationRecord("session", "wa_rooted", normalized, nil)
	t.Run("root-directory-swap", func(t *testing.T) {
		root := RootPath(filepath.Join(t.TempDir(), "root"))
		if err := os.Mkdir(root.String(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := WriteDeclaration(context.Background(), root, record); err != nil {
			t.Fatal(err)
		}
		moved := root.String() + "-old"
		declarationRaceHook = func(stage string) {
			if stage != "root-after-stat" {
				return
			}
			if err := os.Rename(root.String(), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(root.String(), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { declarationRaceHook = nil })
		if _, err := ReadDeclaration(root); err == nil || !strings.Contains(err.Error(), "identity changed") {
			t.Fatalf("root swap error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(moved, DeclarationDirName, DeclarationFileName)); err != nil {
			t.Fatalf("authorized original declaration was damaged: %v", err)
		}
	})

	t.Run("metadata-symlink", func(t *testing.T) {
		root := RootPath(filepath.Join(t.TempDir(), "root"))
		external := t.TempDir()
		if err := os.Mkdir(root.String(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(root.String(), DeclarationDirName)); err != nil {
			t.Fatal(err)
		}
		if err := WriteDeclaration(context.Background(), root, record); err == nil {
			t.Fatal("WriteDeclaration followed a metadata symlink")
		}
		if _, err := os.Stat(filepath.Join(external, DeclarationFileName)); !os.IsNotExist(err) {
			t.Fatalf("external declaration was created: %v", err)
		}
	})

	t.Run("metadata-directory-swap", func(t *testing.T) {
		root := RootPath(filepath.Join(t.TempDir(), "root"))
		if err := os.Mkdir(root.String(), 0o700); err != nil {
			t.Fatal(err)
		}
		declarationRaceHook = func(stage string) {
			if stage != "write-before-publish" {
				return
			}
			metadata := filepath.Join(root.String(), DeclarationDirName)
			if err := os.Rename(metadata, metadata+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(metadata, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { declarationRaceHook = nil })
		if err := WriteDeclaration(context.Background(), root, record); err == nil || !strings.Contains(err.Error(), "identity changed") {
			t.Fatalf("WriteDeclaration swap error = %v", err)
		}
		if _, err := os.Stat(DeclarationPath(root)); !os.IsNotExist(err) {
			t.Fatalf("replacement metadata received declaration: %v", err)
		}
	})

	t.Run("declaration-file-swap", func(t *testing.T) {
		root := RootPath(filepath.Join(t.TempDir(), "root"))
		if err := os.Mkdir(root.String(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := WriteDeclaration(context.Background(), root, record); err != nil {
			t.Fatal(err)
		}
		replacement := record
		replacement.SessionID = "replacement"
		replacementBody, err := json.Marshal(replacement)
		if err != nil {
			t.Fatal(err)
		}
		declarationRaceHook = func(stage string) {
			if stage != "read-after-stat" {
				return
			}
			path := DeclarationPath(root)
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, replacementBody, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { declarationRaceHook = nil })
		if _, err := ReadDeclaration(root); err == nil || !strings.Contains(err.Error(), "identity changed") {
			t.Fatalf("ReadDeclaration swap error = %v", err)
		}
	})
}

func TestDiscoverLayoutDoesNotFollowUserControlledSymlinks(t *testing.T) {
	parent := t.TempDir()
	external := t.TempDir()
	if err := os.Mkdir(filepath.Join(external, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(parent, "legacy-link")); err != nil {
		t.Fatal(err)
	}
	layout, found, err := DiscoverLayout(parent, "legacy-link", "primary")
	if err != nil || found || filepath.Clean(layout.Root.String()) == filepath.Clean(filepath.Join(parent, "legacy-link")) {
		t.Fatalf("symlink legacy discovery = (%+v, %v, %v)", layout, found, err)
	}

	normalized, err := testDeclaration().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	nested, err := NewLayout(parent, "nested-link", normalized.Selected.Leaf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested.Root.String(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, nested.Repository.String()); err != nil {
		t.Fatal(err)
	}
	if err := WriteDeclaration(context.Background(), nested.Root, NewDeclarationRecord("nested-link", "wa_nested_link", normalized, nil)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DiscoverLayout(parent, "nested-link", normalized.Selected.Leaf); err == nil {
		t.Fatal("nested discovery accepted a symlinked declared repository")
	}
}
