# @renseiai/credentials-client

Agent-side credential loader for Node.js. Provides a single API surface
that works in two modes:

- **Daemon mode** — when `RENSEI_CREDENTIAL_SOCKET` is set, the loader
  reads from a unix-socket-based credential service using a small
  line-delimited JSON protocol (`HELLO` / `INITIAL` / `UPDATE` / `BYE`)
  and receives live rotation events.
- **Standalone mode** — when no socket is configured, the loader
  snapshots `process.env` at construction time and serves reads from
  that snapshot.

A defence-in-depth blocklist removes daemon-internal env vars from every
read, regardless of mode.

## Install

```sh
npm install @renseiai/credentials-client
```

## Usage

```ts
import { createLoader } from '@renseiai/credentials-client';

const loader = await createLoader({ sessionId: 'my-session' });
const apiKey = loader.get('SOME_API_KEY');
const unsubscribe = loader.subscribe((delta) => {
  console.log('credentials rotated:', Object.keys(delta));
});
// ... later
unsubscribe();
await loader.close();
```

`createLoader` never rejects: if daemon mode is configured but the
handshake fails, the loader falls back to standalone mode and surfaces
a single info-level log line.

## Modes

| Mode         | Triggered by                                 | Behaviour                                                                   |
| ------------ | -------------------------------------------- | --------------------------------------------------------------------------- |
| `daemon`     | `process.env.RENSEI_CREDENTIAL_SOCKET` set, sessionId resolvable | Connects, sends `HELLO`, reads `INITIAL`, pumps `UPDATE` deltas. |
| `standalone` | otherwise (or on daemon handshake failure)   | Reads from `process.env`; subscribers never fire.                           |

## License

MIT
