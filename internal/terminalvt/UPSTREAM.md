# Owned terminal continuation engine

This package derives from `github.com/charmbracelet/x/vt` at
`v0.0.0-20260712004152-b16d026a9d2e`. Its MIT license is preserved in LICENSE;
UPSTREAM.json records original file hashes. It remains module-private.

Local changes provide a supported bounded checkpoint representation, stable
screen/save/wrap readers, and a receiver-owned response writer. The private
parser comes from `internal/terminalparser`; ANSI and ultraviolet public value
types keep their original identity. The response writer, callbacks and logger
are receiver configuration, never imported checkpoint authority. A read-only
mirror binds `io.Discard` before restoration and renders escape-safe cells only.

Checkpoint version1 interns exact complete cells and stores bounded references
for both screens and scrollback. Limits:16MiB encoded state,2Mi total cell
references,4096 columns/rows,4MiB parser data. Oversized state returns an error;
it never drops history, defaults private fields, or paints an approximation.
The ordinary10000-line history at80 and160 columns is exercised by the tests.

Updating this package requires reviewing every Emulator, Screen and Parser
field against the checkpoint inventory, maintaining original public value-type
compatibility, replaying uninterrupted-versus-restored suffix fixtures, running
literal state-removal controls, and running the ordinary repository gates.
Importing a new upstream revision requires refreshed source hashes and license
review. Upstream checkpoint patches are maintained separately as contribution
artifacts; a local module replace is never a release dependency mechanism.

`github.com/charmbracelet/x/exp/ordered` remains at the already-pinned version;
its direct use here promotes the existing indirect dependency without adding
another library. The external x/vt dependency remains for independent viewer
conformance fixtures, preserving their separate implementation reference.
