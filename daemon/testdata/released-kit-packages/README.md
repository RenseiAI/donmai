# Released kit package conformance fixtures

These seven directories are byte-for-byte imports of `kits/*` from the public
`RenseiAI/donmai-kits` release commit
`0e4621a9336647a2df94436c8af7c12e85910c20`. They intentionally include the
real Sigstore bundles so the consumer tests exercise the embedded public-good
trust root and pinned official workflow identity offline.

Do not regenerate these fixtures from a mutable branch. A future fixture update
must name a new immutable source commit and preserve the previous vectors when
they remain supported.
