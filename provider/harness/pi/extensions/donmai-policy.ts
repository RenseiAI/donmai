// donmai policy extension — the trust boundary for the pi harness.
//
// pi ships NO permission system, no sandbox, no MCP: tools run with the full
// permissions of the spawning user. This extension is the ENTIRE trust
// boundary, owned by the orchestrator. It is shipped INSIDE the donmai binary
// via go:embed and materialized into <worktree>/.pi/extensions/ per session —
// it is never fetched from the network. Its SHA is verified against the
// embedded payload at load (handshake, below); a mismatch kills the session.
//
// Mechanism (design §5.1): override EVERY built-in tool. Each override
// serializes the intended call and raises a structured ctx.ui request, which
// pi's --mode rpc turns into an extension_ui_request/extension_ui_response
// round-trip over stdio. The Go side adjudicates (policy.go) and answers
// allow/deny. On allow we delegate to the original tool; on deny we return a
// tool-error string carrying the reason so the model sees WHY.
//
// This file is intentionally dependency-free and brand-neutral.

const HANDSHAKE_TYPE = "donmai.handshake";
const ADJUDICATE_TYPE = "donmai.adjudicate";

// The built-in tools this extension overrides. Read-only variants are
// included so the Go side can enforce read containment for autonomous
// sessions (design §5.2).
const GUARDED_TOOLS = ["read", "write", "edit", "bash", "grep", "find", "ls"];

export default function activate(pi: any) {
  // Handshake: announce ourselves with a nonce + our own source SHA so the Go
  // side can verify it materialized the extension it expects. No handshake
  // reply (or a SHA mismatch) means the Go side never sends the prompt — the
  // session fails closed.
  const nonce = pi.crypto?.randomUUID?.() ?? String(Date.now());
  const selfSHA = pi.extension?.sourceSHA256?.() ?? "";
  pi.ui
    .request({ type: HANDSHAKE_TYPE, nonce, extensionSHA: selfSHA })
    .catch((err: unknown) => {
      // If the handshake cannot be delivered we must not run unguarded.
      pi.session?.abort?.(`donmai policy handshake failed: ${String(err)}`);
    });

  for (const name of GUARDED_TOOLS) {
    pi.defineTool({
      name,
      override: true,
      async run(args: any, orig: (a: any) => Promise<any>) {
        const call = {
          tool: name,
          args,
          command: typeof args?.command === "string" ? args.command : "",
          path: resolvePath(pi, args),
          cwd: pi.session?.cwd ?? "",
        };
        const verdict = await pi.ui.request({
          type: ADJUDICATE_TYPE,
          nonce,
          call,
        });
        if (!verdict?.allow) {
          return {
            isError: true,
            content: `denied by policy: ${verdict?.reason ?? "no reason given"}`,
          };
        }
        return orig(args);
      },
    });
  }
}

function resolvePath(pi: any, args: any): string {
  const p = args?.path ?? args?.file ?? args?.filename ?? "";
  if (!p) return "";
  try {
    return pi.path?.resolve?.(pi.session?.cwd ?? "", p) ?? p;
  } catch {
    return p;
  }
}
