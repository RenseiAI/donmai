# Normalized execution-event source

`runtime/executionevent` is the OSS runtime source for the accepted durable
execution-event bus contract. It is additive and capability-gated by
`runner.CapabilityExecutionEventIngest` (`execution-event-ingest`). A daemon
that does not advertise that capability receives no execution-event requests.

The source writes compact `donmai.execution-event-source/v1alpha1` records to
a local fsync-backed journal before attempting delivery. Records receive a
deterministic `evt_<sha256>` identity and contiguous `structuredSeq`. The
uploader posts bounded batches (at most 100 records and 1 MiB) to
`/api/daemon/sessions/{id}/execution-events` using worker runtime credentials.

Successful responses advance an atomic acknowledgement. Network and 5xx
failures remain pending for bounded retry/resume; only 400, 404, 409, and 413
are durably quarantined. Other authorization/scope failures remain pending.
`Stop` performs a bounded terminal drain, and a later uploader resumes from
the durable acknowledgement.

Provider-native raw payloads, prompts, tool input/output, and error text are
never sent. Only currently active, contract-shaped normalized topics are
emitted; unsupported source variants are omitted rather than guessed.

SMOKE-GAP: the public `donmai-smokes` suite has no live worker runtime-JWT and
disposable platform session fixture for this ingest route. Unit and race tests
cover the exact wire shape, retry/quarantine/resume behavior, and journal
permissions; live end-to-end acceptance remains downstream provisioning work.
