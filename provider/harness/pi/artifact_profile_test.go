package pi

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const frozenArtifactEnv = "DONMAI_PI_PROFILE_TEST_ARTIFACT"

func frozenArtifactRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv(frozenArtifactEnv)
	if root == "" {
		t.Skip("NOT RUN: exact profiled Pi artifact is unavailable; set $" + frozenArtifactEnv)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve $%s: %v", frozenArtifactEnv, err)
	}
	return root
}

func copyArtifact(t *testing.T) string {
	t.Helper()
	sourceRoot := frozenArtifactRoot(t)
	root := filepath.Join(t.TempDir(), "pi")
	err := filepath.WalkDir(sourceRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(sourceRoot, path)
		dst := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		in, err := os.Open(path) //nolint:gosec // G122: frozen, trusted test fixture copied into an isolated TempDir.
		if err != nil {
			return err
		}
		info, _ := d.Info()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, info.Mode())
		if err != nil {
			_ = in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		inCloseErr := in.Close()
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if inCloseErr != nil {
			return inCloseErr
		}
		return closeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func oldFrozenArtifactRoot(t *testing.T) string {
	t.Helper()
	candidateArtifactDir := filepath.Dir(filepath.Dir(filepath.Dir(frozenArtifactRoot(t))))
	return filepath.Join(
		filepath.Dir(candidateArtifactDir),
		"artifact-profile-prototype", "runtime", "profiled-darwin-arm64", "pi",
	)
}

func TestOldFrozenArtifactNoLongerSelectsReceiptProfile(t *testing.T) {
	oldRoot := oldFrozenArtifactRoot(t)
	if _, err := os.Stat(filepath.Join(oldRoot, "pi")); err != nil {
		t.Fatalf("old frozen control is unavailable: %v", err)
	}
	lease, err := measureArtifactProfile(filepath.Join(oldRoot, "pi"))
	if err == nil || lease != nil {
		t.Fatalf("old artifact selected replacement receipt profile: lease=%v err=%v", lease, err)
	}
}

func TestArtifactProfileFrozenTreeAndTamperControls(t *testing.T) {
	root := copyArtifact(t)
	binary := filepath.Join(root, "pi")
	lease, err := measureArtifactProfile(binary)
	if err != nil || lease == nil {
		t.Fatalf("positive profile: %v", err)
	}
	if err = lease.revalidate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.close() })
	if _, err = measureArtifactProfile(filepath.Join(t.TempDir(), "pi")); err != nil {
		t.Fatalf("missing sidecar must stay legacy: %v", err)
	}
	for _, mutate := range []struct {
		name string
		fn   func(string)
	}{
		{"extra", func(r string) { _ = os.WriteFile(filepath.Join(r, "extra"), []byte("x"), 0o600) }},
		{"byte", func(r string) {
			f, _ := os.OpenFile(filepath.Join(r, "README.md"), os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.Write([]byte("x"))
			_ = f.Close()
		}},
		{"mode", func(r string) { _ = os.Chmod(filepath.Join(r, "README.md"), 0o600) }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			r := copyArtifact(t)
			mutate.fn(r)
			if _, err := measureArtifactProfile(filepath.Join(r, "pi")); err == nil {
				t.Fatal("tamper selected profile")
			}
		})
	}
	for _, mutate := range []struct {
		name string
		fn   func(string) error
	}{
		{"symlink", func(r string) error { return os.Symlink("README.md", filepath.Join(r, "linked")) }},
		{"hardlink", func(r string) error { return os.Link(filepath.Join(r, "README.md"), filepath.Join(r, "linked")) }},
		{"listed-hardlink-outside-root", func(r string) error {
			return os.Link(filepath.Join(r, "README.md"), filepath.Join(filepath.Dir(r), "outside-link"))
		}},
		{"listed-symlink", func(r string) error {
			old := filepath.Join(r, "README.md")
			if err := os.Rename(old, filepath.Join(filepath.Dir(r), "README.original")); err != nil {
				return err
			}
			return os.Symlink("CHANGELOG.md", old)
		}},
		{"unknown-profile", func(r string) error {
			path := filepath.Join(r, "artifact-profile.json")
			if err := os.Chmod(path, 0o600); err != nil {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var profile map[string]any
			if err := json.Unmarshal(raw, &profile); err != nil {
				return err
			}
			profile["profileId"] = "pi/unknown/v1"
			updated, err := json.Marshal(profile)
			if err != nil {
				return err
			}
			return os.WriteFile(path, append(updated, '\n'), 0o600)
		}},
		{"sidecar-symlink", func(r string) error {
			path := filepath.Join(r, "artifact-profile.json")
			if err := os.Rename(path, filepath.Join(filepath.Dir(r), "sidecar.original")); err != nil {
				return err
			}
			return os.Symlink("README.md", path)
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			r := copyArtifact(t)
			if err := mutate.fn(r); err != nil {
				t.Fatal(err)
			}
			if _, err := measureArtifactProfile(filepath.Join(r, "pi")); err == nil {
				t.Fatal("tamper selected profile")
			}
		})
	}
	old := filepath.Join(root, "README.md")
	replacement := filepath.Join(root, "README.new")
	if err = os.Rename(old, replacement); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(old, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = lease.revalidate(); err == nil {
		t.Fatal("inode replacement accepted")
	}
	root2 := copyArtifact(t)
	lease2, err := measureArtifactProfile(filepath.Join(root2, "pi"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease2.close() })
	moved := root2 + ".moved"
	if err = os.Rename(root2, moved); err != nil {
		t.Fatal(err)
	}
	if err = lease2.revalidate(); err == nil {
		t.Fatal("root replacement accepted")
	}
}
