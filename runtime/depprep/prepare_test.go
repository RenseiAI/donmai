package depprep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- The four RED-on-revert tests of the breakdown's D1 unit. ---

// TestPrepareChildEnvironmentIsConstructed plants secret-looking and loader
// variables in this process and requires that no child sees any of them:
// every phase's environment is exactly the constructed set (ADR D4.1).
// RED if the child environment is inherited.
func TestPrepareChildEnvironmentIsConstructed(t *testing.T) {
	const sentinel = "planted-sentinel-7f3c9d"
	planted := []string{"DEPPREP_PLANTED_TOKEN", "NODE_OPTIONS", "DYLD_INSERT_LIBRARIES", "LD_PRELOAD", "NPM_TOKEN", "GIT_ASKPASS", "npm_config_userconfig"}
	for _, k := range planted {
		t.Setenv(k, sentinel)
	}
	f := newFixture(t, fakeManager{})
	p, _ := newPreparer(t, Config{Entries: StaticEntries{pnpmEntry("")}})
	rep, err := p.Prepare(context.Background(), f.request())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if rep.Outcome != OutcomeReady {
		t.Fatalf("outcome = %s, want ready", rep.Outcome)
	}
	// Only the shell that runs the fake manager may add its own bookkeeping.
	shellAdded := map[string]bool{"PWD": true, "SHLVL": true, "_": true, "OLDPWD": true}
	want := map[string]bool{
		"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "CI": true,
		"XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true, "XDG_DATA_HOME": true,
		"XDG_STATE_HOME": true, "XDG_RUNTIME_DIR": true,
		"NPM_CONFIG_STORE_DIR": true, "NPM_CONFIG_PACKAGE_IMPORT_METHOD": true,
		"npm_config_ignore_scripts": true, "npm_config_ignore_pnpmfile": true,
	}
	for _, phase := range []string{"fetch.env", "install.env"} {
		dump := f.readLog(phase)
		if dump == "" {
			t.Fatalf("%s: no environment dump", phase)
		}
		if strings.Contains(dump, sentinel) {
			// Name the keys only; never print the environment.
			var leaked []string
			for k := range f.envKeys(phase) {
				if strings.Contains(f.envValue(phase, k), sentinel) {
					leaked = append(leaked, k)
				}
			}
			t.Fatalf("%s: a planted parent variable reached the child under %v", phase, leaked)
		}
		got := f.envKeys(phase)
		for k := range shellAdded {
			delete(got, k)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s keys = %v, want exactly %v", phase, sortedKeys(got), sortedKeys(want))
		}
		if gotPath := f.envValue(phase, "PATH"); gotPath != f.toolchain+":/usr/bin:/bin" {
			t.Fatalf("%s PATH is not toolchain then /usr/bin:/bin", phase)
		}
		for _, k := range []string{"HOME", "TMPDIR", "XDG_CACHE_HOME", "NPM_CONFIG_STORE_DIR"} {
			if v := f.envValue(phase, k); !strings.HasPrefix(v, f.scratch+string(filepath.Separator)) {
				t.Fatalf("%s %s is not under Scratch", phase, k)
			}
		}
		if f.envValue(phase, "CI") != "1" {
			t.Fatalf("%s CI is not 1", phase)
		}
	}
}

// TestPrepareUnconfinedRunsNoPackageCode: with no Confiner, a dependency's
// postinstall never runs and the repository's .pnpmfile.cjs is not loaded,
// in either phase (ADR D4 rules 2 and 4). RED if no_code is not applied.
func TestPrepareUnconfinedRunsNoPackageCode(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fakeManager{})
	f.writeLeaf(".pnpmfile.cjs", "require('fs').writeFileSync('marker', '1')\n")
	p, _ := newPreparer(t, Config{Entries: StaticEntries{pnpmEntry("")}})
	rep, err := p.Prepare(context.Background(), f.request())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if f.exists("node_modules/dep/postinstall-ran") {
		t.Fatal("a dependency postinstall ran during an unconfined Prepare")
	}
	if !f.exists("node_modules/dep") {
		t.Fatal("the install did not run")
	}
	if f.readLog("pnpmfile-ran") != "" || f.readLog("fetch-pnpmfile-setting") != "true\n" {
		t.Fatal("the fetch phase did not run with repository manager hooks off")
	}
	if got := rep.Entries[0].PackageCode; got != PackageCodeSkipped {
		t.Fatalf("packageCode = %q, want skipped", got)
	}
}

// TestPrepareOnlineFloor: the online step never starts with less than the
// floor remaining; with 59 s left Prepare refuses deadline_exceeded and
// runs no process. RED if the floor is removed.
func TestPrepareOnlineFloor(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fakeManager{})
	p, counter := newPreparer(t, Config{Entries: StaticEntries{pnpmEntry("")}})
	now := time.Now()
	p.now = func() time.Time { return now }
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(59*time.Second))
	defer cancel()
	rep, err := p.Prepare(ctx, f.request())
	_ = wantCode(t, err, CodeDeadlineExceeded)
	if n := counter.n.Load(); n != 0 {
		t.Fatalf("%d processes ran with 59 s left, want 0", n)
	}
	if rep.Outcome != OutcomeFailed || rep.Entries[0].Path != PathFailed {
		t.Fatalf("report = %s / %s, want failed / failed", rep.Outcome, rep.Entries[0].Path)
	}
}

// TestPrepareNotApplicableRunsNothing: with no entry selecting the leaf,
// Prepare returns not_applicable with no_entry and a nil error, and runs
// zero processes (never a built-in command list).
func TestPrepareNotApplicableRunsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		entries StaticEntries
		mutate  func(*fixture)
	}{
		{name: "no entries", entries: nil},
		{name: "no lockfile in the leaf", entries: StaticEntries{pnpmEntry("")}, mutate: func(f *fixture) {
			if err := os.Remove(filepath.Join(f.root, "pnpm-lock.yaml")); err != nil {
				t.Fatal(err)
			}
			f.writeLeaf("package.json", `{"dependencies":{"left-pad":"1.3.0"}}`)
			f.writeLeaf("go.mod", "module x\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, fakeManager{})
			gitInit(t, f.root)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			p, counter := newPreparer(t, Config{Entries: tc.entries})
			rep, err := p.Prepare(context.Background(), f.request())
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if rep.Outcome != OutcomeNotApplicable || rep.MissReason != MissNoEntry {
				t.Fatalf("report = %s / %s, want not_applicable / no_entry", rep.Outcome, rep.MissReason)
			}
			if n := counter.n.Load(); n != 0 {
				t.Fatalf("%d processes ran for a leaf no entry selects, want 0", n)
			}
			if entries, _ := os.ReadDir(f.scratch); len(entries) != 0 {
				t.Fatalf("scratch was written for a not_applicable leaf")
			}
		})
	}
}

// --- Behaviour. ---

func TestPrepareTwoPhaseOrdering(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pin  string
		want []string
		ver  string
	}{
		// Unpinned: the toolchain's manager reports its version first.
		{name: "unpinned", want: []string{"version", "fetch", "install"}, ver: "9.15.9"},
		// Pinned: nothing runs before the online step; the provisioned
		// manager reports after the fetch.
		{name: "pinned", pin: "pnpm@10.4.1+sha512.deadbeef", want: []string{"fetch", "version", "install"}, ver: "10.4.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, fakeManager{})
			if tc.pin != "" {
				f.pin(tc.pin)
			}
			p, _ := newPreparer(t, Config{Entries: StaticEntries{pnpmEntry("")}})
			rep, err := p.Prepare(context.Background(), f.request())
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if got := f.order(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
			er := rep.Entries[0]
			if er.Version != tc.ver || er.Path != PathOnline || er.Exposure != ExposureSessionOnly {
				t.Fatalf("entry report = version %q path %s exposure %s", er.Version, er.Path, er.Exposure)
			}
			var phases []string
			for _, ph := range er.Phases {
				phases = append(phases, ph.Name)
			}
			if !reflect.DeepEqual(phases, tc.want) {
				t.Fatalf("phases = %v, want %v", phases, tc.want)
			}
			if tc.pin != "" && (!er.PinVerified || er.Pin != tc.ver) {
				t.Fatalf("pin = %q verified %v", er.Pin, er.PinVerified)
			}
			// Both phases share one per-run store, under Scratch.
			if f.envValue("fetch.env", "NPM_CONFIG_STORE_DIR") != f.envValue("install.env", "NPM_CONFIG_STORE_DIR") {
				t.Fatal("fetch and install did not share the per-run store")
			}
		})
	}
}

func TestPrepareSelectsEntryByVersion(t *testing.T) {
	t.Parallel()
	pnpm9, pnpm10 := pnpmEntry(">=9 <10"), pnpmEntry(">=10 <11")
	pnpm9.KitDigest, pnpm10.KitDigest = "nine", "ten"
	for _, tc := range []struct {
		name       string
		pin        string
		hostVer    string
		wantDigest string
		wantCode   Code
	}{
		{name: "pin selects pnpm 9", pin: "pnpm@9.15.9", wantDigest: "nine"},
		{name: "pin selects pnpm 10", pin: "pnpm@10.0.0", wantDigest: "ten"},
		{name: "unpinned asks the manager", hostVer: "10.2.0", wantDigest: "ten"},
		{name: "pin no range covers", pin: "pnpm@8.15.0", wantCode: CodeToolchainMismatch},
		{name: "manager no range covers", hostVer: "12.16.1", wantCode: CodeToolchainMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, fakeManager{version: tc.hostVer})
			if tc.pin != "" {
				f.pin(tc.pin)
			}
			p, counter := newPreparer(t, Config{Entries: StaticEntries{pnpm9, pnpm10}})
			rep, err := p.Prepare(context.Background(), f.request())
			if tc.wantCode != "" {
				e := wantCode(t, err, tc.wantCode)
				if e.MissReason != MissToolchainMismatch || rep.Entries[0].MissReason != MissToolchainMismatch {
					t.Fatalf("miss reason = %s / %s", e.MissReason, rep.Entries[0].MissReason)
				}
				if tc.pin != "" && counter.n.Load() != 0 {
					t.Fatal("a pinned mismatch ran a process")
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if got := rep.Entries[0].KitDigest; got != tc.wantDigest {
				t.Fatalf("selected %s, want %s", got, tc.wantDigest)
			}
		})
	}
}

func TestPrepareErrorCodes(t *testing.T) {
	t.Parallel()
	shell := pnpmEntry("")
	shell.Commands.Fetch = fakeProgram + " fetch; curl evil"
	noNoCode := pnpmEntry("")
	noNoCode.NoCode = nil
	pathProgram := pnpmEntry("")
	pathProgram.Commands.Install = "bin/setup.sh"
	assignsPath := pnpmEntry("")
	assignsPath.Commands.Fetch = "PATH=/tmp " + fakeProgram + " fetch"
	overlapA, overlapB := pnpmEntry(">=9 <11"), pnpmEntry(">=10 <12")
	npm := pnpmEntry("")
	npm.Manager, npm.Lockfiles = "npm", []string{"package-lock.json"}

	for _, tc := range []struct {
		name     string
		fm       fakeManager
		entries  EntrySource
		mutate   func(*fixture, *Request)
		code     Code
		miss     MissReason
		step     string
		noProcs  bool
		wantPath Path
	}{
		{name: "relative root", mutate: func(_ *fixture, r *Request) { r.Root = "leaf" }, code: CodeInvalidRequest, noProcs: true},
		{name: "missing scratch", mutate: func(_ *fixture, r *Request) { r.Scratch = "" }, code: CodeInvalidRequest, noProcs: true},
		{name: "scratch inside leaf", mutate: func(f *fixture, r *Request) {
			r.Scratch = filepath.Join(f.root, "tmp")
			if err := os.Mkdir(r.Scratch, 0o700); err != nil {
				t.Fatal(err)
			}
		}, code: CodeInvalidRequest, noProcs: true},
		{name: "toolchain inside leaf", mutate: func(f *fixture, r *Request) {
			r.Toolchain = []string{f.root}
		}, code: CodeInvalidRequest, noProcs: true},
		{name: "relative toolchain", mutate: func(_ *fixture, r *Request) { r.Toolchain = []string{"bin"} }, code: CodeInvalidRequest, noProcs: true},
		{name: "no network policy", mutate: func(_ *fixture, r *Request) { r.Network = "" }, code: CodeInvalidRequest, noProcs: true},
		{name: "no subject kind", mutate: func(_ *fixture, r *Request) { r.Subject.Kind = "" }, code: CodeInvalidRequest, noProcs: true},
		{name: "proxy env smuggles a loader setting", mutate: func(_ *fixture, r *Request) {
			r.ProxyEnv = map[string]string{"NODE_OPTIONS": "--require x"}
		}, code: CodeInvalidRequest, noProcs: true},
		{name: "entry source fails", entries: failingSource{}, code: CodeEntriesUnavailable, noProcs: true},
		{name: "shell syntax in a command", entries: StaticEntries{shell}, code: CodeEntriesUnavailable, noProcs: true},
		{name: "command program is a leaf path", entries: StaticEntries{pathProgram}, code: CodeEntriesUnavailable, noProcs: true},
		{name: "command assigns PATH", entries: StaticEntries{assignsPath}, code: CodeEntriesUnavailable, noProcs: true},
		{
			name: "no no_code and no confiner", entries: StaticEntries{noNoCode}, code: CodeEntriesUnavailable,
			miss: MissConfinementUnavailable, noProcs: true, wantPath: PathFailed,
		},
		{
			name: "overlapping ranges both cover", entries: StaticEntries{overlapA, overlapB},
			mutate: func(f *fixture, _ *Request) { f.pin("pnpm@10.1.0") }, code: CodeEntriesUnavailable, noProcs: true,
		},
		{
			name: "two managers, leaf undecided", entries: StaticEntries{pnpmEntry(""), npm},
			mutate: func(f *fixture, _ *Request) { f.writeLeaf("package-lock.json", "{}") },
			code:   CodeManagerAmbiguous, miss: MissManagerAmbiguous, noProcs: true,
		},
		{name: "manager in no toolchain directory", mutate: func(_ *fixture, r *Request) {
			r.Toolchain = nil
		}, code: CodeToolchainUnavailable, noProcs: true, wantPath: PathFailed},
		{
			name: "network denied without a keeper", mutate: func(_ *fixture, r *Request) { r.Network = NetworkDeny },
			code: CodeNetworkDenied, miss: MissStoreMissOffline, noProcs: true, wantPath: PathFailed,
		},
		{name: "fetch fails", fm: fakeManager{fetchExit: 1}, code: CodeInstallFailed, step: stepFetch, wantPath: PathFailed},
		{name: "install fails", fm: fakeManager{installExit: 2}, code: CodeInstallFailed, step: stepInstall, wantPath: PathFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, tc.fm)
			req := f.request()
			if tc.mutate != nil {
				tc.mutate(f, &req)
			}
			entries := tc.entries
			if entries == nil {
				entries = StaticEntries{pnpmEntry("")}
			}
			p, counter := newPreparer(t, Config{Entries: entries})
			rep, err := p.Prepare(context.Background(), req)
			e := wantCode(t, err, tc.code)
			if rep == nil || rep.Outcome != OutcomeFailed {
				t.Fatalf("report must be returned with the error and be failed: %+v", rep)
			}
			if e.MissReason != tc.miss {
				t.Fatalf("miss reason = %q, want %q", e.MissReason, tc.miss)
			}
			if e.Step != tc.step {
				t.Fatalf("step = %q, want %q", e.Step, tc.step)
			}
			if tc.noProcs && counter.n.Load() != 0 {
				t.Fatalf("%d processes ran, want 0", counter.n.Load())
			}
			if tc.wantPath != "" && (len(rep.Entries) != 1 || rep.Entries[0].Path != tc.wantPath) {
				t.Fatalf("entries = %+v, want one with path %s", rep.Entries, tc.wantPath)
			}
			if tc.code == CodeInstallFailed && rep.Entries[0].Diagnostics == "" && tc.step == stepInstall {
				t.Fatal("a failed install carries no diagnostics")
			}
		})
	}
}

type failingSource struct{}

func (failingSource) Entries(context.Context, Leaf) ([]Entry, error) {
	return nil, errors.New("kit composition conflict")
}

func TestPrepareTimeBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		left      time.Duration
		noCtxDead bool
		wantErr   bool
	}{
		{name: "exactly the floor starts", left: OnlineFloor},
		{name: "a second under the floor refuses", left: OnlineFloor - time.Second, wantErr: true},
		{name: "a seven-minute budget less a three-minute clone", left: 4 * time.Minute},
		{name: "no context deadline uses the default budget", noCtxDead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, fakeManager{})
			p, counter := newPreparer(t, Config{Entries: StaticEntries{pnpmEntry("")}})
			now := time.Now()
			p.now = func() time.Time { return now }
			ctx := context.Background()
			deadline := now.Add(DefaultBudget)
			if !tc.noCtxDead {
				deadline = now.Add(tc.left)
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, deadline)
				defer cancel()
			}
			_, err := p.Prepare(ctx, f.request())
			if tc.wantErr {
				_ = wantCode(t, err, CodeDeadlineExceeded)
				return
			}
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			// Every process ran under the whole deadline less the reserve.
			counter.mu.Lock()
			defer counter.mu.Unlock()
			if len(counter.deadlines) == 0 {
				t.Fatal("no process deadline recorded")
			}
			for _, d := range counter.deadlines {
				if !d.Equal(deadline.Add(-Reserve)) {
					t.Fatalf("step deadline = %s before the budget end, want the %s reserve", deadline.Sub(d), Reserve)
				}
			}
		})
	}
}

// TestPrepareKillsAtStepDeadline: a hung install is killed at the step
// deadline (the whole deadline less the reserve) and reported
// deadline_exceeded, leaving the reserve for the report.
func TestPrepareKillsAtStepDeadline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fakeManager{installSleep: 30})
	p, _ := newPreparer(t, Config{Entries: StaticEntries{pnpmEntry("")}})
	start := time.Now()
	// The floor reads a clock a minute behind, so it sees the floor; the
	// real deadline leaves the step two seconds before the reserve.
	p.now = func() time.Time { return time.Now().Add(-OnlineFloor) }
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(17*time.Second))
	defer cancel()
	rep, err := p.Prepare(ctx, f.request())
	e := wantCode(t, err, CodeDeadlineExceeded)
	if e.Step != stepInstall {
		t.Fatalf("step = %q, want install", e.Step)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Prepare returned after %s; the hung install was not killed at the step deadline", elapsed)
	}
	if rep.Entries[0].Path != PathFailed {
		t.Fatalf("path = %s, want failed", rep.Entries[0].Path)
	}
}

func TestPrepareReadOnlyIsSkipped(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fakeManager{})
	p, counter := newPreparer(t, Config{Entries: StaticEntries{pnpmEntry("")}})
	req := f.request()
	req.ReadOnly = true
	rep, err := p.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if rep.Outcome != OutcomeSkipped || len(rep.Entries) != 1 || rep.Entries[0].Path != PathSkipped {
		t.Fatalf("report = %+v, want skipped", rep)
	}
	if counter.n.Load() != 0 {
		t.Fatal("a read-only leaf ran a process")
	}
}

// fakeConfiner records what it confines and marks the process.
type fakeConfiner struct {
	calls []ConfineRequest
}

func (c *fakeConfiner) Confine(_ context.Context, req ConfineRequest) (ConfinedCommand, error) {
	c.calls = append(c.calls, req)
	return ConfinedCommand{Argv: req.Argv, Env: append(append([]string{}, req.Env...), "DEPPREP_CONFINED=1")}, nil
}

func TestPrepareConfinerWrapsOnlyTheInstall(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fakeManager{})
	confiner := &fakeConfiner{}
	noNoCode := pnpmEntry("")
	noNoCode.NoCode = nil
	for _, entry := range []Entry{pnpmEntry(""), noNoCode} {
		confiner.calls = nil
		p, _ := newPreparer(t, Config{Entries: StaticEntries{entry}, Confiner: confiner})
		rep, err := p.Prepare(context.Background(), f.request())
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if len(confiner.calls) != 1 {
			t.Fatalf("confiner called %d times, want once (install only)", len(confiner.calls))
		}
		call := confiner.calls[0]
		if !reflect.DeepEqual(call.Writable[:1], []string{f.root}) || len(call.Writable) != 2 ||
			!strings.HasPrefix(call.Writable[1], f.scratch+string(filepath.Separator)) {
			t.Fatalf("writable set is not the leaf and the per-run scratch")
		}
		if f.envValue("install.env", "DEPPREP_CONFINED") != "1" || f.envValue("fetch.env", "DEPPREP_CONFINED") != "" {
			t.Fatal("the confiner did not wrap exactly the install phase")
		}
		if f.envKeys("install.env")["npm_config_ignore_scripts"] {
			t.Fatal("a confined install still ran with package code off")
		}
		if !f.envKeys("fetch.env")["npm_config_ignore_scripts"] && entry.NoCode != nil {
			t.Fatal("the fetch phase ran with package code on")
		}
		if !f.exists("node_modules/dep/postinstall-ran") {
			t.Fatal("the confined install did not run the allowlisted postinstall")
		}
		if rep.Entries[0].PackageCode != PackageCodeConfined {
			t.Fatalf("packageCode = %s, want confined", rep.Entries[0].PackageCode)
		}
		if err := os.RemoveAll(filepath.Join(f.root, "node_modules")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrepareLeafHygieneAndRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		fm        fakeManager
		subject   SubjectKind
		status    LeafCheckStatus
		undeclare []string
	}{
		{name: "invocation clean", subject: SubjectInvocation, status: LeafCheckClean},
		{name: "session clean", subject: SubjectSession, status: LeafCheckClean},
		{name: "stray file named", fm: fakeManager{stray: true}, subject: SubjectInvocation, status: LeafCheckDirty, undeclare: []string{"stray.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, tc.fm)
			gitInit(t, f.root)
			gitDir, err := lookGitDir()
			if err != nil {
				t.Fatal(err)
			}
			p, _ := newPreparer(t, Config{Entries: StaticEntries{pnpmEntry("")}})
			req := f.request()
			req.Subject.Kind = tc.subject
			req.Toolchain = append(req.Toolchain, gitDir)
			rep, err := p.Prepare(context.Background(), req)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if rep.Leaf.Status != tc.status || !reflect.DeepEqual(rep.Leaf.Undeclared, tc.undeclare) {
				t.Fatalf("leaf = %+v, want %s %v", rep.Leaf, tc.status, tc.undeclare)
			}
			// The install record lives in the report only; nothing is
			// written into the leaf's reserved metadata.
			if f.exists(".workarea") {
				t.Fatal("an install record was written into the leaf")
			}
			rec := rep.Record
			if rec.Schema != InstallRecordSchema || rec.SubjectKind != tc.subject || len(rec.Entries) != 1 {
				t.Fatalf("record = %+v", rec)
			}
			re := rec.Entries[0]
			if re.Manager != "pnpm" || re.Path != PathOnline || re.Exposure != ExposureSessionOnly ||
				re.LockfileDigest == "" || !reflect.DeepEqual(re.InstalledPaths, []string{"node_modules"}) {
				t.Fatalf("record entry = %+v", re)
			}
			if rep.SubjectDigest == "" || strings.Contains(rep.SubjectDigest, "inv-1") {
				t.Fatal("the subject id is not reported as a digest")
			}
		})
	}
}

func lookGitDir() (string, error) {
	for _, dir := range []string{"/usr/bin", "/bin"} {
		if _, ok := lookProgram("git", []string{dir}); ok {
			return dir, nil
		}
	}
	p, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

func TestNewRefusesUnsupportedConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{name: "no entry source", cfg: Config{}},
		{name: "credentials before delivery exists", cfg: Config{Entries: StaticEntries{}, Credentials: func(context.Context, string, string) (Registry, error) {
			return Registry{}, nil
		}}},
	} {
		if _, err := New(tc.cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("%s: New err = %v, want ErrInvalidConfig", tc.name, err)
		}
	}
}

func TestErrorCodesAreClosed(t *testing.T) {
	t.Parallel()
	if len(Codes()) != 9 || len(sentinels) != 9 {
		t.Fatalf("the closed set has %d codes and %d sentinels, want 9", len(Codes()), len(sentinels))
	}
	for _, c := range Codes() {
		err := error(&Error{Code: c})
		if !errors.Is(err, sentinels[c]) || CodeOf(err) != c {
			t.Fatalf("code %s does not round-trip", c)
		}
		for _, other := range Codes() {
			if other != c && errors.Is(err, sentinels[other]) {
				t.Fatalf("code %s matches the sentinel of %s", c, other)
			}
		}
	}
	if CodeOf(&Error{Code: "invented"}) != CodeInstallFailed || CodeOf(errors.New("x")) != CodeInstallFailed || CodeOf(nil) != "" {
		t.Fatal("an unknown code must read as install_failed")
	}
}

func TestRegistryNeverFormatsItsContent(t *testing.T) {
	t.Parallel()
	const token = "secret-token-value"
	r := Registry{Config: []byte("//registry.example/:_authToken=" + token)}
	for _, s := range []string{r.String(), r.GoString(), fmt.Sprintf("%v %+v %#v %s", r, r, r, r), r.LogValue().String()} {
		if strings.Contains(s, token) {
			t.Fatal("a Registry formatted its credential")
		}
	}
}

func TestParseCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line    string
		program string
		args    []string
		assign  map[string]string
		bad     bool
	}{
		{line: "pnpm fetch", program: "pnpm", args: []string{"fetch"}, assign: map[string]string{}},
		{
			line: "GOPROXY=off GOFLAGS=-mod=mod go mod download", program: "go", args: []string{"mod", "download"},
			assign: map[string]string{"GOPROXY": "off", "GOFLAGS": "-mod=mod"},
		},
		{line: "pip download --only-binary=:all:", program: "pip", args: []string{"download", "--only-binary=:all:"}, assign: map[string]string{}},
		{line: "", bad: true},
		{line: "A=b", bad: true},
		{line: "pnpm install && rm -rf /", bad: true},
		{line: "pnpm $(evil)", bad: true},
		{line: "./bin/setup.sh", bad: true},
		{line: "LD_PRELOAD=x pnpm fetch", bad: true},
		{line: "HOME=/root pnpm fetch", bad: true},
		{line: "HTTPS_PROXY=x pnpm fetch", bad: true},
	} {
		c, err := parseCommand(tc.line)
		if tc.bad {
			if err == nil {
				t.Fatalf("parseCommand(%q) accepted", tc.line)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseCommand(%q): %v", tc.line, err)
		}
		if c.program != tc.program || !reflect.DeepEqual(c.args, tc.args) || !reflect.DeepEqual(c.assign, tc.assign) {
			t.Fatalf("parseCommand(%q) = %+v", tc.line, c)
		}
	}
}

func TestReadLeafPin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		decl string
		want leafPin
	}{
		{decl: "pnpm@9.15.9", want: leafPin{manager: "pnpm", version: "9.15.9", raw: "pnpm@9.15.9"}},
		{decl: "pnpm@9.15.9+sha512.abc", want: leafPin{manager: "pnpm", version: "9.15.9", verified: true, raw: "pnpm@9.15.9+sha512.abc"}},
		{decl: "yarn", want: leafPin{manager: "yarn", raw: "yarn"}},
		{decl: "pnpm@latest", want: leafPin{manager: "pnpm", raw: "pnpm@latest"}},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"packageManager":"`+tc.decl+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readLeafPin(dir); got != tc.want {
			t.Fatalf("readLeafPin(%q) = %+v, want %+v", tc.decl, got, tc.want)
		}
	}
	if got := readLeafPin(t.TempDir()); got != (leafPin{}) {
		t.Fatalf("no package.json declared %+v", got)
	}
}

func TestMatchInstalled(t *testing.T) {
	t.Parallel()
	patterns := []string{"node_modules", "**/node_modules"}
	for p, want := range map[string]bool{
		"node_modules":                true,
		"packages/a/node_modules":     true,
		"node_modules_extra":          false,
		"packages/a/node_modules/x":   false,
		"stray.txt":                   false,
		"packages/node_modules/a/b/c": false,
	} {
		if got := matchesAny(patterns, p); got != want {
			t.Fatalf("matchesAny(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestPrepareFailedInstallKeepsInstalledPathsDeclared: a failed install
// that already created node_modules still reports it as a declared,
// ignored path, not as an undeclared change.
func TestPrepareFailedInstallKeepsInstalledPathsDeclared(t *testing.T) {
	t.Parallel()
	f := newFixture(t, fakeManager{installExit: 1})
	gitInit(t, f.root)
	gitDir, err := lookGitDir()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := newPreparer(t, Config{Entries: StaticEntries{pnpmEntry("")}})
	req := f.request()
	req.Toolchain = append(req.Toolchain, gitDir)
	rep, err := p.Prepare(context.Background(), req)
	_ = wantCode(t, err, CodeInstallFailed)
	if !f.exists("node_modules") {
		t.Fatal("the failing install did not create node_modules")
	}
	if rep.Leaf.Status != LeafCheckClean {
		t.Fatalf("leaf = %+v, want clean", rep.Leaf)
	}
	if !strings.Contains(rep.Entries[0].Diagnostics, "install output line") {
		t.Fatal("the failed install's output tail is missing")
	}
}
