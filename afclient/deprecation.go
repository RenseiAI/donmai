// Package afclient deprecation.go — the declared-removal-version contract for
// surface aliases that span the daemon control API, the CLI, and daemon.yaml.
//
// A surface rename that ships without a working alias breaks every caller
// pinned to the previous release; an alias that ships without a *declared*
// removal version never gets removed. Both halves live here so the daemon
// (server side), afclient (client side) and afcli (CLI side) quote one string
// rather than three that can drift.
package afclient

import "fmt"

// WorkareaAliasRemovalVersion is the release in which the deprecated
// workarea-surface aliases are deleted:
//
//   - GET  /api/daemon/pool/stats      → GET  /api/daemon/workarea/stats
//   - POST /api/daemon/pool/evict      → POST /api/daemon/workarea/evict
//   - GET  /api/daemon/stats?pool=true → GET  /api/daemon/stats?workarea=true
//   - <bin> host stats --pool          → <bin> host stats --workarea
//   - capacity.poolMaxDiskGb           → capacity.workareaMaxDiskGb
//
// Every alias this module registers MUST name a concrete version here (or in
// its own constant) rather than "the next release": an unfalsifiable promise is
// how the previous alias generation survived dozens of tags past its stated
// window. These aliases are created in v0.58.0 and removed one minor later,
// matching the cadence `hostAliasRemovalVersion` set for the `daemon`→`host`
// alias.
//
// Removal precondition: downstream embedders vendor this module at an exact
// version and ship their own CLI leaves against these paths, so they move on
// their own release clock. The removal release must land only after the
// downstream release that adopts the new surface has shipped — otherwise the
// alias is deleted while the callers it exists for are still in the field.
const WorkareaAliasRemovalVersion = "v0.59.0"

const (
	// WorkareaMaxDiskGbKey is the dotted daemon.yaml key naming the local disk
	// envelope of the warm workarea cache, in GiB. 0 means no limit.
	WorkareaMaxDiskGbKey = "capacity.workareaMaxDiskGb"
	// LegacyWorkareaMaxDiskGbKey is the deprecated spelling of
	// WorkareaMaxDiskGbKey, accepted everywhere the new key is until
	// WorkareaAliasRemovalVersion. It read as an org-capacity concept on a
	// machine-local setting, which is the collision the rename resolves.
	LegacyWorkareaMaxDiskGbKey = "capacity.poolMaxDiskGb"

	// workareaMaxDiskGbYAMLKey and legacyWorkareaMaxDiskGbYAMLKey are the same
	// two names as they appear inside the `capacity` mapping of daemon.yaml,
	// without the dotted prefix.
	workareaMaxDiskGbYAMLKey       = "workareaMaxDiskGb"
	legacyWorkareaMaxDiskGbYAMLKey = "poolMaxDiskGb"
)

// DeprecatedSurfaceAdvice renders the remediation half of the notice: what to
// use instead, and the release in which the alias stops existing. It is the
// form to hand to a caller that supplies its own "X is deprecated" preamble —
// pflag's MarkDeprecated, for instance.
func DeprecatedSurfaceAdvice(alias, replacement string) string {
	return fmt.Sprintf(
		"use %s instead; the %s alias is removed in %s.",
		replacement, alias, WorkareaAliasRemovalVersion,
	)
}

// DeprecatedSurfaceNotice renders the whole sentence every workarea-surface
// alias emits, whichever transport carries it (stderr for the CLI, a Warning
// header and a log line for HTTP). alias and replacement are the two surface
// spellings, quoted verbatim so an operator can copy the replacement.
func DeprecatedSurfaceNotice(alias, replacement string) string {
	return alias + " is deprecated — " + DeprecatedSurfaceAdvice(alias, replacement)
}
