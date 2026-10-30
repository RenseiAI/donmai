package pi

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/RenseiAI/donmai/agent"
)

// piEvidenceClaim is a per-handle lease on one layout root. It detects
// concurrent sessions sharing the same root: the second spawn while another
// handle is still live marks every live lease on that root contaminated, so
// each handle's later evidence collection refuses rather than attributing a
// concurrent session's transcript to itself. Sequential reuse is unaffected:
// the previous handle freezes its evidence and releases its lease at Stop,
// so a later spawn sees an empty set and the earlier handle's delayed
// collection returns its frozen outcome, never the later session's files.
//
// Overlap tracking spans processes: controllers share a workarea through
// separate `agent run` processes (daemon/worker_command.go), so an
// in-process map alone never sees a peer in another process. Each claim
// also owns a claim file under the layout root; a spawn that finds another
// live claim file (or a leftover overlap marker) is contaminated, and a
// spawn that finds a live peer leaves the marker so the peer's Stop also
// refuses. The marker is removed once the last live claim is released, so
// sequential reuse after the overlap drains is clean again.
type piEvidenceClaim struct {
	root         string
	contaminated bool
	claimPath    string
}

var piEvidenceMu sync.Mutex

// piEvidenceActive tracks live evidence leases per layout root.
var piEvidenceActive = map[string]map[*piEvidenceClaim]struct{}{}

// piEvidenceClaimPrefix names per-handle live-claim files under a layout
// root. piEvidenceOverlapName is the marker left while an overlap has been
// observed and not yet drained. Neither ends in .jsonl, so the transcript
// walker never visits them as candidates.
const (
	piEvidenceClaimPrefix = ".pi-evidence-claim-"
	piEvidenceOverlapName = ".pi-evidence-overlap"
)

// acquirePiEvidenceClaim records a new live handle on root. A nil root
// (no per-session root was recorded) needs no lease. Callers hold the
// lease from spawn until Stop releases it; Stop freezes evidence before
// releasing so sequential reuse is clean while true overlap stays
// contaminated via the persisted flag and the cross-process marker.
func acquirePiEvidenceClaim(root string) *piEvidenceClaim {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	clean := filepath.Clean(root)
	if clean == "." {
		return nil
	}
	c := &piEvidenceClaim{root: clean}
	piEvidenceMu.Lock()
	if live := piEvidenceActive[clean]; len(live) > 0 {
		c.contaminated = true
		for other := range live {
			other.contaminated = true
		}
	}
	if piEvidenceActive[clean] == nil {
		piEvidenceActive[clean] = map[*piEvidenceClaim]struct{}{}
	}
	piEvidenceActive[clean][c] = struct{}{}
	piEvidenceMu.Unlock()
	fsContaminated, claimPath := acquirePiEvidenceClaimFile(clean)
	if fsContaminated {
		piEvidenceMu.Lock()
		c.contaminated = true
		for other := range piEvidenceActive[clean] {
			other.contaminated = true
		}
		piEvidenceMu.Unlock()
	}
	if claimPath != "" {
		c.claimPath = claimPath
	}
	return c
}

// acquirePiEvidenceClaimFile creates this handle's cross-process claim file
// and reports whether the filesystem shows an overlap: another live claim
// file, or a leftover overlap marker from an overlap that has not drained.
// A detected overlap leaves the marker so the peer's Stop also refuses.
// Filesystem errors fail closed (report overlap) so an unreadable root can
// never silently attribute another session's transcript.
func acquirePiEvidenceClaimFile(clean string) (bool, string) {
	f, err := os.CreateTemp(clean, piEvidenceClaimPrefix+"*")
	if err != nil {
		return true, ""
	}
	claimPath := f.Name()
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
	_ = f.Close()
	overlapped := false
	if _, err := os.Lstat(filepath.Join(clean, piEvidenceOverlapName)); err == nil {
		overlapped = true
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return true, claimPath
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, piEvidenceClaimPrefix) || filepath.Join(clean, name) == claimPath {
			continue
		}
		if piEvidenceClaimFileLive(filepath.Join(clean, name)) {
			overlapped = true
		} else {
			_ = os.Remove(filepath.Join(clean, name))
		}
	}
	if overlapped {
		markPiEvidenceOverlap(clean)
	}
	return overlapped, claimPath
}

// piEvidenceClaimFileLive reports whether a claim file belongs to a live
// controller: its recorded pid still runs. An unreadable or unparsable
// claim fails closed (treated as live) so a peer is never silently
// ignored; a pid that no longer runs is stale and cleaned by the caller.
func piEvidenceClaimFileLive(path string) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is a claim file discovered by listing the session's own state root.
	if err != nil {
		return true
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return true
	}
	return isPiEvidenceProcessAlive(pid)
}

// isPiEvidenceProcessAlive probes one pid. Unknown pids fail closed as
// live. On Windows the Signal(0) probe is unavailable, so any recorded pid
// is treated as live: the workarea lifecycle still removes the claim files
// with the workarea, bounding the false-refusal window to reused workareas.
func isPiEvidenceProcessAlive(pid int) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// markPiEvidenceOverlap leaves the cross-process overlap marker. Best
// effort: a creation failure still leaves the in-process contamination,
// and collection re-checks the live set directly.
func markPiEvidenceOverlap(clean string) {
	f, err := os.OpenFile(filepath.Join(clean, piEvidenceOverlapName), os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: marker path joins the session's own state root with a fixed name.
	if err != nil {
		return
	}
	_ = f.Close()
}

// filesystemOverlapNow re-checks the cross-process set at Stop/collection:
// another live claim file or a leftover overlap marker means this handle
// overlapped, even when the peer started after this handle's spawn. A newly
// observed overlap leaves the marker so the peer also refuses.
func (c *piEvidenceClaim) filesystemOverlapNow() bool {
	if c == nil {
		return false
	}
	clean := c.root
	if strings.TrimSpace(clean) == "" {
		return false
	}
	if _, err := os.Lstat(filepath.Join(clean, piEvidenceOverlapName)); err == nil {
		return true
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return true
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, piEvidenceClaimPrefix) {
			continue
		}
		if c.claimPath != "" && filepath.Join(clean, name) == c.claimPath {
			continue
		}
		if piEvidenceClaimFileLive(filepath.Join(clean, name)) {
			markPiEvidenceOverlap(clean)
			return true
		}
	}
	return false
}

// release removes the lease from the live set and its claim file from the
// layout root. The contaminated flag persists so a handle that overlapped
// still refuses after the peer left. The overlap marker is removed only
// once no live claim remains (in-process or on disk), so sequential reuse
// after the overlap drains is clean again.
func (c *piEvidenceClaim) release() {
	if c == nil {
		return
	}
	piEvidenceMu.Lock()
	if set := piEvidenceActive[c.root]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(piEvidenceActive, c.root)
		}
	}
	piEvidenceMu.Unlock()
	if c.claimPath != "" {
		_ = os.Remove(c.claimPath)
		c.claimPath = ""
	}
	clean := c.root
	if strings.TrimSpace(clean) == "" || clean == "." {
		return
	}
	piEvidenceMu.Lock()
	inProcessLive := len(piEvidenceActive[clean]) > 0
	piEvidenceMu.Unlock()
	if inProcessLive {
		return
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), piEvidenceClaimPrefix) {
			continue
		}
		if piEvidenceClaimFileLive(filepath.Join(clean, e.Name())) {
			return
		}
		_ = os.Remove(filepath.Join(clean, e.Name()))
	}
	_ = os.Remove(filepath.Join(clean, piEvidenceOverlapName))
}

// isContaminated reports whether another handle shared this root while this
// lease was live. A nil lease (handles built without a spawn) is clean.
func (c *piEvidenceClaim) isContaminated() bool {
	if c == nil {
		return false
	}
	piEvidenceMu.Lock()
	contaminated := c.contaminated
	piEvidenceMu.Unlock()
	if contaminated {
		return true
	}
	return c.filesystemOverlapNow()
}

// markContaminated records an observed overlap on this lease and every
// other live in-process lease on the same root.
func (c *piEvidenceClaim) markContaminated() {
	if c == nil {
		return
	}
	piEvidenceMu.Lock()
	defer piEvidenceMu.Unlock()
	c.contaminated = true
	for other := range piEvidenceActive[c.root] {
		other.contaminated = true
	}
}

// Compile-time assertions: both pi spawn modes expose the optional
// terminal-transcript evidence capability the runner captures after Stop.
var (
	_ agent.TerminalTranscriptCapable = (*Handle)(nil)
	_ agent.TerminalTranscriptCapable = (*interactiveStateLossHandle)(nil)
)

// setStateRoot records the exact per-session layout root this handle's
// transcripts live under. Called once at spawn from the materialized layout,
// before the pump starts.
func (h *Handle) setStateRoot(root string) {
	if strings.TrimSpace(root) == "" {
		return
	}
	h.stateRoot = filepath.Clean(root)
}

// setEvidenceBaseline records the transcripts that already existed under the
// layout root before this session's child started. TerminalTranscripts
// excludes them: the state dir is shared across reuses of a workarea, so
// directory membership alone cannot prove session ownership (the tailer in
// interactive_transcript.go snapshots the same baseline for the same reason).
// incomplete marks a failed pre-spawn scan: TerminalTranscripts refuses then
// rather than attributing a transcript the scan never saw. Called once at
// spawn from the pre-spawn snapshot, before the pump starts.
func (h *Handle) setEvidenceBaseline(baseline map[string]struct{}, incomplete bool) {
	h.evidenceBaseline = baseline
	h.evidenceIncomplete = incomplete
}

// snapshotPiTranscriptBaseline records every transcript already present under
// root. Call it after the parent has finished writing the layout (extensions,
// credentials, tmp/cache dirs) and before the child starts, so only files
// this session did not create are recorded. A missing root snapshots to a
// complete empty baseline. The incomplete flag reports a failed scan: the
// caller must refuse evidence then, since a transcript the scan never saw
// would otherwise be misattributed to this session.
func snapshotPiTranscriptBaseline(root string) (map[string]struct{}, bool) {
	if strings.TrimSpace(root) == "" {
		return nil, true
	}
	clean := filepath.Clean(root)
	info, err := os.Lstat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		return nil, true
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, true
	}
	var baseline map[string]struct{}
	if err := walkInteractiveTranscriptFiles(clean, func(path string, _ os.DirEntry) {
		if baseline == nil {
			baseline = make(map[string]struct{})
		}
		baseline[filepath.Clean(path)] = struct{}{}
	}); err != nil {
		return baseline, true
	}
	return baseline, false
}

// captureHeadlessEvidenceAtStop freezes this session's evidence at Stop,
// before the overlap lease is released: a later sequential session in the
// same checkout can never pollute this handle's delayed collection, which
// returns the frozen outcome. Idempotent via evidenceDone.
func (h *Handle) captureHeadlessEvidenceAtStop() {
	h.evidenceMu.Lock()
	defer h.evidenceMu.Unlock()
	if h.evidenceDone {
		return
	}
	if h.evidenceIncomplete {
		h.evidenceDone = true
		h.evidenceCachedErr = errors.New("pi: transcript evidence unavailable: pre-spawn transcript scan was incomplete")
		return
	}
	if h.evidenceClaim.isContaminated() {
		h.evidenceClaim.markContaminated()
		h.evidenceDone = true
		h.evidenceCachedErr = errors.New("pi: transcript evidence unavailable: concurrent session shared this state root")
		return
	}
	root := h.stateRoot
	if root == "" {
		h.evidenceDone = true
		return
	}
	descs, err := collectPiTranscriptDescriptors(root, h.evidenceBaseline)
	if err != nil {
		h.evidenceDone = true
		h.evidenceCachedErr = err
		return
	}
	if len(descs) == 0 {
		h.evidenceDone = true
		return
	}
	h.evidenceDone = true
	h.evidenceCached = descs
}

// TerminalTranscripts implements agent.TerminalTranscriptCapable for the
// headless RPC lane. It returns path-only descriptors for this session's
// own JSONL transcripts beneath the exact per-session layout root, after
// the child has been reaped and the session credential files removed.
//
// It refuses while writers may still be active (before Stop returns) and
// returns nil (no empty artifact) when the session wrote no transcript.
// Only regular .jsonl files beneath the layout root are returned; the
// per-session config home, injected extensions, session tmp/cache content,
// credential files, policy extensions, symlinks, non-JSONL files, and any
// path outside the layout root are never returned.
func (h *Handle) TerminalTranscripts() ([]agent.TerminalTranscriptDescriptor, error) {
	if !h.stopped.Load() {
		return nil, errors.New("pi: transcript evidence unavailable while session writers may still be active")
	}
	h.evidenceMu.Lock()
	defer h.evidenceMu.Unlock()
	if h.evidenceDone {
		if h.evidenceCachedErr != nil {
			return nil, h.evidenceCachedErr
		}
		if len(h.evidenceCached) == 0 {
			return nil, nil
		}
		cp := make([]agent.TerminalTranscriptDescriptor, len(h.evidenceCached))
		copy(cp, h.evidenceCached)
		return cp, nil
	}
	fail := func(err error) ([]agent.TerminalTranscriptDescriptor, error) {
		h.evidenceDone = true
		h.evidenceCachedErr = err
		return nil, err
	}
	if h.evidenceIncomplete {
		return fail(errors.New("pi: transcript evidence unavailable: pre-spawn transcript scan was incomplete"))
	}
	if h.evidenceClaim.isContaminated() {
		return fail(errors.New("pi: transcript evidence unavailable: concurrent session shared this state root"))
	}
	root := h.stateRoot
	if root == "" {
		// No per-session root was recorded at spawn: fail closed rather
		// than guessing ownership across a reused workarea.
		h.evidenceDone = true
		return nil, nil
	}
	descs, err := collectPiTranscriptDescriptors(root, h.evidenceBaseline)
	if err != nil {
		return fail(err)
	}
	if len(descs) == 0 {
		h.evidenceDone = true
		return nil, nil
	}
	h.evidenceDone = true
	h.evidenceCached = descs
	cp := make([]agent.TerminalTranscriptDescriptor, len(descs))
	copy(cp, descs)
	return cp, nil
}

// captureInteractiveEvidenceAtStop freezes this session's evidence once the
// final flush has been enqueued, before the overlap lease is released. See
// captureHeadlessEvidenceAtStop for the delayed-collection rationale.
// The caller must have joined activityFlushed already; if the flush has not
// completed the capture refuses so a partial scan can never freeze as
// success.
func (h *interactiveStateLossHandle) captureInteractiveEvidenceAtStop() {
	h.evidenceMu.Lock()
	defer h.evidenceMu.Unlock()
	if h.evidenceDone {
		return
	}
	select {
	case <-h.activityFlushed:
	default:
		h.evidenceDone = true
		h.evidenceCachedErr = errors.New("pi: transcript evidence unavailable until the final transcript flush completes")
		return
	}
	if h.evidenceIncomplete {
		h.evidenceDone = true
		h.evidenceCachedErr = errors.New("pi: transcript evidence unavailable: pre-spawn transcript scan was incomplete")
		return
	}
	if h.evidenceClaim.isContaminated() {
		h.evidenceClaim.markContaminated()
		h.evidenceDone = true
		h.evidenceCachedErr = errors.New("pi: transcript evidence unavailable: concurrent session shared this state root")
		return
	}
	if strings.TrimSpace(h.stateRoot) == "" {
		h.evidenceDone = true
		return
	}
	descs, err := collectPiTranscriptDescriptors(h.stateRoot, h.evidenceBaseline)
	if err != nil {
		h.evidenceDone = true
		h.evidenceCachedErr = err
		return
	}
	if len(descs) == 0 {
		h.evidenceDone = true
		return
	}
	h.evidenceDone = true
	h.evidenceCached = descs
}

// TerminalTranscripts implements agent.TerminalTranscriptCapable for the
// interactive PTY lane. It exposes the same per-session transcripts as the
// headless handle, but only after the transcript tailer's final flush has
// been enqueued: the tailer reads what the child wrote on its way out after
// InteractiveSession().Done closes, and the wrapper forwards its terminal
// ResultEvent only after that flush. Reporting paths before the flush would
// race the child's trailing writes.
func (h *interactiveStateLossHandle) TerminalTranscripts() ([]agent.TerminalTranscriptDescriptor, error) {
	select {
	case <-h.activityFlushed:
	default:
		return nil, errors.New("pi: transcript evidence unavailable until the final transcript flush completes")
	}
	h.evidenceMu.Lock()
	defer h.evidenceMu.Unlock()
	if h.evidenceDone {
		if h.evidenceCachedErr != nil {
			return nil, h.evidenceCachedErr
		}
		if len(h.evidenceCached) == 0 {
			return nil, nil
		}
		cp := make([]agent.TerminalTranscriptDescriptor, len(h.evidenceCached))
		copy(cp, h.evidenceCached)
		return cp, nil
	}
	fail := func(err error) ([]agent.TerminalTranscriptDescriptor, error) {
		h.evidenceDone = true
		h.evidenceCachedErr = err
		return nil, err
	}
	if h.evidenceIncomplete {
		return fail(errors.New("pi: transcript evidence unavailable: pre-spawn transcript scan was incomplete"))
	}
	if h.evidenceClaim.isContaminated() {
		return fail(errors.New("pi: transcript evidence unavailable: concurrent session shared this state root"))
	}
	if strings.TrimSpace(h.stateRoot) == "" {
		h.evidenceDone = true
		return nil, nil
	}
	descs, err := collectPiTranscriptDescriptors(h.stateRoot, h.evidenceBaseline)
	if err != nil {
		return fail(err)
	}
	if len(descs) == 0 {
		h.evidenceDone = true
		return nil, nil
	}
	h.evidenceDone = true
	h.evidenceCached = descs
	cp := make([]agent.TerminalTranscriptDescriptor, len(descs))
	copy(cp, descs)
	return cp, nil
}

// collectPiTranscriptDescriptors returns path-only descriptors for the
// regular .jsonl transcripts beneath the exact per-session layout root that
// this session created. It reuses the transcript walker so evidence and
// tailing agree on what a transcript is, then applies the evidence
// exclusions: the pre-spawn baseline (previous sessions reusing this
// workarea), the per-session config home, injected extensions, session
// tmp/cache content, credential files, policy extensions, symlinks,
// non-regular files, non-JSONL files, and any path outside the layout root
// are absent. A missing, non-directory, or link-swapped root yields no
// descriptors; a session with no transcript yields nil so callers keep
// today's cleanup behavior with no empty evidence artifact.
func collectPiTranscriptDescriptors(root string, baseline map[string]struct{}) ([]agent.TerminalTranscriptDescriptor, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	clean := filepath.Clean(root)
	info, err := os.Lstat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errors.New("pi: transcript evidence unavailable: post-stop transcript scan was incomplete")
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil
	}
	var descs []agent.TerminalTranscriptDescriptor
	var lstatErr error
	walkErr := walkInteractiveTranscriptFiles(clean, func(path string, _ os.DirEntry) {
		// A transcript that already existed before this session started
		// belongs to a previous session reusing this workarea, never to
		// this one — even when the child later appends to it (the tailer
		// in interactive_transcript.go skips baseline files the same way).
		if _, excluded := baseline[filepath.Clean(path)]; excluded {
			return
		}
		rel, err := filepath.Rel(clean, filepath.Clean(path))
		if err != nil || rel == "." || !filepath.IsLocal(rel) || strings.HasPrefix(rel, "..") {
			return
		}
		if isEvidenceExcludedRel(rel) {
			return
		}
		base := filepath.Base(path)
		switch base {
		case credentialFileName, extensionFileName, noticeClaimFileName, "auth.json":
			return
		}
		fi, err := os.Lstat(path)
		if err != nil {
			// A candidate transcript that cannot be statted fails closed:
			// dropping it would silently return empty or partial success
			// while the transcript exists on disk.
			if lstatErr == nil {
				lstatErr = err
			}
			return
		}
		if !fi.Mode().IsRegular() {
			return
		}
		descs = append(descs, agent.TerminalTranscriptDescriptor{Path: path})
	})
	if walkErr != nil || lstatErr != nil {
		// The post-stop scan hit a read error: fail closed rather than
		// returning a partial set that could hide this session's
		// transcript or misattribute another's.
		return nil, errors.New("pi: transcript evidence unavailable: post-stop transcript scan was incomplete")
	}
	if len(descs) == 0 {
		return nil, nil
	}
	sort.Slice(descs, func(i, j int) bool { return descs[i].Path < descs[j].Path })
	return descs, nil
}

// isEvidenceExcludedRel reports whether a layout-root-relative transcript
// candidate sits under a subtree that never holds session transcripts: the
// per-session config home, the injected-extensions dir, or the per-session
// tmp/cache dirs. The walker already skips these directories; this is
// defense-in-depth so evidence collection stays excluded even if the walk
// ever visits them.
func isEvidenceExcludedRel(rel string) bool {
	first := rel
	if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
		first = rel[:i]
	}
	switch first {
	case agentHomeDir, injectedExtensionsDir, piSessionTmpDir, piSessionCacheDir:
		return true
	default:
		return false
	}
}
