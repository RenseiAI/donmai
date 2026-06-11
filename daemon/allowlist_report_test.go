package daemon

import (
	"reflect"
	"testing"
)

func TestAllowlistEntriesFromConfig(t *testing.T) {
	t.Parallel()

	t.Run("nil and empty input return nil", func(t *testing.T) {
		t.Parallel()
		if got := AllowlistEntriesFromConfig(nil); got != nil {
			t.Errorf("nil input returned %v, want nil", got)
		}
		if got := AllowlistEntriesFromConfig([]ProjectConfig{}); got != nil {
			t.Errorf("empty input returned %v, want nil", got)
		}
	})

	t.Run("sorts by id for stable hashing", func(t *testing.T) {
		t.Parallel()
		in := []ProjectConfig{
			{ID: "zebra", Repository: "github.com/x/zebra"},
			{ID: "alpha", Repository: "github.com/x/alpha"},
			{ID: "middle", Repository: "github.com/x/middle"},
		}
		got := AllowlistEntriesFromConfig(in)
		want := []ProjectAllowlistEntry{
			{ID: "alpha", Repository: "github.com/x/alpha"},
			{ID: "middle", Repository: "github.com/x/middle"},
			{ID: "zebra", Repository: "github.com/x/zebra"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("skips entries missing id or repository", func(t *testing.T) {
		t.Parallel()
		in := []ProjectConfig{
			{ID: "ok", Repository: "github.com/x/ok"},
			{ID: "", Repository: "github.com/x/no-id"},
			{ID: "no-repo", Repository: ""},
			{ID: "", Repository: ""},
		}
		got := AllowlistEntriesFromConfig(in)
		want := []ProjectAllowlistEntry{{ID: "ok", Repository: "github.com/x/ok"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("trims clone/git fields from the wire payload", func(t *testing.T) {
		t.Parallel()
		in := []ProjectConfig{
			{
				ID:            "alpha",
				Repository:    "github.com/x/alpha",
				CloneStrategy: "shallow",
				Git:           &ProjectGit{},
			},
		}
		got := AllowlistEntriesFromConfig(in)
		want := []ProjectAllowlistEntry{{ID: "alpha", Repository: "github.com/x/alpha"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestAllowlistHash(t *testing.T) {
	t.Parallel()

	t.Run("empty input returns empty string", func(t *testing.T) {
		t.Parallel()
		if got := allowlistHash(nil); got != "" {
			t.Errorf("nil input returned %q, want empty", got)
		}
		if got := allowlistHash([]ProjectAllowlistEntry{}); got != "" {
			t.Errorf("empty input returned %q, want empty", got)
		}
	})

	t.Run("deterministic across calls", func(t *testing.T) {
		t.Parallel()
		entries := []ProjectAllowlistEntry{
			{ID: "alpha", Repository: "github.com/x/alpha"},
			{ID: "beta", Repository: "github.com/x/beta"},
		}
		if a, b := allowlistHash(entries), allowlistHash(entries); a != b {
			t.Errorf("hash not deterministic: %q vs %q", a, b)
		}
	})

	t.Run("different entries hash differently", func(t *testing.T) {
		t.Parallel()
		a := allowlistHash([]ProjectAllowlistEntry{{ID: "alpha", Repository: "github.com/x/alpha"}})
		b := allowlistHash([]ProjectAllowlistEntry{{ID: "alpha", Repository: "github.com/x/beta"}})
		if a == b {
			t.Errorf("expected different hashes for different repositories, both %q", a)
		}
		c := allowlistHash([]ProjectAllowlistEntry{{ID: "beta", Repository: "github.com/x/alpha"}})
		if a == c {
			t.Errorf("expected different hashes for different ids, both %q", a)
		}
	})

	t.Run("field separator prevents id|repo collisions", func(t *testing.T) {
		t.Parallel()
		// Without a separator, ("ab", "c") and ("a", "bc") would hash the same.
		a := allowlistHash([]ProjectAllowlistEntry{{ID: "ab", Repository: "c"}})
		b := allowlistHash([]ProjectAllowlistEntry{{ID: "a", Repository: "bc"}})
		if a == b {
			t.Error("hash collision between (ab,c) and (a,bc) — separator missing or weak")
		}
	})
}

func TestNormalizeAllowlistKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   ProjectAllowlistEntry
		want string
	}{
		{ProjectAllowlistEntry{ID: "Alpha", Repository: "github.com/x/alpha.git"}, "alpha@github.com/x/alpha"},
		{ProjectAllowlistEntry{ID: "alpha", Repository: "github.com/x/alpha"}, "alpha@github.com/x/alpha"},
	}
	for _, c := range cases {
		if got := normalizeAllowlistKey(c.in); got != c.want {
			t.Errorf("normalizeAllowlistKey(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
