# Formal A2A v1 client

Donmai ships a strict A2A v1 JSON-RPC client and a public CLI surface for the
four non-streaming core task operations:

```text
donmai a2a send   --card <agent-card-url> --message "..."
donmai a2a get    --card <agent-card-url> --id <task-id>
donmai a2a list   --card <agent-card-url>
donmai a2a cancel --card <agent-card-url> --id <task-id>
```

`--card` is explicit by design. Human handles and peer directories are registry
concerns outside the A2A protocol. An embedding CLI can enable the command and
provide `afcli.Config.A2ACardURL` to resolve its own `--peer` reference without
moving that registry vocabulary into Donmai.

The client selects the first card interface whose binding is `JSONRPC` and
whose protocol major/minor is `1.0`. A numeric patch suffix is accepted, but
every request sends `A2A-Version: 1.0`; there is no silent v0.x fallback. The
selected interface's opaque `tenant` is stamped on every operation.

## Authentication

Embedders provide an `a2a.AuthorizationProvider` through
`afcli.Config.A2AAuthorization`. It runs for every request so short-lived
credentials can rotate during polling. Standalone callers can instead pass
`--bearer-token-file`; the file is reread for every request and the token never
appears in a command-line argument.

## Extensions

Repeat `--extension <uri>` for extensions the caller implements. The client
intersects that set with the card advertisement, refuses an unknown required
extension before the JSON-RPC call, sends only the activated intersection in
`A2A-Extensions`, and uses that same set in `Message.extensions`.

## Output

`--json` emits the protocol response type directly: `SendMessageResponse`,
`Task`, or `ListTasksResponse`. It does not introduce a second CLI envelope.
Without `--json`, commands print a compact task id/state or direct message.

Directory listing and sequence-cursor inboxes are not A2A core operations and
are intentionally absent. Embedders may keep those as separately named
extensions while using this client for formal send/get/list/cancel traffic.
