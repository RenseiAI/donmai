package daemon

import "testing"

// TestMatchProject pins the project matcher both admission paths share: the
// same repository in any spelling matches, a project id or a bare repository
// name still routes the singular field, and nothing that names a different
// host or path matches by suffix.
func TestMatchProject(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		configured string
		incoming   string
		want       bool
	}{
		{name: "exact", configured: "github.com/acme/secondary", incoming: "github.com/acme/secondary", want: true},
		{name: "project id", configured: "github.com/acme/secondary", incoming: "proj", want: true},
		{name: "https url with .git", configured: "github.com/acme/secondary", incoming: "https://github.com/acme/secondary.git", want: true},
		{name: "letter case and trailing slash", configured: "github.com/acme/secondary", incoming: "https://GitHub.com/Acme/Secondary/", want: true},
		{name: "url with userinfo", configured: "https://github.com/acme/secondary.git", incoming: "https://git@github.com/acme/secondary", want: true},
		{name: "scp-style ssh", configured: "github.com/acme/secondary", incoming: "git@github.com:acme/secondary.git", want: true},
		{name: "ssh url", configured: "https://github.com/acme/secondary", incoming: "ssh://git@github.com/acme/secondary.git", want: true},
		{name: "owner/name against a hosted entry", configured: "https://github.com/acme/secondary.git", incoming: "acme/secondary", want: true},
		{name: "github url against an owner/name entry", configured: "acme/secondary", incoming: "https://github.com/acme/secondary", want: true},
		{name: "bare repository name", configured: "github.com/acme/secondary", incoming: "secondary", want: true},
		{name: "same local path", configured: "/srv/git/secondary.git", incoming: "/srv/git/secondary.git", want: true},
		{name: "local path without .git", configured: "/srv/git/secondary.git", incoming: "/srv/git/secondary", want: true},

		{name: "suffix spoof under another host", configured: "github.com/acme/secondary", incoming: "https://evil.example/x/github.com/acme/secondary", want: false},
		{name: "scheme-less suffix spoof", configured: "github.com/acme/secondary", incoming: "evil.example/github.com/acme/secondary", want: false},
		{name: "same owner/name on another host", configured: "github.com/acme/secondary", incoming: "https://evil.example/acme/secondary", want: false},
		{name: "owner/name entry spoofed on another host", configured: "acme/secondary", incoming: "https://evil.example/acme/secondary", want: false},
		{name: "look-alike host", configured: "github.com/acme/secondary", incoming: "https://github.com.evil.example/acme/secondary", want: false},
		{name: "host with port", configured: "github.com/acme/secondary", incoming: "https://github.com:8443/acme/secondary", want: false},
		{name: "another owner", configured: "github.com/acme/secondary", incoming: "github.com/mallory/secondary", want: false},
		{name: "name prefix", configured: "github.com/acme/secondary", incoming: "github.com/acme/secondary-fork", want: false},
		{name: "dot-dot segment", configured: "github.com/acme/secondary", incoming: "https://github.com/acme/x/../secondary", want: false},
		{name: "query", configured: "github.com/acme/secondary", incoming: "https://github.com/acme/secondary?x=1", want: false},
		{name: "fragment", configured: "github.com/acme/secondary", incoming: "github.com/acme/secondary#main", want: false},
		{name: "bare owner name", configured: "github.com/acme/secondary", incoming: "acme", want: false},
		{name: "local path against a hosted entry", configured: "github.com/acme/secondary", incoming: "/acme/secondary", want: false},
		{name: "another local path", configured: "/srv/git/secondary.git", incoming: "/tmp/secondary.git", want: false},
		{name: "empty", configured: "github.com/acme/secondary", incoming: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &ProjectConfig{ID: "proj", Repository: tc.configured}
			if got := matchProject(p, tc.incoming) != nil; got != tc.want {
				t.Fatalf("matchProject(%q, %q) = %v, want %v", tc.configured, tc.incoming, got, tc.want)
			}
		})
	}
}

// TestSameRepositoryLocation pins the location-only match declaration entries
// use: a project id or a bare repository name is not a location.
func TestSameRepositoryLocation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		configured, incoming string
		want                 bool
	}{
		{"github.com/acme/secondary", "https://github.com/acme/secondary.git", true},
		{"github.com/acme/secondary", "secondary", false},
		{"github.com/acme/secondary", "https://evil.example/x/github.com/acme/secondary", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := sameRepositoryLocation(tc.configured, tc.incoming); got != tc.want {
			t.Errorf("sameRepositoryLocation(%q, %q) = %v, want %v", tc.configured, tc.incoming, got, tc.want)
		}
	}
}
