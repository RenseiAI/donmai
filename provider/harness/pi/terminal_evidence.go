package pi

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

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
// Called once at spawn from the pre-spawn snapshot, before the pump starts.
func (h *Handle) setEvidenceBaseline(baseline map[string]struct{}) {
	if len(baseline) == 0 {
		return
	}
	h.evidenceBaseline = baseline
}

// snapshotPiTranscriptBaseline records every transcript already present under
// root. Call it after the parent has finished writing the layout (extensions,
// credentials, tmp/cache dirs) and before the child starts, so only files
// this session did not create are recorded. A missing root snapshots to nil.
func snapshotPiTranscriptBaseline(root string) map[string]struct{} {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	clean := filepath.Clean(root)
	if info, err := os.Lstat(clean); err != nil || !info.IsDir() {
		return nil
	}
	var baseline map[string]struct{}
	walkInteractiveTranscriptFiles(clean, func(path string, _ os.DirEntry) {
		if baseline == nil {
			baseline = make(map[string]struct{})
		}
		baseline[filepath.Clean(path)] = struct{}{}
	})
	return baseline
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
	root := h.stateRoot
	if root == "" {
		// No per-session root was recorded at spawn: fail closed rather
		// than guessing ownership across a reused workarea.
		return nil, nil
	}
	descs := collectPiTranscriptDescriptors(root, h.evidenceBaseline)
	if len(descs) == 0 {
		return nil, nil
	}
	return descs, nil
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
	if strings.TrimSpace(h.stateRoot) == "" {
		return nil, nil
	}
	descs := collectPiTranscriptDescriptors(h.stateRoot, h.evidenceBaseline)
	if len(descs) == 0 {
		return nil, nil
	}
	return descs, nil
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
func collectPiTranscriptDescriptors(root string, baseline map[string]struct{}) []agent.TerminalTranscriptDescriptor {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	clean := filepath.Clean(root)
	info, err := os.Lstat(clean)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	var descs []agent.TerminalTranscriptDescriptor
	walkInteractiveTranscriptFiles(clean, func(path string, _ os.DirEntry) {
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
		if err != nil || !fi.Mode().IsRegular() {
			return
		}
		descs = append(descs, agent.TerminalTranscriptDescriptor{Path: path})
	})
	if len(descs) == 0 {
		return nil
	}
	sort.Slice(descs, func(i, j int) bool { return descs[i].Path < descs[j].Path })
	return descs
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
