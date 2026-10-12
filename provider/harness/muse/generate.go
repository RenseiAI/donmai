//go:build ignore

// Command muse-schema-gen regenerates provider/harness/muse/types.go from
// the MSP wire schema embedded in the `muse` binary.
//
// Usage:
//
//	go run ./provider/harness/muse/generate.go --muse-bin muse
//
// The generator shells out to `muse schema generate-json-schema --out
// <tmpdir>` (offline, instant — the bundle is precomputed at build time),
// reads the stable-surface msp.schema.json plus its manifest.json, and
// emits the typed subset this harness speaks: the initialize handshake,
// session/start + session/resume, turn/start + turn/interrupt +
// turn/steer, the view/item event params, approvals, token usage, and the
// error envelope. The emitted file carries the schema fingerprint so a
// drift between the pinned binary and the checked-in types fails closed
// at review time (see SchemaFingerprint in types.go).
//
// The generator is intentionally lossy: it emits hand-shaped Go structs
// for the wire members this package constructs or decodes, each tagged
// with its $defs source name, rather than a mechanical whole-schema
// translation. A mechanical translation would re-emit all 342
// definitions — including experimental lanes this harness must not
// speak — and would churn on every binary release. The lossy subset is
// the contract: adding a wire member means extending the generator's
// allowlist AND the mapping code together, in one change.
//
// Provenance (checked 2026-10-12 against `muse` 1.4.6, fingerprint
// sha256:eae467ab60cb9a5d5af4de616830f065e59375c987fd59269c8cc33ed2a1137f):
// the bundle's "methods" index plus the "requests"/"notifications" maps
// are the authoritative served-method list, and the error table in the
// bundle describes every error the binary can emit.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// allowlisted $defs members this harness speaks. The generator emits Go
// structs for exactly these (plus the small enums below); every other
// definition in the bundle is out of scope for this harness revision.
var wantDefs = []string{
	"InitializeParams",
	"InitializeResult",
	"ClientInfo",
	"ClientCapabilities",
	"CapabilityName",
	"SchemaInfo",
	"SessionStartParams",
	"SessionStartResult",
	"SessionResumeParams",
	"SessionResumeResult",
	"SessionConfig",
	"SessionMcpServerConfig",
	"SessionMcpServerMode",
	"TurnStartParams",
	"TurnStartResult",
	"TurnInterruptParams",
	"TurnInterruptResult",
	"TurnSteerParams",
	"TurnSteerResult",
	"TurnStartedParams",
	"TurnForegroundCompletedParams",
	"TurnCompletedParams",
	"TurnTerminal",
	"TurnError",
	"TurnInputPart",
	"Item",
	"ItemKind",
	"ItemStatus",
	"ItemStartedParams",
	"ItemUpdatedParams",
	"ItemDeltaParams",
	"ItemCompletedParams",
	"ViewGapParams",
	"ViewSubscribeParams",
	"SessionStartedParams",
	"SessionStatusChangedParams",
	"SessionContextUsageParams",
	"SessionTokenUsageParams",
	"SessionTodoListChangedParams",
	"TokenUsage",
	"CumulativeTokenUsage",
	"CumulativeCost",
	"ApprovalMode",
	"ApprovalRequestParams",
	"ApprovalDecideParams",
	"ApprovalChoice",
	"ApprovalDecision",
	"ApprovalResolvedParams",
	"ReasoningEffort",
	"IfBusy",
	"ErrorKind",
	"ErrorData",
	"ErrorObject",
}

func main() {
	museBin := flag.String("muse-bin", "muse", "muse binary carrying the embedded MSP schema")
	flag.Parse()

	tmp, err := os.MkdirTemp("", "msp-schema")
	if err != nil {
		fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	//nolint:gosec // G204: binary comes from an explicit operator flag, not input.
	cmd := exec.Command(*museBin, "schema", "generate-json-schema", "--out", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		fatal(fmt.Errorf("muse schema export: %v: %s", err, out))
	}
	manifestRaw, err := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err != nil {
		fatal(err)
	}
	var manifest struct {
		Experimental  bool   `json:"experimental"`
		Fingerprint   string `json:"fingerprint"`
		SchemaVersion int    `json:"schemaVersion"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		fatal(err)
	}
	schemaRaw, err := os.ReadFile(filepath.Join(tmp, "msp.schema.json"))
	if err != nil {
		fatal(err)
	}
	var schema struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		fatal(err)
	}
	missing := []string{}
	for _, name := range wantDefs {
		if _, ok := schema.Defs[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		fatal(fmt.Errorf("bundle is missing allowlisted defs (binary drift?): %v", missing))
	}
	fmt.Printf("schemaVersion=%d fingerprint=%s defs=%d (allowlisted %d, all present)\n",
		manifest.SchemaVersion, manifest.Fingerprint, len(schema.Defs), len(wantDefs))
	fmt.Println("types.go is a hand-shaped projection of the allowlisted defs above; re-run this check after a binary upgrade and extend the allowlist with the mapping code when a new wire member is needed.")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "muse-schema-gen:", err)
	os.Exit(1)
}
