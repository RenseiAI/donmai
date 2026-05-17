import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';
import { randomUUID } from 'node:crypto';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createLoader, AGENT_ENV_BLOCKLIST, isBlocked, filterBlocklist } from '../index';

const envSnapshot: NodeJS.ProcessEnv = {};
const TRACKED_ENV_KEYS = [
  'RENSEI_CREDENTIAL_SOCKET',
  'RENSEI_CREDENTIAL_SESSION_ID',
  'FOO',
  'BAR',
  ...AGENT_ENV_BLOCKLIST,
];

function saveEnv(): void {
  for (const k of TRACKED_ENV_KEYS) {
    envSnapshot[k] = process.env[k];
  }
}

function restoreEnv(): void {
  for (const k of TRACKED_ENV_KEYS) {
    const v = envSnapshot[k];
    if (v === undefined) {
      delete process.env[k];
    } else {
      process.env[k] = v;
    }
  }
}

interface FakeServerOptions {
  /** Initial env to send back in the INITIAL message. */
  initial?: Record<string, string>;
  /** If true, do not send INITIAL at all (used to test handshake timeout). */
  withholdInitial?: boolean;
  /** If true, send malformed INITIAL JSON. */
  malformedInitial?: boolean;
  /** Called once the server receives a HELLO. */
  onHello?: (hello: { sessionId: string }, socket: net.Socket) => void;
  /** Called once the server receives a BYE. */
  onBye?: () => void;
}

interface FakeServerHandle {
  path: string;
  pushUpdate: (delta: Record<string, string>) => void;
  close: () => Promise<void>;
  helloReceived: () => Promise<{ sessionId: string }>;
  byeReceived: () => Promise<void>;
}

function newSocketPath(): string {
  return path.join(os.tmpdir(), `cred-test-${randomUUID()}.sock`);
}

async function startFakeServer(
  options: FakeServerOptions = {},
): Promise<FakeServerHandle> {
  const sockPath = newSocketPath();
  let activeConn: net.Socket | null = null;

  let helloResolve: (h: { sessionId: string }) => void = () => undefined;
  const helloPromise = new Promise<{ sessionId: string }>((resolve) => {
    helloResolve = resolve;
  });
  let byeResolve: () => void = () => undefined;
  const byePromise = new Promise<void>((resolve) => {
    byeResolve = resolve;
  });

  const server = net.createServer({ allowHalfOpen: true }, (socket) => {
    activeConn = socket;
    socket.setEncoding('utf8');
    let buf = '';
    socket.on('data', (chunk) => {
      buf += typeof chunk === 'string' ? chunk : chunk.toString('utf8');
      let idx;
      while ((idx = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (line.length === 0) continue;
        let parsed: unknown;
        try {
          parsed = JSON.parse(line);
        } catch {
          continue;
        }
        const msg = parsed as { type?: string; sessionId?: string };
        if (msg.type === 'HELLO') {
          const sid = msg.sessionId ?? '';
          helloResolve({ sessionId: sid });
          options.onHello?.({ sessionId: sid }, socket);
          if (options.withholdInitial) {
            // Do nothing — let the client timeout.
          } else if (options.malformedInitial) {
            socket.write('{not-json\n');
          } else {
            const env = options.initial ?? {};
            socket.write(
              JSON.stringify({ type: 'INITIAL', env }) + '\n',
            );
          }
        } else if (msg.type === 'BYE') {
          byeResolve();
          options.onBye?.();
        }
      }
    });
    socket.on('error', () => {
      /* ignore */
    });
  });

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(sockPath, () => {
      server.off('error', reject);
      resolve();
    });
  });

  return {
    path: sockPath,
    pushUpdate(delta) {
      if (!activeConn || activeConn.destroyed || !activeConn.writable) return;
      activeConn.write(
        JSON.stringify({
          type: 'UPDATE',
          delta,
          rotatedAt: new Date().toISOString(),
        }) + '\n',
      );
    },
    async close() {
      if (activeConn && !activeConn.destroyed) {
        activeConn.destroy();
      }
      await new Promise<void>((resolve) => {
        server.close(() => resolve());
      });
      try {
        if (fs.existsSync(sockPath)) fs.unlinkSync(sockPath);
      } catch {
        /* ignore */
      }
    },
    helloReceived() {
      return helloPromise;
    },
    byeReceived() {
      return byePromise;
    },
  };
}

function silentLogger(): { info: ReturnType<typeof vi.fn>; warn: ReturnType<typeof vi.fn> } {
  return { info: vi.fn(), warn: vi.fn() };
}

beforeEach(() => {
  saveEnv();
  // Clean slate for each test.
  delete process.env.RENSEI_CREDENTIAL_SOCKET;
  delete process.env.RENSEI_CREDENTIAL_SESSION_ID;
});

afterEach(() => {
  restoreEnv();
});

describe('blocklist helpers', () => {
  it('isBlocked returns true for blocklisted names', () => {
    expect(isBlocked('RENSEI_DAEMON_JWT')).toBe(true);
    expect(isBlocked('WORKER_API_KEY')).toBe(true);
    expect(isBlocked('FOO')).toBe(false);
  });

  it('filterBlocklist removes blocked entries and copies others', () => {
    const out = filterBlocklist({
      FOO: 'bar',
      RENSEI_DAEMON_JWT: 'secret',
      BAR: 'baz',
    });
    expect(out).toEqual({ FOO: 'bar', BAR: 'baz' });
  });

  it('AGENT_ENV_BLOCKLIST is frozen', () => {
    expect(Object.isFrozen(AGENT_ENV_BLOCKLIST)).toBe(true);
  });
});

describe('createLoader — standalone mode', () => {
  it('reads values from process.env when no socket is configured', async () => {
    process.env.FOO = 'bar';
    const loader = await createLoader({ logger: silentLogger() });
    try {
      expect(loader.mode).toBe('standalone');
      expect(loader.get('FOO')).toBe('bar');
    } finally {
      await loader.close();
    }
  });

  it('blocks blocklisted names even in standalone mode', async () => {
    process.env.RENSEI_DAEMON_JWT = 'secret';
    process.env.FOO = 'visible';
    const loader = await createLoader({ logger: silentLogger() });
    try {
      expect(loader.get('RENSEI_DAEMON_JWT')).toBeUndefined();
      expect(loader.get('FOO')).toBe('visible');
      const all = loader.all();
      expect(all.RENSEI_DAEMON_JWT).toBeUndefined();
      expect(all.FOO).toBe('visible');
    } finally {
      await loader.close();
    }
  });

  it('subscribe is accepted and unsubscribe is a no-op', async () => {
    const loader = await createLoader({ logger: silentLogger() });
    try {
      const cb = vi.fn();
      const unsub = loader.subscribe(cb);
      unsub();
      expect(cb).not.toHaveBeenCalled();
    } finally {
      await loader.close();
    }
  });

  it('close() is idempotent', async () => {
    const loader = await createLoader({ logger: silentLogger() });
    await loader.close();
    await loader.close();
  });
});

describe('createLoader — daemon mode', () => {
  it('happy path: HELLO/INITIAL populates snapshot', async () => {
    const server = await startFakeServer({
      initial: { FOO: 'from-daemon', RENSEI_DAEMON_JWT: 'blocked' },
    });
    process.env.RENSEI_CREDENTIAL_SOCKET = server.path;
    const loader = await createLoader({
      sessionId: 'test-session-1',
      logger: silentLogger(),
    });
    try {
      expect(loader.mode).toBe('daemon');
      expect(loader.get('FOO')).toBe('from-daemon');
      // Blocklist applied on the daemon's INITIAL.
      expect(loader.get('RENSEI_DAEMON_JWT')).toBeUndefined();
      const hello = await server.helloReceived();
      expect(hello.sessionId).toBe('test-session-1');
    } finally {
      await loader.close();
      await server.close();
    }
  });

  it('falls back to standalone when handshake times out', async () => {
    const server = await startFakeServer({ withholdInitial: true });
    process.env.RENSEI_CREDENTIAL_SOCKET = server.path;
    process.env.STANDALONE_FALLBACK_FOO = 'standalone-value';
    const logger = silentLogger();
    const loader = await createLoader({
      sessionId: 'timeout-session',
      handshakeTimeoutMs: 200,
      logger,
    });
    try {
      expect(loader.mode).toBe('standalone');
      expect(loader.get('STANDALONE_FALLBACK_FOO')).toBe('standalone-value');
      // info-level fallback log surfaced.
      expect(logger.info).toHaveBeenCalled();
    } finally {
      delete process.env.STANDALONE_FALLBACK_FOO;
      await loader.close();
      await server.close();
    }
  });

  it('falls back to standalone on malformed INITIAL', async () => {
    const server = await startFakeServer({ malformedInitial: true });
    process.env.RENSEI_CREDENTIAL_SOCKET = server.path;
    const logger = silentLogger();
    const loader = await createLoader({
      sessionId: 'malformed-session',
      handshakeTimeoutMs: 500,
      logger,
    });
    try {
      expect(loader.mode).toBe('standalone');
      expect(logger.info).toHaveBeenCalled();
    } finally {
      await loader.close();
      await server.close();
    }
  });

  it('subscribe delivers UPDATE deltas (blocklist applied)', async () => {
    const server = await startFakeServer({ initial: { FOO: 'v1' } });
    process.env.RENSEI_CREDENTIAL_SOCKET = server.path;
    const loader = await createLoader({
      sessionId: 'subscribe-session',
      logger: silentLogger(),
    });
    try {
      const received: Array<Record<string, string>> = [];
      const unsub = loader.subscribe((delta) => {
        received.push(delta);
      });

      server.pushUpdate({ FOO: 'v2', RENSEI_DAEMON_JWT: 'blocked' });
      // Give the event loop a couple of ticks to flush the socket.
      await new Promise((r) => setTimeout(r, 50));
      expect(received).toHaveLength(1);
      expect(received[0]).toEqual({ FOO: 'v2' });
      expect(loader.get('FOO')).toBe('v2');
      expect(loader.get('RENSEI_DAEMON_JWT')).toBeUndefined();
      unsub();
    } finally {
      await loader.close();
      await server.close();
    }
  });

  it('unsubscribe stops delivery', async () => {
    const server = await startFakeServer({ initial: { FOO: 'v1' } });
    process.env.RENSEI_CREDENTIAL_SOCKET = server.path;
    const loader = await createLoader({
      sessionId: 'unsub-session',
      logger: silentLogger(),
    });
    try {
      const cb = vi.fn();
      const unsub = loader.subscribe(cb);
      unsub();
      server.pushUpdate({ FOO: 'v2' });
      await new Promise((r) => setTimeout(r, 50));
      expect(cb).not.toHaveBeenCalled();
      // Snapshot still updates internally even when no subscribers.
      expect(loader.get('FOO')).toBe('v2');
    } finally {
      await loader.close();
      await server.close();
    }
  });

  it('close() sends BYE then ends the socket', async () => {
    const server = await startFakeServer({ initial: { FOO: 'v1' } });
    process.env.RENSEI_CREDENTIAL_SOCKET = server.path;
    const loader = await createLoader({
      sessionId: 'bye-session',
      logger: silentLogger(),
    });
    await loader.close();
    // Server should have observed the BYE.
    await Promise.race([
      server.byeReceived(),
      new Promise<void>((_, reject) =>
        setTimeout(() => reject(new Error('BYE not received within 500ms')), 500),
      ),
    ]);
    await server.close();
  });

  it('falls back to standalone when sessionId is missing in daemon mode', async () => {
    const server = await startFakeServer();
    process.env.RENSEI_CREDENTIAL_SOCKET = server.path;
    const logger = silentLogger();
    // No sessionId option, no RENSEI_CREDENTIAL_SESSION_ID env.
    const loader = await createLoader({ logger });
    try {
      expect(loader.mode).toBe('standalone');
      expect(logger.info).toHaveBeenCalled();
    } finally {
      await loader.close();
      await server.close();
    }
  });

  it('picks up sessionId from RENSEI_CREDENTIAL_SESSION_ID when option absent', async () => {
    const server = await startFakeServer({ initial: { FOO: 'env-sid' } });
    process.env.RENSEI_CREDENTIAL_SOCKET = server.path;
    process.env.RENSEI_CREDENTIAL_SESSION_ID = 'env-derived';
    const loader = await createLoader({ logger: silentLogger() });
    try {
      expect(loader.mode).toBe('daemon');
      const hello = await server.helloReceived();
      expect(hello.sessionId).toBe('env-derived');
      expect(loader.get('FOO')).toBe('env-sid');
    } finally {
      await loader.close();
      await server.close();
    }
  });
});

describe('Loader.mode', () => {
  it('returns "standalone" when no socket is configured', async () => {
    const loader = await createLoader({ logger: silentLogger() });
    try {
      expect(loader.mode).toBe('standalone');
    } finally {
      await loader.close();
    }
  });

  it('returns "daemon" after a successful handshake', async () => {
    const server = await startFakeServer({ initial: {} });
    process.env.RENSEI_CREDENTIAL_SOCKET = server.path;
    const loader = await createLoader({
      sessionId: 'mode-session',
      logger: silentLogger(),
    });
    try {
      expect(loader.mode).toBe('daemon');
    } finally {
      await loader.close();
      await server.close();
    }
  });
});
