//go:build darwin || linux

package tempdoctor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/tempown"
)

func reapedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func mkdirFiles(t *testing.T, dir string, files int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range files {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)), []byte("0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixtureRoot builds a temp folder holding one of every class.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdirFiles(t, filepath.Join(root, "tsx-501"), 150)
	mkdirFiles(t, filepath.Join(root, "node-compile-cache", "v26"), 5)
	mkdirFiles(t, filepath.Join(root, "go-build1234"), 2)
	touch(t, filepath.Join(root, ".com.openai.codex.Ab12"))
	mkdirFiles(t, filepath.Join(root, "jiti"), 1)
	mkdirFiles(t, filepath.Join(root, "donmai-pi-ext-cache"), 1)
	home := filepath.Join(root, "donmai-codex-home-1")
	mkdirFiles(t, home, 0)
	touch(t, filepath.Join(home, "config.toml"))
	touch(t, filepath.Join(home, ".donmai-owner.json"))
	mkdirFiles(t, filepath.Join(root, tempown.PrefixClaudeUsage+"p"+strconv.Itoa(reapedPID(t))+"-1"), 1)
	mkdirFiles(t, filepath.Join(root, tempown.PrefixSignalGuard+"p"+strconv.Itoa(os.Getpid())+"-2"), 1)
	mkdirFiles(t, filepath.Join(root, tempown.PrefixClaudeLogin+"3920481723"), 1)
	mkdirFiles(t, filepath.Join(root, "tmp.zLzZSyMFe"), 3)
	return root
}

func groupByLabel(r *Report, label string) *Group {
	for i := range r.Groups {
		if r.Groups[i].Label == label {
			return &r.Groups[i]
		}
	}
	return nil
}

func TestScanReportsEveryProducerClass(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t)
	report, err := Scan(context.Background(), Options{Root: root, DirSample: 50, FlatThreshold: 100})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := map[string]Class{
		"tsx cache":            ClassToolCache,
		"Node compile cache":   ClassToolCache,
		"Go build work dirs":   ClassToolCache,
		"Codex CLI temp files": ClassToolCache,
		"jiti cache":           ClassToolCache,
		"pi extension cache":   ClassDaemonCache,
		"codex homes":          ClassCodex,
		"Claude usage probe":   ClassDaemon,
		"signal guard":         ClassDaemon,
		"Claude login probe":   ClassDaemon,
		"other":                ClassOther,
	}
	for label, class := range want {
		g := groupByLabel(report, label)
		if g == nil || g.Class != class || g.Items != 1 || !g.Measured {
			t.Errorf("group %q = %+v, want class %s with 1 measured item", label, g, class)
		}
	}
	if g := groupByLabel(report, "Claude usage probe"); g.Verdicts[tempown.VerdictYoung] != 1 {
		t.Errorf("dead-owner probe verdicts = %v, want young (inside the grace period)", g.Verdicts)
	}
	if g := groupByLabel(report, "signal guard"); g.Verdicts[tempown.VerdictLive] != 1 {
		t.Errorf("own-pid guard verdicts = %v, want live", g.Verdicts)
	}
	if g := groupByLabel(report, "Claude login probe"); g.Verdicts[tempown.VerdictLegacy] != 1 {
		t.Errorf("untagged probe verdicts = %v, want legacy", g.Verdicts)
	}
	if g := groupByLabel(report, "codex homes"); g.Shapes[providercodex.HomeShapeNeverUsed] != 1 {
		t.Errorf("codex shapes = %v, want one never-used", g.Shapes)
	}
	tsx := groupByLabel(report, "tsx cache")
	if tsx.Entries < 51 || !(tsx.Estimated || tsx.LowerBound) {
		t.Errorf("tsx cache = %+v, want a sampled count past the 50-entry sample", tsx)
	}
	if hint := childCountHint(root); hint != nil {
		if tsx.Entries != 151 {
			t.Errorf("tsx cache entries = %d, want 151 from the filesystem child count", tsx.Entries)
		}
		found := false
		for _, f := range report.FlatDirs {
			found = found || f.Path == filepath.Join(root, "tsx-501")
		}
		if !found {
			t.Errorf("flat dirs = %+v, want tsx-501 flagged", report.FlatDirs)
		}
	}
	if report.Partial {
		t.Errorf("unexpected partial report: %s", report.PartialReason)
	}
	var out bytes.Buffer
	if err := report.WriteText(&out); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"tool-cache", "tsx-*", "1 young", "1 live", "1 legacy", "1 never-used", "Boot cleanup:"} {
		if !strings.Contains(out.String(), needle) {
			t.Errorf("text report lacks %q:\n%s", needle, out.String())
		}
	}
}

func TestScanSamplesLargeGroupsAndExtrapolates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for i := range 40 {
		mkdirFiles(t, filepath.Join(root, "go-build"+strconv.Itoa(i)), 3)
	}
	report, err := Scan(context.Background(), Options{Root: root, MemberSample: 5})
	if err != nil {
		t.Fatal(err)
	}
	g := groupByLabel(report, "Go build work dirs")
	if g.Items != 40 || g.Entries != 160 || !g.Estimated || g.Bytes != 40*3*10 {
		t.Fatalf("group = %+v, want 40 items, 160 entries, 1200 bytes, estimated", g)
	}
}

func TestScanReturnsPartialResultsAtItsDeadline(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t)
	start := time.Now()
	report, err := Scan(context.Background(), Options{Root: root, Timeout: time.Nanosecond})
	if err != nil {
		t.Fatalf("Scan past its deadline must report, not fail: %v", err)
	}
	if !report.Partial || report.PartialReason == "" {
		t.Fatalf("report = %+v, want partial with a reason", report)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("an expired scan still took seconds")
	}
}

func TestScanFailsOnlyWhenTheRootIsUnreadable(t *testing.T) {
	t.Parallel()
	if _, err := Scan(context.Background(), Options{Root: filepath.Join(t.TempDir(), "absent")}); err == nil {
		t.Fatal("Scan of a missing root returned no error")
	}
}

// fixSeams records the sweeps Fix runs without touching anything.
func fixSeams(opts *FixOptions, janitorRoots, codexRoots *[]string, neverUsedOnly *[]bool) {
	opts.janitor = func(_ context.Context, o tempown.SweepOptions) tempown.SweepReport {
		*janitorRoots = append(*janitorRoots, o.Root)
		return tempown.SweepReport{Root: o.Root, Reclaimed: 2}
	}
	opts.codexSweep = func(_ context.Context, o providercodex.SweepOptions) providercodex.SweepReport {
		*codexRoots = append(*codexRoots, o.Root)
		*neverUsedOnly = append(*neverUsedOnly, o.NeverUsedOnly)
		return providercodex.SweepReport{Reclaimed: 1, NeverUsed: 1}
	}
}

func toolCachePaths(root string) []string {
	return []string{
		filepath.Join(root, "tsx-501"),
		filepath.Join(root, "node-compile-cache"),
		filepath.Join(root, "go-build1234"),
		filepath.Join(root, ".com.openai.codex.Ab12"),
		filepath.Join(root, "jiti"),
	}
}

func keptPaths(root string) []string {
	return []string{
		filepath.Join(root, "donmai-pi-ext-cache"),
		filepath.Join(root, "donmai-codex-home-1"),
		filepath.Join(root, "tmp.zLzZSyMFe"),
		filepath.Join(root, tempown.PrefixClaudeLogin+"3920481723"),
	}
}

func allExist(t *testing.T, paths []string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(p), err)
		}
	}
}

func TestFixRunsTheJanitorAndOnlyTheNeverUsedCodexSweep(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t)
	var jr, cr []string
	var nuo []bool
	opts := FixOptions{Root: root, CodexRoots: []string{root, root, ""}}
	fixSeams(&opts, &jr, &cr, &nuo)
	res, err := Fix(context.Background(), opts)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(jr) != 1 || jr[0] != root || len(cr) != 1 || cr[0] != root || !nuo[0] {
		t.Fatalf("janitor roots %q, codex roots %q, neverUsedOnly %v", jr, cr, nuo)
	}
	if res.Janitor.Reclaimed != 2 || res.Codex.NeverUsed != 1 || res.ToolCaches.Requested {
		t.Fatalf("result = %+v", res)
	}
	allExist(t, toolCachePaths(root))
}

func TestFixRefusesToolCachesUnlessNoSeatRunsAndTheOperatorConfirms(t *testing.T) {
	t.Parallel()
	yes := func(string) (bool, error) { return true, nil }
	no := func(string) (bool, error) { return false, nil }
	idle := func(context.Context) (int, error) { return 0, nil }
	cases := map[string]struct {
		seats   func(context.Context) (int, error)
		confirm func(string) (bool, error)
	}{
		"seats running":              {seats: func(context.Context) (int, error) { return 2, nil }, confirm: yes},
		"daemon unreachable":         {seats: func(context.Context) (int, error) { return 0, errors.New("connection refused") }, confirm: yes},
		"no seat check configured":   {seats: nil, confirm: yes},
		"operator declined":          {seats: idle, confirm: no},
		"no confirmation configured": {seats: idle, confirm: nil},
		"confirmation errored":       {seats: idle, confirm: func(string) (bool, error) { return false, errors.New("no tty") }},
		"a seat started after the prompt": {
			seats: func() func(context.Context) (int, error) {
				var calls atomic.Int32
				return func(context.Context) (int, error) {
					if calls.Add(1) == 1 {
						return 0, nil
					}
					return 1, nil
				}
			}(),
			confirm: yes,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := fixtureRoot(t)
			var jr, cr []string
			var nuo []bool
			opts := FixOptions{Root: root, CodexRoots: []string{root}, IncludeToolCaches: true, SeatsRunning: tc.seats, Confirm: tc.confirm}
			fixSeams(&opts, &jr, &cr, &nuo)
			res, err := Fix(context.Background(), opts)
			if !errors.Is(err, ErrToolCachesRefused) || res.ToolCaches.Refused == "" || len(res.ToolCaches.Removed) != 0 {
				t.Fatalf("err = %v, result = %+v; want a refusal", err, res.ToolCaches)
			}
			allExist(t, toolCachePaths(root))
			allExist(t, keptPaths(root))
		})
	}
}

func TestFixRemovesOnlyToolCachesWhenConfirmedWithNoSeatRunning(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t)
	outside := mkdirFiles(t, filepath.Join(t.TempDir(), "elsewhere"), 2)
	if err := os.Symlink(outside, filepath.Join(root, "tsx-linked")); err != nil {
		t.Fatal(err)
	}
	var prompt string
	var jr, cr []string
	var nuo []bool
	opts := FixOptions{
		Root: root, CodexRoots: []string{root}, IncludeToolCaches: true,
		SeatsRunning: func(context.Context) (int, error) { return 0, nil },
		Confirm:      func(p string) (bool, error) { prompt = p; return true, nil },
	}
	fixSeams(&opts, &jr, &cr, &nuo)
	res, err := Fix(context.Background(), opts)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(res.ToolCaches.Removed) != 5 || !strings.Contains(prompt, "5 shared tool cache entries") {
		t.Fatalf("removed %q after prompt %q", res.ToolCaches.Removed, prompt)
	}
	for _, p := range toolCachePaths(root) {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("tool cache %s survived: %v", filepath.Base(p), err)
		}
	}
	allExist(t, keptPaths(root))
	allExist(t, []string{filepath.Join(root, "tsx-linked"), filepath.Join(outside, "f0")})
}
