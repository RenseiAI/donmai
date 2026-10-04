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

// catalogMissError reports a CONFIRMED catalog absence: the probe ran,
// returned a real listing, and no row matched the exact (provider, model)
// pair. prepare() (pi.go) translates it into a fallback onto the injected
// provider (fallbackToInjectedProvider) instead of denying spawn, so a
// model newer than the bundled catalog still starts on launch day. It wraps
// agent.ErrSpawnFailed, preserving the preflight's fail-closed contract at
// this layer for callers that do not translate it — and for the
// untranslatable cases (unknown provider, or no bound base URL for the
// injected provider to register against).
type catalogMissError struct {
	provider string
	model    string
}

func (e *catalogMissError) Error() string {
	return fmt.Sprintf("pi has no built-in model %q for provider %q in its catalog (checked with `pi --list-models %s/%s`); the resolved pin would 400 on its first turn",
		e.model, e.provider, e.provider, e.model)
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
	return fmt.Errorf("%w: %w", agent.ErrSpawnFailed, &catalogMissError{provider: provider, model: model})
}

// fallbackToInjectedProvider reroutes a session whose NATIVE built-in-provider
// pin missed the bundled catalog (a *catalogMissError from
// preflightCatalogCheck) onto the injected "donmai" provider, so a model
// newer than the installed catalog still starts on launch day. The rewrite
// keeps the bound endpoint (its BaseURL, credential env, and wire protocol)
// and the resolved limits (ProviderConfig contextWindow/maxOutputTokens)
// untouched: providerPinEnv registers exactly that endpoint as the injected
// provider's base URL, its wire API (piAPIForProtocol, e.g. the provider's
// own native API for a direct cell), and the provider-native bare model id.
// prepare() logs the fallback at the call site; this function only rewrites.
//
// The provider must be a known built-in provider (it just classified as a
// native "<provider>/<model>" pin) and the endpoint must carry a bound
// base URL for the injected provider to register against. Anything else
// keeps today's denial: an unknown provider refuses (there is no wire API
// or credential mapping to select), and an unbound session refuses (the
// injected provider registers only under a bound endpoint — modelPinArgs'
// injected branch — so there is no endpoint to route it through).
func fallbackToInjectedProvider(spec agent.Spec, miss *catalogMissError) (agent.Spec, error) {
	envVar, known := builtinProviderCredentialEnv[miss.provider]
	if !known {
		return spec, miss
	}
	ep := spec.Endpoint
	if ep == nil || ep.BaseURL == "" {
		return spec, miss
	}
	out := spec
	if ep.Model != "" {
		// applyEndpoint projected Endpoint.Model onto Spec.Model before the
		// preflight ran; the fallback must rewrite both so the binding and
		// the pin cannot disagree about which model serves this session.
		// The provider prefix is pi's own "--model provider/id" selector
		// syntax, never the wire model code: strip it so nativeProviderPin
		// no longer classifies the rewritten pin as native and every
		// consumer (modelPinArgs, providerPinEnv) routes on the injected
		// provider with the provider-native bare model id. Unknown future
		// ids pass through verbatim — no allowlist is consulted here.
		if _, bare, ok := splitBuiltinProviderPin(ep.Model); ok {
			nep := *ep
			nep.Model = bare
			out.Endpoint = &nep
		}
	}
	if _, bare, ok := splitBuiltinProviderPin(out.Model); ok {
		out.Model = bare
	} else {
		out.Model = miss.model
	}
	if out.Env == nil {
		out.Env = map[string]string{}
	} else {
		env := make(map[string]string, len(out.Env))
		for k, v := range out.Env {
			env[k] = v
		}
		out.Env = env
	}
	// The native-route credential mirror (applyEndpoint) may have placed the
	// cell key on the provider's own env var; the injected provider reads
	// the same key from PiKeyEnvVar (providerPinEnv), so make sure it rides
	// there too. Never overwrite an explicit value already present.
	if key := out.Env[envVar]; key != "" {
		if _, already := out.Env[PiKeyEnvVar]; !already {
			out.Env[PiKeyEnvVar] = key
		}
	}
	return out, nil
}

// promoteAggregatorPin selects pi's own built-in aggregator provider for an
// aggregator-bound session when, and only when, pi's catalog lists the exact
// slug — except for the "google/" author, whose slugs always stay on the
// injected Chat Completions lane: the aggregator serves them over a protocol
// that ignores the thinking budget, while the injected lane grades effort.
// spec.Model must be the aggregator's whole "<author>/<model>" slug on
// a non-loopback binding whose BaseURL is the aggregator's own https host
// (builtinAggregatorForBaseURL). On a confirmed match the pin is rewritten to
// "<aggregator>/<slug>" — which nativeProviderPin then routes natively — and
// the cell key is mirrored onto the aggregator's own credential env var.
//
// Every other outcome leaves spec unchanged, so the session stays on the
// injected provider with the whole slug against the bound BaseURL: a slug
// newer than pi's bundled catalog, an unprefixed model id, no probe (no real
// binary), or a probe error. The injected route works for any model the
// aggregator serves; the native route only adds pi's own model metadata.
func (p *Provider) promoteAggregatorPin(ctx context.Context, spec agent.Spec) (agent.Spec, bool) {
	ep := spec.Endpoint
	if ep == nil || ep.Host == agent.HostGateway || spec.Model == "" {
		return spec, false
	}
	// Gemini slugs stay on the injected Chat Completions lane: the
	// aggregator serves them over a protocol that ignores the thinking
	// budget, while the injected lane grades effort. Skip the native
	// promotion for that author even when the catalog lists the slug.
	if strings.HasPrefix(spec.Model, "google/") {
		return spec, false
	}
	agg, ok := builtinAggregatorForBaseURL(ep.BaseURL)
	if !ok || strings.HasPrefix(spec.Model, agg+"/") {
		return spec, false
	}
	probe := p.resolveCatalogProbe()
	if probe == nil {
		return spec, false
	}
	credEnvVar := builtinProviderCredentialEnv[agg]
	key := spec.Env[credEnvVar]
	if key == "" {
		key = pickAPIKey(ep.Env)
	}
	if key == "" {
		key = spec.Env[PiKeyEnvVar]
	}
	timeout := p.opts.CatalogProbeTimeout
	if timeout == 0 {
		timeout = DefaultCatalogProbeTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := probe(pctx, p.binary, agg, spec.Model, credEnvVar, key)
	if err != nil {
		slog.Warn("pi aggregator catalog probe failed; using the injected provider", "provider", agg, "model", spec.Model, "error", err)
		return spec, false
	}
	if !catalogHasModel(raw, agg, spec.Model) {
		return spec, false
	}
	env := make(map[string]string)
	for k, v := range spec.Env {
		env[k] = v
	}
	if _, already := env[credEnvVar]; !already && key != "" {
		env[credEnvVar] = key
	}
	spec.Env = env
	spec.Model = agg + "/" + spec.Model
	return spec, true
}
