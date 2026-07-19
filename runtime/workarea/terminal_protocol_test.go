package workarea_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

func validTerminalLeaseRequest() workarea.TerminalLeaseRequest {
	return workarea.TerminalLeaseRequest{
		SchemaVersion:      workarea.TerminalLeaseRequestSchemaV1,
		SettlementBudgetMS: (17 * time.Minute).Milliseconds(),
		SafetyMarginMS:     time.Minute.Milliseconds(),
		LeaseDurationMS:    (30 * time.Minute).Milliseconds(),
		MaxLeaseDurationMS: (2 * time.Hour).Milliseconds(),
	}
}

func TestTerminalLeaseRequestPolicyValidatesFiniteOrdering(t *testing.T) {
	t.Parallel()

	req := validTerminalLeaseRequest()
	policy, err := req.Policy()
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	if policy.SettlementBudget != 17*time.Minute || policy.LeaseDuration != 30*time.Minute || policy.MaxLeaseDuration != 2*time.Hour {
		t.Fatalf("policy = %+v", policy)
	}

	cases := []struct {
		name string
		edit func(*workarea.TerminalLeaseRequest)
	}{
		{name: "schema", edit: func(r *workarea.TerminalLeaseRequest) { r.SchemaVersion = "unknown" }},
		{name: "budget", edit: func(r *workarea.TerminalLeaseRequest) { r.SettlementBudgetMS = 0 }},
		{name: "ordering", edit: func(r *workarea.TerminalLeaseRequest) { r.LeaseDurationMS = r.SettlementBudgetMS + r.SafetyMarginMS }},
		{name: "max-covers-initial", edit: func(r *workarea.TerminalLeaseRequest) { r.MaxLeaseDurationMS = r.LeaseDurationMS - 1 }},
		{name: "finite-maximum", edit: func(r *workarea.TerminalLeaseRequest) {
			r.MaxLeaseDurationMS = workarea.MaximumLeaseDuration.Milliseconds() + 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			candidate := validTerminalLeaseRequest()
			tc.edit(&candidate)
			if _, err := candidate.Policy(); err == nil {
				t.Fatal("Policy succeeded")
			}
		})
	}
}

func TestTerminalLeaseDescriptorValidatesLocalStateAndBudget(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	lease := workarea.TerminalLease{
		LeaseID:          "twl_1",
		SessionID:        "session-1",
		TerminalResultID: "result-1",
		WorkareaID:       "wa_1",
		AcquiredAt:       now,
		ExpiresAt:        now.Add(30 * time.Minute),
		SettlementBudget: 17 * time.Minute,
		State:            workarea.LeaseActive,
	}
	desc := lease.Descriptor()
	if err := lease.ValidateDescriptor(desc, now.Add(time.Minute), 20*time.Minute); err != nil {
		t.Fatalf("ValidateDescriptor: %v", err)
	}

	mismatch := desc
	mismatch.WorkareaID = "wa_other"
	if err := lease.ValidateDescriptor(mismatch, now, 0); !errors.Is(err, workarea.ErrLeaseConflict) {
		t.Fatalf("mismatch error = %v", err)
	}
	if err := lease.ValidateDescriptor(desc, now.Add(15*time.Minute), 16*time.Minute); err == nil {
		t.Fatal("under-budget descriptor accepted")
	}
	lease.State = workarea.LeaseReleasePending
	if err := lease.ValidateDescriptor(desc, now, 0); err == nil {
		t.Fatal("release-pending lease accepted")
	}
}

func TestIDForPathIsStableAndOpaque(t *testing.T) {
	t.Parallel()

	path := t.TempDir()
	first, err := workarea.IDForPath(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := workarea.IDForPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "wa_") {
		t.Fatalf("ids = %q / %q", first, second)
	}
	if strings.Contains(first, path) {
		t.Fatalf("opaque id %q contains path %q", first, path)
	}
}
