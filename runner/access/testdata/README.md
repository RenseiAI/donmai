# Vendored golden vectors

`narrow-only-vectors.json` is a **vendored copy** of the canonical golden
vectors. It is NOT authored here.

- **Canonical source (single source of truth):**
  `donmai-architecture/golden/narrow-only-vectors.json`
  (ADR-2026-06-06 D5; P3-SPEC §3.1).
- **Sync direction:** corpus → this vendored copy (one-way). Never edit this
  copy directly — edit the canonical file and re-vendor.
- **Anti-drift gate:** `narrow-only-vectors.json.sha256` pins the SHA-256 of the
  vendored bytes. `TestGoldenVectors_VendoredChecksum` in `../golden_test.go`
  recomputes the digest of `narrow-only-vectors.json` and asserts it equals the
  pinned value, so a stale or hand-edited vendor reds CI. This reuses the
  proven `provider-matrix.json` / `capability-matrix.json` vendor-sync-with-parity
  discipline (matrix/parity_test.go), not a new mechanism.

## Re-vendoring after a corpus change

```sh
cp ../../../../donmai-architecture/golden/narrow-only-vectors.json \
   ./narrow-only-vectors.json
shasum -a 256 narrow-only-vectors.json | awk '{print $1"  narrow-only-vectors.json"}' \
   > narrow-only-vectors.json.sha256
```

The TS reader (platform `access-policy.test.ts`) vendors the same file under its
own pinned path and runs the same parity assertions against
`expectedEffectiveSet` / `expectedDropped`. One writer of policy, two gated
readers, byte-agreeing on these vectors.
