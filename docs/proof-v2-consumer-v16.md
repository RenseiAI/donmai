# Proof-v2 consumer V16 evidence

Date: 2026-08-23

Canonical architecture:
`ebf2b448695275c56fcec15b1c78c7b982a568af`.

Each control below temporarily removed one production dependency, ran the
focused test with `GOWORK=off` and `-count=1`, observed RED for the intended
reason, restored the exact dependency, and observed GREEN. No weakened control
was retained.

## Selected-v3 retained output barrier

Removed the post-persistence release of the existing Hello output barrier.

```text
$ go test ./daemon -run '^TestAdoptedRecoveryStagesRealV3FramesUntilCarrierActive$' -count=1 -v
=== RUN   TestAdoptedRecoveryStagesRealV3FramesUntilCarrierActive
    session_shim_activation_seams_test.go:1034: pre-active emit attempts completed = map[]
--- FAIL: TestAdoptedRecoveryStagesRealV3FramesUntilCarrierActive
FAIL
```

Restored the release, then independently bypassed it during handshake before
`carrier_active` and Heartbeat persistence.

```text
=== RUN   TestAdoptedRecoveryStagesRealV3FramesUntilCarrierActive
    session_shim_activation_seams_test.go:1026: replacement adoption: session shim: activate published carriers: pre-active effects = completed:3 last:5 external:6 forwarded:5 ack:5
--- FAIL: TestAdoptedRecoveryStagesRealV3FramesUntilCarrierActive
FAIL
```

With the release restored only after successful selected-v3 Heartbeat
persistence, the real two-controller fixture keeps three bounded Resize, PTY
Output, and Marker attempts behind H, exposes no observer/durable/cursor/ACK
effect, and then publishes exact H+1... bytes once in order.

```text
--- PASS: TestAdoptedRecoveryStagesRealV3FramesUntilCarrierActive
--- PASS: TestSelectedV3FreshCandidateHeartbeatBeforeSnapshotFailsClosed
--- PASS: TestSelectedV3PriorityEventQueueOverflowFailsClosed
PASS
```

The fresh-candidate control sends Heartbeat(K+1) before the mandatory Snapshot.
The shim refuses it as ahead of the real host sequence, fails that candidate
closed, and delivers no candidate event. The unchanged selected-v3 controller
priority queue also refuses its 129th retained event rather than growing without
bound.

## Retained adopted candidate through activation

Removed the retained-activation lookup while leaving batch publication and the
activation callback intact.

```text
$ go test ./daemon -run '^TestAdoptedCandidateRecoveryPublishesAndActivatesWithoutSecondSnapshot$' -count=1 -v
=== RUN   TestAdoptedCandidateRecoveryPublishesAndActivatesWithoutSecondSnapshot
    session_shim_activation_seams_test.go:737: replacement adoption: session shim: activate published carriers: session shim: carrier_active ack did not exactly resolve the staged Snapshot for test-org/adopted-recovery-activation
--- FAIL: TestAdoptedCandidateRecoveryPublishesAndActivatesWithoutSecondSnapshot
FAIL
```

Restored the exact retained activation receipt path.

```text
$ go test ./daemon -run '^TestAdoptedCandidateRecoveryPublishesAndActivatesWithoutSecondSnapshot$' -count=1 -v
=== RUN   TestAdoptedCandidateRecoveryPublishesAndActivatesWithoutSecondSnapshot
--- PASS: TestAdoptedCandidateRecoveryPublishesAndActivatesWithoutSecondSnapshot
PASS
ok  github.com/RenseiAI/donmai/daemon
```

The GREEN path uses two real controller generations around one live shim,
publishes the retained activation in the adoption batch, exact-matches
`carrier_active`, advances the shim acknowledgement to the retained high-water,
and observes zero new Snapshot HostFrames.

## Dynamic readiness at polling, preparation, and activation

Removed the readiness recheck from claim suspension.

```text
session_shim_activation_seams_test.go:385: claim gate remained open after proof-v2 readiness withdrawal
session_shim_activation_seams_test.go:404: poll withdrawal = calls:1 suspended:false
--- FAIL: TestProofV2ReadinessWithdrawalSuspendsPollingAndCandidateOperations
```

Restored claim suspension, then independently removed the preparation recheck.

```text
session_shim_activation_seams_test.go:412: withdrawn preparation = calls:1 err:<nil>
--- FAIL: TestProofV2ReadinessWithdrawalSuspendsPollingAndCandidateOperations
```

Restored preparation, then independently removed the activation recheck.

```text
session_shim_activation_seams_test.go:415: withdrawn readiness allowed carrier activation
--- FAIL: TestProofV2ReadinessWithdrawalSuspendsPollingAndCandidateOperations
```

With all three dependencies restored, every independently false readiness fact
is GREEN as a claim suspension and both candidate operations refuse while any
fact is false.

```text
--- PASS: TestProofV2ReadinessWithdrawalSuspendsPollingAndCandidateOperations
    --- PASS: .../durable_acknowledgement
    --- PASS: .../v1_writer_closure
    --- PASS: .../original_credential_retention
    --- PASS: .../remaining-validity_gate
    --- PASS: .../adopted_recovery
PASS
ok  github.com/RenseiAI/donmai/daemon
```

## Released caller normalization

Removed normalization of the unchanged v0.68.3/v0.68.4
`V2ResumeDisposition` literal.

```text
=== RUN   TestV2ResumeDispositionReleasedV0684CallerNormalizesToRetainedProofV1
    v2_test.go:599: released caller config: attachclient: v2 resume proof schema version is required
--- FAIL: TestV2ResumeDispositionReleasedV0684CallerNormalizesToRetainedProofV1
FAIL
```

Restored the all-legacy-fields-absent normalization to proof-v1 plus exact
same-handoff authority. Partial or invalid new fields remain refusals.

```text
--- PASS: TestV2ResumeDispositionReleasedV0684CallerNormalizesToRetainedProofV1
--- PASS: TestV2ResumeDispositionExplicitPartialAndInvalidNewShapesRefuse
PASS
ok  github.com/RenseiAI/donmai/attachclient
```

## PTY epoch binding

Removed the comparison between the retained resume disposition and the
authenticated adoption preparation process epoch.

```text
=== RUN   TestAdoptedCandidateRecoveryRejectsRemintAndSecondCursorShapes/changed_pty_epoch
    session_shim_activation_seams_test.go:565: invalid adopted-candidate recovery was accepted
--- FAIL: TestAdoptedCandidateRecoveryRejectsRemintAndSecondCursorShapes
```

Restored exact equality.

```text
--- PASS: TestAdoptedCandidateRecoveryRejectsRemintAndSecondCursorShapes
    --- PASS: .../changed_pty_epoch
PASS
ok  github.com/RenseiAI/donmai/daemon
```

## Earlier proof-v2 controls retained by this change

The same repair tree retains the earlier discriminating controls for proof-v1-
only and dual-proof capability tuples, proof-schema lexical drift, unknown and
duplicate predecessor members, proof-v1 fresh admission, missing original
bearer, second proof/receipt correlation, second cursor, new Snapshot authority,
and selected-v2 admission without the full HostFrame v3 rail. Their focused
tests remain part of `make test` and the released-overlap scripts.
