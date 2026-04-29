// Package logsignatures implements the log-signature catalog and pattern-matching
// logic for the `af logs analyze` command.
//
// It is a Go port of the PATTERN_RULES array in the legacy TypeScript
// packages/core/src/orchestrator/log-analyzer.ts.
//
// # Catalog loading
//
// DefaultSignatures() returns the built-in catalog ported from the TS reference.
// LoadCatalog(path) merges a YAML override file from ~/.config/af/log-signatures.yaml
// (or any explicit path) on top of the defaults.
//
// # Matching
//
// Match(line, sigs) scans a single log line against all signatures in order and
// returns the first matched Signature. Only error lines are expected to be passed
// in (the caller filters).
package logsignatures

import (
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// PatternType categorises a detected failure.
type PatternType string

const (
	// PatternPermission covers sandbox and filesystem permission errors.
	PatternPermission PatternType = "permission"
	// PatternToolIssue covers tool execution and network failures.
	PatternToolIssue PatternType = "tool_issue"
	// PatternToolMisuse covers incorrect agent tool usage.
	PatternToolMisuse PatternType = "tool_misuse"
	// PatternPerformance covers timeouts and rate limits.
	PatternPerformance PatternType = "performance"
	// PatternRepeatedFailure covers the same error occurring 3+ times.
	PatternRepeatedFailure PatternType = "repeated_failure"
	// PatternApprovalRequired covers commands needing approval in autonomous mode.
	PatternApprovalRequired PatternType = "approval_required"
)

// Severity of a matched signature.
type Severity string

const (
	// SeverityLow indicates a low-impact issue.
	SeverityLow Severity = "low"
	// SeverityMedium indicates a medium-impact issue.
	SeverityMedium Severity = "medium"
	// SeverityHigh indicates a high-impact issue.
	SeverityHigh Severity = "high"
	// SeverityCritical indicates a critical issue that blocks autonomous operation.
	SeverityCritical Severity = "critical"
)

// Signature is a single entry in the log-signature catalog.
type Signature struct {
	// ID is a short stable identifier (e.g. "approval-required").
	ID string `yaml:"id"`
	// Pattern is the regular expression to match against a log line.
	Pattern string `yaml:"pattern"`
	// Type categorises the failure.
	Type PatternType `yaml:"type"`
	// Severity of the failure.
	Severity Severity `yaml:"severity"`
	// Title is a short human-readable title.
	Title string `yaml:"title"`

	compiled *regexp.Regexp
}

// Compile compiles the Pattern field and caches the result.
// Must be called before Match.
func (s *Signature) Compile() error {
	re, err := regexp.Compile(`(?i)` + s.Pattern)
	if err != nil {
		return fmt.Errorf("compile signature %q pattern: %w", s.ID, err)
	}
	s.compiled = re
	return nil
}

// Match reports whether line matches this signature.
// Compile must have been called first.
func (s *Signature) Match(line string) bool {
	return s.compiled != nil && s.compiled.MatchString(line)
}

// MatchResult is the outcome of matching a single log line.
type MatchResult struct {
	Signature *Signature
	Line      string
}

// GenerateSignatureHash produces a deterministic deduplication hash for a
// (PatternType, title) pair, mirroring the TS generateSignature function.
//
//	normalized = "<type>:<lowercased title (first 100 chars)>"
//	hash       = sha256(normalized)[0:16]  → "agent-env-<hash>"
func GenerateSignatureHash(pt PatternType, title string) string {
	titlePart := title
	if len(titlePart) > 100 {
		titlePart = titlePart[:100]
	}
	normalized := fmt.Sprintf("%s:%s", pt, titlePart)
	sum := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("agent-env-%x", sum[:8])
}

// DefaultSignatures returns the built-in catalog ported from the TS reference
// implementation (packages/core/src/orchestrator/log-analyzer.ts PATTERN_RULES).
//
// Rules are ordered from most-specific to least-specific, mirroring the TS
// "break after first match" behaviour.
func DefaultSignatures() []Signature {
	sigs := []Signature{
		{
			ID:       "approval-required",
			Pattern:  `This command requires approval|requires approval`,
			Type:     PatternApprovalRequired,
			Severity: SeverityCritical,
			Title:    "Command requires approval in autonomous mode",
		},
		{
			ID:       "write-before-read",
			Pattern:  `File has not been read yet`,
			Type:     PatternToolMisuse,
			Severity: SeverityHigh,
			Title:    "Write attempted before read",
		},
		{
			ID:       "file-does-not-exist",
			Pattern:  `File does not exist`,
			Type:     PatternToolMisuse,
			Severity: SeverityMedium,
			Title:    "File does not exist",
		},
		{
			ID:       "path-does-not-exist",
			Pattern:  `Path does not exist`,
			Type:     PatternToolMisuse,
			Severity: SeverityMedium,
			Title:    "Path does not exist",
		},
		{
			ID:       "invalid-tool-param",
			Pattern:  `Unknown JSON field`,
			Type:     PatternToolMisuse,
			Severity: SeverityHigh,
			Title:    "Invalid tool parameter",
		},
		{
			ID:       "glob-in-write",
			Pattern:  `Glob patterns are not allowed in write`,
			Type:     PatternToolMisuse,
			Severity: SeverityMedium,
			Title:    "Glob pattern used in write operation",
		},
		{
			ID:       "tool-api-error",
			Pattern:  `<tool_use_error>.*</tool_use_error>`,
			Type:     PatternToolMisuse,
			Severity: SeverityHigh,
			Title:    "Tool API error",
		},
		{
			ID:       "file-too-large",
			Pattern:  `exceeds maximum allowed tokens`,
			Type:     PatternToolIssue,
			Severity: SeverityMedium,
			Title:    "File too large to read",
		},
		{
			ID:       "dir-blocked",
			Pattern:  `cd in .* was blocked|only change directories to the allowed`,
			Type:     PatternPermission,
			Severity: SeverityHigh,
			Title:    "Directory change blocked by sandbox",
		},
		{
			ID:       "sandbox-permission",
			Pattern:  `sandbox.*not allowed|operation not permitted`,
			Type:     PatternPermission,
			Severity: SeverityHigh,
			Title:    "Sandbox permission error",
		},
		{
			ID:       "file-permission-denied",
			Pattern:  `permission denied|EACCES|access denied`,
			Type:     PatternPermission,
			Severity: SeverityHigh,
			Title:    "File permission denied",
		},
		{
			ID:       "file-not-found",
			Pattern:  `ENOENT|no such file or directory`,
			Type:     PatternToolIssue,
			Severity: SeverityMedium,
			Title:    "File not found error",
		},
		{
			ID:       "network-timeout",
			Pattern:  `timeout|ETIMEDOUT|connection timed out`,
			Type:     PatternPerformance,
			Severity: SeverityMedium,
			Title:    "Network timeout",
		},
		{
			ID:       "rate-limit",
			Pattern:  `rate limit|429|too many requests`,
			Type:     PatternPerformance,
			Severity: SeverityHigh,
			Title:    "Rate limit exceeded",
		},
		{
			ID:       "network-connection",
			Pattern:  `ECONNREFUSED|ENOTFOUND|connection refused`,
			Type:     PatternToolIssue,
			Severity: SeverityMedium,
			Title:    "Network connection error",
		},
		{
			ID:       "worktree-conflict",
			Pattern:  `is already used by worktree|already checked out`,
			Type:     PatternToolIssue,
			Severity: SeverityHigh,
			Title:    "Git worktree conflict",
		},
		{
			ID:       "tool-execution-failed",
			Pattern:  `tool.*error|tool.*failed|command failed`,
			Type:     PatternToolIssue,
			Severity: SeverityMedium,
			Title:    "Tool execution failed",
		},
	}

	for i := range sigs {
		// ignore compile errors on default catalog (patterns are validated at authoring time)
		_ = sigs[i].Compile()
	}
	return sigs
}

// catalogFile is the YAML structure for an override file.
type catalogFile struct {
	Signatures []Signature `yaml:"signatures"`
}

// LoadCatalog reads a YAML catalog from path, compiles each signature, and
// returns the merged slice (user catalog appended after defaults so user rules
// take precedence on first-match semantics — user rules go first).
func LoadCatalog(path string) ([]Signature, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304 -- user-supplied via CLI flag
	if err != nil {
		return nil, fmt.Errorf("read catalog %q: %w", path, err)
	}

	var cf catalogFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse catalog %q: %w", path, err)
	}

	// Compile user-supplied signatures.
	for i := range cf.Signatures {
		if err := cf.Signatures[i].Compile(); err != nil {
			return nil, err
		}
	}

	// User signatures go first (higher priority on first-match).
	merged := make([]Signature, 0, len(cf.Signatures)+len(DefaultSignatures()))
	merged = append(merged, cf.Signatures...)
	merged = append(merged, DefaultSignatures()...)
	return merged, nil
}

// Match scans line against sigs in order and returns the first match.
// Returns nil if no signature matches.
func Match(line string, sigs []Signature) *MatchResult {
	for i := range sigs {
		if sigs[i].Match(line) {
			return &MatchResult{
				Signature: &sigs[i],
				Line:      line,
			}
		}
	}
	return nil
}
