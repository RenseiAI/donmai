// Package daemon workarea_archive.go — on-disk workarea archive registry
// powering the Layer-3 workarea operator surface.
//
// Wave 9 / Track A3 / ADR-2026-05-07-daemon-http-control-api.md §D4a.
//
// Archive layout. Each archive is a directory under the daemon's archive
// root (default ~/.donmai/workareas/<archiveID>/) containing:
//
//	manifest.json   — metadata sidecar (id, sessionId, createdAt,
//	                  sizeBytes, sourceProvider, capabilities,
//	                  disposition); free-form extra fields permitted.
//	tree/           — the workarea filesystem snapshot. Diffs and
//	                  restores walk this subtree only; everything outside
//	                  it (manifest.json, daemon-private bookkeeping) is
//	                  ignored. The well-known .donmai/ directory under
//	                  tree/ is also excluded from diff walks per ADR D4a.
//
// The registry is stateless w.r.t. process lifecycle — every call hits
// disk. That's fine: archive directories are small in count (operator
// scale), the OS dentry cache absorbs repeated listings, and avoiding
// in-memory state means the daemon never serves a stale view after an
// out-of-band write to ~/.donmai/workareas/.
package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

const (
	workareaArchiveSchemaV1     = "donmai.workarea-archive.v1"
	workareaArchiveLegacyFlatV1 = "donmai.workarea-archive.legacy-flat-v1"
)

// defaultArchiveDir resolves the default archive root, ~/.donmai/workareas.
func defaultArchiveDir() string {
	return statepath.Resolve("workareas", "/tmp/.donmai/workareas")
}

// WorkareaArchiveErrCode is the sentinel set used by the registry for
// programmatic error discrimination at the HTTP layer. Wrapped with %w
// so handlers can errors.Is() against them.
var (
	// ErrArchiveNotFound — the named archive id is not present on disk.
	ErrArchiveNotFound = errors.New("workarea archive not found")
	// ErrArchiveCorrupted — the archive exists but its manifest is
	// missing/malformed, or the tree directory cannot be walked.
	ErrArchiveCorrupted = errors.New("workarea archive corrupted")
	// ErrArchiveExists — restore would collide with an existing archive
	// entry on disk (never reached today; archives are immutable, but
	// the check is here for a future "archive on restore" code path).
	ErrArchiveExists = errors.New("workarea archive already exists")
)

// archiveManifest is the on-disk shape of manifest.json. The wire shape
// emitted to clients is afclient.WorkareaSummary / Workarea — this is the
// internal carrier.
type archiveManifest struct {
	SchemaVersion  string                        `json:"schemaVersion,omitempty"`
	ID             string                        `json:"id"`
	AcquisitionID  string                        `json:"acquisitionId,omitempty"`
	WorkareaID     string                        `json:"workareaId,omitempty"`
	SessionID      string                        `json:"sessionId,omitempty"`
	OriginalRoot   string                        `json:"originalRoot,omitempty"`
	SelectedLeaf   string                        `json:"selectedLeaf,omitempty"`
	Repositories   []afclient.WorkareaRepository `json:"repositories,omitempty"`
	ProjectID      string                        `json:"projectId,omitempty"`
	ProviderID     string                        `json:"providerId,omitempty"`
	SourceProvider string                        `json:"sourceProvider,omitempty"`
	Disposition    string                        `json:"disposition,omitempty"`
	CreatedAt      string                        `json:"createdAt,omitempty"`
	SizeBytes      int64                         `json:"sizeBytes,omitempty"`
	Repository     string                        `json:"repository,omitempty"`
	Ref            string                        `json:"ref,omitempty"`
	Capabilities   []string                      `json:"capabilities,omitempty"`
	Toolchain      map[string]string             `json:"toolchain,omitempty"`
	// Extra holds any fields not declared above so consumers can render
	// them without the registry needing to evolve the manifest schema in
	// lockstep with archive producers.
	Extra map[string]any `json:"-"`
}

// WorkareaRootArchiveSpec is the secret-free whole-root capture input used by
// the terminal release authority.
type WorkareaRootArchiveSpec struct {
	AcquisitionID string
	WorkareaID    string
	SessionID     string
	WorkareaRoot  string
	SelectedPath  string
}

// WorkareaArchiveRegistry is the on-disk archive index. Construct via
// NewWorkareaArchiveRegistry. Methods are safe for concurrent use.
type WorkareaArchiveRegistry struct {
	root string
	// activeProvider is consulted by List for the active pool members so
	// the GET /api/daemon/workareas response can return both kinds in one
	// shape. May be nil — list returns archives only in that case.
	activeProvider ActiveWorkareaProvider

	// poolGuard enforces the saturation contract on Restore. Consulted at
	// the start of each restore; the implementation lives outside this
	// package (the daemon's WorkerSpawner / pool manager).
	poolGuard PoolCapacityGuard

	mu sync.Mutex
}

// ActiveWorkareaProvider exposes the daemon's live pool members in the
// canonical wire shape so List can union them with on-disk archives.
// Implementations MUST return a stable order. Empty list (zero pool
// members) is a perfectly valid, non-error response.
type ActiveWorkareaProvider interface {
	ActiveWorkareas() []afclient.WorkareaSummary
}

// StrictActiveWorkareaProvider is the additive fail-closed accounting surface.
// Production providers implement it; the legacy interface remains source
// compatible for embedders whose projections cannot fail.
type StrictActiveWorkareaProvider interface {
	ActiveWorkareasStrict() ([]afclient.WorkareaSummary, error)
}

// PoolCapacityGuard tells Restore whether a fresh pool member can be
// admitted. Returning a non-zero retryAfter indicates saturation —
// Restore propagates that to the HTTP handler as 503 + Retry-After.
type PoolCapacityGuard interface {
	// CheckCapacity returns nil + zero retryAfter when a new member
	// fits, or a non-zero retryAfter and an explanatory error when the
	// pool is saturated.
	CheckCapacity() (retryAfter time.Duration, err error)
}

// WorkareaArchiveOptions configures a registry.
type WorkareaArchiveOptions struct {
	// Root is the directory the registry scans. Empty selects the
	// default ~/.donmai/workareas.
	Root string
	// ActiveProvider is the live pool view; may be nil (archives-only
	// list, see ActiveWorkareaProvider).
	ActiveProvider ActiveWorkareaProvider
	// PoolGuard is consulted on Restore. May be nil — restore proceeds
	// without a saturation check.
	PoolGuard PoolCapacityGuard
}

// NewWorkareaArchiveRegistry constructs a registry against the given
// archive root. The directory is NOT created at construction time —
// missing-or-empty roots return an empty list (HTTP 200) per ADR D4a.
func NewWorkareaArchiveRegistry(opts WorkareaArchiveOptions) *WorkareaArchiveRegistry {
	root := opts.Root
	if root == "" {
		root = defaultArchiveDir()
	}
	return &WorkareaArchiveRegistry{
		root:           root,
		activeProvider: opts.ActiveProvider,
		poolGuard:      opts.PoolGuard,
	}
}

// Root returns the archive root directory the registry scans. Exposed
// for tests and operators surfacing the path.
func (r *WorkareaArchiveRegistry) Root() string { return r.root }

// ArchiveRoot atomically captures a complete session generation, including its
// declaration record and every declared leaf. A successful byte-identical retry
// is idempotent; an identity collision fails closed.
func (r *WorkareaArchiveRegistry) ArchiveRoot(ctx context.Context, spec WorkareaRootArchiveSpec) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if spec.WorkareaID == "" || spec.SessionID == "" || !validArchiveID(spec.WorkareaID) {
		return fmt.Errorf("archive root: invalid workarea identity")
	}
	root := workarea.RootPath(spec.WorkareaRoot)
	if !filepath.IsAbs(root.String()) || !filepath.IsAbs(spec.SelectedPath) {
		return fmt.Errorf("archive root: absolute root and selected path are required")
	}
	rootInfo, err := os.Lstat(root.String())
	if err != nil {
		return fmt.Errorf("archive root: inspect source root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("archive root: source root is not a real directory")
	}
	nested := filepath.Clean(root.String()) != filepath.Clean(spec.SelectedPath)
	schemaVersion := workareaArchiveLegacyFlatV1
	if nested {
		schemaVersion = workareaArchiveSchemaV1
	}
	manifest := archiveManifest{
		SchemaVersion: schemaVersion, ID: spec.WorkareaID,
		AcquisitionID: spec.AcquisitionID, WorkareaID: spec.WorkareaID,
		SessionID: spec.SessionID, OriginalRoot: root.String(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Disposition: "archive", SourceProvider: "local",
	}
	if nested {
		declaration, err := workarea.ReadDeclaration(root)
		if err != nil {
			return fmt.Errorf("archive root: read declaration: %w", err)
		}
		if err := workarea.ValidateDeclaredRoot(root, declaration); err != nil {
			return fmt.Errorf("archive root: validate declaration: %w", err)
		}
		if declaration.WorkareaID != spec.WorkareaID || declaration.SessionID != spec.SessionID || declaration.AcquisitionID == "" || declaration.AcquisitionID != spec.AcquisitionID {
			return fmt.Errorf("archive root: declaration identity mismatch")
		}
		for _, repository := range declaration.Repositories {
			manifest.Repositories = append(manifest.Repositories, afclient.WorkareaRepository{
				Name: repository.Name, Leaf: repository.Leaf, Role: string(repository.Role),
				Authority: string(repository.Authority), RequestedRef: repository.RequestedRef, ResolvedRef: repository.ResolvedRef,
			})
			if repository.Name == declaration.SelectedRepository {
				manifest.SelectedLeaf = repository.Leaf
			}
		}
		if manifest.SelectedLeaf == "" || filepath.Clean(filepath.Join(root.String(), manifest.SelectedLeaf)) != filepath.Clean(spec.SelectedPath) {
			return fmt.Errorf("archive root: selected repository path mismatch")
		}
	}
	manifest.SizeBytes, err = workarea.PhysicalUsage(root)
	if err != nil {
		return fmt.Errorf("archive root: physical accounting: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(r.root, 0o700); err != nil {
		return fmt.Errorf("archive root: create archive registry: %w", err)
	}
	final := r.archiveDir(spec.WorkareaID)
	if _, err := os.Lstat(final); err == nil {
		existing, readErr := r.readManifest(spec.WorkareaID)
		if readErr == nil && existing.SchemaVersion == schemaVersion && existing.AcquisitionID == spec.AcquisitionID && existing.WorkareaID == spec.WorkareaID && existing.SessionID == spec.SessionID {
			archivedRoot := workarea.RootPath(r.treeDir(spec.WorkareaID))
			if existing.SchemaVersion == workareaArchiveLegacyFlatV1 && spec.AcquisitionID == "" && existing.SelectedLeaf == "" {
				if info, treeErr := os.Lstat(archivedRoot.String()); treeErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
					return nil
				}
			}
			archivedDeclaration, declarationErr := workarea.ReadDeclaration(archivedRoot)
			if declarationErr == nil && archivedDeclaration.AcquisitionID == spec.AcquisitionID && archivedDeclaration.WorkareaID == spec.WorkareaID && archivedDeclaration.SessionID == spec.SessionID && workarea.ValidateDeclaredRoot(archivedRoot, archivedDeclaration) == nil {
				return nil
			}
		}
		return fmt.Errorf("archive root: identity collision: %w", ErrArchiveExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("archive root: inspect archive destination: %w", err)
	}
	stage, err := os.MkdirTemp(r.root, ".archive-"+spec.WorkareaID+"-")
	if err != nil {
		return fmt.Errorf("archive root: create stage: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := copyTree(root.String(), filepath.Join(stage, "tree")); err != nil {
		return fmt.Errorf("archive root: copy complete tree: %w", err)
	}
	body, err := json.Marshal(&manifest)
	if err != nil {
		return fmt.Errorf("archive root: encode manifest: %w", err)
	}
	stageRoot, err := os.OpenRoot(stage)
	if err != nil {
		return fmt.Errorf("archive root: open archive stage: %w", err)
	}
	defer func() { _ = stageRoot.Close() }()
	manifestFile, err := stageRoot.OpenFile("manifest.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("archive root: create manifest: %w", err)
	}
	if _, err := manifestFile.Write(body); err != nil {
		_ = manifestFile.Close()
		return fmt.Errorf("archive root: write manifest: %w", err)
	}
	if err := manifestFile.Sync(); err != nil {
		_ = manifestFile.Close()
		return fmt.Errorf("archive root: sync manifest: %w", err)
	}
	if err := manifestFile.Close(); err != nil {
		return fmt.Errorf("archive root: close manifest: %w", err)
	}
	if err := syncArchiveTree(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, final); err != nil {
		return fmt.Errorf("archive root: publish archive: %w", err)
	}
	if err := archiveSyncDir(r.root); err != nil {
		return err
	}
	committed = true
	return nil
}

// List walks the archive root and returns the union of on-disk archives
// (ordered deterministically by id) and the active pool members
// reported by the configured ActiveWorkareaProvider, if any. Missing-or-
// empty root is NOT an error — the response is just (empty active +
// empty archived).
func (r *WorkareaArchiveRegistry) List() (active, archived []afclient.WorkareaSummary, err error) {
	if r.activeProvider != nil {
		active, err = r.activeWorkareas()
		if err != nil {
			return nil, nil, err
		}
	}
	archived, err = r.listArchives()
	if err != nil {
		return active, nil, err
	}
	return active, archived, nil
}

func (r *WorkareaArchiveRegistry) activeWorkareas() ([]afclient.WorkareaSummary, error) {
	if r.activeProvider == nil {
		return nil, nil
	}
	if strict, ok := r.activeProvider.(StrictActiveWorkareaProvider); ok {
		return strict.ActiveWorkareasStrict()
	}
	return r.activeProvider.ActiveWorkareas(), nil
}

// listArchives scans the archive root non-recursively for direct
// children that contain a readable manifest.json. Subdirectories without
// a manifest are skipped (not errored) so future-proofing scratch
// directories don't break the listing.
func (r *WorkareaArchiveRegistry) listArchives() ([]afclient.WorkareaSummary, error) {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []afclient.WorkareaSummary{}, nil
		}
		return nil, fmt.Errorf("read archive root %q: %w", r.root, err)
	}
	out := make([]afclient.WorkareaSummary, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		summary, ok, err := r.summaryFor(ent.Name())
		if err != nil {
			// Corrupted manifest: surface as an entry with the disposition
			// flag so operators can see + clean up, rather than dropping
			// the row silently.
			out = append(out, afclient.WorkareaSummary{
				ID:          ent.Name(),
				Kind:        afclient.WorkareaKindArchived,
				Status:      afclient.WorkareaStatusArchived,
				Disposition: "corrupted: " + err.Error(),
			})
			continue
		}
		if !ok {
			continue
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns the full archive record for the named id. The Workarea
// Kind field is set to WorkareaKindArchived. Returns ErrArchiveNotFound
// when the id is absent.
func (r *WorkareaArchiveRegistry) Get(id string) (*afclient.Workarea, error) {
	if err := requireValidArchiveID("get archive", id); err != nil {
		return nil, err
	}
	manifest, err := r.readManifest(id)
	if err != nil {
		return nil, err
	}
	wa := manifestToWorkarea(id, manifest, r.treeDir(id))
	return &wa, nil
}

// Diff returns the structured per-path delta between two archives.
// Both ids MUST resolve to archives (live diffs are out of scope per ADR
// D4a). Walks are deterministic — entries are sorted by path. The
// well-known .donmai/ subtree under each archive's tree/ root is
// excluded.
func (r *WorkareaArchiveRegistry) Diff(idA, idB string) (*afclient.WorkareaDiffResult, error) {
	for _, id := range []string{idA, idB} {
		if err := requireValidArchiveID("diff", id); err != nil {
			return nil, err
		}
	}
	treeA := r.treeDir(idA)
	treeB := r.treeDir(idB)
	for _, p := range []string{treeA, treeB} {
		if _, err := os.Stat(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("diff: %s: %w", filepath.Base(p), ErrArchiveNotFound)
			}
			return nil, fmt.Errorf("stat %q: %w", p, err)
		}
	}
	walkA, err := walkArchiveTree(treeA)
	if err != nil {
		return nil, fmt.Errorf("walk archive A: %w: %w", err, ErrArchiveCorrupted)
	}
	walkB, err := walkArchiveTree(treeB)
	if err != nil {
		return nil, fmt.Errorf("walk archive B: %w: %w", err, ErrArchiveCorrupted)
	}
	entries, summary := diffWalkers(idA, idB, walkA, walkB)
	return &afclient.WorkareaDiffResult{
		Summary: summary,
		Entries: entries,
	}, nil
}

// DiffStream emits diff entries through the supplied callback as they
// are computed. The callback receives one entry at a time; if it
// returns a non-nil error the walk halts and the error is returned.
// After all entries are emitted DiffStream returns the aggregate
// summary so callers can write the trailing NDJSON line.
//
// The streaming variant exists so the HTTP handler can switch its
// Content-Type on entry count without buffering the entire diff.
func (r *WorkareaArchiveRegistry) DiffStream(
	idA, idB string,
	emit func(afclient.WorkareaDiffEntry) error,
) (*afclient.WorkareaDiffSummary, error) {
	for _, id := range []string{idA, idB} {
		if err := requireValidArchiveID("diff", id); err != nil {
			return nil, err
		}
	}
	treeA := r.treeDir(idA)
	treeB := r.treeDir(idB)
	for _, p := range []string{treeA, treeB} {
		if _, err := os.Stat(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("diff: %s: %w", filepath.Base(p), ErrArchiveNotFound)
			}
			return nil, fmt.Errorf("stat %q: %w", p, err)
		}
	}
	walkA, err := walkArchiveTree(treeA)
	if err != nil {
		return nil, fmt.Errorf("walk archive A: %w: %w", err, ErrArchiveCorrupted)
	}
	walkB, err := walkArchiveTree(treeB)
	if err != nil {
		return nil, fmt.Errorf("walk archive B: %w: %w", err, ErrArchiveCorrupted)
	}
	summary := afclient.WorkareaDiffSummary{WorkareaA: idA, WorkareaB: idB}
	for _, entry := range mergeDiffEntries(walkA, walkB) {
		if err := emit(entry); err != nil {
			return nil, err
		}
		switch entry.Status {
		case afclient.WorkareaDiffStatusAdded:
			summary.Added++
		case afclient.WorkareaDiffStatusRemoved:
			summary.Removed++
		case afclient.WorkareaDiffStatusModified:
			summary.Modified++
		}
		summary.Total++
	}
	return &summary, nil
}

// CountDiff returns the number of differing entries between two
// archives without buffering or streaming them. The handler uses this
// to pick JSON vs NDJSON before opening the response stream.
func (r *WorkareaArchiveRegistry) CountDiff(idA, idB string) (int, error) {
	for _, id := range []string{idA, idB} {
		if err := requireValidArchiveID("diff", id); err != nil {
			return 0, err
		}
	}
	treeA := r.treeDir(idA)
	treeB := r.treeDir(idB)
	for _, p := range []string{treeA, treeB} {
		if _, err := os.Stat(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return 0, fmt.Errorf("diff: %s: %w", filepath.Base(p), ErrArchiveNotFound)
			}
			return 0, fmt.Errorf("stat %q: %w", p, err)
		}
	}
	walkA, err := walkArchiveTree(treeA)
	if err != nil {
		return 0, fmt.Errorf("walk archive A: %w: %w", err, ErrArchiveCorrupted)
	}
	walkB, err := walkArchiveTree(treeB)
	if err != nil {
		return 0, fmt.Errorf("walk archive B: %w: %w", err, ErrArchiveCorrupted)
	}
	return len(mergeDiffEntries(walkA, walkB)), nil
}

// Restore materialises an archive into a fresh active pool member. The
// returned Workarea has Kind=Active and a NEW id distinct from the
// archive id (archives are immutable per ADR D4a). The tree/ subtree is
// copied to a per-restore directory under the archive root's sibling
// "restored/" so operators can find the materialised state from the
// daemon's host filesystem.
//
// IntoSessionID conflicts return ErrConflict; saturation returns
// ErrUnavailable + a non-zero retryAfter; corrupted archives return
// ErrArchiveCorrupted; missing archives return ErrArchiveNotFound.
func (r *WorkareaArchiveRegistry) Restore(
	archiveID string,
	req afclient.WorkareaRestoreRequest,
) (*afclient.Workarea, time.Duration, error) {
	if err := requireValidArchiveID("restore", archiveID); err != nil {
		return nil, 0, err
	}

	// Read the source manifest first — fast-fails on missing archive
	// before we touch the pool guard.
	manifest, err := r.readManifest(archiveID)
	if err != nil {
		return nil, 0, err
	}

	if r.poolGuard != nil {
		retryAfter, gerr := r.poolGuard.CheckCapacity()
		if gerr != nil || retryAfter > 0 {
			if retryAfter <= 0 {
				retryAfter = 30 * time.Second
			}
			if gerr == nil {
				gerr = errors.New("pool saturated")
			}
			return nil, retryAfter, fmt.Errorf("restore: %w (%s)", afclient.ErrUnavailable, gerr.Error())
		}
	}
	if manifest.SchemaVersion == workareaArchiveSchemaV1 {
		if req.IntoSessionID != "" && req.IntoSessionID != manifest.SessionID {
			return nil, 0, fmt.Errorf("restore: session-root archive is bound to session %q: %w", manifest.SessionID, afclient.ErrConflict)
		}
		workarea, err := r.restoreSessionRootArchive(archiveID, manifest)
		return workarea, 0, err
	}

	// Treat IntoSessionID conflict as same-id-already-restored — the
	// registry-side check is best-effort: the daemon's pool is the
	// authoritative source for live session ids. Here we look for a
	// previously-restored directory with the same intoSessionId, which
	// catches the common operator mistake of double-restoring.
	if req.IntoSessionID != "" {
		if conflicts, err := r.intoSessionIDInUse(req.IntoSessionID); err != nil {
			return nil, 0, fmt.Errorf("restore: %w", err)
		} else if conflicts {
			return nil, 0, fmt.Errorf("restore: intoSessionId %q already in use: %w",
				req.IntoSessionID, afclient.ErrConflict)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Generate a new id — derive from the archive id + timestamp so
	// operators can see the lineage at a glance and uniqueness is
	// guaranteed under second-resolution.
	now := time.Now().UTC()
	newID := fmt.Sprintf("%s-restore-%d", archiveID, now.UnixNano())
	restoredDir := r.restoredDir()
	if err := os.MkdirAll(restoredDir, 0o700); err != nil {
		return nil, 0, fmt.Errorf("restore: create restored root: %w", err)
	}
	dest := filepath.Join(restoredDir, newID)
	srcTree := r.treeDir(archiveID)

	// Source tree is a hard requirement for restore.
	if _, err := os.Stat(srcTree); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, fmt.Errorf("restore: archive tree missing for %q: %w",
				archiveID, ErrArchiveCorrupted)
		}
		return nil, 0, fmt.Errorf("restore: stat tree: %w", err)
	}
	if err := copyTree(srcTree, dest); err != nil {
		// Best-effort cleanup; ignore unlink errors. The path is
		// derived from a controlled archive id + restored dir; gosec's
		// taint analysis flags the call but the input is sanitised by
		// the registry's own id validation upstream.
		_ = os.RemoveAll(dest) //nolint:gosec
		return nil, 0, fmt.Errorf("restore: copy tree: %w: %w", err, ErrArchiveCorrupted)
	}

	// Persist a sidecar describing the restore so subsequent
	// intoSessionIDInUse() lookups can find it. This is intentionally
	// simple — a single JSON file at <restoredDir>/<newID>.json.
	sidecar := struct {
		RestoreID     string    `json:"restoreId"`
		ArchiveID     string    `json:"archiveId"`
		IntoSessionID string    `json:"intoSessionId,omitempty"`
		Reason        string    `json:"reason,omitempty"`
		RestoredAt    time.Time `json:"restoredAt"`
	}{
		RestoreID:     newID,
		ArchiveID:     archiveID,
		IntoSessionID: req.IntoSessionID,
		Reason:        req.Reason,
		RestoredAt:    now,
	}
	sidecarPath := filepath.Join(r.restoredDir(), newID+".json")
	if data, err := json.MarshalIndent(&sidecar, "", "  "); err == nil {
		// sidecarPath is composed from the registry's restored dir
		// (configured at construction time) and a newID we just
		// generated; not externally tainted, gosec's taint analyser
		// flags it because filepath.Join's first arg is dynamic.
		_ = os.WriteFile(sidecarPath, data, 0o600) //nolint:gosec
	}

	wa := manifestToWorkarea(archiveID, manifest, dest)
	wa.ID = newID
	wa.Kind = afclient.WorkareaKindActive
	wa.Status = afclient.WorkareaStatusReady
	wa.AcquiredAt = nil
	if req.IntoSessionID != "" {
		wa.SessionID = req.IntoSessionID
	}
	wa.ArchiveLocation = r.archiveDir(archiveID)
	now2 := now
	wa.AcquiredAt = &now2

	return &wa, 0, nil
}

func (r *WorkareaArchiveRegistry) restoreSessionRootArchive(archiveID string, manifest *archiveManifest) (*afclient.Workarea, error) {
	if manifest.AcquisitionID == "" || manifest.WorkareaID == "" || manifest.SessionID == "" || !filepath.IsAbs(manifest.OriginalRoot) {
		return nil, fmt.Errorf("restore: incomplete session-root archive identity: %w", ErrArchiveCorrupted)
	}
	parent := filepath.Dir(manifest.OriginalRoot)
	acquisitions, err := workarea.NewAcquisitionStore(parent, nil)
	if err != nil {
		return nil, fmt.Errorf("restore: open acquisition authority: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := os.Lstat(manifest.OriginalRoot); err == nil {
		return nil, fmt.Errorf("restore: original root is occupied: %w", afclient.ErrConflict)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("restore: inspect original root: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".workarea-restore-")
	if err != nil {
		return nil, fmt.Errorf("restore: create atomic root stage: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := copyTree(r.treeDir(archiveID), stage); err != nil {
		return nil, fmt.Errorf("restore: copy whole root: %w: %w", err, ErrArchiveCorrupted)
	}
	if err := syncArchiveTree(stage); err != nil {
		return nil, err
	}
	if err := os.Rename(stage, manifest.OriginalRoot); err != nil {
		return nil, fmt.Errorf("restore: publish original root: %w", err)
	}
	if err := archiveSyncDir(parent); err != nil {
		return nil, err
	}
	committed = true
	if _, err := acquisitions.AdoptRestoredRoot(
		manifest.AcquisitionID, manifest.WorkareaID, manifest.SessionID, workarea.RootPath(manifest.OriginalRoot),
	); err != nil {
		return nil, fmt.Errorf("restore: re-enter acquisition authority: %w", err)
	}
	wa := manifestToWorkarea(archiveID, manifest, manifest.OriginalRoot)
	wa.ID = manifest.WorkareaID
	wa.Kind = afclient.WorkareaKindActive
	wa.Status = afclient.WorkareaStatusReady
	wa.SessionID = manifest.SessionID
	wa.ArchiveLocation = r.archiveDir(archiveID)
	now := time.Now().UTC()
	wa.AcquiredAt = &now
	return &wa, nil
}

// validArchiveID reports whether id is a safe single-component archive id:
// non-empty, carrying no path separator, and not a "." / ".." traversal
// component. Archive ids index directly into the archive root via
// filepath.Join (archiveDir), so an id carrying a separator or a traversal
// segment would let a read/diff/restore escape the root. The local control API
// hands the id straight from the request path, and net/http only redirects a
// LITERAL ".." segment — a percent-encoded one ("%2e%2e") decodes to ".." and
// reaches here intact — so this must be enforced at the registry, not left to
// the HTTP router. Callers treat a rejected id as a missing archive.
func validArchiveID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, `/\`) {
		return false
	}
	// A well-formed single component is unchanged by Clean; anything Clean
	// rewrites (embedded traversal, redundant separators) is rejected.
	return filepath.Clean(id) == id
}

// requireValidArchiveID returns an ErrArchiveNotFound-wrapped error when id is
// not a safe single-component archive id (see validArchiveID). Rejected ids are
// reported as not-found so an escape attempt is indistinguishable from a miss.
func requireValidArchiveID(op, id string) error {
	if !validArchiveID(id) {
		return fmt.Errorf("%s: invalid archive id: %w", op, ErrArchiveNotFound)
	}
	return nil
}

// archiveDir returns the absolute path to a single archive directory. Callers
// MUST have validated id through requireValidArchiveID first — id is joined to
// the root with no further sanitisation.
func (r *WorkareaArchiveRegistry) archiveDir(id string) string {
	return filepath.Join(r.root, id)
}

// treeDir returns the absolute path to the tree subtree of an archive.
func (r *WorkareaArchiveRegistry) treeDir(id string) string {
	return filepath.Join(r.archiveDir(id), "tree")
}

// restoredDir returns the directory restored archives are materialised
// into. Sibling of the archive root.
func (r *WorkareaArchiveRegistry) restoredDir() string {
	return filepath.Join(filepath.Dir(r.root), filepath.Base(r.root)+"-restored")
}

// summaryFor reads an archive's manifest and returns the wire-level
// summary. Returns (zero, false, nil) when the directory exists but
// lacks a manifest — that's a normal "skip me" signal, not an error.
func (r *WorkareaArchiveRegistry) summaryFor(id string) (afclient.WorkareaSummary, bool, error) {
	manifestPath := filepath.Join(r.archiveDir(id), "manifest.json")
	info, err := os.Stat(manifestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return afclient.WorkareaSummary{}, false, nil
		}
		return afclient.WorkareaSummary{}, false, fmt.Errorf("stat manifest: %w", err)
	}
	if info.IsDir() {
		return afclient.WorkareaSummary{}, false, fmt.Errorf("manifest is a directory: %w", ErrArchiveCorrupted)
	}
	manifest, err := r.readManifest(id)
	if err != nil {
		return afclient.WorkareaSummary{}, false, err
	}
	created, _ := parseRFC3339(manifest.CreatedAt)
	repositoryPath := ""
	workareaRoot := ""
	repositories := append([]afclient.WorkareaRepository(nil), manifest.Repositories...)
	if manifest.SchemaVersion == workareaArchiveSchemaV1 || manifest.SchemaVersion == workareaArchiveLegacyFlatV1 {
		workareaRoot = r.treeDir(id)
		repositoryPath = workareaRoot
		if manifest.SelectedLeaf != "" {
			repositoryPath = filepath.Join(workareaRoot, manifest.SelectedLeaf)
		}
		for index := range repositories {
			repositories[index].Path = filepath.Join(workareaRoot, repositories[index].Leaf)
		}
	}
	return afclient.WorkareaSummary{
		ID:             firstNonEmptyStr(manifest.ID, id),
		Kind:           afclient.WorkareaKindArchived,
		ProviderID:     manifest.ProviderID,
		SessionID:      manifest.SessionID,
		ProjectID:      manifest.ProjectID,
		Status:         afclient.WorkareaStatusArchived,
		Ref:            manifest.Ref,
		Repository:     manifest.Repository,
		CreatedAt:      created,
		SizeBytes:      manifest.SizeBytes,
		SourceProvider: manifest.SourceProvider,
		Disposition:    manifest.Disposition,
		WorkareaRoot:   workareaRoot, RepositoryWorktreePath: repositoryPath,
		Repositories: repositories,
	}, true, nil
}

func (r *WorkareaArchiveRegistry) readManifest(id string) (*archiveManifest, error) {
	manifestPath := filepath.Join(r.archiveDir(id), "manifest.json")
	data, err := os.ReadFile(manifestPath) //nolint:gosec
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("manifest missing for %q: %w", id, ErrArchiveNotFound)
		}
		return nil, fmt.Errorf("read manifest %q: %w: %w", id, err, ErrArchiveCorrupted)
	}
	var m archiveManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %q: %w: %w", id, err, ErrArchiveCorrupted)
	}
	// Surface every key present in the source JSON via Extra so the
	// inspect endpoint can render fields the registry's struct doesn't
	// know about.
	var extra map[string]any
	if err := json.Unmarshal(data, &extra); err == nil {
		m.Extra = extra
	}
	return &m, nil
}

// intoSessionIDInUse scans previously-recorded restores for a sidecar
// matching intoSessionID. Returns true with no error when at least one
// matches.
func (r *WorkareaArchiveRegistry) intoSessionIDInUse(sessionID string) (bool, error) {
	dir := r.restoredDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read restored dir: %w", err)
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name())) //nolint:gosec
		if err != nil {
			continue
		}
		var sidecar struct {
			IntoSessionID string `json:"intoSessionId"`
		}
		if err := json.Unmarshal(data, &sidecar); err != nil {
			continue
		}
		if sidecar.IntoSessionID == sessionID {
			return true, nil
		}
	}
	return false, nil
}

// ── helpers ────────────────────────────────────────────────────────────────

// archiveEntry is one walked entry under a tree directory. Either
// IsSymlink or content-bearing; directories carry no hash. Path is the
// repo-relative slash-separated path so cross-platform hashing and
// sorting are deterministic.
type archiveEntry struct {
	Path      string
	IsDir     bool
	IsSymlink bool
	SymlinkTo string
	Size      int64
	ModeStr   string
	Hash      string // sha256 hex; empty for directories
}

// walkArchiveTree walks a tree root, skipping the well-known .donmai
// daemon-private subtree. The result is sorted by Path so subsequent
// merge-walks are deterministic.
func walkArchiveTree(root string) ([]archiveEntry, error) {
	out := make([]archiveEntry, 0, 64)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// Skip the well-known daemon-private subtree per ADR D4a.
		if rel == ".donmai" || strings.HasPrefix(rel, ".donmai/") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := archiveEntry{
			Path:    rel,
			IsDir:   d.IsDir(),
			Size:    info.Size(),
			ModeStr: info.Mode().String(),
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			entry.IsSymlink = true
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %q: %w", path, err)
			}
			entry.SymlinkTo = target
			entry.Hash = "" // symlinks compared by target string per ADR D4a
		case d.IsDir():
			// no hash for directories
		default:
			h, err := hashFile(path)
			if err != nil {
				return fmt.Errorf("hash %q: %w", path, err)
			}
			entry.Hash = h
		}
		out = append(out, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReader(f)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// mergeDiffEntries walks two sorted entry slices in lockstep and emits
// per-path diff entries. The output is sorted by path (inherited from
// the input ordering).
func mergeDiffEntries(a, b []archiveEntry) []afclient.WorkareaDiffEntry {
	out := make([]afclient.WorkareaDiffEntry, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i].Path < b[j].Path:
			out = append(out, entryAdded(a[i], false /* fromB */))
			i++
		case a[i].Path > b[j].Path:
			out = append(out, entryAdded(b[j], true /* fromB */))
			j++
		default:
			if entry, modified := entryModified(a[i], b[j]); modified {
				out = append(out, entry)
			}
			i++
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, entryAdded(a[i], false))
	}
	for ; j < len(b); j++ {
		out = append(out, entryAdded(b[j], true))
	}
	return out
}

// entryAdded emits a per-path entry for a path that exists in only one
// side of the diff. If fromB is true the path is in archive B (hence
// "added" relative to A); otherwise it's in archive A only ("removed"
// relative to B).
func entryAdded(e archiveEntry, fromB bool) afclient.WorkareaDiffEntry {
	out := afclient.WorkareaDiffEntry{Path: e.Path}
	if fromB {
		out.Status = afclient.WorkareaDiffStatusAdded
		out.SizeB = e.Size
		out.ModeB = e.ModeStr
		out.HashB = e.Hash
	} else {
		out.Status = afclient.WorkareaDiffStatusRemoved
		out.SizeA = e.Size
		out.ModeA = e.ModeStr
		out.HashA = e.Hash
	}
	return out
}

// entryModified emits a per-path entry only when the two entries
// differ. Returns (zero, false) when they are byte-equivalent so callers
// can skip emit entirely.
func entryModified(a, b archiveEntry) (afclient.WorkareaDiffEntry, bool) {
	switch {
	case a.IsSymlink || b.IsSymlink:
		if a.SymlinkTo == b.SymlinkTo && a.IsSymlink == b.IsSymlink && a.ModeStr == b.ModeStr {
			return afclient.WorkareaDiffEntry{}, false
		}
	case a.IsDir && b.IsDir:
		// directories equal — mode+path identical means same dir
		if a.ModeStr == b.ModeStr {
			return afclient.WorkareaDiffEntry{}, false
		}
	default:
		if a.Hash == b.Hash && a.Size == b.Size && a.ModeStr == b.ModeStr {
			return afclient.WorkareaDiffEntry{}, false
		}
	}
	return afclient.WorkareaDiffEntry{
		Path:   a.Path,
		Status: afclient.WorkareaDiffStatusModified,
		SizeA:  a.Size, SizeB: b.Size,
		ModeA: a.ModeStr, ModeB: b.ModeStr,
		HashA: a.Hash, HashB: b.Hash,
	}, true
}

// diffWalkers is the buffer-and-summarise variant used by Diff. It
// shares logic with DiffStream's mergeDiffEntries but materialises both
// entries and summary in one pass.
func diffWalkers(idA, idB string, a, b []archiveEntry) (
	[]afclient.WorkareaDiffEntry, afclient.WorkareaDiffSummary,
) {
	entries := mergeDiffEntries(a, b)
	summary := afclient.WorkareaDiffSummary{WorkareaA: idA, WorkareaB: idB}
	for _, e := range entries {
		switch e.Status {
		case afclient.WorkareaDiffStatusAdded:
			summary.Added++
		case afclient.WorkareaDiffStatusRemoved:
			summary.Removed++
		case afclient.WorkareaDiffStatusModified:
			summary.Modified++
		}
		summary.Total++
	}
	return entries, summary
}

// copyTree copies the source directory tree to dst, preserving symlinks
// (re-created with their original target string) and file modes.
// Every traversal and mutation after the two root opens is descriptor-relative.
func copyTree(src, dst string) error {
	sourceParent, sourceLeaf := filepath.Dir(src), filepath.Base(src)
	if sourceLeaf == "." || sourceLeaf == ".." || sourceLeaf == "" {
		return fmt.Errorf("archive copy source is not a safe leaf")
	}
	sourceParentRoot, err := os.OpenRoot(sourceParent)
	if err != nil {
		return err
	}
	defer func() { _ = sourceParentRoot.Close() }()
	sourceInfo, err := sourceParentRoot.Lstat(sourceLeaf)
	if err != nil {
		return err
	}
	if !sourceInfo.IsDir() || sourceInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("archive copy source is not a real directory")
	}
	sourceRoot, err := sourceParentRoot.OpenRoot(sourceLeaf)
	if err != nil {
		return err
	}
	defer func() { _ = sourceRoot.Close() }()
	openedSourceInfo, err := sourceRoot.Stat(".")
	if err != nil || !os.SameFile(sourceInfo, openedSourceInfo) {
		return fmt.Errorf("archive copy source identity changed while opening")
	}
	destinationParent, destinationLeaf := filepath.Dir(dst), filepath.Base(dst)
	if destinationLeaf == "." || destinationLeaf == ".." || destinationLeaf == "" {
		return fmt.Errorf("archive copy destination is not a safe leaf")
	}
	parentRoot, err := os.OpenRoot(destinationParent)
	if err != nil {
		return err
	}
	defer func() { _ = parentRoot.Close() }()
	if err := parentRoot.MkdirAll(destinationLeaf, sourceInfo.Mode().Perm()); err != nil {
		return err
	}
	destinationRoot, err := parentRoot.OpenRoot(destinationLeaf)
	if err != nil {
		return err
	}
	defer func() { _ = destinationRoot.Close() }()
	if err := destinationRoot.Chmod(".", sourceInfo.Mode().Perm()); err != nil {
		return err
	}
	return copyRootContents(sourceRoot, destinationRoot)
}

func copyRootContents(source, destination *os.Root) error {
	directory, err := source.Open(".")
	if err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	if closeErr := directory.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		info, err := source.Lstat(name)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := source.Readlink(name)
			if err != nil {
				return err
			}
			if err := destination.Symlink(target, name); err != nil {
				return err
			}
		case info.IsDir():
			if err := destination.Mkdir(name, info.Mode().Perm()); err != nil {
				return err
			}
			sourceChild, err := source.OpenRoot(name)
			if err != nil {
				return err
			}
			openedChildInfo, err := sourceChild.Stat(".")
			if err != nil || !os.SameFile(info, openedChildInfo) {
				_ = sourceChild.Close()
				return fmt.Errorf("archive copy source directory identity changed")
			}
			destinationChild, err := destination.OpenRoot(name)
			if err != nil {
				_ = sourceChild.Close()
				return err
			}
			copyErr := copyRootContents(sourceChild, destinationChild)
			_ = sourceChild.Close()
			_ = destinationChild.Close()
			if copyErr != nil {
				return copyErr
			}
			if err := destination.Chmod(name, info.Mode().Perm()); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyRootFile(source, destination, name, info); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive copy refuses special file %q", name)
		}
	}
	return nil
}

func copyRootFile(source, destination *os.Root, name string, expected os.FileInfo) error {
	in, err := source.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(expected, opened) {
		return fmt.Errorf("archive copy source file identity changed")
	}
	out, err := destination.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, expected.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, bufio.NewReader(in)); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return destination.Chmod(name, expected.Mode().Perm())
}

func syncArchiveTree(root string) error {
	var directories []string
	// root is an archive stage freshly minted by ArchiveRoot or a restored tree
	// copied from a validated archive id; it is never a request path.
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error { //nolint:gosec
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("archive root: enumerate directories for sync: %w", err)
	}
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		if err := archiveSyncDir(directory); err != nil {
			return err
		}
	}
	return nil
}

func archiveSyncDir(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return fmt.Errorf("archive root: open directory root for sync: %w", err)
	}
	defer func() { _ = root.Close() }()
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("archive root: open directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("archive root: sync directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("archive root: close synced directory: %w", err)
	}
	return nil
}

func parseRFC3339(s string) (*time.Time, bool) {
	if s == "" {
		return nil, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, false
	}
	return &t, true
}

// manifestToWorkarea builds the wire-level Workarea record for an
// archive. The base path is the archive's tree root.
func manifestToWorkarea(id string, m *archiveManifest, treePath string) afclient.Workarea {
	var manifestExtra map[string]any
	if m.Extra != nil {
		manifestExtra = m.Extra
	}
	path := treePath
	workareaRoot := ""
	repositoryPath := ""
	repositories := append([]afclient.WorkareaRepository(nil), m.Repositories...)
	if m.SchemaVersion == workareaArchiveSchemaV1 || m.SchemaVersion == workareaArchiveLegacyFlatV1 {
		workareaRoot = treePath
		repositoryPath = treePath
		if m.SelectedLeaf != "" {
			repositoryPath = filepath.Join(treePath, m.SelectedLeaf)
		}
		path = repositoryPath
		for index := range repositories {
			repositories[index].Path = filepath.Join(treePath, repositories[index].Leaf)
		}
	}
	return afclient.Workarea{
		ID:                     firstNonEmptyStr(m.ID, id),
		Kind:                   afclient.WorkareaKindArchived,
		ProviderID:             m.ProviderID,
		SessionID:              m.SessionID,
		ProjectID:              m.ProjectID,
		Status:                 afclient.WorkareaStatusArchived,
		Path:                   path,
		WorkareaRoot:           workareaRoot,
		RepositoryWorktreePath: repositoryPath,
		Repositories:           repositories,
		Ref:                    m.Ref,
		Repository:             m.Repository,
		Toolchain:              m.Toolchain,
		ArchiveLocation:        filepath.Dir(treePath),
		Manifest:               manifestExtra,
	}
}
