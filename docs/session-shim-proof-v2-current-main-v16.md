# Session-shim proof-v2 current-main V16 evidence

Date: 2026-08-23

This artifact records discriminating controls added on top of Donmai commit
`343cf0c11d457b15814a0add2c57b0834c1e8a83` (pull request #395). That commit is
the authority for the proof-v2 recovery state machine, retained candidate
state, publication and activation receipts, capacity projection, spawner
pause/reopen behavior, and heartbeat-acknowledged recovery. The controls below
exercise only the remaining consumer edges on that authority.

Each RED was produced by a temporary, single-purpose source mutation. The
mutation was restored before the corresponding GREEN run. None of the RED
mutations is part of the delivered diff.

## Released attach caller compatibility

Removing the omitted-field normalization produced:

```text
=== RUN   TestV2ResumeDispositionReleasedV0684CallerNormalizesToRetainedProofV1
    v2_test.go:597: released active caller: attachclient: v2 resume proof schema version is required
--- FAIL: TestV2ResumeDispositionReleasedV0684CallerNormalizesToRetainedProofV1 (0.00s)
FAIL
```

Restoring it produced:

```text
=== RUN   TestV2ResumeDispositionReleasedV0684CallerNormalizesToRetainedProofV1
--- PASS: TestV2ResumeDispositionReleasedV0684CallerNormalizesToRetainedProofV1 (0.00s)
=== RUN   TestV2ResumeDispositionExplicitPartialAndInvalidNewShapesRefuse
=== RUN   TestV2ResumeDispositionExplicitPartialAndInvalidNewShapesRefuse/schema_only
=== RUN   TestV2ResumeDispositionExplicitPartialAndInvalidNewShapesRefuse/authority_only
=== RUN   TestV2ResumeDispositionExplicitPartialAndInvalidNewShapesRefuse/unknown_schema
=== RUN   TestV2ResumeDispositionExplicitPartialAndInvalidNewShapesRefuse/unknown_authority
--- PASS: TestV2ResumeDispositionExplicitPartialAndInvalidNewShapesRefuse (0.00s)
PASS
```

The compatibility default is applied only when both fields are omitted. It is
exactly proof schema `1` with `same_handoff`; explicit partial or unknown shapes
are not rewritten.

## Live five-fact readiness

Removing each live check at its own edge produced these independent REDs:

```text
claim-and-poll:
    session_shim_activation_seams_test.go:381: live claim check stayed open

direct-admission:
    session_shim_activation_seams_test.go:404: withdrawal state = withdrawn:false state:running accepting:true registration:idle

activation:
    session_shim_activation_seams_test.go:409: live activation check stayed open
```

The restored run re-resolved every fact at every edge:

```text
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/original_credential_retention/claim-and-poll
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/original_credential_retention/direct-admission
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/original_credential_retention/activation
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/remaining_validity_gate/claim-and-poll
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/remaining_validity_gate/direct-admission
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/remaining_validity_gate/activation
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/adopted_recovery/claim-and-poll
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/adopted_recovery/direct-admission
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/adopted_recovery/activation
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/durable_acknowledgement/claim-and-poll
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/durable_acknowledgement/direct-admission
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/durable_acknowledgement/activation
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/v1_writer_closure/claim-and-poll
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/v1_writer_closure/direct-admission
=== RUN   TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail/v1_writer_closure/activation
--- PASS: TestProofV2LiveReadinessFactsWithdrawEveryNewWorkRail (0.00s)
PASS
```

Each refusal also asserted the existing #395 withdrawal projection:
`withdrawn=true`, daemon state `recovering`, spawner not accepting, registration
`draining`, and zero HTTP poll calls after the claim fence closes.

## Retained recovery output ordering

The lifecycle uses two real selected-v3 controllers. Controller one durably
acknowledges N and emits retained Snapshot H=N+1 without acknowledging H.
Controller two begins Resize, PTY Output, and Marker attempts while the Hello
output barrier is closed. Before publication and `carrier_active(H)`, the test
requires zero completed attempts, zero external observer or durable callbacks,
no ACK of H, and no second Snapshot. The daemon then persists Heartbeat(H), and
the ordinary durable path receives the exact H+1 sequence once and in order.

Disabling the durable-advance release produced a bounded deadlock RED:

```text
=== RUN   TestConsumedRecoveryHeartbeatReleasesBlockedV3ProgressAfterCarrierActive
    session_shim_spawn_test.go:697: blocked attempts completed = map[]
--- FAIL: TestConsumedRecoveryHeartbeatReleasesBlockedV3ProgressAfterCarrierActive (5.25s)
FAIL
```

Releasing the barrier at Hello instead produced a pre-active leak RED:

```text
=== RUN   TestConsumedRecoveryHeartbeatReleasesBlockedV3ProgressAfterCarrierActive
    session_shim_spawn_test.go:689: consumed recovery activation: session shim: activate published carriers: pre-active effects = completed:3 last:5 external:6 forwarded:5 ack:5
--- FAIL: TestConsumedRecoveryHeartbeatReleasesBlockedV3ProgressAfterCarrierActive (0.23s)
FAIL
```

Restoring the durable Heartbeat edge produced:

```text
=== RUN   TestConsumedRecoveryHeartbeatReleasesBlockedV3ProgressAfterCarrierActive
--- PASS: TestConsumedRecoveryHeartbeatReleasesBlockedV3ProgressAfterCarrierActive (0.30s)
PASS
```

## Durable ACK and bounded failure controls

Moving barrier release after the synchronous reply write reproduced the local
crash window:

```text
=== RUN   TestSelectedV3CommittedAckReleasesBarrierWhenReplyWriteIsLost
    durable_ack_test.go:462: durably committed ACK left local barrier closed after reply loss
--- FAIL: TestSelectedV3CommittedAckReleasesBarrierWhenReplyWriteIsLost (5.19s)
FAIL
```

Making equal/no-op ACKs release the barrier produced:

```text
=== RUN   TestSelectedV3EqualHeartbeatDoesNotReleaseProofBarrier
    durable_ack_test.go:516: equal/no-op Heartbeat released proof barrier: <nil>
--- FAIL: TestSelectedV3EqualHeartbeatDoesNotReleaseProofBarrier (0.18s)
FAIL
```

Changing the selected-v3 priority queue bound from 128 to 129 produced:

```text
=== RUN   TestSelectedV3PriorityEventQueueOverflowFailsClosed
    controller_test.go:149: selected-v3 priority queue limit = 129, want 128
--- FAIL: TestSelectedV3PriorityEventQueueOverflowFailsClosed (0.00s)
FAIL
```

Removing the timeout close produced:

```text
=== RUN   TestSelectedV3HeartbeatTimeoutClosesController
    controller_test.go:197: heartbeat timeout did not enter closed state
--- FAIL: TestSelectedV3HeartbeatTimeoutClosesController (6.00s)
FAIL
```

The restored control group produced:

```text
=== RUN   TestSelectedV3PriorityEventQueueOverflowFailsClosed
--- PASS: TestSelectedV3PriorityEventQueueOverflowFailsClosed (0.00s)
=== RUN   TestSelectedV3HeartbeatTimeoutClosesController
--- PASS: TestSelectedV3HeartbeatTimeoutClosesController (5.00s)
=== RUN   TestSelectedV3CommittedAckReleasesBarrierWhenReplyWriteIsLost
--- PASS: TestSelectedV3CommittedAckReleasesBarrierWhenReplyWriteIsLost (0.20s)
=== RUN   TestSelectedV3EqualHeartbeatDoesNotReleaseProofBarrier
--- PASS: TestSelectedV3EqualHeartbeatDoesNotReleaseProofBarrier (0.23s)
=== RUN   TestSelectedV3FreshAheadHeartbeatFailsCandidateClosed
--- PASS: TestSelectedV3FreshAheadHeartbeatFailsCandidateClosed (0.17s)
=== RUN   TestSelectedV3AckPersistenceFailureDoesNotReleaseToCandidate
--- PASS: TestSelectedV3AckPersistenceFailureDoesNotReleaseToCandidate (0.17s)
PASS
```

A strictly advancing durable put is therefore the only successful release
edge. It releases the shim-local bounded output barrier before writing the
synchronous receipt. If that receipt is lost, local output can proceed into the
bounded ring, while the daemon cannot advance its cursor without the receipt.
Equal, ahead, failed-persistence, overflow, and timeout paths do not create a
candidate-visible success edge.
