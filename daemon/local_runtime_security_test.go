package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/runner"
)

func localSecurityAdmission(t *testing.T) *runner.LocalAdmission {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.Symlink("/bin/sh", binary); err != nil {
		t.Fatal(err)
	}
	provider, err := codex.New(codex.Options{CodexBin: binary})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := runner.NewRegistryWithOptions(runner.RegistryOptions{RuntimeTransportMode: runner.RuntimeTransportLocal})
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	model := executioncell.ModelRef{ID: "gpt-5-codex", Author: "openai"}
	producer, err := runner.NewLocalAdmissionProducer(registry, runner.LocalAdmissionConfig{
		ScopeID: "local-scope", WorkerID: "local-worker", PlacementID: "local-host", ConfigurationRevision: "configured-revision",
		Harness: agent.HarnessCodex, Model: model, ModelCatalogEntry: &runner.LocalModelCatalogEntry{Model: model, Host: agent.HostOAuthCLI, Revision: "operator-revision"},
		EndpointID: "local-endpoint", AuthBindingID: "local-auth", BinaryPath: binary, CheckHostAuth: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := NewLocalExecutionSecurityStamp("local-scope", InitialLocalExecutionSecurity(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	request := runner.LocalAdmissionRequest{SessionID: "local-session", ReceiptID: "local-receipt", PreflightChallengeID: "local-challenge"}
	request.Work.Body = "Implement the authored change."
	request.Work.WorkType = "development"
	request.Work.ExecutionSecurity = stamp
	admission, err := producer.Admit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

func TestLocalSecurityRequiresActualProducerStampAndReport(t *testing.T) {
	t.Parallel()
	admission := localSecurityAdmission(t)
	if err := ValidateLocalExecutionSecurityEvidence("local-scope", admission.OperationalPayload, admission.HostPreflight); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"missing-stamp", "foreign-scope", "wrong-digest", "missing-report", "wrong-plan-digest", "weaker-report"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			payload := append(json.RawMessage(nil), admission.OperationalPayload...)
			hostRaw := append(json.RawMessage(nil), admission.HostPreflight...)
			scope := "local-scope"
			var work map[string]json.RawMessage
			if err := json.Unmarshal(payload, &work); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing-stamp":
				delete(work, "executionSecurity")
			case "foreign-scope":
				scope = "other-scope"
			case "wrong-digest", "weaker-report":
				var stamp agent.ExecutionSecurity
				if err := json.Unmarshal(work["executionSecurity"], &stamp); err != nil {
					t.Fatal(err)
				}
				if mode == "weaker-report" {
					policy := InitialLocalExecutionSecurity()
					policy.Network = agent.NetworkNone
					stronger, err := NewLocalExecutionSecurityStamp("local-scope", policy, time.Now())
					if err != nil {
						t.Fatal(err)
					}
					stamp = *stronger
				} else {
					stamp.Digest = "sha256:wrong"
				}
				work["executionSecurity"], _ = json.Marshal(stamp)
			case "missing-report", "wrong-plan-digest":
				host, err := executioncell.DecodeHostAdaptationReceipt(hostRaw)
				if err != nil {
					t.Fatal(err)
				}
				var plan agent.PreparedHarness
				if err = json.Unmarshal(host.Plan, &plan); err != nil {
					t.Fatal(err)
				}
				if mode == "missing-report" {
					plan.ExecutionSecurity = nil
					host.Plan, _ = json.Marshal(plan)
					host.PlanDigest = agent.DigestPreparedHarness(&plan)
				} else {
					host.PlanDigest = "sha256:wrong"
				}
				hostRaw, err = json.Marshal(host)
				if err != nil {
					t.Fatal(err)
				}
			}
			payload, _ = json.Marshal(work)
			if err := ValidateLocalExecutionSecurityEvidence(scope, payload, hostRaw); err == nil {
				t.Fatalf("%s was admitted", mode)
			}
		})
	}
}

func TestLocalAuthorityRefusesDeletedOrUnappliedConfiguration(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"deleted-file", "deleted-policy", "unapplied-policy"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "daemon.yaml")
			cfg := &Config{APIVersion: LocalRuntimeConfigAPIVersion, Machine: MachineConfig{ID: "local-host"}, Orchestrator: OrchestratorConfig{URL: "file:///tmp/local-owned-queue"}, LocalRuntime: &LocalRuntimeConfig{ExecutionSecurity: InitialLocalExecutionSecurity()}}
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			applied, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			d := New(Options{ConfigPath: path})
			d.config = applied
			local := &localRuntime{daemon: d}
			if err = local.checkCurrentConfiguration(); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "deleted-file":
				err = os.Remove(path)
			case "deleted-policy":
				cfg.LocalRuntime.ExecutionSecurity = nil
				raw, err = yaml.Marshal(cfg)
			case "unapplied-policy":
				cfg.LocalRuntime.ExecutionSecurity.Network = agent.NetworkNone
				raw, err = yaml.Marshal(cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			if change != "deleted-file" {
				if err = os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err = local.checkCurrentConfiguration(); err == nil {
				t.Fatal("stale applied configuration remained authoritative")
			}
			// No store or auth was initialized: refusing here must precede either access.
			if _, err = local.validateSecretRelease(SessionSpec{SessionID: "local-session"}); err == nil {
				t.Fatal("invalid own policy reached credential release")
			}
		})
	}
}
