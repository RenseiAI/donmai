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
// every daemon*.yaml sibling in the canonical config directory, plus the
// explicit path when set. Missing files are filtered out by the sweep
// itself; the result is sorted and deduplicated.
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
	if explicit != "" {
		add(explicit)
	}
	canonical := DefaultConfigPath()
	add(canonical)
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(canonical), "daemon*.yaml")); err == nil {
		sort.Strings(matches)
		for _, m := range matches {
			add(m)
		}
	}
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
// file is absent (a globbed or default candidate that simply does not
// exist), (result, nil) for examined files, and (nil, err) when an
// explicitly requested file is missing — the caller cannot tell those
// apart, so absence of a path that was passed explicitly is reported as an
// error while every other absence is a silent skip. In practice the sweep
// passes its candidates uniformly, so: absent files are silent skips, and
// callers that need a missing explicit path to fail check existence first.
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
	lines, unsafe, err := retiredKeyLines(original)
	if err != nil {
		return &RetiredKeySweepFile{Path: path, Skipped: true, SkippedReason: err.Error()}, nil
	}
	if len(lines) == 0 && !unsafe {
		return &RetiredKeySweepFile{Path: path, Untouched: true}, nil
	}
	if unsafe {
		return &RetiredKeySweepFile{
			Path:          path,
			Skipped:       true,
			SkippedReason: "non-inline retired key value needs a hand edit",
		}, nil
	}
	stripped := deleteSourceLines(original, lines)
	recount, _, err := retiredKeyLines(stripped)
	if err != nil || len(recount) != 0 {
		return &RetiredKeySweepFile{
			Path:          path,
			Skipped:       true,
			SkippedReason: "post-strip verification failed; file left as found",
		}, nil
	}
	removed := len(lines)
	backupPath, err := writeRetiredKeySweep(path, info.Mode().Perm(), original, stripped)
	if err != nil {
		return &RetiredKeySweepFile{Path: path, Skipped: true, SkippedReason: err.Error()}, nil
	}
	return &RetiredKeySweepFile{Path: path, Removed: removed, BackupPath: backupPath}, nil
}

// retiredKeyLines locates the retired key's source lines (1-based) under
// the projects and repositories entry sequences. It returns unsafe=true
// when an occurrence cannot be removed by deleting its own line (a
// multi-line value), in which case the caller must leave the file alone.
func retiredKeyLines(data []byte) (lines []int, unsafe bool, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("parse config: %w", err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, false, nil
	}
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
				v := item.Content[j+1]
				if k.Line <= 0 || v.Kind != yaml.ScalarNode || v.Line != k.Line {
					return nil, true, nil
				}
				// A block scalar (or any multi-line value) reports the
				// key's line but spans more: deleting one line would
				// orphan its continuation. Only a single-line value is
				// safe to strip by line.
				if strings.Contains(v.Value, "\n") {
					return nil, true, nil
				}
				lines = append(lines, k.Line)
			}
		}
	}
	sort.Ints(lines)
	return lines, false, nil
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
