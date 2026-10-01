package localqueue

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/executioncell"
)

var (
	localIDPattern       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)
	ownerRepoPattern     = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	credentialRefPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{1,127}$`)
	sha256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validLocalID(value string) bool { return localIDPattern.MatchString(value) }

func validSHA256(value string) bool { return sha256Pattern.MatchString(value) }

func validTimestamp(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !parsed.IsZero()
}

func validateSource(source GitHubSource) error {
	if source.RepositoryID <= 0 || source.IssueNumber <= 0 || !ownerRepoPattern.MatchString(source.OwnerRepo) {
		return fmt.Errorf("%w: GitHub source identity is incomplete", ErrInvalidEnvelope)
	}
	parsed, err := url.Parse(source.IssueURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return fmt.Errorf("%w: GitHub issue URL is not canonical", ErrInvalidEnvelope)
	}
	wantPath := "/" + source.OwnerRepo + "/issues/" + strconv.Itoa(source.IssueNumber)
	if !strings.EqualFold(parsed.Path, wantPath) {
		return fmt.Errorf("%w: GitHub issue URL disagrees with source identity", ErrInvalidEnvelope)
	}
	return nil
}

func validateCredentialHandle(handle string) error {
	if !credentialRefPattern.MatchString(handle) || strings.HasPrefix(handle, "rsp_") ||
		strings.HasPrefix(handle, "rsk_") || strings.HasPrefix(handle, "ghp_") ||
		strings.HasPrefix(handle, "github_pat_") {
		return fmt.Errorf("%w: credential handle must be an opaque local reference", ErrInvalidEnvelope)
	}
	return nil
}

// DispatchKey is the closed, deterministic dispatch-outbox key for a session.
func DispatchKey(sessionID string) string { return "dispatch:" + sessionID }

// DigestAdmissionEnvelope hashes the evidence to which the initial dispatch
// outbox points. The digest field itself is excluded to avoid recursion.
func DigestAdmissionEnvelope(envelope AdmissionEnvelope) (string, error) {
	digestInput := persistEnvelope(envelope)
	digestInput.Dispatch.EnvelopeSHA256 = ""
	canonical, err := executioncell.CanonicalJSON(digestInput)
	if err != nil {
		return "", fmt.Errorf("%w: canonical admission: %v", ErrInvalidEnvelope, err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func validateAdmission(envelope *AdmissionEnvelope, binding ConfiguredHostBinding, requireSelectors bool) (string, error) {
	if !validLocalID(envelope.ScopeID) || envelope.ScopeID != binding.ScopeID ||
		!validLocalID(binding.HostID) || !validLocalID(binding.WorkerID) {
		return "", fmt.Errorf("%w: local scope or host binding mismatch", ErrInvalidEnvelope)
	}
	if err := validateSource(envelope.Source); err != nil {
		return "", err
	}
	if err := validateCredentialHandle(envelope.CredentialHandle); err != nil {
		return "", err
	}
	if requireSelectors && (binding.Selectors == nil || binding.Selectors.HarnessVersions == nil ||
		binding.Selectors.Models == nil || binding.Selectors.Endpoints == nil ||
		binding.Selectors.AuthBindings == nil || binding.Selectors.Placements == nil) {
		return "", fmt.Errorf("%w: selector registry is required for admission", ErrInvalidEnvelope)
	}
	intentRaw, err := json.Marshal(envelope.Intent)
	if err != nil {
		return "", fmt.Errorf("%w: encode intent: %v", ErrInvalidEnvelope, err)
	}
	selectors := binding.Selectors
	if !requireSelectors {
		selectors = nil // historical admission survives later catalog drift.
	}
	intent, err := executioncell.DecodeDispatchIntent(intentRaw, selectors)
	if err != nil {
		return "", fmt.Errorf("%w: intent: %v", ErrInvalidEnvelope, err)
	}
	if intent.SessionMode != executioncell.SessionAutonomous || len(intent.FallbackAlternatives) != 0 {
		return "", fmt.Errorf("%w: local v1 requires autonomous exact selection", ErrInvalidEnvelope)
	}
	sessionRaw, err := json.Marshal(envelope.Session)
	if err != nil {
		return "", fmt.Errorf("%w: encode session: %v", ErrInvalidEnvelope, err)
	}
	session, err := executioncell.DecodeSessionRef(sessionRaw)
	if err != nil {
		return "", fmt.Errorf("%w: session: %v", ErrInvalidEnvelope, err)
	}
	sessionValue := session.Value()
	if sessionValue.SessionID != intent.RequestID || sessionValue.ClaimReceiptID != "" ||
		!validLocalID(sessionValue.SessionID) || sessionValue.Mode != executioncell.SessionAutonomous ||
		sessionValue.Capabilities.TakeControl {
		return "", fmt.Errorf("%w: local session identity or claim field mismatch", ErrInvalidEnvelope)
	}
	receipt, err := executioncell.DecodeAdmissionReceipt(envelope.Receipt)
	if err != nil {
		return "", fmt.Errorf("%w: admission receipt: %v", ErrInvalidEnvelope, err)
	}
	if err := executioncell.AssertAdmissionProvenance(intent, receipt); err != nil {
		return "", fmt.Errorf("%w: admission provenance: %v", ErrInvalidEnvelope, err)
	}
	receiptValue := receipt.Value()
	if receiptValue.Decision != executioncell.AdmissionAdmitted || receiptValue.Cell == nil ||
		receiptValue.ReceiptID != sessionValue.AdmissionReceiptID || receiptValue.RequestID != sessionValue.SessionID ||
		sessionValue.Mode != intent.SessionMode {
		return "", fmt.Errorf("%w: admitted receipt and session disagree", ErrInvalidEnvelope)
	}
	cell, err := executioncell.DecodeResolvedExecutionCell(envelope.EffectiveCell)
	if err != nil {
		return "", fmt.Errorf("%w: effective cell: %v", ErrInvalidEnvelope, err)
	}
	cellBytes, err := executioncell.CanonicalJSON(cell)
	if err != nil {
		return "", fmt.Errorf("%w: canonical cell: %v", ErrInvalidEnvelope, err)
	}
	receiptCellBytes, err := executioncell.CanonicalJSON(receiptValue.Cell)
	if err != nil || !bytes.Equal(cellBytes, receiptCellBytes) || cell.Placement.ID != binding.HostID ||
		cell.Placement.Kind != executioncell.PlacementHost || cell.Placement.Resolution != executioncell.PlacementExact {
		return "", fmt.Errorf("%w: effective cell differs from exact-host admission", ErrInvalidEnvelope)
	}
	if err := validateProducerEvidence(envelope.ProducerEvidence, *envelope, binding, cell, receiptValue); err != nil {
		return "", err
	}
	operational, err := executioncell.NormalizeOperationalPayload(envelope.OperationalPayload)
	if err != nil || !bytes.Equal(operational, envelope.OperationalPayload) {
		return "", fmt.Errorf("%w: operational payload must be canonical and sidecar-free", ErrInvalidEnvelope)
	}
	operationalDigest, err := executioncell.DigestOperationalPayload(operational)
	if err != nil || receiptValue.OperationalPayloadDigest != operationalDigest {
		return "", fmt.Errorf("%w: operational payload digest mismatch", ErrInvalidEnvelope)
	}
	bindingValue, err := executioncell.DecodeRuntimeBinding(envelope.RuntimeBinding)
	if err != nil || bindingValue.ContractVersion != executioncell.RuntimeBindingV2ContractVersion ||
		bindingValue.RequestID != sessionValue.SessionID || bindingValue.WorkerID != binding.WorkerID ||
		bindingValue.PlacementID != binding.HostID || bindingValue.ClaimID != "" {
		return "", fmt.Errorf("%w: runtime binding is not exact-host and claim-free", ErrInvalidEnvelope)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(envelope.HostPreflight)
	if err != nil || host.Decision != "ready" || host.RequestID != sessionValue.SessionID ||
		host.WorkerID != binding.WorkerID || host.PlacementID != binding.HostID || host.ClaimID != "" {
		return "", fmt.Errorf("%w: host preflight does not match exact-host admission", ErrInvalidEnvelope)
	}
	if err := executioncell.AssertSecretFreeReceipt(host); err != nil {
		return "", fmt.Errorf("%w: host preflight carries secret material", ErrInvalidEnvelope)
	}
	var planIdentity struct {
		OperationalPayloadDigest string `json:"operationalPayloadDigest"`
	}
	if err := json.Unmarshal(host.Plan, &planIdentity); err != nil || planIdentity.OperationalPayloadDigest != operationalDigest {
		return "", fmt.Errorf("%w: host plan operational digest mismatch", ErrInvalidEnvelope)
	}
	if envelope.InitialEvent.Type != EventSessionAdmitted || envelope.InitialEvent.ScopeID != envelope.ScopeID ||
		envelope.InitialEvent.SessionID != sessionValue.SessionID ||
		envelope.InitialEvent.ReceiptID != receiptValue.ReceiptID || !validTimestamp(envelope.InitialEvent.RecordedAt) {
		return "", fmt.Errorf("%w: initial event is not the admitted session", ErrInvalidEnvelope)
	}
	if envelope.Dispatch.State != "" && envelope.Dispatch.State != DispatchPending {
		return "", fmt.Errorf("%w: initial dispatch must be pending", ErrInvalidEnvelope)
	}
	if envelope.Dispatch.Key != "" && envelope.Dispatch.Key != DispatchKey(sessionValue.SessionID) {
		return "", fmt.Errorf("%w: dispatch key mismatch", ErrInvalidEnvelope)
	}
	envelope.Dispatch.Key = DispatchKey(sessionValue.SessionID)
	envelope.Dispatch.State = DispatchPending
	digest, err := DigestAdmissionEnvelope(*envelope)
	if err != nil {
		return "", err
	}
	if envelope.Dispatch.EnvelopeSHA256 != "" && envelope.Dispatch.EnvelopeSHA256 != digest {
		return "", fmt.Errorf("%w: dispatch envelope digest mismatch", ErrInvalidEnvelope)
	}
	envelope.Dispatch.EnvelopeSHA256 = digest
	return digest, nil
}
