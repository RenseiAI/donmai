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
| Candidate | `83fb95624b0439ee660c2679b43b38c8b63769cd` | Upstream tag `v1.4.0` (release commit) |
| Control   | `b16d026a9d2e`                             | Owned engine upstream (`x/vt` pseudo-version; `internal/terminalvt/UPSTREAM.json`, `go.mod`) |

Candidate pin, full 40-hex commit (tags move; the hash does not):

```text
83fb95624b0439ee660c2679b43b38c8b63769cd
```

Rerun rule: verify the local checkout before running the matrix. The tag is
annotated: its tag-object hash is not the checkout commit above. Fetching is
an explicit preparation step, never performed by a test:

```sh
git clone --depth 1 --branch v1.4.0 https://github.com/ghostty-org/ghostty.git /tmp/ghostty-vt-spike
GOWORK=off go test -race ./ptyhost -run '^TestCandidateCheckout$' -v -args -vt-candidate-checkout=/tmp/ghostty-vt-spike
GOWORK=off go test -race ./ptyhost -run TestCandidate -v -args -vt-candidate-checkout=/tmp/ghostty-vt-spike
```

The checkout lane fails closed on a Git error or any other HEAD commit.
Without the flag it explicitly skips; the hermetic stand-in matrix still runs.
The constants `vtCandidateUpstream`, `vtCandidateSHAs`, and
`vtCandidateToolchain` record the same pins in code.
Toolchains: Go `go1.27.3` (host), `go1.26.10` (module floor / Linux suite);
Zig `0.17.0` in the pin's
`build.zig.zon`. Release builds ship `CGO_ENABLED=0`; race tests use Cgo.

License at pin: MIT (`LICENSE` file header in the upstream tree). No legal
blocker found; the verdict is architectural, not legal.

## Why HOLD (not ADOPT, not REJECT)

At v1.4.0 the previous missing-screen-model blocker is removed:
`include/ghostty/vt.h` includes terminal, grid, render, formatter and snapshot
APIs. `ghostty_terminal_new`, `ghostty_terminal_vt_write`,
`ghostty_terminal_resize`, and `ghostty_terminal_grid_ref` expose stateful
terminal operation to C consumers. There is no existing binding/cgo shim in
this spike to migrate: the candidate remains a pure-Go seam stand-in.

The [upstream release notes](https://ghostty.org/docs/install/release-notes/1-4-0#libghostty)
describe a complete terminal emulator API and freestanding builds without
libc++. That removes the earlier API obstacle; it does not prove a Go binding,
static linking, or compatibility with our Cgo-free release lane. The API/ABI
is still unstable, with further breaks expected before libghostty 1.0.
Pin source, headers, binding and artifact together on any adoption attempt.
HOLD now means integration and distribution evidence is outstanding, rather
than that upstream lacks a screen API. Do not adopt or track upstream `main`.

### Security implications for adoption

The [v1.4.0 security fixes](https://ghostty.org/docs/install/release-notes/1-4-0#security)
include GHSA-mhff-mv4q-fp63 (Kitty graphics crash, hang, excessive allocation,
and file access outside temporary directories) and GHSA-w9q4-5jvv-mcv4
(scrollback-clear memory corruption with small limits and styled/hyperlinked
cursors). Both process untrusted terminal output and matter to a future VT
embedding. Keep graphics disabled unless deliberately integrated; test zero
and small scrollback limits with styled/hyperlinked cursors in a real binding.
The stand-in neither links the vulnerable code nor validates these upstream
fixes. Moving the evaluation pin is not security-regression evidence for a
linked emulator. Full Kitty graphics support does not expand this corpus.

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
GREEN on restore. Verified by revert → RED → restore → GREEN in-session
and re-verified on the current tree.

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
| macOS arm64 | Candidate matrix passes with `-race`; full host suite fails the firehose throughput floor (1.34 / 1.16 MB/s, required 2.0). |
| Linux | `make test-podman` passes, including `ok  	github.com/RenseiAI/donmai/ptyhost	34.625s`. |
| Local daemon path (`Session.Spawn` construction) | Unchanged — the spike does not alter `Spawn`; the candidate is test-only and never constructed outside tests. |
| Sandbox/in-box distribution, binary size, startup, CPU/memory | **Not measured** — no upstream artifact is linked into this spike. Any future stateful candidate must report: static vs dynamic link, `CGO_ENABLED` impact on the `CGO_ENABLED=0` release builds, cross-builds (darwin arm64/amd64, linux arm64/amd64), sandbox/in-box delivery, binary-size delta, startup cost, steady-state CPU/memory vs control, shipped symbols, and signing/notarization consequences. |
| Cross-builds | **Not measured** — a separate source toolchain and Go link design still need evaluation for the `CGO_ENABLED=0` release lane. |
| C ABI / Cgo integration | Stateful C entry points exist at this pin; no Go binding is linked. Header and source-build observations are recorded below; neither proves snapshot parity with upstream. |
| Upstream source build at pin (`zig build -Demit-lib-vt=true`) | See the v1.4.0 source-build lane below; prior-pin build failures are not results for this pin. |

Unavailable lanes are reported, not passed — per the brief.

## Upstream build lane (v1.4.0, macOS arm64)

Verify the checkout first using the checkout lane above. Documented source
build step for this pin (the former `lib-vt` step is replaced by an option):

```sh
cd /tmp/ghostty-vt-spike
zig build -Demit-lib-vt=true
# Isolate the native library from optional Apple packaging:
zig build -Demit-lib-vt=true -Demit-xcframework=false
cc -fsyntax-only -I include example/c-vt-grid-traverse/src/main.c
```

These are manual evaluation steps, never run by Go tests. Upstream source
is data; no other upstream scripts are part of this spike. The pin's
`src/terminal/c/main.zig` exports terminal and grid APIs as well as the earlier
parser/encoder helpers. Prior v1.3.1 toolchain failures and export inventory
are superseded; they must not be reported as v1.4.0 measurements.

Build observation: the default command attempts an xcframework on this host;
Zig 0.17.0 reports `error: no field named 'fd' in struct
'Io.Threaded.NullFile__struct_14'` while compiling an iOS simulator dependency;
the remaining default-build work was stopped after that diagnostic.
The native-only command passes (exit 0), producing static and shared libraries. The C grid-traversal example
passes `cc -fsyntax-only` (exit 0). Freestanding support is reported
upstream and present in the build configuration; it is not a measured Go
release-distribution lane. Binary-size, startup, CPU/memory, symbol, signing,
cross-build and sandbox-delivery measurements remain outstanding until a real
adapter is linked.

## Costs (measured where measurable)

- Parity-harness code: ~1.5k lines test-only (`vt_candidate.go`,
  `vt_candidate_parity_test.go`), zero production
  lines changed. No new module dependencies (`go.mod` untouched).
- Full `ptyhost` suite with `-race`: Linux container passes; host throughput
  failures are recorded above rather than reported as green. The unchanged
  main source snapshot also fails the same floor (0.82 / 1.01 MB/s).
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
