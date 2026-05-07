// Package workarea hosts the canonical renderers for the af /
// rensei `workarea` command tree, sourced from afclient.Workarea wire
// types. Lifted from the previous rensei-tui resident copy under
// rensei-tui/internal/views/workarea per ADR-2026-05-07-daemon-http-
// control-api.md §D3, with the wire shape upgraded to ADR D4a — both
// active pool members and on-disk archives flow through one renderer
// keyed by the Kind field, and the diff renderer consumes the
// structured WorkareaDiffResult shape (path / status / hashes) rather
// than the previous {added,removed,modified} arrays.
//
// Two output paths:
//
//   - RenderList / RenderInspect / RenderRestore / RenderDiff —
//     ANSI rendering for TTY users.
//   - PlainList / PlainInspect / PlainRestore / PlainDiff —
//     deterministic plain-text rendering used by rensei-smokes
//     integration tests and by `--plain` mode. No ANSI escapes,
//     no emoji, deterministic ordering.
//
// rensei-smokes' diff smoke pins against PlainDiff's tabular output;
// any change to its column layout is a contract change the smokes
// will catch.
package workarea

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// RenderList writes a unified table of workareas covering both active
// pool members and on-disk archives. Active members come first
// (typically the operator's primary interest); archives follow.
// When noColor is true (NO_COLOR=1 or plainMode), ANSI escapes are
// suppressed.
func RenderList(out io.Writer, resp *afclient.ListWorkareasResponse, noColor bool) error {
	if resp == nil || (len(resp.Active) == 0 && len(resp.Archived) == 0) {
		_, _ = fmt.Fprintln(out, muted("No workareas found.", noColor))
		return nil
	}

	if len(resp.Active) > 0 {
		_, _ = fmt.Fprintln(out, sectionHeader("ACTIVE", noColor))
		if err := writeWorkareaTable(out, resp.Active, noColor); err != nil {
			return err
		}
	}
	if len(resp.Archived) > 0 {
		if len(resp.Active) > 0 {
			_, _ = fmt.Fprintln(out)
		}
		_, _ = fmt.Fprintln(out, sectionHeader("ARCHIVED", noColor))
		if err := writeWorkareaTable(out, resp.Archived, noColor); err != nil {
			return err
		}
	}
	return nil
}

// PlainList is the smoke-pinning plain-text variant. Equivalent to
// RenderList with noColor=true. Exposed as a stable name so smokes can
// import it directly.
func PlainList(out io.Writer, resp *afclient.ListWorkareasResponse) error {
	return RenderList(out, resp, true)
}

func writeWorkareaTable(out io.Writer, rows []afclient.WorkareaSummary, noColor bool) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n",
		colHeader("ID", noColor),
		colHeader("STATUS", noColor),
		colHeader("REF", noColor),
		colHeader("AGE", noColor),
		colHeader("PROJECT", noColor),
		colHeader("PROVIDER", noColor),
	)
	// Sort by ID for deterministic plain-text output.
	sorted := append([]afclient.WorkareaSummary(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, wa := range sorted {
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			bold(shortID(wa.ID, ""), noColor),
			statusStr(wa.Status, noColor),
			muted(shortRef(wa.Ref), noColor),
			muted(formatAge(wa.AgeSeconds), noColor),
			muted(shortID(wa.ProjectID, ""), noColor),
			muted(wa.ProviderID, noColor),
		)
	}
	return w.Flush()
}

// RenderInspect writes the full detail view for a single workarea.
// Works for both active and archived workareas; the Kind field is
// surfaced explicitly so operators can see at a glance whether they're
// looking at a live pool member or a frozen archive.
func RenderInspect(out io.Writer, wa *afclient.Workarea, noColor bool) error {
	if wa == nil {
		_, _ = fmt.Fprintln(out, muted("No workarea record.", noColor))
		return nil
	}
	label(out, "ID:          ", wa.ID, noColor)
	if wa.Kind != "" {
		label(out, "Kind:        ", string(wa.Kind), noColor)
	}
	if wa.SessionID != "" {
		label(out, "Session:     ", wa.SessionID, noColor)
	}
	if wa.OwnerSession != "" && wa.OwnerSession != wa.SessionID {
		label(out, "Owner:       ", wa.OwnerSession, noColor)
	}
	label(out, "Status:      ", statusStr(wa.Status, noColor), noColor)
	if wa.ProviderID != "" {
		label(out, "Provider:    ", wa.ProviderID, noColor)
	}
	if wa.Repository != "" {
		label(out, "Repository:  ", wa.Repository, noColor)
	}
	if wa.Ref != "" {
		label(out, "Ref:         ", wa.Ref, noColor)
	}
	if wa.Mode != "" {
		label(out, "Mode:        ", wa.Mode, noColor)
	}
	if wa.AcquirePath != "" {
		label(out, "AcquirePath: ", wa.AcquirePath, noColor)
	}
	if wa.Path != "" {
		label(out, "Path:        ", wa.Path, noColor)
	}
	if wa.CleanStateChecksum != "" {
		label(out, "Checksum:    ", wa.CleanStateChecksum, noColor)
	}
	if wa.ArchiveLocation != "" {
		label(out, "Archive:     ", wa.ArchiveLocation, noColor)
	}
	if wa.AcquiredAt != nil {
		label(out, "Acquired:    ", wa.AcquiredAt.Format(time.RFC3339), noColor)
	}
	if wa.ReleasedAt != nil {
		label(out, "Released:    ", wa.ReleasedAt.Format(time.RFC3339), noColor)
	}
	if len(wa.Toolchain) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, bold("Toolchain:", noColor))
		// Sort keys for determinism.
		keys := make([]string, 0, len(wa.Toolchain))
		for k := range wa.Toolchain {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			_, _ = fmt.Fprintf(out, "  %s%s\n",
				muted(k+": ", noColor),
				bold(wa.Toolchain[k], noColor),
			)
		}
	}
	return nil
}

// PlainInspect is the smoke-pinning variant.
func PlainInspect(out io.Writer, wa *afclient.Workarea) error {
	return RenderInspect(out, wa, true)
}

// RenderRestore writes the result of a workarea restore operation.
// Per ADR D4a the restore returns a NEW active workarea distinct from
// the source archive; the renderer emphasises the lineage.
func RenderRestore(out io.Writer, res *afclient.WorkareaRestoreResult, noColor bool) error {
	if res == nil {
		_, _ = fmt.Fprintln(out, muted("No restore result.", noColor))
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s %s\n",
		green(checkSymbol(noColor), noColor),
		bold("Workarea restored", noColor),
	)
	label(out, "ID:          ", res.Workarea.ID, noColor)
	label(out, "Kind:        ", string(res.Workarea.Kind), noColor)
	label(out, "Status:      ", statusStr(res.Workarea.Status, noColor), noColor)
	if res.Workarea.SessionID != "" {
		label(out, "Session:     ", res.Workarea.SessionID, noColor)
	}
	if res.Workarea.Path != "" {
		label(out, "Path:        ", res.Workarea.Path, noColor)
	}
	if res.Workarea.ArchiveLocation != "" {
		label(out, "FromArchive: ", res.Workarea.ArchiveLocation, noColor)
	}
	return nil
}

// PlainRestore is the smoke-pinning variant.
func PlainRestore(out io.Writer, res *afclient.WorkareaRestoreResult) error {
	return RenderRestore(out, res, true)
}

// RenderDiff writes the structured per-path delta between two archived
// workareas. Plain-text mode emits a deterministic tabular output that
// rensei-smokes pins against:
//
//	A: <idA>
//	B: <idB>
//	STATUS    PATH                                            ...
//	added     new-file.txt
//	modified  src/main.go
//	removed   old-file.txt
//	---
//	added: 1, removed: 1, modified: 1, total: 3
//
// Hashes are not surfaced in the table form to keep the column layout
// stable regardless of file size — operators inspect via --json when
// they need them.
func RenderDiff(out io.Writer, diff *afclient.WorkareaDiffResult, noColor bool) error {
	if diff == nil {
		_, _ = fmt.Fprintln(out, muted("No diff result.", noColor))
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s  %s\n",
		muted("A:", noColor), bold(diff.Summary.WorkareaA, noColor))
	_, _ = fmt.Fprintf(out, "%s  %s\n",
		muted("B:", noColor), bold(diff.Summary.WorkareaB, noColor))
	_, _ = fmt.Fprintln(out)

	if len(diff.Entries) == 0 {
		_, _ = fmt.Fprintln(out, muted("No differences.", noColor))
		return nil
	}

	// Determinism: entries are already sorted by path on the wire, but
	// re-sort defensively to insulate the renderer from server-side
	// regressions.
	rows := append([]afclient.WorkareaDiffEntry(nil), diff.Entries...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Status != rows[j].Status {
			return statusOrder(rows[i].Status) < statusOrder(rows[j].Status)
		}
		return rows[i].Path < rows[j].Path
	})

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "%s\t%s\n",
		colHeader("STATUS", noColor),
		colHeader("PATH", noColor),
	)
	for _, e := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\n",
			diffStatusStr(e.Status, noColor),
			e.Path,
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush tabwriter: %w", err)
	}
	_, _ = fmt.Fprintln(out, muted("---", noColor))
	_, _ = fmt.Fprintf(out, "%s\n",
		muted(fmt.Sprintf("added: %d, removed: %d, modified: %d, total: %d",
			diff.Summary.Added,
			diff.Summary.Removed,
			diff.Summary.Modified,
			diff.Summary.Total,
		), noColor),
	)
	return nil
}

// PlainDiff is the smoke-pinning variant.
func PlainDiff(out io.Writer, diff *afclient.WorkareaDiffResult) error {
	return RenderDiff(out, diff, true)
}

// NoColorEnv returns true when NO_COLOR is set in the environment.
func NoColorEnv() bool {
	return os.Getenv("NO_COLOR") != ""
}

// ---- formatting helpers -----------------------------------------------------

// statusOrder enforces a stable cross-status ordering: added → modified
// → removed. Within a status, paths sort lexicographically. Smokes pin
// against this exact order.
func statusOrder(s afclient.WorkareaDiffStatus) int {
	switch s {
	case afclient.WorkareaDiffStatusAdded:
		return 0
	case afclient.WorkareaDiffStatusModified:
		return 1
	case afclient.WorkareaDiffStatusRemoved:
		return 2
	default:
		return 99
	}
}

// shortID returns a shortened display form of an ID. Falls back to
// fallback when id is empty.
func shortID(id, fallback string) string {
	if id == "" {
		return fallback
	}
	if len(id) > 24 {
		return id[:10] + ".." + id[len(id)-8:]
	}
	return id
}

// shortRef trims a git ref to a concise display form.
func shortRef(ref string) string {
	if strings.HasPrefix(ref, "refs/heads/") {
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

// formatAge converts an age in seconds to a human-readable duration.
func formatAge(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ---- ANSI helpers -----------------------------------------------------------

const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiRed     = "\033[31m"
	ansiCyan    = "\033[36m"
	ansiMagenta = "\033[35m"
)

func bold(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiBold + s + ansiReset
}

func muted(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiDim + s + ansiReset
}

func green(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiGreen + s + ansiReset
}

func colHeader(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiDim + ansiBold + s + ansiReset
}

func sectionHeader(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiMagenta + ansiBold + s + ansiReset
}

func label(out io.Writer, key, value string, noColor bool) {
	_, _ = fmt.Fprintf(out, "%s %s\n", muted(key, noColor), bold(value, noColor))
}

func checkSymbol(noColor bool) string {
	if noColor {
		return "[ok]"
	}
	return "✓"
}

func statusStr(status afclient.WorkareaPoolStatus, noColor bool) string {
	if noColor {
		return string(status)
	}
	switch status {
	case afclient.WorkareaStatusAcquired:
		return ansiCyan + string(status) + ansiReset
	case afclient.WorkareaStatusReady:
		return ansiGreen + string(status) + ansiReset
	case afclient.WorkareaStatusWarming, afclient.WorkareaStatusReleasing:
		return ansiYellow + string(status) + ansiReset
	case afclient.WorkareaStatusInvalid, afclient.WorkareaStatusRetired:
		return ansiRed + string(status) + ansiReset
	default:
		return ansiDim + string(status) + ansiReset
	}
}

func diffStatusStr(s afclient.WorkareaDiffStatus, noColor bool) string {
	if noColor {
		return string(s)
	}
	switch s {
	case afclient.WorkareaDiffStatusAdded:
		return ansiGreen + string(s) + ansiReset
	case afclient.WorkareaDiffStatusModified:
		return ansiYellow + string(s) + ansiReset
	case afclient.WorkareaDiffStatusRemoved:
		return ansiRed + string(s) + ansiReset
	default:
		return string(s)
	}
}
