package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// This file implements the proactive migration for the retired
// per-repository clone override: files that still carry the key keep
// loading (with a once-per-process warning), and the sweep below strips
// the key from every daemon config file so operators never have to edit
// YAML by hand. `host update` runs the sweep on the upgrade path and
// `host doctor --fix` runs it on demand.

// retiredKeyName is the retired per-repository clone override key. Writers
// never emit it and readers ignore it; only this sweep deletes it.
const retiredKeyName = "cloneStrategy"

// RetiredKeySweepFile is the per-file outcome of SweepRetiredKeys.
type RetiredKeySweepFile struct {
	// Path is the config file examined.
	Path string
	// Removed is the number of entries the key was stripped from.
	Removed int
	// BackupPath is the sibling backup holding the pre-strip content.
	// Empty when the file was left untouched.
	BackupPath string
	// Untouched is true when the file held no retired key and was not
	// written (no backup, mode and bytes preserved trivially).
	Untouched bool
	// Skipped is true when the file could not be stripped safely (not a
	// regular file, unreadable, unparseable, or a non-scalar occurrence).
	// SkippedReason names the cause. The file is left exactly as found.
	Skipped bool
	// SkippedReason explains a skip. Empty unless Skipped is true.
	SkippedReason string
}

// RetiredKeySweepSummary aggregates one sweep run over every candidate
// config file.
type RetiredKeySweepSummary struct {
	Files []RetiredKeySweepFile
}

// FilesCleaned reports how many files lost at least one retired key.
func (s RetiredKeySweepSummary) FilesCleaned() int {
	n := 0
	for _, f := range s.Files {
		if f.Removed > 0 {
			n++
		}
	}
	return n
}

// KeysRemoved reports the total number of entries stripped.
func (s RetiredKeySweepSummary) KeysRemoved() int {
	n := 0
	for _, f := range s.Files {
		n += f.Removed
	}
	return n
}

// FilesChecked reports how many files the sweep examined (skips included).
func (s RetiredKeySweepSummary) FilesChecked() int {
	return len(s.Files)
}

// Line renders the single summary line callers print after a sweep.
func (s RetiredKeySweepSummary) Line() string {
	if s.KeysRemoved() > 0 {
		return fmt.Sprintf("retired-key sweep: stripped %d retired key(s) from %d of %d config file(s); backups kept alongside",
			s.KeysRemoved(), s.FilesCleaned(), s.FilesChecked())
	}
	return fmt.Sprintf("retired-key sweep: no retired keys in %d config file(s)", s.FilesChecked())
}

// DefaultSweepPaths returns the candidate config files a sweep examines:
// every daemon*.yaml sibling in the canonical config directory (and in the
// explicit path's directory when one is given), plus the explicit path
// itself. Missing files are filtered out by the sweep itself; the result
// is sorted and deduplicated.
func DefaultSweepPaths(explicit string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(path string) {
		if path == "" {
			return
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		if _, ok := seen[abs]; ok {
			return
		}
		seen[abs] = struct{}{}
		out = append(out, path)
	}
	addSiblingConfigs := func(dir string) {
		if matches, err := filepath.Glob(filepath.Join(dir, "daemon*.yaml")); err == nil {
			sort.Strings(matches)
			for _, m := range matches {
				add(m)
			}
		}
	}
	if explicit != "" {
		add(explicit)
		addSiblingConfigs(filepath.Dir(explicit))
	}
	canonical := DefaultConfigPath()
	add(canonical)
	addSiblingConfigs(filepath.Dir(canonical))
	sort.Strings(out)
	return out
}

// SweepRetiredKeys strips the retired key from every config file in paths.
// Files without the key are left untouched: no write, no backup. Stripped
// files are rewritten atomically (sibling temp file + rename) with the
// source mode preserved, and the pre-strip content is kept in a sibling
// backup. The rewrite deletes exactly the retired key's source lines, so
// every other field survives byte-for-byte, and a second run is a no-op.
//
// Files that cannot be stripped safely (missing, not a regular file,
// unreadable, unparseable, or a non-inline occurrence) are recorded as
// skipped and left exactly as found. An explicit path that does not exist
// is an error; globbed or default candidates that are absent are skipped
// silently. The returned error joins every per-file failure.
func SweepRetiredKeys(paths []string) (RetiredKeySweepSummary, error) {
	var summary RetiredKeySweepSummary
	var errs []error
	for _, path := range paths {
		result, err := sweepRetiredKeysInFile(path)
		if err != nil {
			// A missing explicit candidate is a caller error, not a skip.
			errs = append(errs, err)
			continue
		}
		if result == nil {
			continue
		}
		if result.Skipped {
			errs = append(errs, fmt.Errorf("sweep %q: %s", path, result.SkippedReason))
		}
		summary.Files = append(summary.Files, *result)
	}
	return summary, errors.Join(errs...)
}

// sweepRetiredKeysInFile examines one file. It returns (nil, nil) when the
// file is absent — globbed and default candidates that simply do not exist
// are silent skips. Callers that pass an explicit path check its existence
// first so a mistyped flag fails loudly instead of sweeping nothing.
func sweepRetiredKeysInFile(path string) (*RetiredKeySweepFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return &RetiredKeySweepFile{Path: path, Skipped: true, SkippedReason: err.Error()}, nil
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return &RetiredKeySweepFile{
			Path:          path,
			Skipped:       true,
			SkippedReason: "not a regular file",
		}, nil
	}
	original, err := os.ReadFile(path) //nolint:gosec // operator-owned config path
	if err != nil {
		return &RetiredKeySweepFile{Path: path, Skipped: true, SkippedReason: err.Error()}, nil
	}
	plan, err := planRetiredKeyStrip(original)
	if err != nil {
		return &RetiredKeySweepFile{Path: path, Skipped: true, SkippedReason: err.Error()}, nil
	}
	if len(plan.lines) == 0 && !plan.unsafe {
		return &RetiredKeySweepFile{Path: path, Untouched: true}, nil
	}
	if plan.unsafe {
		return &RetiredKeySweepFile{
			Path:          path,
			Skipped:       true,
			SkippedReason: plan.reason + "; file left as found",
		}, nil
	}
	stripped := deleteSourceLines(original, plan.lines)
	if err := verifyRetiredKeyStrip(original, stripped); err != nil {
		return &RetiredKeySweepFile{
			Path:          path,
			Skipped:       true,
			SkippedReason: "post-strip verification failed (" + err.Error() + "); file left as found",
		}, nil
	}
	removed := len(plan.lines)
	backupPath, err := writeRetiredKeySweep(path, info.Mode().Perm(), original, stripped)
	if err != nil {
		return &RetiredKeySweepFile{Path: path, Skipped: true, SkippedReason: err.Error()}, nil
	}
	return &RetiredKeySweepFile{Path: path, Removed: removed, BackupPath: backupPath}, nil
}

// retiredStripPlan is the verified set of 1-based source lines whose
// deletion removes exactly the retired key and nothing else. unsafe with a
// reason means the file must be left as found ("needs a hand edit").
type retiredStripPlan struct {
	lines  []int
	unsafe bool
	reason string
}

// retiredEntryShape snapshots one projects/repositories entry from the
// parse tree so the post-strip check can prove nothing but the retired key
// was removed: the same entry count, and per entry the same non-retired
// keys. A bare key count is not enough — deleting a whole flow-style line
// would zero the retired keys while silently dropping a sibling field.
type retiredEntryShape struct {
	count int
	keys  []string
}

// planRetiredKeyStrip locates the retired key's source lines (1-based)
// under the projects and repositories entry sequences. An occurrence is
// safe to strip only when its `key: value` pair owns its source line: a
// scalar value on the same line as the key, no other key on that line, and
// a block-style mapping. Anything else (multi-line value, flow-style
// mapping, or a line shared with another key — including the key sitting on
// the `- ` item line) marks the whole file unsafe so the caller leaves it
// alone for a hand edit.
func planRetiredKeyStrip(data []byte) (retiredStripPlan, error) {
	var plan retiredStripPlan
	occurrences, err := retiredKeyOccurrences(data)
	if err != nil {
		return plan, err
	}
	seen := make(map[int]struct{})
	sourceLines := strings.Split(string(data), "\n")
	for _, occ := range occurrences {
		if occ.item.Style&yaml.FlowStyle != 0 {
			plan.unsafe = true
			plan.reason = "flow-style entry needs a hand edit"
			return plan, nil
		}
		// A key on the entry's opening line sits on the `- ` item
		// marker: deleting that line would drop the sequence marker
		// (and the whole entry when it is the entry's only key) along
		// with the retired key.
		if occ.key.Line == occ.item.Line {
			plan.unsafe = true
			plan.reason = "retired key is on the entry's `- ` line; needs a hand edit"
			return plan, nil
		}
		if occ.key.Line <= 0 || occ.key.Line > len(sourceLines) || occ.value.Kind != yaml.ScalarNode || occ.value.Line != occ.key.Line {
			plan.unsafe = true
			plan.reason = "non-inline retired key value needs a hand edit"
			return plan, nil
		}
		// Decoded values lose source line breaks through YAML folding and
		// chomping. Parse the source line alone to prove it holds the whole
		// scalar; block scalars always require a hand edit, even when empty.
		var inline map[string]yaml.Node
		if occ.value.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 ||
			yaml.Unmarshal([]byte(sourceLines[occ.key.Line-1]), &inline) != nil ||
			inline[retiredKeyName].Kind != yaml.ScalarNode ||
			inline[retiredKeyName].Value != occ.value.Value {
			plan.unsafe = true
			plan.reason = "non-inline retired key value needs a hand edit"
			return plan, nil
		}
		// The `key: value` pair must own its source line. When the
		// retired key shares the line with any other key of the same
		// mapping — the flow case above, or a block entry whose first
		// key sits on the `- ` item line — deleting the line would
		// drop a sibling field along with the key.
		for j := 0; j < len(occ.item.Content); j += 2 {
			other := occ.item.Content[j]
			if other == occ.key {
				continue
			}
			if other.Kind == yaml.ScalarNode && other.Line == occ.key.Line {
				plan.unsafe = true
				plan.reason = "retired key shares its line with another key; needs a hand edit"
				return plan, nil
			}
		}
		if _, ok := seen[occ.key.Line]; !ok {
			seen[occ.key.Line] = struct{}{}
			plan.lines = append(plan.lines, occ.key.Line)
		}
	}
	sort.Ints(plan.lines)
	return plan, nil
}

// retiredOccurrence ties a retired key node to its value node and the
// entry mapping that owns them, so the planner can judge whether the key's
// source line holds anything else.
type retiredOccurrence struct {
	key   *yaml.Node
	value *yaml.Node
	item  *yaml.Node
}

// retiredKeyOccurrences lists every retired key occurrence under the
// projects and repositories entry sequences. Entries reached twice through
// a repeated YAML alias are reported once, keyed by the key node's line.
func retiredKeyOccurrences(data []byte) ([]retiredOccurrence, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil
	}
	var out []retiredOccurrence
	seen := make(map[int]struct{})
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		if key.Value != "projects" && key.Value != "repositories" {
			continue
		}
		entries := root.Content[i+1]
		if entries.Kind == yaml.AliasNode && entries.Alias != nil {
			entries = entries.Alias
		}
		if entries.Kind != yaml.SequenceNode {
			continue
		}
		for _, entry := range entries.Content {
			item := entry
			if item.Kind == yaml.AliasNode && item.Alias != nil {
				item = item.Alias
			}
			if item.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(item.Content); j += 2 {
				k := item.Content[j]
				if k.Kind != yaml.ScalarNode || k.Value != retiredKeyName {
					continue
				}
				// One entry counts once even when reached through a
				// YAML alias that appears several times.
				if _, ok := seen[k.Line]; ok {
					break
				}
				seen[k.Line] = struct{}{}
				out = append(out, retiredOccurrence{key: k, value: item.Content[j+1], item: item})
				break
			}
		}
	}
	return out, nil
}

// retiredEntryShapes snapshots the projects and repositories entry shapes
// of a config document: per sequence, one fingerprint per entry listing
// its non-retired keys. Aliased entries are fingerprinted once per
// appearance (the sweep treats each appearance's line independently).
func retiredEntryShapes(data []byte) ([]retiredEntryShape, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil
	}
	var out []retiredEntryShape
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		if key.Value != "projects" && key.Value != "repositories" {
			continue
		}
		entries := root.Content[i+1]
		if entries.Kind == yaml.AliasNode && entries.Alias != nil {
			entries = entries.Alias
		}
		if entries.Kind != yaml.SequenceNode {
			continue
		}
		for _, entry := range entries.Content {
			item := entry
			if item.Kind == yaml.AliasNode && item.Alias != nil {
				item = item.Alias
			}
			if item.Kind != yaml.MappingNode {
				out = append(out, retiredEntryShape{count: 1})
				continue
			}
			var keys []string
			for j := 0; j+1 < len(item.Content); j += 2 {
				k := item.Content[j]
				if k.Kind != yaml.ScalarNode || k.Value == retiredKeyName {
					continue
				}
				keys = append(keys, k.Value)
			}
			out = append(out, retiredEntryShape{count: 1, keys: keys})
		}
	}
	return out, nil
}

// verifyRetiredKeyStrip proves the stripped document differs from the
// original by exactly the retired key: no retired occurrences remain, the
// projects/repositories entry count is unchanged, and every entry keeps
// the same non-retired keys in the same order. A "zero retired keys
// remain" check alone is not enough: deleting a whole flow-style line
// would pass it while silently dropping an entry.
func verifyRetiredKeyStrip(original, stripped []byte) error {
	occurrences, err := retiredKeyOccurrences(stripped)
	if err != nil {
		return err
	}
	if len(occurrences) != 0 {
		return fmt.Errorf("%d retired key(s) remain", len(occurrences))
	}
	before, err := retiredEntryShapes(original)
	if err != nil {
		return err
	}
	after, err := retiredEntryShapes(stripped)
	if err != nil {
		return err
	}
	if len(after) != len(before) {
		return fmt.Errorf("entry count changed: %d before, %d after", len(before), len(after))
	}
	for i := range before {
		if len(after[i].keys) != len(before[i].keys) {
			return fmt.Errorf("entry %d key count changed: %q before, %q after", i, before[i].keys, after[i].keys)
		}
		for j := range before[i].keys {
			if after[i].keys[j] != before[i].keys[j] {
				return fmt.Errorf("entry %d keys changed: %q before, %q after", i, before[i].keys, after[i].keys)
			}
		}
	}
	return nil
}

// deleteSourceLines removes exactly the given 1-based source lines,
// leaving every other byte untouched.
func deleteSourceLines(data []byte, lines []int) []byte {
	drop := make(map[int]struct{}, len(lines))
	for _, ln := range lines {
		drop[ln] = struct{}{}
	}
	rawLines := strings.SplitAfter(string(data), "\n")
	kept := rawLines[:0]
	for i, ln := range rawLines {
		if _, ok := drop[i+1]; !ok {
			kept = append(kept, ln)
		}
	}
	return []byte(strings.Join(kept, ""))
}

// writeRetiredKeySweep keeps the pre-strip backup and rewrites the file
// atomically with the source mode preserved. All writes go through an
// os.OpenRoot handle on the config directory so sibling names cannot
// escape it, mirroring the daemon's other config backup path.
func writeRetiredKeySweep(path string, mode os.FileMode, original, stripped []byte) (string, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("open config directory %q: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	backupName := base + ".sweep-backup-" + stamp
	if err := root.WriteFile(backupName, original, mode); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	tmpName := base + ".sweep-tmp"
	tmp, err := root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return "", fmt.Errorf("create temp config: %w", err)
	}
	tmpPath := filepath.Join(dir, tmpName)
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(stripped); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("rename temp config: %w", err)
	}
	return filepath.Join(dir, backupName), nil
}
