package pi

import (
	"context"
	"fmt"
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
func defaultCatalogProbe(ctx context.Context, binary, provider, model, credEnvVar, credEnvValue string) (string, error) {
	// nolint:gosec // G204: binary is the resolved-from-PATH path New() also
	// uses to exec `pi --mode rpc`; --list-models is a read-only query, and
	// provider/model are this package's own already-classified strings
	// (builtin_providers.go's allowlist + the pin split), never raw
	// caller-supplied shell text.
	cmd := exec.CommandContext(ctx, binary, "--list-models", provider+"/"+model, "--offline")
	if credEnvVar != "" && credEnvValue != "" {
		cmd.Env = append(os.Environ(), credEnvVar+"="+credEnvValue)
	}
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
		// Unverifiable, not fatal — see doc comment (DEC-2 precedent).
		return nil
	}
	if catalogHasModel(raw, provider, model) {
		return nil
	}
	return fmt.Errorf("%w: pi has no built-in model %q for provider %q in its catalog (checked with `pi --list-models %s/%s`); the resolved pin would 400 on its first turn",
		agent.ErrSpawnFailed, model, provider, provider, model)
}
