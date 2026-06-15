package landing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeFS is a programmable lockFS for lock-file tests.
type fakeFS struct {
	mu sync.Mutex
	// files maps a path to its content. Absence means stat/readFile fail.
	files map[string]string
	// statErr / readErr override default behaviour when set.
	statErr error
	// written records the last writeFile(path, content).
	wroteContent map[string]string
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string]string{}, wroteContent: map[string]string{}}
}

func (f *fakeFS) stat(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statErr != nil {
		return f.statErr
	}
	if _, ok := f.files[path]; ok {
		return nil
	}
	return errors.New("not exist")
}

func (f *fakeFS) readFile(path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.files[path]; ok {
		return c, nil
	}
	return "", errors.New("not exist")
}

func (f *fakeFS) writeFile(path, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = content
	f.wroteContent[path] = content
	return nil
}

func (f *fakeFS) lastWrite(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.wroteContent[path]
	return c, ok
}

func TestLockFileRegeneration_ShouldRegenerate(t *testing.T) {
	l := NewLockFileRegeneration()
	tests := []struct {
		pm   PackageManager
		flag bool
		want bool
	}{
		{PMPnpm, true, true},
		{PMNpm, true, true},
		{PMYarn, true, true},
		{PMBun, true, true},
		{PMPnpm, false, false},
		{PMNone, false, false},
		{PMNone, true, false},
	}
	for _, tt := range tests {
		if got := l.ShouldRegenerate(tt.pm, tt.flag); got != tt.want {
			t.Errorf("ShouldRegenerate(%q,%v) = %v, want %v", tt.pm, tt.flag, got, tt.want)
		}
	}
}

func TestLockFileRegeneration_LockFileName(t *testing.T) {
	l := NewLockFileRegeneration()
	tests := []struct {
		pm   PackageManager
		want string
	}{
		{PMPnpm, "pnpm-lock.yaml"},
		{PMNpm, "package-lock.json"},
		{PMYarn, "yarn.lock"},
		{PMBun, "bun.lockb"},
		{PMNone, ""},
	}
	for _, tt := range tests {
		if got := l.LockFileName(tt.pm); got != tt.want {
			t.Errorf("LockFileName(%q) = %q, want %q", tt.pm, got, tt.want)
		}
	}
}

func TestLockFileRegeneration_Regenerate(t *testing.T) {
	tests := []struct {
		name        string
		pm          PackageManager
		lockExists  bool
		reply       func(name string, args []string) (string, error)
		wantSuccess bool
		wantLock    string
		wantErrSub  string
		wantInstall string // expected install command line
		wantCalls   int    // total runner calls
	}{
		{
			name:        "pnpm deletes, installs, stages",
			pm:          PMPnpm,
			lockExists:  true,
			wantSuccess: true,
			wantLock:    "pnpm-lock.yaml",
			wantInstall: "pnpm install --no-frozen-lockfile",
			wantCalls:   3,
		},
		{
			name:        "npm uses plain install",
			pm:          PMNpm,
			lockExists:  true,
			wantSuccess: true,
			wantLock:    "package-lock.json",
			wantInstall: "npm install",
			wantCalls:   3,
		},
		{
			name:        "yarn uses plain install",
			pm:          PMYarn,
			lockExists:  true,
			wantSuccess: true,
			wantLock:    "yarn.lock",
			wantInstall: "yarn install",
			wantCalls:   3,
		},
		{
			name:        "bun uses plain install",
			pm:          PMBun,
			lockExists:  true,
			wantSuccess: true,
			wantLock:    "bun.lockb",
			wantInstall: "bun install",
			wantCalls:   3,
		},
		{
			name:       "install failure is surfaced (not an error return)",
			pm:         PMPnpm,
			lockExists: true,
			reply:      errReply("install", "ENOENT: pnpm not found", ""),
			wantLock:   "pnpm-lock.yaml",
			wantErrSub: "pnpm not found",
			wantCalls:  2, // rm succeeds, install fails (no git add)
		},
		{
			name:        "missing lock file skips delete",
			pm:          PMPnpm,
			lockExists:  false,
			wantSuccess: true,
			wantLock:    "pnpm-lock.yaml",
			wantInstall: "pnpm install --no-frozen-lockfile",
			wantCalls:   2, // install + git add (rm skipped)
		},
		{
			name:       "none is unsupported",
			pm:         PMNone,
			wantLock:   "",
			wantErrSub: "Unsupported package manager",
			wantCalls:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{reply: tt.reply}
			ffs := newFakeFS()
			if tt.lockExists {
				lockName := map[PackageManager]string{
					PMPnpm: "pnpm-lock.yaml", PMNpm: "package-lock.json",
					PMYarn: "yarn.lock", PMBun: "bun.lockb",
				}[tt.pm]
				ffs.files["/tmp/wt/"+lockName] = "old"
			}
			l := &LockFileRegeneration{runner: fr, fs: ffs}

			res, err := l.Regenerate(context.Background(), "/tmp/wt", tt.pm)
			if err != nil {
				t.Fatalf("Regenerate returned error: %v", err)
			}
			if res.Success != tt.wantSuccess {
				t.Errorf("Success = %v, want %v", res.Success, tt.wantSuccess)
			}
			if res.LockFile != tt.wantLock {
				t.Errorf("LockFile = %q, want %q", res.LockFile, tt.wantLock)
			}
			if res.PackageManager != tt.pm {
				t.Errorf("PackageManager = %q, want %q", res.PackageManager, tt.pm)
			}
			if tt.wantErrSub != "" && !strings.Contains(res.Error, tt.wantErrSub) {
				t.Errorf("Error = %q, want it to contain %q", res.Error, tt.wantErrSub)
			}
			if tt.wantErrSub == "" && res.Error != "" {
				t.Errorf("unexpected Error = %q", res.Error)
			}
			if len(fr.calls) != tt.wantCalls {
				t.Fatalf("runner call count = %d, want %d (%v)", len(fr.calls), tt.wantCalls, fr.commandLines())
			}
			if tt.wantInstall != "" {
				if !commandLinesContain(fr.commandLines(), tt.wantInstall) {
					t.Errorf("commands %v do not contain install %q", fr.commandLines(), tt.wantInstall)
				}
				// First call (when lock exists) is the rm; otherwise install.
				if tt.lockExists {
					if got := fr.calls[0].commandLine(); !strings.HasPrefix(got, "rm ") {
						t.Errorf("first call = %q, want it to start with 'rm '", got)
					}
					if got := fr.calls[len(fr.calls)-1].commandLine(); !strings.HasPrefix(got, "git add ") {
						t.Errorf("last call = %q, want 'git add ...'", got)
					}
				}
			}
		})
	}
}

func TestLockFileRegeneration_EnsureGitAttributes(t *testing.T) {
	const path = "/repo/.gitattributes"
	tests := []struct {
		name        string
		pm          PackageManager
		existing    string // "" + notPresent ⇒ file absent
		filePresent bool
		wantWrite   bool
		wantContent string
	}{
		{
			name:        "creates file when absent",
			pm:          PMPnpm,
			filePresent: false,
			wantWrite:   true,
			wantContent: "pnpm-lock.yaml merge=ours\n",
		},
		{
			name:        "appends entry preserving trailing newline",
			pm:          PMPnpm,
			existing:    "*.md linguist-documentation\n",
			filePresent: true,
			wantWrite:   true,
			wantContent: "*.md linguist-documentation\npnpm-lock.yaml merge=ours\n",
		},
		{
			name:        "adds newline separator when file lacks trailing newline",
			pm:          PMNpm,
			existing:    "*.md linguist-documentation",
			filePresent: true,
			wantWrite:   true,
			wantContent: "*.md linguist-documentation\npackage-lock.json merge=ours\n",
		},
		{
			name:        "skips when entry already present",
			pm:          PMPnpm,
			existing:    "pnpm-lock.yaml merge=ours\n",
			filePresent: true,
			wantWrite:   false,
		},
		{
			name:        "none is a no-op",
			pm:          PMNone,
			filePresent: false,
			wantWrite:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ffs := newFakeFS()
			if tt.filePresent {
				ffs.files[path] = tt.existing
			}
			l := &LockFileRegeneration{runner: &fakeRunner{}, fs: ffs}

			if err := l.EnsureGitAttributes(context.Background(), "/repo", tt.pm); err != nil {
				t.Fatalf("EnsureGitAttributes error: %v", err)
			}
			got, wrote := ffs.lastWrite(path)
			if wrote != tt.wantWrite {
				t.Fatalf("wrote = %v, want %v", wrote, tt.wantWrite)
			}
			if tt.wantWrite && got != tt.wantContent {
				t.Errorf("written content = %q, want %q", got, tt.wantContent)
			}
		})
	}
}

func TestLockFileRegeneration_EnsureGitAttributes_AllManagers(t *testing.T) {
	const path = "/repo/.gitattributes"
	want := map[PackageManager]string{
		PMPnpm: "pnpm-lock.yaml merge=ours\n",
		PMNpm:  "package-lock.json merge=ours\n",
		PMYarn: "yarn.lock merge=ours\n",
		PMBun:  "bun.lockb merge=ours\n",
	}
	for pm, expected := range want {
		ffs := newFakeFS() // file absent
		l := &LockFileRegeneration{runner: &fakeRunner{}, fs: ffs}
		if err := l.EnsureGitAttributes(context.Background(), "/repo", pm); err != nil {
			t.Fatalf("EnsureGitAttributes(%q) error: %v", pm, err)
		}
		got, wrote := ffs.lastWrite(path)
		if !wrote || got != expected {
			t.Errorf("%q wrote %q (wrote=%v), want %q", pm, got, wrote, expected)
		}
	}
}

func commandLinesContain(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
