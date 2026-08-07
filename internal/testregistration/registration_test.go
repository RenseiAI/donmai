package testregistration

import (
	"os"
	"strings"
	"testing"
)

func TestCustomTags(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   []string
	}{
		{
			name:   "no build line",
			source: "package foo\n\nfunc TestX(t *testing.T) {}\n",
			want:   nil,
		},
		{
			name:   "single custom tag",
			source: "//go:build integration\n\npackage foo\n",
			want:   []string{"integration"},
		},
		{
			name:   "underscored custom tag",
			source: "//go:build runner_integration\n\npackage foo\n",
			want:   []string{"runner_integration"},
		},
		{
			name:   "GOOS constraint is toolchain-controlled",
			source: "//go:build darwin || linux\n\npackage foo\n",
			want:   nil,
		},
		{
			name:   "negated GOOS constraint is toolchain-controlled",
			source: "//go:build !windows\n\npackage foo\n",
			want:   nil,
		},
		{
			name:   "race is set by a flag, not by -tags",
			source: "//go:build race\n\npackage foo\n",
			want:   nil,
		},
		{
			name:   "go version constraints are not tags",
			source: "//go:build go1.24\n\npackage foo\n",
			want:   nil,
		},
		{
			name:   "custom tag mixed with a GOOS constraint is still reported",
			source: "//go:build integration && linux\n\npackage foo\n",
			want:   []string{"integration"},
		},
		{
			name:   "repeated tag is reported once",
			source: "//go:build a || a\n\npackage foo\n",
			want:   []string{"a"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CustomTags(tc.source)
			if len(got) != len(tc.want) {
				t.Fatalf("CustomTags() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("CustomTags() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSuppliedTags(t *testing.T) {
	tests := []struct {
		name  string
		build string
		want  []string
	}{
		{name: "space separated", build: "go test -tags integration ./...", want: []string{"integration"}},
		{name: "equals form", build: "go vet -tags=integration ./...", want: []string{"integration"}},
		{name: "comma list", build: "go vet -tags a,b ./...", want: []string{"a", "b"}},
		{name: "quoted list", build: `go vet -tags "a,b" ./...`, want: []string{"a", "b"}},
		{name: "double dash", build: "go vet --tags a ./...", want: []string{"a"}},
		{name: "no tags flag", build: "go test -race ./...", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SuppliedTags(tc.build)
			for _, want := range tc.want {
				if !got[want] {
					t.Fatalf("SuppliedTags(%q) missing %q; got %v", tc.build, want, got)
				}
			}
			if len(tc.want) == 0 && len(got) != 0 {
				t.Fatalf("SuppliedTags(%q) = %v, want empty", tc.build, got)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	t.Run("flags a tag nothing supplies", func(t *testing.T) {
		got := Check(
			map[string]string{"a/x_test.go": "//go:build orphan\n\npackage a\n"},
			"go test -race ./...",
		)
		if len(got) != 1 || got[0].Tag != "orphan" || got[0].File != "a/x_test.go" {
			t.Fatalf("Check() = %+v, want one orphan finding", got)
		}
	})

	t.Run("accepts a tag a target supplies", func(t *testing.T) {
		got := Check(
			map[string]string{"a/x_test.go": "//go:build orphan\n\npackage a\n"},
			"go vet -tags orphan ./...",
		)
		if len(got) != 0 {
			t.Fatalf("Check() = %+v, want no findings", got)
		}
	})

	t.Run("accepts an untagged test file", func(t *testing.T) {
		got := Check(map[string]string{"a/x_test.go": "package a\n"}, "")
		if len(got) != 0 {
			t.Fatalf("Check() = %+v, want no findings", got)
		}
	})

	t.Run("reports findings in stable file order", func(t *testing.T) {
		got := Check(map[string]string{
			"b/z_test.go": "//go:build zt\n\npackage b\n",
			"a/y_test.go": "//go:build yt\n\npackage a\n",
		}, "")
		if len(got) != 2 || got[0].File != "a/y_test.go" || got[1].File != "b/z_test.go" {
			t.Fatalf("Check() = %+v, want a/y before b/z", got)
		}
	})
}

// TestRepoBuildTagsAreRegistered is the live gate: every build-tag-gated test
// file in this repository must have its tag supplied by a real target or CI
// step, so the toolchain at least compiles it. A tagged file nothing supplies
// is never run, never compiled, and never syntax-checked — the suite stays
// green while the file rots. See the package doc and PROTOCOL.md § V V18.
func TestRepoBuildTagsAreRegistered(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, err := RepoRoot(wd)
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	testFiles, err := CollectTestFiles(root)
	if err != nil {
		t.Fatalf("collect test files: %v", err)
	}
	if len(testFiles) == 0 {
		t.Fatal("collected zero test files — the walk is broken, not the repo")
	}
	buildText, err := CollectBuildText(root)
	if err != nil {
		t.Fatalf("collect build text: %v", err)
	}
	if !strings.Contains(buildText, "go test") {
		t.Fatal("build text contains no `go test` — Makefile/workflow collection is broken")
	}

	unregistered := Check(testFiles, buildText)
	for _, finding := range unregistered {
		t.Errorf(
			"%s is gated behind //go:build %s, which no Makefile target or workflow step supplies via -tags.\n"+
				"    It is never run, never compiled, and never syntax-checked; the suite is green regardless.\n"+
				"    Fix: add %s to the tag list of the `test-tagged` target in the Makefile (and its CI step),\n"+
				"    or delete the file if the suite is dead.",
			finding.File, finding.Tag, finding.Tag,
		)
	}
}
