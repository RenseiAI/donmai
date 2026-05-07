// Package afview hosts the canonical composed renderers for the af /
// rensei command surfaces sourced from the daemon's HTTP control API.
//
// Per ADR-2026-05-07-daemon-http-control-api.md §D3, afview is a public
// peer of afclient/afcli/worker. Renderers live in surface-named
// sub-packages (afview/provider, afview/kit, afview/workarea,
// afview/routing). Each sub-package depends on afclient types for the
// wire shape and on tui-components for theme/format primitives. Rensei
// imports afview so it does not fork its own renderers — the
// "soldered-in" OSS principle.
//
// Plain-text fallbacks (PlainList / PlainShow) write deterministic
// ASCII to an io.Writer and are what rensei-smokes pins against. The
// Bubble Tea models (when present) sit alongside but are not loaded
// from the smoke harness — TTY users see them; integration tests do
// not.
package afview
