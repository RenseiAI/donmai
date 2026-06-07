// Package access is the pure, DB-free OSS enforcement mirror of the platform's
// narrow-only model-access semantics
// (platform/src/lib/billing/access-policy.ts). It applies per-machine and
// per-workload narrowing to a cell the platform already resolved + narrowed
// against org∩project.
//
// NARROW-ONLY, FAIL-CLOSED. The single load-bearing invariant
// (ADR-2026-06-06 D5, mirrored byte-for-byte in donmai-architecture and
// rensei-architecture):
//
//	For any machine M, workload W, and cell (company, model, authMode):
//	  effective(M, W) = platformAllowed(org, project) ∩ machineAllowed(M, W)
//
// where ∩ is set intersection and platformAllowed is the immutable ceiling — a
// closed, already-evaluated set, NOT a raw matrix. A machine block can produce a
// result ⊆ platformAllowed and never ⊋ platformAllowed. The worst a buggy,
// stale, or malicious daemon block can do is shrink the set to ∅ (over-restrict,
// degrading availability); it can never grant a cell the org/project policy did
// not already allow. A nil machine block ⇒ effective = platformAllowed.
//
// This package has NO I/O, NO DB, NO platform SDK, and no heavy imports. It
// imports the donmai/agent package solely for the shared AuthMode value type
// (the canonical OSS-mirrored 5-mode set). It is consumed by the daemon spawn
// path one step BEFORE the credential hop; on deny the caller NACKs the claim
// and never reaches key injection.
package access

import (
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// AuthMode re-exports the canonical OSS-mirrored auth mode type so callers can
// refer to access.AuthMode without an extra import. It is identical to
// agent.AuthMode (the canonical 5-mode set: byok, metered, shared, host-session,
// local).
type AuthMode = agent.AuthMode

// AuthModePreferenceOrder mirrors AUTH_MODE_PREFERENCE_ORDER in
// access-policy.ts:148-154 — the canonical immutable auth-mode preference ladder.
// Reordering is a behavioral migration; new modes are appended. Used as the
// default pick ladder when a workload declares no AuthOrder.
var AuthModePreferenceOrder = []AuthMode{
	agent.AuthBYOK,
	agent.AuthMetered,
	agent.AuthShared,
	agent.AuthHostSession,
	agent.AuthLocal,
}

// ModelAccessConfig is the daemon.yaml block the platform syncs. Field-aligned
// with the platform access_policy JSONB shape. nil = no machine narrowing.
type ModelAccessConfig struct {
	Default   AccessPolicy            `yaml:"default"             json:"default"`
	Workloads map[string]AccessPolicy `yaml:"workloads,omitempty" json:"workloads,omitempty"`
}

// AccessPolicy mirrors the platform AccessPolicy JSONB shape verbatim
// (access-policy.ts OrgAccessPolicy/AccessMatrix). matrix[company|model][authMode]
// = AccessCostCell. AuthOrder is the per-workload pick ladder
// (replaces the KG_PREFERENCE_ORDER constant); empty ⇒ AuthModePreferenceOrder.
//
//nolint:revive // Name is the P3-SPEC §1.3 contract name, byte-aligned with the platform access_policy JSONB (AccessPolicy); renaming would break the documented mirror.
type AccessPolicy struct {
	Matrix    map[string]map[string]AccessCostCell `yaml:"matrix"              json:"matrix"`
	AuthOrder []AuthMode                           `yaml:"authOrder,omitempty" json:"authOrder,omitempty"`
}

// AccessCostCell mirrors access-policy.ts AccessCostCell. The ENFORCEMENT reader
// only ever reads Allowed (+ the Host/Model narrowing pins, D4). Cost fields ride
// for audit parity but are NOT load-bearing on the enforcement path.
//
//nolint:revive // Name is the P3-SPEC §1.3 contract name, byte-aligned with the platform AccessCostCell type; renaming would break the documented mirror.
type AccessCostCell struct {
	Allowed bool   `yaml:"allowed"           json:"allowed"`
	Host    string `yaml:"host,omitempty"    json:"host,omitempty"`    // narrowing pin (oauth-cli|vertex|...)
	Model   string `yaml:"model,omitempty"   json:"model,omitempty"`   // narrowing pin (claude-haiku|...)
	Enforce string `yaml:"enforce,omitempty" json:"enforce,omitempty"` // cost; carried, not enforced here
}

// NarrowedCell is the result of a successful ResolveMachineCell. AuthMode is
// always ∈ platformAllowed. Host/Model may be PINNED TIGHTER by the surviving
// machine cell; they are never broadened.
type NarrowedCell struct {
	AuthMode AuthMode // == requested if it survived, else a further-restricted pick from the effective set
	Host     string   // pinned tighter by the machine cell (e.g. "vertex"); never broadened
	Model    string   // pinned tighter by the machine cell (e.g. "claude-haiku"); never broadened
}

// AccessDeniedError mirrors resolve-model.ts:92 AccessDeniedError fail-closed
// semantics: (platformAllowed ∩ machineAllowed) == ∅ for this company/model ⇒
// deny. It carries the cell identity for the NACK log and carries NO credential
// and NO key material.
//
//nolint:revive // Name mirrors resolve-model.ts:92 AccessDeniedError (P3-SPEC §1.2); the matching name is the documented contract, intentional over the non-stuttering DeniedError.
type AccessDeniedError struct {
	Company  string
	Model    string
	AuthMode AuthMode
	Workload string
	Scope    string // "machine" | "machine-workload"
}

func (e *AccessDeniedError) Error() string {
	return fmt.Sprintf(
		"access denied: %s policy denies (company=%s × model=%s × authMode=%s, workload=%q)",
		e.Scope, e.Company, e.Model, e.AuthMode, e.Workload,
	)
}

// isCellAllowed mirrors isCellAllowed (access-policy.ts:206-209): a nil cell
// (absent rule) inherits ⇒ allow; an explicit allowed:false ⇒ deny; any other
// present cell ⇒ allow.
//
// Note: because Go AccessCostCell is a value type, an absent rule is modeled by
// passing a nil *AccessCostCell (the pointer is the "present?" signal), matching
// the TS null/undefined "inherit" sentinel exactly.
func isCellAllowed(cell *AccessCostCell) bool {
	if cell == nil {
		return true // absent = inherit = allow
	}
	return cell.Allowed
}

// resolveAccessCell mirrors resolveAccessCell (access-policy.ts:233-266): single-
// scope cell resolution using the exact model → provider(company) → '*' → nil
// resolution order. A returned nil means "no rule applies at any level" — the
// caller interprets that as inherit (allow).
//
// The platform's resolveAccessCell takes (modelId, providerId) as separate axes;
// here the matrix is keyed by company|model, so model is the most-specific key
// and company is the provider-level key. This is faithful to the platform shape
// where matrix["claude-sonnet"] (model row) is tried before matrix["anthropic"]
// (provider row) before matrix["*"] (wildcard).
func resolveAccessCell(matrix map[string]map[string]AccessCostCell, model, company string, mode AuthMode) *AccessCostCell {
	if matrix == nil {
		return nil
	}
	key := string(mode)

	// 1. Exact model match.
	if model != "" {
		if row, ok := matrix[model]; ok {
			if cell, ok := row[key]; ok {
				return &cell
			}
		}
	}

	// 2. Provider-level (company) match.
	if row, ok := matrix[company]; ok {
		if cell, ok := row[key]; ok {
			return &cell
		}
	}

	// 3. Wildcard match.
	if row, ok := matrix["*"]; ok {
		if cell, ok := row[key]; ok {
			return &cell
		}
	}

	// 4. Absent at every level — inherit from the (closed) ceiling.
	return nil
}

// selectPolicy chooses the workload policy block, falling back to Default.
// Mirrors 02 §4.2:777-780 — modelAccess.set writes Default vs Workloads[workload].
// An empty workload or an unknown workload key uses Default (the broadest, safest
// narrowing baseline). It also reports the enforcement scope for the NACK log.
func selectPolicy(machine *ModelAccessConfig, workload string) (AccessPolicy, string) {
	if workload != "" {
		if pol, ok := machine.Workloads[workload]; ok {
			return pol, "machine-workload"
		}
	}
	return machine.Default, "machine"
}

// ResolveMachineCell applies per-machine + per-workload narrowing to a cell the
// platform already resolved + narrowed against org∩project. NARROW-ONLY: it may
// only SUBTRACT from platformAllowed, never add. Fail-closed.
//
//	machine          synced daemon.yaml block (config.ModelAccessConfig); nil = no machine narrowing.
//	workload         e.g. "kg-extraction"; "" => the Default block.
//	company          endpoint company key, e.g. "anthropic" (the cell axis key — D4: company|model).
//	model            resolved model id (for the model > provider > '*' wildcard resolution order).
//	requested        the platform's selectAuthMode() pick (resolve-model.ts:142).
//	platformAllowed  the CLOSED set the platform already narrowed (org∩project) — the immutable CEILING.
//
// On success returns a NarrowedCell whose AuthMode ∈ platformAllowed. On an empty
// intersection returns *AccessDeniedError (carrying cell identity, never a key).
func ResolveMachineCell(
	machine *ModelAccessConfig,
	workload string,
	company string,
	model string,
	requested AuthMode,
	platformAllowed map[AuthMode]bool,
) (NarrowedCell, error) {
	// --- nil machine block ⇒ identity against the ceiling (§2.2). ---------------
	// No narrowing: the ceiling still holds. Honor `requested` iff it survived the
	// ceiling; otherwise fall to the ladder over the ceiling. A nil block can
	// never broaden, so this stays ⊆ platformAllowed by construction.
	if machine == nil {
		mode, ok := pickMode(platformAllowed, requested, nil)
		if !ok {
			return NarrowedCell{}, &AccessDeniedError{
				Company: company, Model: model, AuthMode: requested,
				Workload: workload, Scope: "machine",
			}
		}
		// nil block ⇒ no machine cell ⇒ no pin (Host/Model unset, model passes through).
		return NarrowedCell{AuthMode: mode, Host: "", Model: model}, nil
	}

	pol, scope := selectPolicy(machine, workload)

	// --- G1: INTERSECTION, NEVER UNION (mirrors resolveEffectiveAllowedModes
	// loop, access-policy.ts:458-466). Start from the CLOSED ceiling and only
	// ever delete. There is no path below that inserts a key into `final`. -------
	final := make(map[AuthMode]bool, len(platformAllowed))
	for mode := range platformAllowed {
		final[mode] = true
	}
	for mode := range final {
		// G2: the machine cell is consulted ONLY to deny. isCellAllowed==false
		// (explicit allowed:false) ⇒ delete. A machine cell asserting allowed:true
		// for a mode is never read to ADD — the mode is already in `final` (it came
		// from the ceiling) or it isn't in the ceiling at all (so it was never a
		// candidate). The cell's allowed:true is structurally never able to broaden.
		cell := resolveAccessCell(pol.Matrix, model, company, mode)
		if !isCellAllowed(cell) {
			delete(final, mode)
		}
	}
	// G3: `platformAllowed` was received as an already-evaluated CLOSED set, not a
	// raw parent matrix. The machine block is never merged against a silent
	// parent, so the mergeMatrices inherit-all broadening branch
	// (access-policy.ts:365-369) is structurally unreachable at this tier.

	if len(final) == 0 {
		return NarrowedCell{}, &AccessDeniedError{
			Company: company, Model: model, AuthMode: requested,
			Workload: workload, Scope: scope,
		}
	}

	// --- Pick the surviving AuthMode + read the (tighter-or-equal) pins. --------
	mode, ok := pickMode(final, requested, pol.AuthOrder)
	if !ok {
		// Unreachable in practice (final is non-empty), but fail-closed regardless.
		return NarrowedCell{}, &AccessDeniedError{
			Company: company, Model: model, AuthMode: requested,
			Workload: workload, Scope: scope,
		}
	}

	host, pinModel := readPins(pol.Matrix, model, company, mode)
	return NarrowedCell{AuthMode: mode, Host: host, Model: pinModel}, nil
}

// pickMode selects a concrete AuthMode from the surviving set `allowed`.
// Preference: honor `requested` if it survived; otherwise the first surviving
// mode in `ladder` (the per-workload AuthOrder), falling back to
// AuthModePreferenceOrder. This can only further-restrict — it never reaches
// outside `allowed`. Returns (mode, true) on success; (_, false) only when
// `allowed` is empty.
func pickMode(allowed map[AuthMode]bool, requested AuthMode, ladder []AuthMode) (AuthMode, bool) {
	if len(allowed) == 0 {
		return "", false
	}
	if requested != "" && allowed[requested] {
		return requested, true
	}
	order := ladder
	if len(order) == 0 {
		order = AuthModePreferenceOrder
	}
	for _, m := range order {
		if allowed[m] {
			return m, true
		}
	}
	// Defensive: a surviving mode not present in either ladder (e.g. an
	// out-of-band custom mode in the ceiling). Pick any — still ⊆ allowed, so the
	// invariant holds. This branch is not exercised by the canonical 5-mode set.
	for m := range allowed {
		return m, true
	}
	return "", false
}

// readPins reads the Host/Model narrowing pins off the surviving machine cell for
// the chosen mode, using the same model → provider → '*' resolution order. A nil
// cell (no machine rule) yields no pin; the model passes through unchanged. Pins
// only narrow (D4 / P-PIN-TIGHTENS) — the function never invents a broader value.
func readPins(matrix map[string]map[string]AccessCostCell, model, company string, mode AuthMode) (host, pinModel string) {
	cell := resolveAccessCell(matrix, model, company, mode)
	pinModel = model // default: the platform-resolved model passes through.
	if cell == nil {
		return "", pinModel
	}
	if cell.Host != "" {
		host = cell.Host
	}
	if cell.Model != "" {
		pinModel = cell.Model // tighter-or-equal pin.
	}
	return host, pinModel
}

// EffectiveSet computes the full effective auth-mode set for a (machine, workload,
// company, model) tuple given the closed ceiling: platformAllowed ∩
// machineAllowed. It is the same delete-only intersection ResolveMachineCell uses
// internally, exposed for the property/parity tests (P-SUBSET) and for callers
// that need the set rather than a single pick. A nil machine ⇒ a copy of the
// ceiling (identity). The result is always ⊆ platformAllowed.
func EffectiveSet(
	machine *ModelAccessConfig,
	workload string,
	company string,
	model string,
	platformAllowed map[AuthMode]bool,
) map[AuthMode]bool {
	final := make(map[AuthMode]bool, len(platformAllowed))
	for mode := range platformAllowed {
		final[mode] = true
	}
	if machine == nil {
		return final
	}
	pol, _ := selectPolicy(machine, workload)
	for mode := range final {
		cell := resolveAccessCell(pol.Matrix, model, company, mode)
		if !isCellAllowed(cell) {
			delete(final, mode)
		}
	}
	return final
}
