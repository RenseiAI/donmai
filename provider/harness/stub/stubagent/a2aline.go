package stubagent

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/a2a"
)

// A2ALinePrefix marks a line of stdout as an agent-to-agent message. A
// consumer reads the PTY as an opaque byte stream, so the marker is what makes
// one line machine-addressable without teaching that consumer to parse a
// terminal. The trailing space is part of the prefix.
const A2ALinePrefix = "DONMAI-A2A "

// ErrNotA2ALine reports that a line does not carry the A2ALinePrefix.
var ErrNotA2ALine = errors.New("stubagent: line is not an agent-to-agent line")

func validateRole(role string) error {
	switch a2a.Role(role) {
	case "", a2a.RoleAgent, a2a.RoleUser:
		return nil
	default:
		return fmt.Errorf("unknown a2a role %q (want %q or %q)", role, a2a.RoleAgent, a2a.RoleUser)
	}
}

// A2AMessage builds the a2a.Message a directive encodes to. The message id is
// DERIVED from (scenario name, seed, step index) rather than drawn from a
// random source or a clock, which is what lets two runs of the same scenario
// be compared byte for byte — the property an integration environment needs
// and a real agent cannot offer.
func A2AMessage(scenario Scenario, index int, directive A2ADirective) (a2a.Message, error) {
	if strings.TrimSpace(directive.Text) == "" {
		return a2a.Message{}, errors.New("stubagent: a2a text is required")
	}
	if err := validateRole(directive.Role); err != nil {
		return a2a.Message{}, fmt.Errorf("stubagent: %w", err)
	}
	role := a2a.Role(directive.Role)
	if role == "" {
		role = a2a.RoleAgent
	}
	return a2a.Message{
		MessageID: deterministicMessageID(scenario.Name, scenario.Seed, index),
		ContextID: directive.ContextID,
		TaskID:    directive.TaskID,
		Role:      role,
		Parts:     []a2a.Part{a2a.TextPart(directive.Text)},
	}, nil
}

// EncodeA2ALine renders one directive as a single prefixed line WITHOUT its
// trailing newline. The JSON body is a real a2a.Message, so a consumer
// asserting on it is asserting against the protocol type rather than a shape
// invented here that could drift away from it.
func EncodeA2ALine(scenario Scenario, index int, directive A2ADirective) (string, error) {
	message, err := A2AMessage(scenario, index, directive)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return "", fmt.Errorf("stubagent: encode a2a message: %w", err)
	}
	return A2ALinePrefix + string(encoded), nil
}

// ParseA2ALine is the reader half of EncodeA2ALine, exported so a scenario
// runner or smoke never has to restate the wire format. A line without the
// prefix returns ErrNotA2ALine; a prefixed line that does not decode is a
// hard error, because a corrupted marked line is a defect, not ordinary
// terminal output.
func ParseA2ALine(line string) (a2a.Message, error) {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, A2ALinePrefix) {
		return a2a.Message{}, ErrNotA2ALine
	}
	var message a2a.Message
	body := strings.TrimPrefix(trimmed, A2ALinePrefix)
	if err := json.Unmarshal([]byte(body), &message); err != nil {
		return a2a.Message{}, fmt.Errorf("stubagent: decode a2a line: %w", err)
	}
	return message, nil
}

// deterministicMessageID hashes the scenario identity and step index. sha256
// is not a security choice here — it is simply a stable, well-defined function
// of the inputs, which a PRNG seeded per process is not once step ordering or
// step count changes.
func deterministicMessageID(name string, seed int64, index int) string {
	digest := sha256.New()
	digest.Write([]byte(name))
	digest.Write([]byte{0})
	var scratch [8]byte
	binary.BigEndian.PutUint64(scratch[:], uint64(seed)) //nolint:gosec // two's-complement round trip is intended
	digest.Write(scratch[:])
	binary.BigEndian.PutUint64(scratch[:], uint64(index)) //nolint:gosec // index is never negative
	digest.Write(scratch[:])
	return "stub-msg-" + hex.EncodeToString(digest.Sum(nil))[:16]
}
