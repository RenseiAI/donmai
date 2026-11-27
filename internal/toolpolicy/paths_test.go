package toolpolicy

import (
	"strconv"
	"strings"
	"testing"
)

func readFamily(name string) bool { return name == "Read" }

// TestPathForms pins path-glob semantics for denies and allows.
func TestPathForms(t *testing.T) {
	t.Parallel()
	const root, home = "/w", "/home/u"
	for _, tc := range []struct {
		pattern            string
		side               Side
		matched, unmatched []string
	}{
		{"Read(~/.ssh/**)", Deny, []string{"/home/u/.ssh/id_rsa", "/home/u/.ssh", "/home/u/.ssh/a/b"}, []string{"/home/u/.sshx", "/home/v/.ssh/id_rsa"}},
		{"Read(/srv/**/.env*)", Deny, []string{"/srv/.env", "/srv/a/.env.local", "/srv/a/b/.env"}, []string{"/srv/a/env", "/srvx/.env"}},
		{"Read(//etc/shadow)", Deny, []string{"/etc/shadow"}, []string{"/etc/passwd"}},
		{"Read(secret/**)", Deny, []string{"/w/secret/k", "secret/k", "/w/x/../secret/k"}, []string{"/w/secrets/k"}},
		{"Read(*.pem)", Deny, []string{"/w/a.pem", "/w/x/y/a.pem"}, []string{"/w/a.pemx", "/other/a.pem"}},
		{"Read(/srv/secret)", Deny, []string{"/srv/secret", "/srv/secret/k"}, []string{"/srv/secretx"}},
		{"Read(/s/{a,b}.key)", Deny, []string{"/s/a.key", "/s/b.key"}, []string{"/s/c.key"}},
		{"Read(/s/[ab].key)", Deny, []string{"/s/a.key"}, []string{"/s/c.key"}},
		{"Read(/s/[!ab].key)", Deny, []string{"/s/c.key"}, []string{"/s/a.key"}},
		{"Read(/s/?.key)", Deny, []string{"/s/a.key"}, []string{"/s/ab.key", "/s/x/a.key"}},
		{"Read(/s/*/k)", Deny, []string{"/s/a/k"}, []string{"/s/a/b/k"}},
		{"Read", Deny, []string{"/anything"}, nil},
		{"Edit(/w/x)", Deny, nil, []string{"/w/x"}}, // another family
		{"Read(/srv/docs)", Allow, []string{"/srv/docs"}, []string{"/srv/docs/x", "/srv/docsx"}},
		{"Read(*.md)", Allow, []string{"/w/a.md"}, []string{"/w/x/a.md"}},
		{"Read(docs/**)", Allow, []string{"/w/docs/a/b.md", "/w/docs"}, []string{"/w/docsx"}},
		// A newline in a name does not stop "**" or the deny subtree.
		{"Read(~/.ssh/**)", Deny, []string{"/home/u/.ssh/a\nb"}, nil},
		{"Read(/srv/secret)", Deny, []string{"/srv/secret/a\nb"}, nil},
		{"Read(/srv/**/.env*)", Deny, []string{"/srv/a\nb/.env"}, nil},
		// A class and a non-segment "**" stay within one directory.
		{"Read(/w/x[!a]y)", Allow, []string{"/w/xby"}, []string{"/w/x/y", "/w/xay"}},
		{"Read(/w/x[^a]y)", Allow, []string{"/w/xby"}, []string{"/w/x/y", "/w/xay"}},
		{"Read(/w/docs**)", Allow, []string{"/w/docsx"}, []string{"/w/docsx/secret"}},
	} {
		rules, errs := CompilePaths([]string{tc.pattern}, readFamily, tc.side, root, home)
		if len(errs) != 0 {
			t.Errorf("%s: unexpected errors %v", tc.pattern, errs)
		}
		for _, path := range tc.matched {
			if !rules.Match(path, root) {
				t.Errorf("%s (side %d): %q not matched", tc.pattern, tc.side, path)
			}
		}
		for _, path := range tc.unmatched {
			if rules.Match(path, root) {
				t.Errorf("%s (side %d): %q matched", tc.pattern, tc.side, path)
			}
		}
	}
}

// TestPathRootIsLiteral: glob characters in the root or home directory are
// literal text.
func TestPathRootIsLiteral(t *testing.T) {
	t.Parallel()
	const root, home = "/w/[a]*", "/h/{x}"
	for _, tc := range []struct{ pattern, matched, unmatched string }{
		{"Read(secret/**)", "/w/[a]*/secret/k", "/w/a/secret/k"},
		{"Read(~/.ssh/**)", "/h/{x}/.ssh/k", "/h/x/.ssh/k"},
	} {
		rules, errs := CompilePaths([]string{tc.pattern}, readFamily, Deny, root, home)
		if len(errs) != 0 || !rules.Match(tc.matched, root) || rules.Match(tc.unmatched, root) {
			t.Errorf("%s: errs %v, %q matched %v, %q matched %v", tc.pattern, errs,
				tc.matched, rules.Match(tc.matched, root), tc.unmatched, rules.Match(tc.unmatched, root))
		}
	}
}

// TestPathMultibyte: non-ASCII characters in a pattern, root or home are
// literal whole characters, so a deny written with them still denies.
func TestPathMultibyte(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, root, home string
		side                Side
		matched, unmatched  []string
	}{
		{"Read(~/.ssh/**)", "/w", "/home/josé", Deny, []string{"/home/josé/.ssh/id_rsa"}, []string{"/home/jose/.ssh/id_rsa", "/home/josè/.ssh/id_rsa"}},
		{"Read(/srv/données/**)", "/w", "/h", Deny, []string{"/srv/données/k", "/srv/données"}, []string{"/srv/donnees/k", "/srv/donnéesx/k"}},
		{"Read(secret)", "/wörk", "/h", Deny, []string{"/wörk/secret", "/wörk/a/secret/k"}, []string{"/work/secret"}},
		{"Read(/s/日本/*.key)", "/w", "/h", Allow, []string{"/s/日本/a.key"}, []string{"/s/日本/a/b.key", "/s/日/a.key"}},
		{"Read(/s/?.key)", "/w", "/h", Deny, []string{"/s/é.key"}, []string{"/s/éé.key"}},
		{"Read(/s/[éa].key)", "/w", "/h", Deny, []string{"/s/é.key", "/s/a.key"}, []string{"/s/e.key"}},
		{"Read(/s/\\é.key)", "/w", "/h", Deny, []string{"/s/é.key"}, []string{"/s/e.key"}},
	} {
		rules, errs := CompilePaths([]string{tc.pattern}, readFamily, tc.side, tc.root, tc.home)
		if len(errs) != 0 {
			t.Errorf("%s: unexpected errors %v", tc.pattern, errs)
		}
		for _, path := range tc.matched {
			if !rules.Match(path, tc.root) {
				t.Errorf("%s (root %q, home %q): %q not matched", tc.pattern, tc.root, tc.home, path)
			}
		}
		for _, path := range tc.unmatched {
			if rules.Match(path, tc.root) {
				t.Errorf("%s (root %q, home %q): %q matched", tc.pattern, tc.root, tc.home, path)
			}
		}
	}
}

// TestPathFailsClosed: a pattern that cannot be compiled denies every path as a
// deny and allows none as an allow, and is reported.
func TestPathFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ pattern, root, home string }{
		{"Read(/s/[ab.key)", "/w", "/h"},
		{"Read(/s/{a,b.key)", "/w", "/h"},
		{"Read(/s/a}.key)", "/w", "/h"},
		{"Read(~other/.ssh)", "/w", "/h"},
		{"Read(~/.ssh/**)", "/w", ""},
		{"Read(docs/**)", "", "/h"},
		{"Read()", "/w", "/h"},
		{"Read(/s/a\nb)", "/w", "/h"},
		{"Read(/s/[!].key)", "/w", "/h"},
		{"Read(/s/\xffa)", "/w", "/h"},
		{"Read(secret)", "/w\xff", "/h"},
	} {
		deny, errs := CompilePaths([]string{tc.pattern}, readFamily, Deny, tc.root, tc.home)
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), strconv.Quote(tc.pattern)) {
			t.Errorf("%s: errors = %v, want one naming the pattern", tc.pattern, errs)
		}
		if !deny.Match("/unrelated/file", tc.root) {
			t.Errorf("%s: deny does not fail closed", tc.pattern)
		}
		allow, _ := CompilePaths([]string{tc.pattern}, readFamily, Allow, tc.root, tc.home)
		if allow.Match("/unrelated/file", tc.root) || allow.Len() != 0 {
			t.Errorf("%s: allow does not fail closed", tc.pattern)
		}
	}
}
