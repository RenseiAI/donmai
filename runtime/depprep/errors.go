package depprep

import (
	"errors"
	"strings"
)

// Code is the closed error code of a failed Prepare (ADR D1). A new code
// needs an ADR. A caller treats a code it does not know as CodeInstallFailed;
// CodeOf does that mapping.
type Code string

// The nine codes.
const (
	// CodeInvalidRequest: Root is not an absolute directory, Scratch is
	// missing, the toolchain directories are unclean, or another request
	// field is malformed. A caller bug; refuse.
	CodeInvalidRequest Code = "invalid_request"
	// CodeEntriesUnavailable: the entry source failed (an invalid kit, a
	// composition conflict without a lock selection), or the selected entry
	// cannot be used under this configuration. Refuse, not retryable.
	CodeEntriesUnavailable Code = "entries_unavailable"
	// CodeManagerAmbiguous: two entries of one ecosystem match and the leaf
	// does not decide. Refuse, not retryable.
	CodeManagerAmbiguous Code = "manager_ambiguous"
	// CodeToolchainUnavailable: the manager is in no toolchain directory, or
	// its version cannot be determined. Refuse; a host fix.
	CodeToolchainUnavailable Code = "toolchain_unavailable"
	// CodeToolchainMismatch: the leaf pins a manager version that no
	// selected entry's versions range covers. Refuse; a kit or repository fix.
	CodeToolchainMismatch Code = "toolchain_mismatch"
	// CodeNetworkDenied: every offline path missed and the network is
	// denied (miss reason store_miss_offline). Refuse.
	CodeNetworkDenied Code = "network_denied"
	// CodeCredentialsUnavailable: the credential callback failed for the
	// scope. Refuse, retryable.
	CodeCredentialsUnavailable Code = "credentials_unavailable"
	// CodeInstallFailed: the last permitted step exited non-zero. Refuse;
	// Error.Step names the step.
	CodeInstallFailed Code = "install_failed"
	// CodeDeadlineExceeded: the budget ran out, or too little remained to
	// start the online step. Refuse, retryable.
	CodeDeadlineExceeded Code = "deadline_exceeded"
)

// Sentinels, one per code, for errors.Is.
var (
	ErrInvalidRequest         = errors.New("depprep: " + string(CodeInvalidRequest))
	ErrEntriesUnavailable     = errors.New("depprep: " + string(CodeEntriesUnavailable))
	ErrManagerAmbiguous       = errors.New("depprep: " + string(CodeManagerAmbiguous))
	ErrToolchainUnavailable   = errors.New("depprep: " + string(CodeToolchainUnavailable))
	ErrToolchainMismatch      = errors.New("depprep: " + string(CodeToolchainMismatch))
	ErrNetworkDenied          = errors.New("depprep: " + string(CodeNetworkDenied))
	ErrCredentialsUnavailable = errors.New("depprep: " + string(CodeCredentialsUnavailable))
	ErrInstallFailed          = errors.New("depprep: " + string(CodeInstallFailed))
	ErrDeadlineExceeded       = errors.New("depprep: " + string(CodeDeadlineExceeded))
)

var sentinels = map[Code]error{
	CodeInvalidRequest:         ErrInvalidRequest,
	CodeEntriesUnavailable:     ErrEntriesUnavailable,
	CodeManagerAmbiguous:       ErrManagerAmbiguous,
	CodeToolchainUnavailable:   ErrToolchainUnavailable,
	CodeToolchainMismatch:      ErrToolchainMismatch,
	CodeNetworkDenied:          ErrNetworkDenied,
	CodeCredentialsUnavailable: ErrCredentialsUnavailable,
	CodeInstallFailed:          ErrInstallFailed,
	CodeDeadlineExceeded:       ErrDeadlineExceeded,
}

// Codes returns the closed code set in declaration order.
func Codes() []Code {
	return []Code{
		CodeInvalidRequest, CodeEntriesUnavailable, CodeManagerAmbiguous,
		CodeToolchainUnavailable, CodeToolchainMismatch, CodeNetworkDenied,
		CodeCredentialsUnavailable, CodeInstallFailed, CodeDeadlineExceeded,
	}
}

// Error is the only non-nil error type Prepare returns.
type Error struct {
	Code Code
	// Manager is the entry's manager, when the failure belongs to one.
	Manager string
	// Step is the step that failed: "version", "fetch" or "install".
	Step string
	// MissReason is the closed miss reason, when one applies.
	MissReason MissReason
	// Detail is display-only text. It never carries a credential, a scope
	// value or a subject attribute.
	Detail string
	// Err is the underlying cause, when there is one.
	Err error
}

// Error renders the code, manager, step and detail.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("depprep: ")
	b.WriteString(string(e.Code))
	if e.Manager != "" {
		b.WriteString(": ")
		b.WriteString(e.Manager)
	}
	if e.Step != "" {
		b.WriteString(" ")
		b.WriteString(e.Step)
	}
	if e.MissReason != "" {
		b.WriteString(" (")
		b.WriteString(string(e.MissReason))
		b.WriteString(")")
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// Is matches the sentinel of e's code.
func (e *Error) Is(target error) bool {
	s, ok := sentinels[e.Code]
	return ok && s == target
}

// CodeOf returns err's code: "" for nil, the code of a *Error whose code is
// in the closed set, and CodeInstallFailed for anything else (ADR D1: an
// unknown code is treated as install_failed).
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		if _, ok := sentinels[e.Code]; ok {
			return e.Code
		}
	}
	return CodeInstallFailed
}
