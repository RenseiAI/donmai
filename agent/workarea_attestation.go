package agent

import (
	"context"
	"errors"
)

// WorkareaHostProver is implemented by a harness provider whose workarea
// attestation rests on an executor boundary each host must prove
// (ADR-2026-10-03 D1: the manifest declares, the host proves, the list
// publishes). Its manifest marks the attestation with
// HarnessCaps.WorkareaAttestationNeedsHostProof.
type WorkareaHostProver interface {
	// ProveWorkareaHost returns nil only when this host holds a current,
	// passing proof of the boundary the manifest's workarea attestation
	// rests on. Any error withholds that attestation on this host.
	ProveWorkareaHost(ctx context.Context) error
}

// ErrWorkareaHostProofUnavailable is the withheld reason when a manifest
// needs a host proof and the provider value offers none to run.
var ErrWorkareaHostProofUnavailable = errors.New("agent: the harness needs a workarea host proof and the provider offers none")

// WorkareaAttestation is the workarea part of a harness manifest: the
// multi-repository workarea protocols, the repository authority
// enforcement, and whether the executor can keep a selected read-only
// working directory read-only.
type WorkareaAttestation struct {
	Protocols           []string
	Enforcement         string
	ReadOnlySelectedCWD bool
}

// Empty reports whether the attestation claims nothing.
func (a WorkareaAttestation) Empty() bool {
	return len(a.Protocols) == 0 && a.Enforcement == "" && !a.ReadOnlySelectedCWD
}

// ProvenWorkareaAttestation returns the workarea attestation harness may
// publish at registration and bind sessions against on this host. It is the
// manifest's declaration unless the manifest needs a host proof: then the
// provider must implement WorkareaHostProver and its proof must pass, or
// the attestation is withheld whole (empty) and withheld carries the
// reason. A withheld attestation makes the harness a legacy single-path
// executor on this host, so declared work is excluded at placement instead
// of refused at spawn, and sibling context keeps its legacy placement.
//
// A provider value that hides WorkareaHostProver (a wrapper that does not
// forward it) is withheld, never trusted: the manifest, not the wrapper,
// says a proof is needed.
func ProvenWorkareaAttestation(ctx context.Context, harness HarnessProvider) (attestation WorkareaAttestation, withheld error) {
	caps := harness.Manifest().Caps
	declared := WorkareaAttestation{
		Protocols:           append([]string(nil), caps.MultiRepositoryWorkareaProtocols...),
		Enforcement:         caps.RepositoryAuthorityEnforcement,
		ReadOnlySelectedCWD: caps.SupportsReadOnlySelectedCWD,
	}
	if declared.Empty() || !caps.WorkareaAttestationNeedsHostProof {
		return declared, nil
	}
	prover, ok := harness.(WorkareaHostProver)
	if !ok {
		return WorkareaAttestation{}, ErrWorkareaHostProofUnavailable
	}
	if err := prover.ProveWorkareaHost(ctx); err != nil {
		return WorkareaAttestation{}, err
	}
	return declared, nil
}
