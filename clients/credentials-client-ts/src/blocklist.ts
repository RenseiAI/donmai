/**
 * Agent env-var blocklist.
 *
 * These environment variables are never returned to callers, regardless of
 * whether they arrive from the daemon's INITIAL/UPDATE messages or from
 * `process.env` in standalone mode. They represent the daemon's own
 * control-plane auth surface and must not leak into agent subprocesses.
 *
 * CANONICAL SOURCE: this list is duplicated from the Go reference at
 *   agentfactory-tui/internal/credentials/blocklist.go (AgentEnvBlocklist).
 * The Go file is the single source of truth; this TypeScript copy exists
 * because the OSS boundary blocks a shared import today.
 *
 * Sync obligation: any addition / removal in the Go file must be mirrored
 * here in the same change. A CI-diff check is planned to enforce this
 * automatically; until it ships, reviewers should grep both files when
 * touching either.
 */
export const AGENT_ENV_BLOCKLIST: readonly string[] = Object.freeze([
  'RENSEI_DAEMON_JWT',
  'RENSEI_DAEMON_API_KEY',
  'M2M_JWT_SECRET',
  'AUDIT_SIGNING_KEY_PRIVATE',
  'AUDIT_SIGNING_KEY_PUBLIC',
  'WORKOS_API_KEY',
  'WORKOS_COOKIE_PASSWORD',
  'RENSEI_RUNTIME_JWT',
  'WORKER_API_KEY',
]);

const BLOCKLIST_SET: ReadonlySet<string> = new Set(AGENT_ENV_BLOCKLIST);

/**
 * Reports whether the given env-var name is on the blocklist.
 *
 * Comparison is exact (case-sensitive). POSIX env-var names are
 * case-sensitive on all supported platforms.
 */
export function isBlocked(name: string): boolean {
  return BLOCKLIST_SET.has(name);
}

/**
 * Returns a new object containing every entry of `src` whose key is not
 * on the blocklist. Mutating the result is safe; `src` is not modified.
 */
export function filterBlocklist(src: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(src)) {
    if (BLOCKLIST_SET.has(k)) continue;
    out[k] = v;
  }
  return out;
}
