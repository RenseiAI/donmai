package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	workClaimAttemptContractVersion = "work-claim-attempt/v1"
	maxWorkClaimSessionIDBytes      = 256
	maxPollClaimAttemptProofs       = 64
)

var canonicalUUIDv4 = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
)

// workClaimAttemptProof is transport correlation for one exact poll claim. It
// is not execution authority and never enters work JSON, SessionDetail, the
// operational payload, receipts, runtime state, or logs.
type workClaimAttemptProof struct {
	ContractVersion string
	SessionID       string
	AttemptToken    string
}

// UnmarshalJSON keeps PollResponse's permissive unknown-field behavior while
// validating the optional claimAttempts envelope from the original bytes. A
// bad proof never blocks ordinary work: it leaves every process-only binding
// nil and records one bounded refusal code for PollService to log.
func (response *PollResponse) UnmarshalJSON(raw []byte) error {
	type pollResponseAlias PollResponse
	var decoded pollResponseAlias
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*response = PollResponse(decoded)
	response.bindClaimAttemptProofs(raw)
	response.bindNackContracts(raw)
	return nil
}

func (response *PollResponse) bindNackContracts(raw []byte) {
	contractsRaw, present, ok := rawPollEnvelopeMember(raw, "nackContracts")
	if !present || !ok {
		return
	}
	if !utf8.Valid(contractsRaw) {
		return
	}
	var contracts []string
	if err := json.Unmarshal(contractsRaw, &contracts); err != nil || contracts == nil || len(contracts) > 8 {
		return
	}
	known := 0
	for _, contract := range contracts {
		if contract == "" || len(contract) > 128 || !utf8.ValidString(contract) {
			return
		}
		if contract == preSpawnPermanentDenialContractVersion {
			known++
		}
	}
	if known != 1 {
		return
	}
	for index := range response.Work {
		response.Work[index].preSpawnPermanentDenialV1 = true
	}
}

// rawPollEnvelopeMember returns one exact top-level member without making the
// otherwise permissive PollResponse decoder reject unrelated future members.
// Duplicate exact members make only this optional negotiation unavailable.
func rawPollEnvelopeMember(raw []byte, want string) (json.RawMessage, bool, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil {
		return nil, false, false
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return nil, false, false
	}
	var found json.RawMessage
	present := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false, false
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false, false
		}
		if key != want {
			continue
		}
		if present {
			return nil, true, false
		}
		present = true
		found = append(json.RawMessage(nil), value...)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, false, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false, false
	}
	return found, present, true
}

func (response *PollResponse) bindClaimAttemptProofs(raw []byte) {
	proofRaw, present, code := rawClaimAttemptMap(raw)
	if code != "" {
		response.claimAttemptBindingRefusal = code
		return
	}
	if !present {
		return
	}
	proofs, code := decodeClaimAttemptProofMap(proofRaw)
	if code != "" {
		response.claimAttemptBindingRefusal = code
		return
	}

	workCounts := make(map[string]int, len(response.Work))
	for _, item := range response.Work {
		workCounts[item.SessionID]++
	}
	for sessionID, count := range workCounts {
		if count > 1 {
			response.claimAttemptBindingRefusal = "duplicate_work_session"
			return
		}
		if sessionID == "" {
			response.claimAttemptBindingRefusal = "empty_work_session"
			return
		}
	}
	for sessionID := range proofs {
		if workCounts[sessionID] != 1 {
			response.claimAttemptBindingRefusal = "orphan_claim_attempt"
			return
		}
	}

	// Bind only after the complete envelope has passed validation. Copy each
	// value so no map entry or sibling work item aliases mutable proof state.
	for index := range response.Work {
		proof, ok := proofs[response.Work[index].SessionID]
		if !ok {
			continue
		}
		bound := proof
		response.Work[index].claimAttempt = &bound
	}
}

func rawClaimAttemptMap(raw []byte) (json.RawMessage, bool, string) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil {
		return nil, false, "invalid_poll_envelope"
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return nil, false, "invalid_poll_envelope"
	}
	var found json.RawMessage
	seen := false
	workMembers := 0
	canonicalWorkMembers := 0
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false, "invalid_poll_envelope"
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false, "invalid_poll_envelope"
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false, "invalid_poll_envelope"
		}
		if strings.EqualFold(key, "work") {
			workMembers++
			if key == "work" {
				canonicalWorkMembers++
			}
		}
		if key != "claimAttempts" {
			continue
		}
		if seen {
			return nil, false, "duplicate_claim_attempts_member"
		}
		seen = true
		found = append(json.RawMessage(nil), value...)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, false, "invalid_poll_envelope"
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false, "invalid_poll_envelope"
	}
	if seen && (workMembers != 1 || canonicalWorkMembers != 1) {
		return nil, false, "ambiguous_work_member"
	}
	return found, seen, ""
}

func decodeClaimAttemptProofMap(raw json.RawMessage) (map[string]workClaimAttemptProof, string) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil {
		return nil, "invalid_claim_attempts_type"
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return nil, "invalid_claim_attempts_type"
	}
	proofs := make(map[string]workClaimAttemptProof)
	for decoder.More() {
		if len(proofs) >= maxPollClaimAttemptProofs {
			return nil, "too_many_claim_attempts"
		}
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, "invalid_claim_attempts_map"
		}
		sessionID, ok := keyToken.(string)
		if !ok || sessionID == "" || len(sessionID) > maxWorkClaimSessionIDBytes {
			return nil, "invalid_claim_attempt_key"
		}
		if _, duplicate := proofs[sessionID]; duplicate {
			return nil, "duplicate_claim_attempt_key"
		}
		var proofRaw json.RawMessage
		if err := decoder.Decode(&proofRaw); err != nil {
			return nil, "invalid_claim_attempt_proof"
		}
		proof, code := decodeClaimAttemptProof(proofRaw)
		if code != "" {
			return nil, code
		}
		if proof.SessionID != sessionID {
			return nil, "claim_attempt_key_mismatch"
		}
		proofs[sessionID] = proof
	}
	if _, err := decoder.Token(); err != nil {
		return nil, "invalid_claim_attempts_map"
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, "invalid_claim_attempts_map"
	}
	return proofs, ""
}

func decodeClaimAttemptProof(raw json.RawMessage) (workClaimAttemptProof, string) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil {
		return workClaimAttemptProof{}, "invalid_claim_attempt_proof"
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return workClaimAttemptProof{}, "invalid_claim_attempt_proof"
	}
	values := make(map[string]string, 3)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return workClaimAttemptProof{}, "invalid_claim_attempt_proof"
		}
		key, ok := keyToken.(string)
		if !ok {
			return workClaimAttemptProof{}, "invalid_claim_attempt_proof"
		}
		if key != "contractVersion" && key != "sessionId" && key != "attemptToken" {
			return workClaimAttemptProof{}, "unknown_claim_attempt_member"
		}
		if _, duplicate := values[key]; duplicate {
			return workClaimAttemptProof{}, "duplicate_claim_attempt_member"
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return workClaimAttemptProof{}, "invalid_claim_attempt_member_type"
		}
		values[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return workClaimAttemptProof{}, "invalid_claim_attempt_proof"
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return workClaimAttemptProof{}, "invalid_claim_attempt_proof"
	}
	if len(values) != 3 {
		return workClaimAttemptProof{}, "missing_claim_attempt_member"
	}
	if values["contractVersion"] != workClaimAttemptContractVersion {
		return workClaimAttemptProof{}, "unsupported_claim_attempt_version"
	}
	if values["sessionId"] == "" || len(values["sessionId"]) > maxWorkClaimSessionIDBytes {
		return workClaimAttemptProof{}, "invalid_claim_attempt_session"
	}
	if !canonicalUUIDv4.MatchString(values["attemptToken"]) {
		return workClaimAttemptProof{}, "invalid_claim_attempt_token"
	}
	return workClaimAttemptProof{
		ContractVersion: values["contractVersion"],
		SessionID:       values["sessionId"],
		AttemptToken:    values["attemptToken"],
	}, ""
}

func (proof workClaimAttemptProof) MarshalJSON() ([]byte, error) {
	// Keep the output closed even if the in-memory type later gains process-only
	// annotations. This is the only serialization point, on the distinct NACK
	// member; PollWorkItem itself never exports the proof.
	return json.Marshal(struct {
		ContractVersion string `json:"contractVersion"`
		SessionID       string `json:"sessionId"`
		AttemptToken    string `json:"attemptToken"`
	}{
		ContractVersion: proof.ContractVersion,
		SessionID:       proof.SessionID,
		AttemptToken:    proof.AttemptToken,
	})
}
