package pi

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// DefaultCatalogProbeTimeout bounds the launch-time catalog preflight
// (requirement 2), mirroring DefaultVersionProbeTimeout's construction-time
// probe budget.
const DefaultCatalogProbeTimeout = 5 * time.Second

// catalogProbeFunc runs a read-only pi catalog query for provider/model and
// returns pi's raw `--list-models` output. credEnvVar/credEnvValue (empty
// when the cell resolved no credential) carry the SAME provider-native
// credential applyEndpoint mirrored onto spec.Env — pi's own `--list-models`
// only lists a provider's models once it sees ANY configured credential for
// it (verified against the pinned-adjacent binary: `--list-models zai` is
// empty on a host with no zai credential configured at all, populated once
// one is set), so probing without it would report every real, correctly
// classified pair as "absent" and the preflight would deny closed on every
// spawn. Overridable via Options.CatalogProbe so tests can prove the
// preflight's RED/GREEN behavior with a scripted catalog, without a real pi
// binary on PATH — this package's own hosted CI does not install node/pi
// (doc.go), so the default exec path is exercised opportunistically, never
// by `make test` itself.
type catalogProbeFunc func(ctx context.Context, binary, provider, model, credEnvVar, credEnvValue string) (string, error)

// defaultCatalogProbe execs `<binary> --list-models <provider>/<model>
// --offline`, with credEnvVar=credEnvValue added to the child's env when
// both are non-empty (see catalogProbeFunc's doc comment for why). --offline
// (pi's flag; mirrors the PI_OFFLINE env default this package already sets —
// offlinePostureEnv) keeps the preflight bounded and deterministic: it must
// answer from pi's already-installed/cached catalog only, never block a
// spawn on a live catalog refresh.
//
// Two isolation properties, both load-bearing (found in review):
//
//   - Credential isolation. pi's own resolution order puts an auth.json
//     entry ABOVE an environment variable (docs/providers.md: "Auth file
//     credentials take priority over environment variables"). A bare
//     exec.Command would inherit whatever PI_CODING_AGENT_DIR the daemon
//     process happens to have (unset ⇒ pi's own default, ~/.pi/agent) — so
//     an OPERATOR'S PERSONAL LOGIN for the same provider (an ambient
//     auth.json entry entirely unrelated to this cell) would satisfy pi's
//     own auth check regardless of credEnvValue, reporting the model
//     "present" even when the cell's OWN BYOK credential is empty or wrong.
//     defaultCatalogProbe therefore points PI_CODING_AGENT_DIR at a
//     throwaway, freshly-created, per-call directory — never the real
//     ~/.pi/agent, never any session's real agentHome — so the ONLY
//     credential source pi can resolve for this probe is the env var this
//     function explicitly sets.
//   - Env-hygiene parity. The child env is built from a MINIMAL, explicit
//     base (PATH, HOME, TMPDIR, the isolated agent dir, and the single
//     resolved credential var) rather than inheriting the full parent
//     process env (os.Environ()): the daemon process may carry ambient
//     credentials for OTHER providers/companies that composeChildEnv's
//     AgentEnvBlocklist (runtime/env/composer.go) exists specifically to
//     keep out of a harness child. A read-only preflight probe must honor
//     the same boundary, not bypass it via a wider default.
func defaultCatalogProbe(ctx context.Context, binary, provider, model, credEnvVar, credEnvValue string) (string, error) {
	agentDir, err := os.MkdirTemp("", "donmai-pi-catalog-probe-*")
	if err != nil {
		return "", fmt.Errorf("pi --list-models: create isolated agent dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(agentDir) }()

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.TempDir(),
		piCodingAgentDirEnvVar + "=" + agentDir,
	}
	if credEnvVar != "" && credEnvValue != "" {
		env = append(env, credEnvVar+"="+credEnvValue)
	}

	// nolint:gosec // G204: binary is the resolved-from-PATH path New() also
	// uses to exec `pi --mode rpc`; --list-models is a read-only query, and
	// provider/model are this package's own already-classified strings
	// (builtin_providers.go's allowlist + the pin split), never raw
	// caller-supplied shell text.
	cmd := exec.CommandContext(ctx, binary, "--list-models", provider+"/"+model, "--offline")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("pi --list-models: %w", err)
	}
	return string(out), nil
}

// catalogHasModel reports whether raw (defaultCatalogProbe's stdout, or a
// test-scripted equivalent) lists provider/model as an EXACT row. pi's
// `--list-models <pattern>` applies FUZZY search (docs: "supports … fuzzy
// matching"), so a merely non-empty result is not enough to confirm the
// exact pair exists — this discards the header row and any row whose first
// two whitespace-separated columns are not an exact, case-sensitive match
// against provider and model.
func catalogHasModel(raw, provider, model string) bool {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// The header row's own column labels ("provider", "model") can never
		// legitimately be queried for — no entry in builtin_providers.go's
		// allowlist is named "provider" — so skip it explicitly rather than
		// assuming a fixed line position (robust to leading blank lines or
		// extra program output ahead of the table).
		if fields[0] == "provider" && fields[1] == "model" {
			continue
		}
		if fields[0] == provider && fields[1] == model {
			return true
		}
	}
	return false
}

// resolveCatalogProbe returns the catalogProbeFunc prepare() should run, or
// nil to skip the preflight entirely. An explicit Options.CatalogProbe
// (tests) always wins; otherwise the preflight only runs when p.realBinary
// is true — i.e. New() actually resolved a real pi binary from PATH/PiBin
// (see the Provider.realBinary doc comment for why this is NOT the same
// check as !opts.skipProcess).
func (p *Provider) resolveCatalogProbe() catalogProbeFunc {
	if p.opts.CatalogProbe != nil {
		return p.opts.CatalogProbe
	}
	if !p.realBinary {
		return nil
	}
	return defaultCatalogProbe
}

// preflightCatalogCheck is requirement 2: fail fast, before any child spawns,
// when a NATIVE built-in-provider routing decision (nativeProviderPin's
// useNative case) resolves to a (provider, model) pair pi's own catalog does
// not recognize. This is deliberately scoped to the native-routing case
// only — the injected "donmai" provider's registered model is one this
// package constructs itself (providerPinEnv), so it is trivially "present"
// by definition and has nothing external to preflight against.
//
// A probe ERROR (binary unresolvable, catalog empty offline, timeout) is NOT
// fatal — pi's catalog surface is not guaranteed reachable in every
// environment this runs in, and DEC-2's precedent (probe.go
// checkVersionPin) is to label an unverifiable condition rather than block
// on it. Only a CONFIRMED absence — the probe ran, returned a real listing,
// and no row matches — denies spawn: that is exactly the "spawn succeeds,
// first turn 400s" failure shape moved earlier, to a clear, actionable
// error instead of a live upstream rejection.
func (p *Provider) preflightCatalogCheck(ctx context.Context, probe catalogProbeFunc, provider, model, credEnvVar, credEnvValue string) error {
	timeout := p.opts.CatalogProbeTimeout
	if timeout == 0 {
		timeout = DefaultCatalogProbeTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := probe(pctx, p.binary, provider, model, credEnvVar, credEnvValue)
	if err != nil {
		// Unverifiable, not fatal — see doc comment (DEC-2 precedent). Still
		// logged (not swallowed silently): an unsupported `--list-models`/
		// `--offline` flag on some future pi release, or any other probe
		// failure, should be visible to an operator even though it does not
		// block this spawn.
		slog.Warn("pi catalog preflight probe failed; proceeding unverified", "provider", provider, "model", model, "error", err)
		return nil
	}
	if catalogHasModel(raw, provider, model) {
		return nil
	}
	return fmt.Errorf("%w: pi has no built-in model %q for provider %q in its catalog (checked with `pi --list-models %s/%s`); the resolved pin would 400 on its first turn",
		agent.ErrSpawnFailed, model, provider, provider, model)
}
