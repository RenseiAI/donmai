/**
 * @renseiai/credentials-client
 *
 * Agent-side credential loader. Exposes the same API surface in two
 * runtime modes:
 *
 *   - daemon mode: the host process sets DONMAI_CREDENTIAL_SOCKET to a
 *     unix-socket path. The loader dials the socket, sends a HELLO
 *     identifying its session, and receives an INITIAL snapshot followed
 *     by zero or more UPDATE messages.
 *   - standalone mode: no socket is configured. The loader snapshots
 *     `process.env` at construction time. Subscriptions are accepted but
 *     never receive updates.
 *
 * The blocklist (see ./blocklist.ts) is applied on every read in both
 * modes. The same list is enforced server-side in daemon mode, but we
 * re-filter here as defence-in-depth.
 */
import net from 'node:net';
import { filterBlocklist, isBlocked } from './blocklist';

export { AGENT_ENV_BLOCKLIST, isBlocked, filterBlocklist } from './blocklist';

/** Minimum logger surface the loader uses for diagnostics. */
export interface Logger {
  info: (msg: string) => void;
  warn: (msg: string) => void;
}

/** Options accepted by `createLoader`. */
export interface Options {
  /**
   * Session identifier sent in the HELLO message.
   *
   * Required in daemon mode. If omitted, the loader checks
   * `process.env.DONMAI_CREDENTIAL_SESSION_ID` (the convention used by
   * the daemon spawner). If still missing, daemon mode is skipped and
   * the loader falls back to standalone.
   *
   * Ignored entirely in standalone mode.
   */
  sessionId?: string;

  /**
   * Optional per-session credential-socket capability. A non-empty value
   * is sent as HELLO.capability. When empty or omitted, the loader reads
   * `process.env.DONMAI_CREDENTIAL_CAPABILITY`; if that is also empty, the
   * property is omitted to preserve the legacy HELLO shape.
   */
  capability?: string;

  /**
   * Handshake timeout in milliseconds. Applies to both the socket
   * connect and the wait for the INITIAL reply. Default 5000.
   */
  handshakeTimeoutMs?: number;

  /** Optional logger; defaults to console-backed info/warn. */
  logger?: Logger;
}

/** Public Loader contract. */
export interface Loader {
  /**
   * Returns the credential value for the given env-var name, or
   * undefined if absent or blocklisted.
   */
  get(name: string): string | undefined;

  /**
   * Returns a shallow copy of every credential, with the blocklist
   * applied. Subsequent mutations do not affect the loader's internal
   * state.
   */
  all(): Record<string, string>;

  /**
   * Register a callback for UPDATE deltas (daemon mode only). The
   * callback receives a blocklist-filtered map of changed keys; absent
   * keys are unchanged. Returns an unsubscribe function. In standalone
   * mode the callback is registered but never invoked.
   */
  subscribe(cb: (delta: Record<string, string>) => void): () => void;

  /**
   * Release socket resources. Idempotent. In daemon mode this sends a
   * BYE message before closing the socket.
   */
  close(): Promise<void>;

  /** "daemon" or "standalone". */
  readonly mode: 'daemon' | 'standalone';
}

const DEFAULT_HANDSHAKE_TIMEOUT_MS = 5_000;
const CREDENTIAL_CAPABILITY_ENV_VAR = 'DONMAI_CREDENTIAL_CAPABILITY';

function defaultLogger(): Logger {
  return {
    info: (msg) => console.info(msg),
    warn: (msg) => console.warn(msg),
  };
}

/**
 * Public entry point. Always resolves (never rejects). On any daemon
 * failure the returned Loader is in standalone mode with a single
 * info-level log line explaining the fallback.
 */
export async function createLoader(options?: Options): Promise<Loader> {
  const logger = options?.logger ?? defaultLogger();
  const socketPath = process.env.DONMAI_CREDENTIAL_SOCKET;

  if (!socketPath) {
    return makeStandaloneLoader(logger);
  }

  const sessionId =
    options?.sessionId ?? process.env.DONMAI_CREDENTIAL_SESSION_ID ?? '';
  if (!sessionId) {
    logger.info(
      'credentials-client: DONMAI_CREDENTIAL_SOCKET set but no sessionId provided; falling back to standalone mode',
    );
    return makeStandaloneLoader(logger);
  }

  const capability =
    options?.capability || process.env[CREDENTIAL_CAPABILITY_ENV_VAR] || '';
  const handshakeTimeout =
    options?.handshakeTimeoutMs ?? DEFAULT_HANDSHAKE_TIMEOUT_MS;

  try {
    return await makeDaemonLoader({
      socketPath,
      sessionId,
      capability,
      handshakeTimeoutMs: handshakeTimeout,
      logger,
    });
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    logger.info(
      `credentials-client: daemon handshake failed (${message}); falling back to standalone mode`,
    );
    return makeStandaloneLoader(logger);
  }
}

/* ------------------------------------------------------------------ */
/* Standalone mode                                                     */
/* ------------------------------------------------------------------ */

function snapshotProcessEnv(): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (typeof v === 'string') {
      out[k] = v;
    }
  }
  return out;
}

function makeStandaloneLoader(_logger: Logger): Loader {
  const snapshot = snapshotProcessEnv();
  const subscribers = new Set<(delta: Record<string, string>) => void>();
  let closed = false;

  return {
    mode: 'standalone',
    get(name: string): string | undefined {
      if (isBlocked(name)) return undefined;
      const v = snapshot[name];
      return typeof v === 'string' ? v : undefined;
    },
    all(): Record<string, string> {
      return filterBlocklist(snapshot);
    },
    subscribe(cb): () => void {
      subscribers.add(cb);
      return () => {
        subscribers.delete(cb);
      };
    },
    async close(): Promise<void> {
      if (closed) return;
      closed = true;
      subscribers.clear();
    },
  };
}

/* ------------------------------------------------------------------ */
/* Daemon mode                                                         */
/* ------------------------------------------------------------------ */

interface DaemonLoaderArgs {
  socketPath: string;
  sessionId: string;
  capability: string;
  handshakeTimeoutMs: number;
  logger: Logger;
}

function isStringMap(v: unknown): v is Record<string, string> {
  if (v === null || typeof v !== 'object') return false;
  for (const val of Object.values(v as Record<string, unknown>)) {
    if (typeof val !== 'string') return false;
  }
  return true;
}

async function makeDaemonLoader(args: DaemonLoaderArgs): Promise<Loader> {
  const { socketPath, sessionId, capability, handshakeTimeoutMs, logger } = args;

  const socket = net.createConnection({ path: socketPath });

  // Wait for the connection to actually open (or fail) before HELLO.
  // We bundle connect + handshake into a single deadline.
  const state: { snapshot: Record<string, string> } = { snapshot: {} };
  const subscribers = new Set<(delta: Record<string, string>) => void>();
  let buffer = '';
  let receivedInitial = false;
  let closed = false;

  const handleLine = (line: string): void => {
    if (line.length === 0) return;
    let msg: unknown;
    try {
      msg = JSON.parse(line);
    } catch {
      logger.warn(`credentials-client: malformed JSON line (len=${line.length}); ignored`);
      return;
    }
    if (msg === null || typeof msg !== 'object') {
      logger.warn('credentials-client: non-object JSON message; ignored');
      return;
    }
    const t = (msg as { type?: unknown }).type;
    if (t === 'INITIAL') {
      const env = (msg as { env?: unknown }).env;
      if (!isStringMap(env)) {
        throw new Error('INITIAL message had non-string-map env field');
      }
      state.snapshot = filterBlocklist(env);
      receivedInitial = true;
      return;
    }
    if (t === 'UPDATE') {
      const delta = (msg as { delta?: unknown }).delta;
      if (!isStringMap(delta)) {
        logger.warn('credentials-client: UPDATE without string-map delta; ignored');
        return;
      }
      const filtered = filterBlocklist(delta);
      for (const [k, v] of Object.entries(filtered)) {
        state.snapshot[k] = v;
      }
      for (const cb of subscribers) {
        try {
          cb(filtered);
        } catch (err) {
          const m = err instanceof Error ? err.message : String(err);
          logger.warn(`credentials-client: subscriber threw: ${m}`);
        }
      }
      return;
    }
    if (t === 'BYE') {
      // Daemon-initiated close; do nothing extra — the socket will end.
      return;
    }
    logger.warn(`credentials-client: unknown message type "${String(t)}"; ignored`);
  };

  const flushBuffer = (): void => {
    let idx;
    while ((idx = buffer.indexOf('\n')) >= 0) {
      const line = buffer.slice(0, idx);
      buffer = buffer.slice(idx + 1);
      handleLine(line);
    }
  };

  socket.setEncoding('utf8');

  // Single persistent data listener — used both during handshake (resolves
  // the awaited promise once INITIAL lands) and post-handshake (delivers
  // UPDATE messages). A second listener would double-process every chunk.
  let onInitial: ((err?: Error) => void) | null = null;
  socket.on('data', (chunk: Buffer | string) => {
    buffer += typeof chunk === 'string' ? chunk : chunk.toString('utf8');
    try {
      flushBuffer();
    } catch (err) {
      if (onInitial) {
        const cb = onInitial;
        onInitial = null;
        cb(err instanceof Error ? err : new Error(String(err)));
        return;
      }
      const m = err instanceof Error ? err.message : String(err);
      logger.warn(`credentials-client: malformed post-handshake message (${m})`);
      return;
    }
    if (receivedInitial && onInitial) {
      const cb = onInitial;
      onInitial = null;
      cb();
    }
  });

  // Wait for INITIAL within handshakeTimeoutMs of `createLoader` invocation.
  await new Promise<void>((resolve, reject) => {
    let settled = false;
    const settle = (err?: Error): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      if (err) reject(err);
      else resolve();
    };
    const timeout = setTimeout(() => {
      socket.destroy(new Error('handshake timeout'));
      settle(new Error(`handshake timeout after ${handshakeTimeoutMs}ms`));
    }, handshakeTimeoutMs);
    if (typeof timeout.unref === 'function') timeout.unref();

    onInitial = (err?: Error) => settle(err);

    socket.once('connect', () => {
      const helloFrame = capability
        ? { type: 'HELLO', sessionId, capability }
        : { type: 'HELLO', sessionId };
      const hello = JSON.stringify(helloFrame) + '\n';
      socket.write(hello, (err) => {
        if (err) settle(err);
      });
    });
    socket.once('error', (err) => settle(err));
    socket.once('close', () => settle(new Error('socket closed before INITIAL received')));
  });

  // Post-handshake error handling.
  socket.on('error', (err) => {
    if (closed) return;
    logger.warn(`credentials-client: socket error after handshake: ${err.message}`);
  });

  // Allow the host process to exit if it has no other work.
  if (typeof socket.unref === 'function') socket.unref();

  return {
    mode: 'daemon',
    get(name: string): string | undefined {
      if (isBlocked(name)) return undefined;
      const v = state.snapshot[name];
      return typeof v === 'string' ? v : undefined;
    },
    all(): Record<string, string> {
      // snapshot is already blocklist-filtered on ingest, but re-apply on
      // read in case AGENT_ENV_BLOCKLIST grows at runtime in some future
      // refactor (defence-in-depth).
      return filterBlocklist(state.snapshot);
    },
    subscribe(cb): () => void {
      subscribers.add(cb);
      return () => {
        subscribers.delete(cb);
      };
    },
    async close(): Promise<void> {
      if (closed) return;
      closed = true;
      subscribers.clear();
      // Best-effort BYE; ignore any write errors during shutdown.
      await new Promise<void>((resolve) => {
        if (socket.destroyed || !socket.writable) {
          try {
            socket.destroy();
          } catch {
            // ignore
          }
          resolve();
          return;
        }
        const bye = JSON.stringify({ type: 'BYE' }) + '\n';
        socket.write(bye, () => {
          socket.end(() => {
            resolve();
          });
        });
        // Hard backstop so close() always resolves.
        const t = setTimeout(() => {
          try {
            socket.destroy();
          } catch {
            // ignore
          }
          resolve();
        }, 250);
        if (typeof t.unref === 'function') t.unref();
      });
    },
  };
}
