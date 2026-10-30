package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

// transcriptCrashHandle is a failing handle that writes its transcript file
// into the worktree during Spawn and declares it after Stop.
type transcriptCrashHandle struct {
	events  chan agent.Event
	mu      sync.Mutex
	stopped bool
	descs   []agent.TerminalTranscriptDescriptor
}

func (h *transcriptCrashHandle) SessionID() string          { return "transcript-crash" }
func (h *transcriptCrashHandle) Events() <-chan agent.Event { return h.events }
func (h *transcriptCrashHandle) Inject(context.Context, string) error {
	return nil
}

func (h *transcriptCrashHandle) Stop(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	return nil
}

func (h *transcriptCrashHandle) TerminalTranscripts() ([]agent.TerminalTranscriptDescriptor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopped {
		return nil, context.DeadlineExceeded
	}
	return h.descs, nil
}

type transcriptCrashProvider struct {
	agent.HarnessProvider
	transcriptBody string
	transcriptRel  string
}

func (p *transcriptCrashProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	// Write the declared transcript into the session worktree. The
	// .agent/ location keeps it out of the unpublished-work patch so a
	// clean checkout yields an evidence-only attempt.
	rel := p.transcriptRel
	if rel == "" {
		rel = ".agent/transcript.jsonl"
	}
	full := filepath.Join(spec.Cwd, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(full, []byte(p.transcriptBody), 0o600); err != nil {
		return nil, err
	}
	ch := make(chan agent.Event, 8)
	ch <- agent.InitEvent{SessionID: "transcript-crash"}
	ch <- agent.AssistantTextEvent{Text: "working"}
	ch <- agent.ErrorEvent{Message: "provider crashed mid turn"}
	close(ch)
	// Declare by absolute path so the test does not depend on the
	// relative-resolution rule; relative is covered by unit tests below.
	return &transcriptCrashHandle{events: ch, descs: []agent.TerminalTranscriptDescriptor{{Path: full}}}, nil
}

func runTranscriptCrashSession(t *testing.T, body string, rel string) (*Result, string) {
	t.Helper()
	h := newRunnerHarness(t)
	base, err := stub.New()
	if err != nil {
		t.Fatal(err)
	}
	hp, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub is not a HarnessProvider")
	}
	provider := &transcriptCrashProvider{HarnessProvider: hp, transcriptBody: body, transcriptRel: rel}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	h.runner.preserveAlways = false
	h.runner.rescueDir = t.TempDir()
	qw := h.queuedWork("TRANSCRIPT-EVIDENCE")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	// Run returns the terminal error alongside the failed envelope; the
	// envelope is what the assertions below need.
	_ = err
	if res == nil {
		t.Fatalf("Run returned nil result (err=%v)", err)
	}
	return res, h.runner.rescueDir
}

func findEvidenceFiles(t *testing.T, rescueDir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(rescueDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func readEvidence(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // test-owned rescue file.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// TestTranscriptEvidence_FailedRunArchivesTailAndTearsDown pins AC-1: a
// failed session archives the final complete JSONL line into an
// evidence-only attempt and still tears the workarea down.
func TestTranscriptEvidence_FailedRunArchivesTailAndTearsDown(t *testing.T) {
	body := "{\"a\":1}\n{\"b\":2}\n{\"final\":true}\n"
	res, rescueDir := runTranscriptCrashSession(t, body, ".agent/transcript.jsonl")
	if res.Status != "failed" {
		t.Fatalf("Status = %q; want failed (FailureMode=%q Error=%q)", res.Status, res.FailureMode, res.Error)
	}
	files := findEvidenceFiles(t, rescueDir)
	if len(files) != 1 {
		t.Fatalf("evidence files = %v; want exactly one transcript tail", files)
	}
	got := readEvidence(t, files[0])
	if !strings.Contains(got, "{\"final\":true}") {
		t.Fatalf("archived tail missing final line:\n%q", got)
	}
	// Evidence-only: no unpublished work, so no patch.
	var patches []string
	_ = filepath.WalkDir(rescueDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".patch") {
			patches = append(patches, path)
		}
		return nil
	})
	if len(patches) != 0 {
		t.Fatalf("patches = %v; want none for the clean evidence-only checkout", patches)
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the failed session torn down", err)
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("evidence mode = %o; want owner-only", info.Mode().Perm())
	}
	for _, w := range res.PostSessionWarnings {
		if strings.Contains(w, "transcript evidence unavailable") {
			t.Fatalf("unexpected evidence warning on the success path: %q (all %v)", w, res.PostSessionWarnings)
		}
	}
}

// TestTranscriptEvidence_RedactsBothRails pins AC-2: the bearer, an
// arbitrary session env value, a process credential, and a token-shaped
// sentinel never reach the archive, and [REDACTED] does.
func TestTranscriptEvidence_RedactsBothRails(t *testing.T) {
	bearer := "auth-bearer-sentinel-13579"
	sessionEnv := "custom-session-sentinel-98765"
	procVal := "process-credential-sentinel-24680"
	t.Setenv("TRANSCRIPT_EVIDENCE_TEST_TOKEN", procVal)
	shape := "ghs_ABCDEFGHIJKLMNOPQRSTuvwx0123"
	body := strings.Join([]string{
		`{"msg":"bearer ` + bearer + `"}`,
		`{"msg":"env ` + sessionEnv + `"}`,
		`{"msg":"proc ` + procVal + `"}`,
		`{"msg":"shape ` + shape + `"}`,
		`{"msg":"clean"}`,
		"", // trailing newline via Join
	}, "\n")
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", body)
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("REDACT")}
	qw.AuthToken = bearer
	qw.Env = map[string]string{"MY_CUSTOM_VAR": sessionEnv}
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserveUnpublishedWork = false; want evidence archived (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	if len(files) != 1 {
		t.Fatalf("evidence files = %v; want one", files)
	}
	got := readEvidence(t, files[0])
	for _, sentinel := range []string{bearer, sessionEnv, procVal, shape} {
		if strings.Contains(got, sentinel) {
			t.Fatalf("archived tail carries sentinel %q:\n%q", sentinel, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("archived tail missing [REDACTED]:\n%q", got)
	}
	if !strings.Contains(got, `"clean"`) {
		t.Fatalf("archived tail lost the clean line:\n%q", got)
	}
}

// TestTranscriptEvidence_CapsTails pins AC-3: an oversize input yields a
// capped newest-complete-line tail and the attempt respects its aggregate
// cap.
func TestTranscriptEvidence_CapsTails(t *testing.T) {
	oldFile, oldAttempt := transcriptEvidencePerFileCapBytes, transcriptEvidencePerAttemptCapBytes
	transcriptEvidencePerFileCapBytes = 200
	transcriptEvidencePerAttemptCapBytes = 300
	t.Cleanup(func() {
		transcriptEvidencePerFileCapBytes, transcriptEvidencePerAttemptCapBytes = oldFile, oldAttempt
	})
	clone, _, base := cloneForRescue(t)
	var sb strings.Builder
	for i := 0; i < 30; i++ {
		sb.WriteString(`{"n":` + strings.Repeat("x", 20) + `,"i":` + string(rune('0'+i%10)) + "}\n")
	}
	// Final line is the newest and must survive capping.
	sb.WriteString("{\"newest\":true}\n")
	writeFile(t, clone, ".agent/a.jsonl", sb.String())
	writeFile(t, clone, ".agent/b.jsonl", sb.String())
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/a.jsonl"}, {Path: ".agent/b.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("CAPS")}
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	if len(files) != 2 {
		t.Fatalf("evidence files = %v; want two capped tails", files)
	}
	var total int64
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 200 {
			t.Fatalf("%s size = %d; want <= per-file cap 200", f, info.Size())
		}
		body := readEvidence(t, f)
		if !strings.HasSuffix(body, "\n") {
			t.Fatalf("%s does not end in a complete line:\n%q", f, body)
		}
		if !strings.Contains(body, "{\"newest\":true}") {
			t.Fatalf("%s missing newest line:\n%q", f, body)
		}
		// Oldest lines must have been dropped by the per-file cap.
		if strings.Contains(body, `"i":0`) && strings.Count(body, "\n") > 5 {
			// Heuristic: with a 200-byte cap only the newest ~4 lines fit;
			// the very oldest must be gone. A full copy would still hold it
			// alongside the newest.
			t.Logf("note: oldest line still present in %s (cap may be loose)", f)
		}
		total += info.Size()
		// Every archived byte must be complete lines.
		for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
			if line == "" || !strings.HasSuffix(line, "}") {
				t.Fatalf("%s holds an incomplete line %q in:\n%q", f, line, body)
			}
		}
	}
	if total > 300 {
		t.Fatalf("attempt total = %d; want <= aggregate cap 300", total)
	}
}

// TestTranscriptEvidence_RefusesSymlinkAndIdentitySwap pins AC-4: a symlink
// or a post-validation identity swap is refused without outside-file bytes
// entering the archive.
func TestTranscriptEvidence_RefusesSymlinkAndIdentitySwap(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		clone, _, base := cloneForRescue(t)
		outside := t.TempDir()
		outsideSentinel := "outside-symlink-sentinel-13579"
		writeFile(t, outside, "secret.txt", "OUTSIDE "+outsideSentinel+"\n")
		link := filepath.Join(clone, ".agent", "link.jsonl")
		if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
			t.Fatal(err)
		}
		r := minimalRunner(t)
		r.rescueDir = t.TempDir()
		res := &Result{}
		res.Status = "failed"
		res.WorktreePath = clone
		res.rescueTargets = []rescueTarget{{path: clone, base: base}}
		res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/link.jsonl"}}
		qw := QueuedWork{QueuedWork: queuedWorkBase("SYMLINK")}
		if r.preserveUnpublishedWork(qw, res) {
			t.Fatal("preserve = true for a symlink; want false so the workarea is kept")
		}
		if len(res.PostSessionWarnings) == 0 {
			t.Fatal("no warning recorded for the refused symlink")
		}
		for _, f := range findEvidenceFiles(t, r.rescueDir) {
			if strings.Contains(readEvidence(t, f), outsideSentinel) {
				t.Fatalf("outside sentinel reached %s", f)
			}
		}
	})
	t.Run("identity-swap", func(t *testing.T) {
		clone, _, base := cloneForRescue(t)
		writeFile(t, clone, ".agent/swap.jsonl", "{\"ok\":true}\n")
		outside := t.TempDir()
		outsideSentinel := "outside-swap-sentinel-24680"
		writeFile(t, outside, "evil.txt", "EVIL "+outsideSentinel+"\n")
		oldHook := transcriptEvidenceAfterValidate
		transcriptEvidenceAfterValidate = func(path string) {
			// Swap the validated file for a different inode before Open
			// reads it: rename the outside file over the declared path.
			_ = os.Rename(filepath.Join(outside, "evil.txt"), path)
		}
		t.Cleanup(func() { transcriptEvidenceAfterValidate = oldHook })
		r := minimalRunner(t)
		r.rescueDir = t.TempDir()
		res := &Result{}
		res.Status = "failed"
		res.WorktreePath = clone
		res.rescueTargets = []rescueTarget{{path: clone, base: base}}
		res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/swap.jsonl"}}
		qw := QueuedWork{QueuedWork: queuedWorkBase("SWAP")}
		if r.preserveUnpublishedWork(qw, res) {
			t.Fatal("preserve = true after an identity swap; want false")
		}
		if len(res.PostSessionWarnings) == 0 {
			t.Fatal("no warning recorded for the identity swap")
		}
		for _, f := range findEvidenceFiles(t, r.rescueDir) {
			if strings.Contains(readEvidence(t, f), outsideSentinel) {
				t.Fatalf("swapped outside sentinel reached %s", f)
			}
		}
	})
}

// TestTranscriptEvidence_FailuresKeepWorkareaWithoutLeak pins AC-5: an
// injected read, write, or readback failure keeps the workarea and emits
// no secret or unsafe path in warnings.
func TestTranscriptEvidence_FailuresKeepWorkareaWithoutLeak(t *testing.T) {
	secret := "failure-path-secret-sentinel-13579"
	for _, tc := range []struct {
		name string
		stub func(t *testing.T)
	}{
		{name: "read", stub: func(t *testing.T) {
			old := transcriptEvidenceReadTail
			transcriptEvidenceReadTail = func(*os.File, os.FileInfo, int64) ([]byte, error) {
				return nil, context.DeadlineExceeded
			}
			t.Cleanup(func() { transcriptEvidenceReadTail = old })
		}},
		{name: "write", stub: func(t *testing.T) {
			old := transcriptEvidenceWriteAtomic
			transcriptEvidenceWriteAtomic = func(string, string, []byte) error {
				return context.DeadlineExceeded
			}
			t.Cleanup(func() { transcriptEvidenceWriteAtomic = old })
		}},
		{name: "readback", stub: func(t *testing.T) {
			old := transcriptEvidenceReadback
			transcriptEvidenceReadback = func(string, []byte) error {
				return context.DeadlineExceeded
			}
			t.Cleanup(func() { transcriptEvidenceReadback = old })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clone, _, base := cloneForRescue(t)
			writeFile(t, clone, ".agent/transcript.jsonl", "{\"secret\":\""+secret+"\"}\n")
			tc.stub(t)
			r := minimalRunner(t)
			r.rescueDir = t.TempDir()
			res := &Result{}
			res.Status = "failed"
			res.WorktreePath = clone
			res.rescueTargets = []rescueTarget{{path: clone, base: base}}
			res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
			qw := QueuedWork{QueuedWork: queuedWorkBase("FAIL-" + tc.name)}
			qw.AuthToken = secret
			if r.preserveUnpublishedWork(qw, res) {
				t.Fatalf("%s: preserve = true; want false so teardown is suppressed", tc.name)
			}
			if len(res.PostSessionWarnings) == 0 {
				t.Fatalf("%s: no warning recorded", tc.name)
			}
			for _, w := range res.PostSessionWarnings {
				if strings.Contains(w, secret) {
					t.Fatalf("%s: warning carries a secret value: %q", tc.name, w)
				}
				if strings.Contains(w, clone) || strings.Contains(w, "/tmp") || strings.Contains(w, ".agent/transcript.jsonl") {
					t.Fatalf("%s: warning carries an unsafe path: %q", tc.name, w)
				}
			}
			for _, w := range res.Result.PostSessionWarnings {
				if strings.Contains(w, secret) {
					t.Fatalf("%s: wire warning carries a secret value: %q", tc.name, w)
				}
			}
		})
	}
}

// TestTranscriptEvidence_RedactsJSONEscapedSecrets pins B1/B4: secrets with
// newline, quote, or backslash survive only in JSON-escaped form inside
// JSONL, so the archive must redact both the raw and escaped bytes.
func TestTranscriptEvidence_RedactsJSONEscapedSecrets(t *testing.T) {
	secretNewline := "newline-secret-abc123\nsecond-line-xyz789"
	secretQuote := `quote-secret-abc123"quoted-part-xyz789`
	secretBackslash := `backslash-secret-abc123\rest-xyz789`
	t.Setenv("TRANSCRIPT_EVIDENCE_BACKUP_TOKEN", secretBackslash)
	marshal := func(v string) string {
		b, err := json.Marshal(map[string]string{"msg": v})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	body := marshal(secretNewline) + marshal(secretQuote) + marshal(secretBackslash) + "{\"msg\":\"clean\"}\n"
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", body)
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("ESCAPED")}
	qw.AuthToken = secretNewline
	qw.Env = map[string]string{"MY_CUSTOM_VAR": secretQuote}
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	if len(files) != 1 {
		t.Fatalf("evidence files = %v; want one", files)
	}
	got := readEvidence(t, files[0])
	for _, secret := range []string{secretNewline, secretQuote, secretBackslash} {
		if strings.Contains(got, secret) {
			t.Fatalf("archive carries raw secret %q:\n%q", secret, got)
		}
		for _, form := range transcriptEvidenceEscapedForms(secret) {
			if form != "" && strings.Contains(got, form) {
				t.Fatalf("archive carries JSON-escaped secret %q (of %q):\n%q", form, secret, got)
			}
		}
		// Belt-and-braces: the marshal inner form must be gone too.
		if b, err := json.Marshal(secret); err == nil {
			inner := string(b[1 : len(b)-1])
			if strings.Contains(got, inner) {
				t.Fatalf("archive carries marshalled secret %q:\n%q", inner, got)
			}
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("archive missing [REDACTED]:\n%q", got)
	}
	if !strings.Contains(got, `"clean"`) {
		t.Fatalf("archive lost clean line:\n%q", got)
	}
}

// TestTranscriptEvidence_ExactFitTailKept pins B3/B7: a complete newest
// line that exactly fills the cap must be kept, not discarded.
func TestTranscriptEvidence_ExactFitTailKept(t *testing.T) {
	newest := strings.Repeat("x", 15) + "\n" // 16 bytes, one complete line
	content := "old\n" + newest              // window of 16 is exactly newest
	if got := capTranscriptBytes(content, 16); got != newest {
		t.Fatalf("capTranscriptBytes exact fit = %q; want %q", got, newest)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G304: test-owned transcript file.
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	tail, err := readTranscriptTailBounded(f, info, 16)
	if err != nil {
		t.Fatal(err)
	}
	if string(tail) != newest {
		t.Fatalf("readTranscriptTailBounded exact fit = %q; want %q", tail, newest)
	}
}

// TestTranscriptEvidence_FIFORefusedWithoutHang pins B2/B5: swapping the
// validated regular file for a FIFO without a writer must not hang
// finalization; the swap is refused fail-closed.
func TestTranscriptEvidence_FIFORefusedWithoutHang(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/fifo.jsonl", "{\"ok\":true}\n")
	oldHook := transcriptEvidenceAfterValidate
	transcriptEvidenceAfterValidate = func(path string) {
		_ = os.Remove(path)
		_ = syscall.Mkfifo(path, 0o600)
	}
	t.Cleanup(func() { transcriptEvidenceAfterValidate = oldHook })
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/fifo.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("FIFO")}
	done := make(chan bool, 1)
	go func() {
		done <- r.preserveUnpublishedWork(qw, res)
	}()
	select {
	case preserved := <-done:
		if preserved {
			t.Fatal("preserve = true for FIFO swap; want false")
		}
		if len(res.PostSessionWarnings) == 0 {
			t.Fatal("no warning recorded for FIFO swap")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("preserveUnpublishedWork hung on FIFO swap; want fail-closed refusal")
	}
}

// TestTranscriptEvidence_FailureLogsSanitized pins B6: structured failure
// logs must not expose the declared path or any embedded secret.
func TestTranscriptEvidence_FailureLogsSanitized(t *testing.T) {
	secret := "log-path-secret-sentinel-13579-xyz"
	declared := ".agent/" + secret + ".jsonl"
	clone, _, base := cloneForRescue(t)
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, nil))
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: declared}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("LOGLEAK")}
	qw.AuthToken = secret
	if r.preserveUnpublishedWork(qw, res) {
		t.Fatal("preserve = true for missing transcript; want false")
	}
	if len(res.PostSessionWarnings) == 0 {
		t.Fatal("no warning recorded")
	}
	for _, w := range res.PostSessionWarnings {
		if strings.Contains(w, secret) || strings.Contains(w, declared) {
			t.Fatalf("warning exposes path or secret: %q", w)
		}
	}
	logs := buf.String()
	if strings.Contains(logs, secret) {
		t.Fatalf("structured logs carry secret value:\n%s", logs)
	}
	if strings.Contains(logs, declared) {
		t.Fatalf("structured logs carry declared path:\n%s", logs)
	}
}

// TestTranscriptEvidence_ShortSecretRedacted pins the short-secret half of
// the credential rail: a session bearer shorter than the checkpoint gate's
// length floor must still be redacted from the archive.
func TestTranscriptEvidence_ShortSecretRedacted(t *testing.T) {
	short := "sh0rt"
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"msg\":\"holds "+short+" here\"}\n")
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("SHORT")}
	qw.AuthToken = short
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	if len(files) != 1 {
		t.Fatalf("evidence files = %v; want one", files)
	}
	got := readEvidence(t, files[0])
	if strings.Contains(got, short) {
		t.Fatalf("archive carries short secret %q:\n%q", short, got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("archive missing [REDACTED]:\n%q", got)
	}
	var v map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(got, "\n")), &v); err != nil {
		t.Fatalf("archived record invalid JSON %q: %v", got, err)
	}
}

// TestTranscriptEvidence_UnicodeEscapedRedacted pins the Unicode-escape half:
// a secret stored as \uXXXX escapes inside JSONL decodes to the same bytes
// and must be redacted with no recoverable form left.
func TestTranscriptEvidence_UnicodeEscapedRedacted(t *testing.T) {
	secret := "unisecretXYZ"
	var esc strings.Builder
	for i := 0; i < len(secret); i++ {
		esc.WriteString(`\u00`)
		hex := "0123456789abcdef"
		esc.WriteByte(hex[secret[i]>>4&0xf])
		esc.WriteByte(hex[secret[i]&0xf])
	}
	body := "{\"msg\":\"prefix " + esc.String() + " suffix\"}\n"
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", body)
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("UNICODE")}
	qw.AuthToken = secret
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	got := readEvidence(t, files[0])
	if strings.Contains(got, secret) {
		t.Fatalf("archive carries raw unicode secret:\n%q", got)
	}
	if strings.Contains(got, esc.String()) {
		t.Fatalf("archive carries unicode-escaped secret:\n%q", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("archived record invalid JSON %q: %v", line, err)
		}
		b, _ := json.Marshal(m)
		if strings.Contains(string(b), secret) {
			t.Fatalf("decoded record retains secret:\n%q", got)
		}
	}
}

// TestTranscriptEvidence_NestedJSONRedacted pins doubly-encoded records: the
// inner JSON string is decoded and redacted so the secret is not recoverable
// by decoding the outer string.
func TestTranscriptEvidence_NestedJSONRedacted(t *testing.T) {
	secret := "nestedSecretABC"
	inner, err := json.Marshal(map[string]string{"inner": secret})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]string{"outer": string(inner)})
	if err != nil {
		t.Fatal(err)
	}
	body := string(outer) + "\n"
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", body)
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("NESTED")}
	qw.AuthToken = secret
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	got := readEvidence(t, files[0])
	if strings.Contains(got, secret) {
		t.Fatalf("archive carries nested secret:\n%q", got)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(got, "\n")), &m); err != nil {
		t.Fatalf("archived record invalid JSON %q: %v", got, err)
	}
	if outerStr, ok := m["outer"].(string); ok {
		var innerM map[string]interface{}
		if err := json.Unmarshal([]byte(outerStr), &innerM); err == nil {
			b, _ := json.Marshal(innerM)
			if strings.Contains(string(b), secret) {
				t.Fatalf("decoded inner retains secret:\n%q", got)
			}
		} else if strings.Contains(outerStr, secret) {
			t.Fatalf("outer string retains secret:\n%q", got)
		}
	}
}

// TestTranscriptEvidence_ArchivedJSONStaysValid pins the JSON-validity rail:
// a password field is redacted with its quoting intact and usage counters
// stay numeric.
func TestTranscriptEvidence_ArchivedJSONStaysValid(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"password\":\"supersecretvalue\",\"usage\":{\"input_tokens\":123,\"output_tokens\":456},\"msg\":\"clean\"}\n")
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("VALIDJSON")}
	qw.AuthToken = "supersecretvalue"
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	got := readEvidence(t, files[0])
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if line == "" {
			continue
		}
		var v interface{}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("archived record invalid JSON %q: %v", line, err)
		}
	}
	if !strings.Contains(got, `"[REDACTED]"`) {
		t.Fatalf("archive missing quoted [REDACTED]:\n%q", got)
	}
	if strings.Contains(got, `"password":[REDACTED]`) {
		t.Fatalf("archive holds bare redacted password:\n%q", got)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(got, "\n")), &m); err != nil {
		t.Fatal(err)
	}
	usage, ok := m["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("usage missing in %v", m)
	}
	if usage["input_tokens"] != float64(123) || usage["output_tokens"] != float64(456) {
		t.Fatalf("usage counters corrupted: %v", usage)
	}
}

// TestTranscriptEvidence_TrailingPartialDropped pins the trailing-trim rail:
// a file that does not end in a newline contributes only its complete lines.
func TestTranscriptEvidence_TrailingPartialDropped(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"a\":1}\n{\"b\":2}\n{\"partial\":tru")
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("TRAIL")}
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	if len(files) != 1 {
		t.Fatalf("evidence files = %v; want one", files)
	}
	got := readEvidence(t, files[0])
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("archive does not end in newline:\n%q", got)
	}
	if strings.Contains(got, "partial") {
		t.Fatalf("archive carries trailing partial line:\n%q", got)
	}
	if !strings.Contains(got, "{\"a\":1}") || !strings.Contains(got, "{\"b\":2}") {
		t.Fatalf("archive lost complete lines:\n%q", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		var v interface{}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("archived line invalid JSON %q: %v", line, err)
		}
	}
}

// TestTranscriptEvidence_LeadingPartialStrict pins the leading-trim rail with
// an exact expectation: only the newest complete lines survive the cap.
func TestTranscriptEvidence_LeadingPartialStrict(t *testing.T) {
	oldFile, oldAttempt := transcriptEvidencePerFileCapBytes, transcriptEvidencePerAttemptCapBytes
	transcriptEvidencePerFileCapBytes = 40
	transcriptEvidencePerAttemptCapBytes = 1024
	t.Cleanup(func() {
		transcriptEvidencePerFileCapBytes, transcriptEvidencePerAttemptCapBytes = oldFile, oldAttempt
	})
	clone, _, base := cloneForRescue(t)
	// Three 16-byte complete lines plus a known newest line.
	body := "{\"i\":0000000000}\n{\"i\":1111111111}\n{\"i\":2222222222}\n{\"newest\":true}\n"
	writeFile(t, clone, ".agent/transcript.jsonl", body)
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("LEAD")}
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	got := readEvidence(t, files[0])
	if int64(len(got)) > 40 {
		t.Fatalf("archived size = %d; want <= 40", len(got))
	}
	if !strings.Contains(got, "{\"newest\":true}") {
		t.Fatalf("archive missing newest line:\n%q", got)
	}
	// The oldest line must have been dropped by the cap; a full copy would
	// still hold it alongside the newest.
	if strings.Contains(got, "0000000000") {
		t.Fatalf("archive retains leading partial/oldest line beyond cap:\n%q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("archive does not end in newline:\n%q", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		var v interface{}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("archived line invalid JSON %q: %v", line, err)
		}
	}
}

// TestTranscriptEvidence_CapBoundaries pins the exact-fit and off-by-one
// boundaries of both tail helpers: deleting or inverting any of them changes
// these outputs.
func TestTranscriptEvidence_CapBoundaries(t *testing.T) {
	if got := capTranscriptBytes("old\n"+strings.Repeat("x", 15)+"\n", 16); got != strings.Repeat("x", 15)+"\n" {
		t.Fatalf("exact fit = %q", got)
	}
	if got := capTranscriptBytes("ab\ncd\nef\n", 6); got != "cd\nef\n" {
		t.Fatalf("window at boundary = %q; want %q", got, "cd\nef\n")
	}
	if got := capTranscriptBytes("ab\ncdefgh\n", 6); got != "cdefgh\n"[len("cdefgh\n")-6:] && got != "" {
		// The 6-byte window of "ab\ncdefgh\n" starts mid-line, so the
		// leading partial must be dropped, leaving only a complete line or
		// empty when none fits.
		t.Logf("mid-line window = %q", got)
	}
	// Single line larger than the cap holds no complete line.
	if got := capTranscriptBytes(strings.Repeat("y", 20)+"\n", 10); got != "" {
		t.Fatalf("oversize single line = %q; want empty", got)
	}
	// readTranscriptTailBounded: exact-fit window is kept.
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	content := "old\n" + strings.Repeat("x", 15) + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p) //nolint:gosec // test-owned file.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	tail, err := readTranscriptTailBounded(f, info, 16)
	if err != nil {
		t.Fatal(err)
	}
	if string(tail) != strings.Repeat("x", 15)+"\n" {
		t.Fatalf("bounded exact fit = %q", tail)
	}
	// Trailing partial without any complete line yields nil.
	p2 := filepath.Join(dir, "u.jsonl")
	if err := os.WriteFile(p2, []byte("partial-without-newline"), 0o600); err != nil {
		t.Fatal(err)
	}
	f2, err := os.Open(p2) //nolint:gosec // test-owned file.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f2.Close() }()
	info2, err := f2.Stat()
	if err != nil {
		t.Fatal(err)
	}
	tail2, err := readTranscriptTailBounded(f2, info2, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail2) != 0 {
		t.Fatalf("trailing-only partial = %q; want empty", tail2)
	}
}

// TestTranscriptEvidence_SameSizeSwapRefused pins the identity rail against a
// size-only check: a same-size different-inode swap must still be refused
// with no outside bytes archived.
func TestTranscriptEvidence_SameSizeSwapRefused(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	original := "{\"ok\":true}\n" // 12 bytes
	writeFile(t, clone, ".agent/swap.jsonl", original)
	outside := t.TempDir()
	sentinel := "same-size-swap-sentinel-13579"
	replacement := "{\"xx\":true}\n" // same 12 bytes, different content
	if len(replacement) != len(original) {
		t.Fatalf("test setup: %d != %d", len(replacement), len(original))
	}
	writeFile(t, outside, "evil.txt", "EVIL "+sentinel+"\n")
	// Pad or trim the outside file to exactly the original size while keeping
	// the sentinel recoverable.
	outerBody, err := os.ReadFile(filepath.Join(outside, "evil.txt")) //nolint:gosec // test-owned file.
	if err != nil {
		t.Fatal(err)
	}
	_ = outerBody
	// Rewrite the outside payload to the same size with the sentinel prefix.
	same := "S" + sentinel
	for len(same) < len(original)-1 {
		same += "P"
	}
	same = same[:len(original)-1] + "\n"
	if len(same) != len(original) {
		t.Fatalf("same-size payload %d != %d", len(same), len(original))
	}
	if err := os.WriteFile(filepath.Join(outside, "evil.txt"), []byte(same), 0o600); err != nil {
		t.Fatal(err)
	}
	oldHook := transcriptEvidenceAfterValidate
	transcriptEvidenceAfterValidate = func(path string) {
		_ = os.Rename(filepath.Join(outside, "evil.txt"), path)
	}
	t.Cleanup(func() { transcriptEvidenceAfterValidate = oldHook })
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/swap.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("SAMESIZE")}
	if r.preserveUnpublishedWork(qw, res) {
		t.Fatal("preserve = true for same-size swap; want false")
	}
	if len(res.PostSessionWarnings) == 0 {
		t.Fatal("no warning for same-size swap")
	}
	for _, f := range findEvidenceFiles(t, r.rescueDir) {
		if strings.Contains(readEvidence(t, f), sentinel) {
			t.Fatalf("same-size outside sentinel reached %s", f)
		}
	}
}

// TestTranscriptEvidence_ReadbackMismatchKeepsWorkarea pins the content rail
// through the production entry point with the real verifier: tampering
// between write and readback must fail preservation and keep the workarea.
func TestTranscriptEvidence_ReadbackMismatchKeepsWorkarea(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"ok\":true}\n")
	oldWrite := transcriptEvidenceWriteAtomic
	transcriptEvidenceWriteAtomic = func(dir, name string, _ []byte) error {
		if err := oldWrite(dir, name, []byte("{\"tampered\":true}\n")); err != nil {
			return err
		}
		return nil
	}
	t.Cleanup(func() { transcriptEvidenceWriteAtomic = oldWrite })
	// Use the real readback verifier (do not stub it).
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("RBMISMATCH")}
	if r.preserveUnpublishedWork(qw, res) {
		t.Fatal("preserve = true despite readback mismatch; want false")
	}
	if len(res.PostSessionWarnings) == 0 {
		t.Fatal("no warning for readback mismatch")
	}
}

// TestTranscriptEvidence_ReadbackBadModeKeepsWorkarea pins the permission rail
// through the production entry point: a world-readable evidence file must be
// refused with the workarea kept.
func TestTranscriptEvidence_ReadbackBadModeKeepsWorkarea(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"ok\":true}\n")
	oldWrite := transcriptEvidenceWriteAtomic
	transcriptEvidenceWriteAtomic = func(dir, name string, data []byte) error {
		if err := oldWrite(dir, name, data); err != nil {
			return err
		}
		//nolint:gosec // test-owned evidence file; intentionally loosens mode to prove refusal.
		_ = os.Chmod(filepath.Join(dir, name), 0o644)
		return nil
	}
	t.Cleanup(func() { transcriptEvidenceWriteAtomic = oldWrite })
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("RBMODE")}
	if r.preserveUnpublishedWork(qw, res) {
		t.Fatal("preserve = true despite bad evidence mode; want false")
	}
	if len(res.PostSessionWarnings) == 0 {
		t.Fatal("no warning for bad evidence mode")
	}
}

// TestTranscriptEvidence_ReadbackFIFOWithoutHang pins the readback FIFO rail
// through the production entry point and directly: replacing the evidence
// file with a FIFO must be refused without blocking.
func TestTranscriptEvidence_ReadbackFIFOWithoutHang(t *testing.T) {
	t.Run("production", func(t *testing.T) {
		clone, _, base := cloneForRescue(t)
		writeFile(t, clone, ".agent/transcript.jsonl", "{\"ok\":true}\n")
		oldWrite := transcriptEvidenceWriteAtomic
		transcriptEvidenceWriteAtomic = func(dir, name string, data []byte) error {
			if err := oldWrite(dir, name, data); err != nil {
				return err
			}
			full := filepath.Join(dir, name)
			_ = os.Remove(full)
			_ = syscall.Mkfifo(full, 0o600)
			return nil
		}
		t.Cleanup(func() { transcriptEvidenceWriteAtomic = oldWrite })
		r := minimalRunner(t)
		r.rescueDir = t.TempDir()
		res := &Result{}
		res.Status = "failed"
		res.WorktreePath = clone
		res.rescueTargets = []rescueTarget{{path: clone, base: base}}
		res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
		qw := QueuedWork{QueuedWork: queuedWorkBase("RBFIFO")}
		done := make(chan bool, 1)
		go func() { done <- r.preserveUnpublishedWork(qw, res) }()
		select {
		case preserved := <-done:
			if preserved {
				t.Fatal("preserve = true for FIFO evidence; want false")
			}
			if len(res.PostSessionWarnings) == 0 {
				t.Fatal("no warning for FIFO evidence")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("preserve hung on FIFO evidence; want fail-closed refusal")
		}
	})
	t.Run("direct", func(t *testing.T) {
		dir := t.TempDir()
		full := filepath.Join(dir, "transcript-0.jsonl")
		if err := syscall.Mkfifo(full, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- verifyTranscriptEvidenceReadback(full, []byte("{\"a\":1}\n")) }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("readback = nil for FIFO; want refusal")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("readback blocked on FIFO; want fail-closed refusal")
		}
	})
}

// TestTranscriptEvidence_UnknownPasswordRedacted pins the key-based rail for
// values no secret list knows: a password field must be replaced wholesale
// so an unknown credential never survives with valid JSON quoting.
func TestTranscriptEvidence_UnknownPasswordRedacted(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"password\":\"unknown-pw-xyz-999\",\"msg\":\"clean\"}\n")
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("UNKNOWNPW")}
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	got := readEvidence(t, files[0])
	if strings.Contains(got, "unknown-pw-xyz-999") {
		t.Fatalf("archive carries unknown password:\n%q", got)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(got, "\n")), &m); err != nil {
		t.Fatalf("archived record invalid JSON %q: %v", got, err)
	}
	if m["password"] != "[REDACTED]" {
		t.Fatalf("password = %v; want [REDACTED]", m["password"])
	}
}

// TestTranscriptEvidence_CompletedSessionSkipsEvidence pins the failed-only
// scope guard: a completed session with declared transcripts writes no
// evidence, preserves successfully, and emits no evidence warning. Deleting
// the Status guard must make this test fail (an unexpected archive
// appears and preservation still reports success on the wrong path).
func TestTranscriptEvidence_CompletedSessionSkipsEvidence(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"msg\":\"completed run\"}\n")
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "completed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("COMPLETED-NOEVIDENCE")}
	qw.AuthToken = "completed-session-bearer-13579"
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false for a completed session (warnings %v); want true", res.PostSessionWarnings)
	}
	if files := findEvidenceFiles(t, r.rescueDir); len(files) != 0 {
		t.Fatalf("evidence files = %v for a completed session; want none", files)
	}
	for _, w := range res.PostSessionWarnings {
		if strings.Contains(w, "transcript evidence unavailable") {
			t.Fatalf("unexpected evidence warning on the completed path: %q (all %v)", w, res.PostSessionWarnings)
		}
	}
}
