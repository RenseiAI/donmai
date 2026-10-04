package confinement

import (
	"errors"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// Reason is the closed ConfinementUnavailableReason enum of
// ADR-2026-10-03-executor-os-confinement.md D5. Consumers branch on it; the
// detail an Error carries is display-only.
type Reason string

// The closed reasons. An unknown value is malformed and denies (ParseReason).
const (
	// ReasonBackendAbsent: no backend for this OS, or its implementation is
	// missing.
	ReasonBackendAbsent Reason = "backend_absent"
	// ReasonSelfTestFailed: a probe failed on this host, or no self-test has
	// passed for the requested session mode.
	ReasonSelfTestFailed Reason = "self_test_failed"
	// ReasonSelfTestStale: the executable, the OS build or the backend changed
	// since the last passing self-test.
	ReasonSelfTestStale Reason = "self_test_stale"
	// ReasonNestedSandbox: the executor is already inside a profile (D4.2).
	ReasonNestedSandbox Reason = "nested_sandbox"
	// ReasonNamespaceUnavailable: the kernel or container refused the
	// namespace (Linux backends).
	ReasonNamespaceUnavailable Reason = "namespace_unavailable"
	// ReasonWritableSetUnrepresentable: the declared set cannot be confined as
	// declared — a shared git common directory (D2.4), a hard-linked store
	// (D2.3), a symlinked leaf, a writable root that covers a protected path.
	ReasonWritableSetUnrepresentable Reason = "writable_set_unrepresentable"
	// ReasonRuleUnrenderable: a composer rule the backend cannot express (D4.3).
	ReasonRuleUnrenderable Reason = "rule_unrenderable"
	// ReasonModeUnsupported: the session mode is not one the confinement
	// declares (D4.4).
	ReasonModeUnsupported Reason = "mode_unsupported"
)

var knownReasons = map[Reason]bool{
	ReasonBackendAbsent:              true,
	ReasonSelfTestFailed:             true,
	ReasonSelfTestStale:              true,
	ReasonNestedSandbox:              true,
	ReasonNamespaceUnavailable:       true,
	ReasonWritableSetUnrepresentable: true,
	ReasonRuleUnrenderable:           true,
	ReasonModeUnsupported:            true,
}

// Reasons returns the closed reason enum in declaration order.
func Reasons() []Reason {
	return []Reason{
		ReasonBackendAbsent, ReasonSelfTestFailed, ReasonSelfTestStale, ReasonNestedSandbox,
		ReasonNamespaceUnavailable, ReasonWritableSetUnrepresentable, ReasonRuleUnrenderable,
		ReasonModeUnsupported,
	}
}

// ParseReason accepts exactly one of the closed reasons. Anything else is
// malformed and is refused rather than guessed.
func ParseReason(raw string) (Reason, error) {
	reason := Reason(raw)
	if !knownReasons[reason] {
		return "", fmt.Errorf("confinement: unknown unavailable reason %q", raw)
	}
	return reason, nil
}

// RefusalCode is the execution-security refusal a spawn-time confinement
// failure is reported under (D5.2): the adaptation plan is denied with
// execution_security_unrenderable carrying the closed Reason. No refusal code
// is added.
const RefusalCode = agent.ExecutionSecurityUnrenderable

// Error is a typed refusal: confinement was requested and cannot be applied.
// Reason is the closed enum; Detail is display-only.
type Error struct {
	Reason Reason
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("confinement unavailable: %s", e.Reason)
	}
	return fmt.Sprintf("confinement unavailable: %s: %s", e.Reason, e.Detail)
}

// Code returns the execution-security refusal code this error is reported
// under.
func (e *Error) Code() agent.ExecutionSecurityRefusalCode { return RefusalCode }

// ReasonOf returns the closed reason carried by err, or false when err is not
// a confinement refusal.
func ReasonOf(err error) (Reason, bool) {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Reason, true
	}
	return "", false
}

func refuse(reason Reason, format string, args ...any) *Error {
	return &Error{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}
