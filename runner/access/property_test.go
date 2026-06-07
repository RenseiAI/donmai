package access

import (
	"math/rand"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// The property tests are the security backbone of P3 (P3-SPEC §2.3). They run
// over a REPRODUCIBLE random corpus: the RNG is seeded with a fixed constant so
// the corpus is identical on every run and under -race. It is NEVER seeded from
// wall-clock time or OS entropy.
const propertySeed int64 = 0x50334E41 // "P3NA" — fixed; deterministic corpus.

// propertyCorpusSize is the number of random vectors each property is checked
// over. Large enough to exercise adversarial broadening, deny-all, and pin
// combinations; small enough to stay fast under -race.
const propertyCorpusSize = 4000

// seededRNG builds the deterministic corpus generator. A FIXED-SEED math/rand is
// intentional and required by P3-SPEC §3.2 ("seed the RNG with a fixed constant
// (deterministic), do not seed from wall-clock time or OS entropy"). gosec G404
// (weak RNG) does not apply: this is test-corpus generation, not security
// randomness — using crypto/rand here would destroy the reproducibility the
// security proof depends on.
func seededRNG(offset int64) *rand.Rand {
	//nolint:gosec // G404: deterministic fixed-seed corpus is a hard requirement (P3-SPEC §3.2).
	return rand.New(rand.NewSource(propertySeed + offset))
}

// canonicalModes is the full canonical auth-mode set used to build random
// ceilings and adversarial machine blocks.
var canonicalModes = []AuthMode{
	agent.AuthBYOK, agent.AuthMetered, agent.AuthShared, agent.AuthHostSession, agent.AuthLocal,
}

// randVector is one randomly-generated test case.
type randVector struct {
	ceiling   map[AuthMode]bool
	machine   *ModelAccessConfig
	workload  string
	company   string
	model     string
	requested AuthMode
}

// genVector builds an adversarial random vector. The machine block is built from
// the FULL canonical mode universe (not just the ceiling) and freely asserts
// allowed:true on modes ABSENT from the ceiling, plus host/model pins — so the
// corpus actively tries to broaden. genVector is pure over its *rand.Rand, so a
// fixed seed makes the corpus fully reproducible.
func genVector(rng *rand.Rand) randVector {
	companies := []string{"anthropic", "openai", "google"}
	models := []string{"claude-sonnet", "claude-haiku", "gpt-5", "gemini-pro", ""}
	workloads := []string{"", "kg-extraction", "interview", "sdlc", "unknown-workload"}
	hosts := []string{"", "oauth-cli", "vertex", "bedrock", "direct"}

	// Random non-empty-ish ceiling (a closed, already-evaluated set).
	ceiling := map[AuthMode]bool{}
	for _, m := range canonicalModes {
		if rng.Intn(2) == 0 {
			ceiling[m] = true
		}
	}
	// Occasionally an empty ceiling (P-EMPTY style fail-closed coverage).
	if rng.Intn(8) != 0 && len(ceiling) == 0 {
		ceiling[canonicalModes[rng.Intn(len(canonicalModes))]] = true
	}

	company := companies[rng.Intn(len(companies))]
	model := models[rng.Intn(len(models))]
	workload := workloads[rng.Intn(len(workloads))]
	requested := canonicalModes[rng.Intn(len(canonicalModes))]

	// 1-in-12 vectors use a nil machine block (identity coverage in-corpus).
	var machine *ModelAccessConfig
	if rng.Intn(12) != 0 {
		machine = genMachine(rng, company, model, hosts)
	}

	return randVector{
		ceiling: ceiling, machine: machine, workload: workload,
		company: company, model: model, requested: requested,
	}
}

// genMachine builds an adversarial ModelAccessConfig: random allowed/deny cells
// across model/provider/'*' rows, with random host/model pins. It deliberately
// sets allowed:true on modes that may be outside the ceiling and may loosen pins.
func genMachine(rng *rand.Rand, company, model string, hosts []string) *ModelAccessConfig {
	buildPolicy := func() AccessPolicy {
		matrix := map[string]map[string]AccessCostCell{}
		keys := []string{company, model, "*", "openai"} // mix of provider/model/wildcard/other
		for _, key := range keys {
			if key == "" || rng.Intn(2) == 0 {
				continue
			}
			row := map[string]AccessCostCell{}
			for _, m := range canonicalModes {
				if rng.Intn(2) == 0 {
					continue
				}
				row[string(m)] = AccessCostCell{
					Allowed: rng.Intn(2) == 0, // freely true/false, including broadening
					Host:    hosts[rng.Intn(len(hosts))],
					Model:   pickModelPin(rng),
				}
			}
			if len(row) > 0 {
				matrix[key] = row
			}
		}
		pol := AccessPolicy{Matrix: matrix}
		if rng.Intn(2) == 0 {
			// Random AuthOrder (a per-workload ladder permutation).
			pol.AuthOrder = shuffledModes(rng)
		}
		return pol
	}

	cfg := &ModelAccessConfig{Default: buildPolicy()}
	if rng.Intn(2) == 0 {
		cfg.Workloads = map[string]AccessPolicy{
			"kg-extraction": buildPolicy(),
			"interview":     buildPolicy(),
		}
	}
	return cfg
}

func pickModelPin(rng *rand.Rand) string {
	pins := []string{"", "claude-haiku", "gpt-5-mini", "gemini-flash"}
	return pins[rng.Intn(len(pins))]
}

func shuffledModes(rng *rand.Rand) []AuthMode {
	out := append([]AuthMode(nil), canonicalModes...)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// removeCell returns a deep copy of machine with the (key, mode) cell removed
// from every policy block (default + workloads). Used by P-DROP-BROADENING to
// prove a broadening cell is a no-op vs. its own absence.
func removeCell(machine *ModelAccessConfig, key string, mode AuthMode) *ModelAccessConfig {
	if machine == nil {
		return nil
	}
	stripPolicy := func(p AccessPolicy) AccessPolicy {
		nm := map[string]map[string]AccessCostCell{}
		for k, row := range p.Matrix {
			nr := map[string]AccessCostCell{}
			for am, cell := range row {
				if k == key && am == string(mode) {
					continue // drop the targeted cell
				}
				nr[am] = cell
			}
			if len(nr) > 0 {
				nm[k] = nr
			}
		}
		return AccessPolicy{Matrix: nm, AuthOrder: p.AuthOrder}
	}
	out := &ModelAccessConfig{Default: stripPolicy(machine.Default)}
	if machine.Workloads != nil {
		out.Workloads = map[string]AccessPolicy{}
		for w, p := range machine.Workloads {
			out.Workloads[w] = stripPolicy(p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// P-SUBSET — the core security property. For ANY ceiling and ANY (adversarial)
// machine block, the effective set is a subset of the ceiling AND any returned
// pick lives inside the ceiling. NO machine block can produce a cell outside
// platformAllowed.
// ---------------------------------------------------------------------------

func TestProperty_Subset(t *testing.T) {
	rng := seededRNG(0)
	for i := 0; i < propertyCorpusSize; i++ {
		v := genVector(rng)

		eff := EffectiveSet(v.machine, v.workload, v.company, v.model, v.ceiling)
		for m := range eff {
			if !v.ceiling[m] {
				t.Fatalf("P-SUBSET violated (i=%d): effective mode %q ∉ ceiling %v; machine=%+v",
					i, m, keysOf(v.ceiling), v.machine)
			}
		}

		got, err := ResolveMachineCell(v.machine, v.workload, v.company, v.model, v.requested, v.ceiling)
		if err != nil {
			continue // deny is allowed; it can never broaden.
		}
		if !v.ceiling[got.AuthMode] {
			t.Fatalf("P-SUBSET violated (i=%d): picked authMode %q ∉ ceiling %v",
				i, got.AuthMode, keysOf(v.ceiling))
		}
	}
}

// ---------------------------------------------------------------------------
// P-DROP-BROADENING — a machine cell granting an unallowed mode (allowed:true for
// a mode ∉ ceiling) is dropped: the result is identical to the same block with
// that cell removed. Directly exercises G2/G3.
// ---------------------------------------------------------------------------

func TestProperty_DropBroadening(t *testing.T) {
	rng := seededRNG(1)
	checked := 0
	for i := 0; i < propertyCorpusSize; i++ {
		v := genVector(rng)
		if v.machine == nil {
			continue
		}
		// Find a cell in the default block that broadens: allowed:true for a mode
		// absent from the ceiling, on a key that resolves for (model, company).
		for key, row := range v.machine.Default.Matrix {
			for am, cell := range row {
				mode := AuthMode(am)
				if !cell.Allowed || v.ceiling[mode] {
					continue
				}
				// Only meaningful if this key participates in resolution.
				if key != v.company && key != v.model && key != "*" {
					continue
				}
				stripped := removeCell(v.machine, key, mode)

				effFull := EffectiveSet(v.machine, v.workload, v.company, v.model, v.ceiling)
				effStrip := EffectiveSet(stripped, v.workload, v.company, v.model, v.ceiling)
				if !sameSet(effFull, effStrip) {
					t.Fatalf("P-DROP-BROADENING violated (i=%d): cell %s.%s broadened the effective set\nfull=%v stripped=%v",
						i, key, am, keysOf(effFull), keysOf(effStrip))
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("P-DROP-BROADENING exercised zero broadening cells — corpus generation is broken")
	}
	t.Logf("P-DROP-BROADENING: verified %d broadening cells were no-ops", checked)
}

// ---------------------------------------------------------------------------
// P-NIL-IDENTITY — ResolveMachineCell(nil, ...) returns `requested` unchanged
// (no error) iff requested ∈ ceiling; otherwise it denies or falls to the ladder
// over the ceiling. A nil block is the ceiling identity.
// ---------------------------------------------------------------------------

func TestProperty_NilIdentity(t *testing.T) {
	rng := seededRNG(2)
	for i := 0; i < propertyCorpusSize; i++ {
		v := genVector(rng)

		got, err := ResolveMachineCell(nil, v.workload, v.company, v.model, v.requested, v.ceiling)

		if len(v.ceiling) == 0 {
			if err == nil {
				t.Fatalf("P-NIL-IDENTITY (i=%d): empty ceiling must deny, got cell %+v", i, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("P-NIL-IDENTITY (i=%d): non-empty ceiling must not deny under nil block, got err %v", i, err)
		}
		// nil block performs no narrowing ⇒ effective == ceiling exactly.
		eff := EffectiveSet(nil, v.workload, v.company, v.model, v.ceiling)
		if !sameSet(eff, v.ceiling) {
			t.Fatalf("P-NIL-IDENTITY (i=%d): nil block changed the set: eff=%v ceiling=%v",
				i, keysOf(eff), keysOf(v.ceiling))
		}
		if v.ceiling[v.requested] && got.AuthMode != v.requested {
			t.Fatalf("P-NIL-IDENTITY (i=%d): requested %q ∈ ceiling but pick was %q",
				i, v.requested, got.AuthMode)
		}
		// nil block ⇒ no pin; model passes through.
		if got.Host != "" {
			t.Fatalf("P-NIL-IDENTITY (i=%d): nil block produced a host pin %q", i, got.Host)
		}
		if got.Model != v.model {
			t.Fatalf("P-NIL-IDENTITY (i=%d): nil block changed model %q→%q", i, v.model, got.Model)
		}
		if !v.ceiling[got.AuthMode] {
			t.Fatalf("P-NIL-IDENTITY (i=%d): pick %q ∉ ceiling", i, got.AuthMode)
		}
	}
}

// ---------------------------------------------------------------------------
// P-EMPTY-DENIES — a block that denies every mode in the ceiling returns
// AccessDeniedError, never a fallback to an unallowed mode.
// ---------------------------------------------------------------------------

func TestProperty_EmptyDenies(t *testing.T) {
	rng := seededRNG(3)
	for i := 0; i < propertyCorpusSize; i++ {
		v := genVector(rng)
		if len(v.ceiling) == 0 {
			continue // covered structurally; build a non-trivial deny-all below.
		}
		// Build a deny-all block: allowed:false for every canonical mode on the
		// wildcard row. This denies the entire ceiling regardless of company/model.
		denyRow := map[string]AccessCostCell{}
		for _, m := range canonicalModes {
			denyRow[string(m)] = AccessCostCell{Allowed: false}
		}
		denyAll := &ModelAccessConfig{
			Default: AccessPolicy{Matrix: map[string]map[string]AccessCostCell{"*": denyRow}},
		}

		eff := EffectiveSet(denyAll, "", v.company, v.model, v.ceiling)
		if len(eff) != 0 {
			t.Fatalf("P-EMPTY-DENIES (i=%d): deny-all left %v in the effective set", i, keysOf(eff))
		}
		_, err := ResolveMachineCell(denyAll, "", v.company, v.model, v.requested, v.ceiling)
		if _, ok := err.(*AccessDeniedError); !ok {
			t.Fatalf("P-EMPTY-DENIES (i=%d): deny-all must return AccessDeniedError, got %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// P-IDEMPOTENT — re-running the gate on its own output cell is a no-op: it
// agrees-or-further-restricts, never conflicts. Feeding the resolved (model, pin)
// back through with a ceiling collapsed to the picked mode yields the same pick.
// ---------------------------------------------------------------------------

func TestProperty_Idempotent(t *testing.T) {
	rng := seededRNG(4)
	checked := 0
	for i := 0; i < propertyCorpusSize; i++ {
		v := genVector(rng)
		got, err := ResolveMachineCell(v.machine, v.workload, v.company, v.model, v.requested, v.ceiling)
		if err != nil {
			continue
		}
		// (a) Pure determinism: the SAME inputs must yield the SAME output.
		again, errAgain := ResolveMachineCell(v.machine, v.workload, v.company, v.model, v.requested, v.ceiling)
		if errAgain != nil {
			t.Fatalf("P-IDEMPOTENT (i=%d): re-run with identical inputs errored: %v", i, errAgain)
		}
		if again != got {
			t.Fatalf("P-IDEMPOTENT (i=%d): non-deterministic: first=%+v second=%+v", i, got, again)
		}

		// (b) No-conflict: feeding the gate's OWN picked mode back as `requested`
		// (same model) must agree — never flip an allowed pick to denied, never
		// pick a different mode. The picked mode survived, so requested honors it.
		got2, err2 := ResolveMachineCell(v.machine, v.workload, v.company, v.model, got.AuthMode, v.ceiling)
		if err2 != nil {
			t.Fatalf("P-IDEMPOTENT (i=%d): re-run denied a previously-allowed cell: first=%+v err=%v",
				i, got, err2)
		}
		if got2.AuthMode != got.AuthMode {
			t.Fatalf("P-IDEMPOTENT (i=%d): re-run changed authMode %q→%q",
				i, got.AuthMode, got2.AuthMode)
		}
		// The re-pick must stay inside the original effective set (never broaden).
		origEff := EffectiveSet(v.machine, v.workload, v.company, v.model, v.ceiling)
		if !origEff[got2.AuthMode] {
			t.Fatalf("P-IDEMPOTENT (i=%d): re-pick %q escaped the original effective set %v",
				i, got2.AuthMode, keysOf(origEff))
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("P-IDEMPOTENT exercised zero allowed cells — corpus generation is broken")
	}
}

// ---------------------------------------------------------------------------
// P-PIN-TIGHTENS — any Host/Model pin in the result is == or strictly more
// specific than the input. A pin never widens, and the result's model is never
// broader than the resolved input model. (The input model has no "broader"
// notion here beyond identity; we assert the result model is either the input or
// a machine-declared pin, never a value invented by the gate.)
// ---------------------------------------------------------------------------

func TestProperty_PinTightens(t *testing.T) {
	rng := seededRNG(5)
	for i := 0; i < propertyCorpusSize; i++ {
		v := genVector(rng)
		got, err := ResolveMachineCell(v.machine, v.workload, v.company, v.model, v.requested, v.ceiling)
		if err != nil {
			continue
		}
		// The result model is either the platform-resolved model (pass-through) or
		// a value declared by the surviving machine cell — never fabricated.
		if got.Model != v.model {
			if !pinDeclared(v.machine, v.workload, v.company, v.model, got.AuthMode, got.Model, false) {
				t.Fatalf("P-PIN-TIGHTENS (i=%d): result model %q neither input %q nor a declared machine pin",
					i, got.Model, v.model)
			}
		}
		// The host pin, if present, must be a value the surviving machine cell
		// declared — the gate never invents a host.
		if got.Host != "" {
			if !pinDeclared(v.machine, v.workload, v.company, v.model, got.AuthMode, got.Host, true) {
				t.Fatalf("P-PIN-TIGHTENS (i=%d): result host %q is not a declared machine pin", i, got.Host)
			}
		}
		// nil machine ⇒ no host pin ever.
		if v.machine == nil && got.Host != "" {
			t.Fatalf("P-PIN-TIGHTENS (i=%d): nil machine produced host pin %q", i, got.Host)
		}
	}
}

// pinDeclared reports whether `value` is the Host (host=true) or Model
// (host=false) the surviving machine cell declares for (model, company, mode) via
// the same model→provider→'*' resolution order. Proves the gate only ever
// surfaces operator-declared pins.
func pinDeclared(machine *ModelAccessConfig, workload, company, model string, mode AuthMode, value string, host bool) bool {
	if machine == nil {
		return false
	}
	pol, _ := selectPolicy(machine, workload)
	cell := resolveAccessCell(pol.Matrix, model, company, mode)
	if cell == nil {
		return false
	}
	if host {
		return cell.Host == value
	}
	return cell.Model == value
}

// --- small helpers ---------------------------------------------------------

func keysOf(set map[AuthMode]bool) []string {
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, string(m))
	}
	return out
}

func sameSet(a, b map[AuthMode]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for m := range a {
		if !b[m] {
			return false
		}
	}
	return true
}
