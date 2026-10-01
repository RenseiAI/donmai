package localqueue

import (
	"bytes"

	"github.com/RenseiAI/donmai/executioncell"
)

// Raw receipt/preflight bytes are opaque to the outer journal encoder.
// Encoding them as []byte in the private transaction stores base64, so RFC8785
// canonicalization of the transaction cannot reorder a byte-bound nested plan.
type persistedAdmissionEnvelope struct {
	ScopeID            string                       `json:"scopeId"`
	Source             GitHubSource                 `json:"source"`
	Session            executioncell.SessionRef     `json:"session"`
	Intent             executioncell.DispatchIntent `json:"intent"`
	Receipt            []byte                       `json:"receiptBytes"`
	EffectiveCell      []byte                       `json:"effectiveCellBytes"`
	RuntimeBinding     []byte                       `json:"runtimeBindingBytes"`
	OperationalPayload []byte                       `json:"operationalPayloadBytes"`
	HostPreflight      []byte                       `json:"hostPreflightBytes"`
	ProducerEvidence   []byte                       `json:"producerEvidenceBytes"`
	InitialEvent       LifecycleEvent               `json:"initialEvent"`
	Dispatch           DispatchOutbox               `json:"dispatch"`
	CredentialHandle   string                       `json:"credentialHandle"`
}

type persistedAdmissionRecord struct {
	Revision         Revision                   `json:"revision"`
	Envelope         persistedAdmissionEnvelope `json:"envelope"`
	EnvelopeSHA256   string                     `json:"envelopeSha256"`
	DispatchState    string                     `json:"dispatchState"`
	CurrentAttemptID string                     `json:"currentAttemptId,omitempty"`
}

func persistEnvelope(value AdmissionEnvelope) persistedAdmissionEnvelope {
	return persistedAdmissionEnvelope{
		ScopeID: value.ScopeID, Source: value.Source,
		Session: cloneValue(value.Session), Intent: cloneValue(value.Intent),
		Receipt: bytes.Clone(value.Receipt), EffectiveCell: bytes.Clone(value.EffectiveCell),
		RuntimeBinding: bytes.Clone(value.RuntimeBinding), OperationalPayload: bytes.Clone(value.OperationalPayload),
		HostPreflight: bytes.Clone(value.HostPreflight), ProducerEvidence: bytes.Clone(value.ProducerEvidence),
		InitialEvent: value.InitialEvent, Dispatch: value.Dispatch, CredentialHandle: value.CredentialHandle,
	}
}

func restoreEnvelope(value persistedAdmissionEnvelope) AdmissionEnvelope {
	return AdmissionEnvelope{
		ScopeID: value.ScopeID, Source: value.Source,
		Session: cloneValue(value.Session), Intent: cloneValue(value.Intent),
		Receipt: bytes.Clone(value.Receipt), EffectiveCell: bytes.Clone(value.EffectiveCell),
		RuntimeBinding: bytes.Clone(value.RuntimeBinding), OperationalPayload: bytes.Clone(value.OperationalPayload),
		HostPreflight: bytes.Clone(value.HostPreflight), ProducerEvidence: bytes.Clone(value.ProducerEvidence),
		InitialEvent: value.InitialEvent, Dispatch: value.Dispatch, CredentialHandle: value.CredentialHandle,
	}
}

func persistRecord(value AdmissionRecord) persistedAdmissionRecord {
	return persistedAdmissionRecord{
		Revision: value.Revision, Envelope: persistEnvelope(value.Envelope), EnvelopeSHA256: value.EnvelopeSHA256,
		DispatchState: value.DispatchState, CurrentAttemptID: value.CurrentAttemptID,
	}
}

func restoreRecord(value persistedAdmissionRecord) AdmissionRecord {
	return AdmissionRecord{
		Revision: value.Revision, Envelope: restoreEnvelope(value.Envelope), EnvelopeSHA256: value.EnvelopeSHA256,
		DispatchState: value.DispatchState, CurrentAttemptID: value.CurrentAttemptID,
	}
}

func cloneAdmissionEnvelope(value AdmissionEnvelope) AdmissionEnvelope {
	return restoreEnvelope(persistEnvelope(value))
}

func cloneAdmissionRecord(value AdmissionRecord) AdmissionRecord {
	return restoreRecord(persistRecord(value))
}
