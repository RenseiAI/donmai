# Interactive-session snapshot-authority spike: verdict and matrix

Status: **HOLD** (do not adopt; do not track upstream `main`).

This is the ADR-grade result of the spike: a measured go/no-go on replacing
the swappable snapshot authority behind the `ptyhost.terminal` seam
(`ptyhost/vt.go`) with a pinned upstream revision. The production default is
unchanged: `Session` still builds the owned engine, and this spike adds no
production dependency.

## Pins (exact)

| Role      | Revision                                   | Note                                   |
|-----------|--------------------------------------------|----------------------------------------|
| Candidate | `332b2aefc6e9c293c8adb6a140742649b7e42d3a` … see below | Upstream tag `v1.3.1` (release commit) |
| Control   | `b16d026a9d2e`                             | Owned engine upstream (`x/vt` pseudo-version; `internal/terminalvt/UPSTREAM.json`, `go.mod`) |

Candidate pin, full 40-hex commit (tags move; the hash does not):

```text
332b2aefc6e72d363aa93ab6ecfc86eeeeb5ed28
```

Rerun rule: re-verify with `git rev-parse HEAD` in a checkout of that hash
before trusting any number below. The constants `vtCandidateUpstream`,
`vtCandidateSHAs`, and `vtCandidateToolchain` in `ptyhost/vt_candidate.go`
record the same pins in code.

Toolchain floor: Go `go1.27.2` (this repo); any source-level build of the
upstream tree additionally requires the minimum Zig version in that tree's
`build.zig.zon`, which is absent from PATH here and not in this repo's
dependency stack. Release builds ship `CGO_ENABLED=0` (`.goreleaser.yaml`);
tests run with `CGO_ENABLED=1`.

License at pin: MIT (`LICENSE` file header in the upstream tree). No legal
blocker found; the verdict is architectural, not legal.

## Why HOLD (not ADOPT, not REJECT)

The pinned C ABI (`include/ghostty/vt.h` at the pin) is a **stateless**
surface: OSC/SGR/key-encode/paste helpers plus a WASM allocator shim. It
exposes **no hosted screen model** — no terminal/screen/cursor/grid entry
point — so there is nothing in it a snapshot authority can point at. A Go
adapter over that ABI could only re-host the state machine this repo already
owns (grid, scrollback, modes, wrap, cursor books): a rewrite of the grid,
not an adoption.

The stateful Terminal (grid, scrollback, modes, wrap, Kitty keyboard stack,
query responders) exists in the same pinned tree only as a **Zig module**
(`src/lib_vt.zig` re-exporting `src/terminal/*`). Reaching it from Go means
driving a Zig build artifact: a second toolchain this repo does not ship,
plus a C ABI or static-link seam that does not exist at the pin. That is a
larger integration than a snapshot-authority swap — new toolchain, new link
model, new signing/symbol surface — and it is not justified while the C ABI
cannot host the authority at all.

REJECT would overclaim: a later upstream revision may grow a stateful C
screen model, at which point the parity harness in this spike plugs a real
candidate in unchanged. HOLD says: keep the seam, keep the harness, revisit
when the upstream C surface can actually host a screen.

## Parity harness (same corpus, same contract, same bytes)

The candidate (`ptyhost/vt_candidate.go`) is a narrow pure-Go stand-in
behind the existing `terminal` seam — independent grid/scrollback/mode/
wrap/cursor/Kitty books, driven by the **shared** DEC ANSI tokenizer (the
same parser the control uses), serialized through the **unchanged**
`buildScreen` path to the same `snapFormat-0x01` bytes. `TERM=xterm-256color`
is preserved throughout: the corpus feeds raw PTY bytes, never a terminfo
lookup.

Parity axes, one lane each (`ptyhost/vt_candidate_parity_test.go`):

- primary screen (16 deterministic feeds: text, SGR, cursor motion,
  erase, regions, tabs, insert/delete, truecolor, origin, saved cursor,
  cursor style) — **0 diffs**
- alternate screens (`?1047`/`?47`/`?1049` enter/exit + restore) — **0 diffs**
- scrollback (40-line churn over a 6-row grid) — **0 diffs**
- wide graphemes (CJK, mixed, combining, wrap-edge, emoji) — **0 diffs**
- pending wrap (fill sets the bit, next glyph wraps) — **agree**
- saved cursor (DECSC/DECRC round-trip) — **agree**
- modes (bracketed paste, app cursor keys, focus events, mouse matrix) — **0 diffs**
- resize (20×6 → 30×8 content preservation) — **0 diffs**
- query responses (CPR/DA1/DA2/DSR/DECRQM/DECRQSS/OSC-color/DECXCPR/Kitty
  query/?2048 suppression; replies on the response writer, nothing painted) — **pass**
- snapshot serialization (all of the above round-trip `Encode`/`DecodeScreen`) — **pass**
- reset (RIS clears grids/modes/Kitty/scrollback) — **0 diffs**
- memory bounds (600-line churn capped at 200 tail lines; grids + tail reported) — **bounded**

Recorded-fixture lane (`tmux_vim`, `tmux_split`, `vim` byte corpus):
characterizes drift rather than gating at zero — recorded bytes carry
emulator-divergent timing/title/device replies. Counts are logged per
fixture; the gate is the deterministic corpus above.

Regression pin: `TestCandidateRegressionPin` goes RED on a deliberate
adapter break (dropping the SGR bold bit: `FAIL` with 3 cell diffs) and
GREEN on restore. Verified by revert → RED → restore → GREEN in-session.

Kitty keyboard fidelity lane (`TestCandidateKittyKeyboardFidelity`):
push/set-or/set-not/pop/query round-trip measured on the candidate stack.
Graphics are never enabled and the wire sanitizer is unchanged — state
fidelity only. The control holds no Kitty state (no query reply), which the
lane asserts rather than hides.

Query-response invariant (viewers display-only): every query in the live
contract is answered synchronously on the response writer (the master) and
leaves no reply bytes in either grid, on both engines. The sanitizer still
strips the Kitty keyboard query viewer-side; that behavior is unchanged.

## Build and distribution matrix

| Lane | Result |
|------|--------|
| macOS (arm64, this host): `go vet`, `go test -race` on `ptyhost` | PASS (quoted below) |
| Linux | **Not run from here** — reported, not passed. Rerun under `make test-podman` (Linux container) before any follow-up. |
| Local daemon path (`Session.Spawn` construction) | Unchanged — the spike does not alter `Spawn`; the candidate is test-only and never constructed outside tests. |
| Sandbox/in-box distribution, binary size, startup, CPU/memory | **Not measured** — no linkable candidate artifact exists at the pin (no screen model in the C ABI). Any future stateful candidate must report: static vs dynamic link, `CGO_ENABLED` impact on the `CGO_ENABLED=0` release builds, cross-builds (darwin arm64/amd64, linux arm64/amd64), sandbox/in-box delivery, binary-size delta, startup cost, steady-state CPU/memory vs control, shipped symbols, and signing/notarization consequences. |
| Cross-builds | **Not attempted** — same reason. The Zig-toolchain requirement alone blocks the existing `CGO_ENABLED=0` release lane. |
| C ABI / Cgo integration | Characterized from headers only: the pinned C ABI cannot host a snapshot authority (no screen model). A Cgo or static-link lane is premature until a stateful C entry point exists. |

Unavailable lanes are reported, not passed — per the brief.

## Costs (measured where measurable)

- Parity-harness code: ~1.5k lines test-only (`vt_candidate.go`,
  `vt_candidate_parity_test.go`, `vt_spike_notes.go`), zero production
  lines changed. No new module dependencies (`go.mod` untouched).
- Full `ptyhost` suite with `-race` on this host: green (quoted below).
- Upstream integration cost if revisited: Zig toolchain vendoring +
  pinning, C ABI or static-link design, `CGO_ENABLED=0` release-lane
  impact, cross-build matrix, signing/symbol review, and a migration that
  keeps the `terminal` seam swappable.

## Migration / rollback outline (for the revisit, not this spike)

- Migration: pin the new revision by full commit hash; vendor or link the
  stateful screen model behind `ptyhost.terminal`; extend the parity
  harness (same corpus, same contract) until the deterministic lanes read
  0 diffs; run the recorded-fixture lane and the Linux container lane;
  report the distribution matrix above; flip the `Session` constructor
  behind a flag with the control one revert away.
- Rollback: the constructor flip is the only production line; revert it
  and the control (untouched, still the default) resumes. The harness
  stays as the characterization suite either way.

## Non-goals honored

No production dependency or default changes (`go.mod`, `Session.Spawn`,
`attachwire`, sanitizer untouched). No Kitty graphics. No wire-sanitizer
change. No `TERM` identity change.
