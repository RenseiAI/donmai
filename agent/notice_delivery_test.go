package agent

import (
	"errors"
	"testing"
)

// TestValidateSpecCapabilities_LiveNoticeAdmission pins the admission half of
// the notice-delivery axis.
//
// The failure it prevents: a session is launched carrying a coordination
// handle — something upstream is holding a pointer to this agent and will try
// to reach it — onto a harness with no mechanism to be reached. Nothing fails
// at launch, so the agent runs, the messages go somewhere that reports success,
// and the sender waits for a reply that structurally cannot come. Refusing at
// admission is cheap; discovering it from a silent non-reply is not.
//
// Undeclared is denied for the same reason it is not defaulted: a new harness
// must answer the question, not inherit an answer.
func TestValidateSpecCapabilities_LiveNoticeAdmission(t *testing.T) {
	tests := []struct {
		name        string
		requires    bool
		declared    NoticeDelivery
		wantDenied  bool
		wantCode    SpecAdmissionDenialCode
		wantField   string
		description string
	}{
		{
			name:     "a session with no coordination handle is admitted anywhere",
			declared: NoticeDeliveryNone,
		},
		{
			name:     "undeclared is admitted when nothing needs to reach the session",
			declared: "",
		},
		{
			name:     "pty-notice satisfies a live-notice requirement",
			requires: true, declared: NoticeDeliveryPTYNotice,
		},
		{
			name:     "hook satisfies it",
			requires: true, declared: NoticeDeliveryHook,
		},
		{
			name:     "in-box-loop satisfies it",
			requires: true, declared: NoticeDeliveryInBoxLoop,
		},
		{
			name:     "an explicit none is refused",
			requires: true, declared: NoticeDeliveryNone,
			wantDenied: true, wantCode: SpecDenialNoticeDeliveryUnavailable, wantField: "requiresLiveNotice",
		},
		{
			name:     "an undeclared manifest is refused, not defaulted",
			requires: true, declared: "",
			wantDenied: true, wantCode: SpecDenialNoticeDeliveryUnavailable, wantField: "requiresLiveNotice",
		},
		{
			name:     "an unknown mechanism is refused rather than trusted",
			requires: true, declared: NoticeDelivery("carrier-pigeon"),
			wantDenied: true, wantCode: SpecDenialNoticeDeliveryUnavailable, wantField: "requiresLiveNotice",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifest := HarnessManifest{
				Name: HarnessShell,
				Caps: HarnessCaps{NoticeDelivery: tc.declared},
			}
			err := ValidateSpecCapabilities(Spec{RequiresLiveNotice: tc.requires}, manifest)
			if !tc.wantDenied {
				if err != nil {
					t.Fatalf("ValidateSpecCapabilities = %v; want admitted", err)
				}
				return
			}
			var denial *SpecAdmissionError
			if !errors.As(err, &denial) {
				t.Fatalf("ValidateSpecCapabilities = %v; want a typed *SpecAdmissionError so callers "+
					"can distinguish an unreachable-agent refusal from any other spawn failure", err)
			}
			if denial.Code != tc.wantCode {
				t.Errorf("denial code = %q; want %q", denial.Code, tc.wantCode)
			}
			if denial.Field != tc.wantField {
				t.Errorf("denial field = %q; want %q", denial.Field, tc.wantField)
			}
		})
	}
}

// TestNoticeDelivery_DeclaredAndCanDeliver pins the two questions the type
// answers, and keeps them separate. "Is this a known mechanism" and "does that
// mechanism carry anything" are different, and the empty value answers no to
// both — which is what makes omission safe.
func TestNoticeDelivery_DeclaredAndCanDeliver(t *testing.T) {
	tests := []struct {
		value           NoticeDelivery
		wantDeclared    bool
		wantCanDelivery bool
	}{
		{value: NoticeDeliveryNone, wantDeclared: true},
		{value: NoticeDeliveryHook, wantDeclared: true, wantCanDelivery: true},
		{value: NoticeDeliveryMCPRPC, wantDeclared: true, wantCanDelivery: true},
		{value: NoticeDeliveryHTTPSession, wantDeclared: true, wantCanDelivery: true},
		{value: NoticeDeliveryACP, wantDeclared: true, wantCanDelivery: true},
		{value: NoticeDeliveryRPCSteer, wantDeclared: true, wantCanDelivery: true},
		{value: NoticeDeliveryResumeInject, wantDeclared: true, wantCanDelivery: true},
		{value: NoticeDeliveryInBoxLoop, wantDeclared: true, wantCanDelivery: true},
		{value: NoticeDeliveryPTYNotice, wantDeclared: true, wantCanDelivery: true},
		{value: ""},
		{value: NoticeDelivery("smoke-signal")},
	}

	for _, tc := range tests {
		t.Run(string(tc.value), func(t *testing.T) {
			if got := tc.value.Declared(); got != tc.wantDeclared {
				t.Errorf("Declared() = %v; want %v", got, tc.wantDeclared)
			}
			if got := tc.value.CanDeliver(); got != tc.wantCanDelivery {
				t.Errorf("CanDeliver() = %v; want %v", got, tc.wantCanDelivery)
			}
		})
	}
}
