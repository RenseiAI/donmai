# Per-call span emission

This package implements the additive June 28 span wire contract without putting
prompt, completion, tool-input, or tool-output bodies on the telemetry hot path.
Provider adapters emit measured `llm_call` events where native usage exists;
aggregate-only providers get one event marked `synthetic: true` and
`usageSource: "aggregate"`. Aggregate counts are copied unchanged and are never
divided across guessed calls.

## OpenTelemetry compatibility delta (verified 2026-07-09)

The accepted `agent.LlmCallSpan` JSON fixture remains byte-compatible with the
June 28 ADR. The current OpenTelemetry GenAI semantic conventions have moved:

| Accepted compatibility field | Current OTLP attribute |
|---|---|
| `genAi.system` | `gen_ai.provider.name` (`gen_ai.system` was renamed) |
| implicit chat operation | `gen_ai.operation.name = "chat"` |
| `genAi.usageCacheReadInputTokens` | `gen_ai.usage.cache_read.input_tokens` |
| `genAi.responseFinishReason` | one value in `gen_ai.response.finish_reasons` |

The current inference-span name recommendation is
`{gen_ai.operation.name} {gen_ai.request.model}` and the span kind is `CLIENT`
for remote model calls. Primary sources:

- [OpenTelemetry GenAI span model](https://github.com/open-telemetry/semantic-conventions/blob/main/model/gen-ai/spans.yaml)
- [OpenTelemetry semantic-conventions v1.37 rename](https://github.com/open-telemetry/semantic-conventions/releases/tag/v1.37.0)
- [OpenTelemetry GenAI conventions repository](https://github.com/open-telemetry/semantic-conventions-genai)

Do not rename the accepted JSON keys in this emitter: that would break the Go
golden fixture and existing structural consumers. A compatible OTLP exporter or
ingest normalizer should translate the table above (and may dual-write legacy
attributes during migration). Changing the public wire itself requires a
versioned contract/ADR update and matching downstream fixtures.

## Activation and delivery

The runner enables emission only when its host opts in or the session advertises
the `llm-span-ingest` capability. The command boundary recognizes
`DONMAI_OTEL_TRACES=1`; the library itself reads no environment variables.
Completed spans are delivered as a bounded JSON array to the caller-supplied
base URL plus `/api/daemon/traces`, flushing every 100 ms or 20 spans. Queue
overflow and exhausted delivery retries drop telemetry with a warning rather
than blocking the agent.
