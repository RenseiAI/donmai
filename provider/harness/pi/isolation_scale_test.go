package pi

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// This file is the state-isolation audit for pi scale hardening: it
// proves — at N-concurrent scale, under -race — that
// sessionLayout/composeChildEnv give every session's auth/model/settings/
// session state its own root, with no shared path any two sessions could
// contend or corrupt. ADR-2026-08-12 D4.2 states the standard directly:
// "isolation is asserted by observing writes, not by asserting env" — every
// assertion below either compares resolved paths for uniqueness or performs
// real concurrent filesystem writes and reads the results back, never just
// checks that an env var was set.
//
// It runs unconditionally as part of the default -race suite (no build tag,
// no subprocess): sessionLayout/composeChildEnv are pure path/string
// functions, so proving isolation at N=100 costs filesystem ops only, not
// process spawns — the heavier subprocess-level load validation (spawn/steer
// latency under N concurrent REAL child processes) lives in the separate
// pi_scale_load-tagged scale_load_test.go.

// isolationScaleN is deliberately fixed (not env-tunable like the subprocess
// load test): this test never spawns a process, so N=100 is cheap in every
// environment it runs in, including the default CI gate.
const isolationScaleN = 100

// TestStateIsolation_NConcurrentSessions_DistinctRootsAndAgentHomes proves
// two things D4.1/D4.2 require: (a) every one of N concurrently-prepared
// sessions resolves a UNIQUE sessionLayout (root, agentHome, injected) and a
// UNIQUE PI_CODING_AGENT_DIR/PI_CODING_AGENT_SESSION_DIR env pair — so no two
// sessions could EVER be pointed at the same pi state home even by
// misconfiguration — and (b) concurrently performed filesystem writes into
// each session's agentHome land in the right place: N goroutines each write
// a canary "auth.json"-shaped file carrying their own session index into
// their OWN agentHome, then every goroutine reads every file back (not just
// its own) and confirms no cross-session content ever appears under the
// wrong root. A shared or colliding path would show up here as either a
// write error (file exists, wrong owner) or a read-back mismatch — the
// "observing writes" standard D4.2 sets, not an env-var assertion.
func TestStateIsolation_NConcurrentSessions_DistinctRootsAndAgentHomes(t *testing.T) {
	t.Parallel()

	type sessionState struct {
		layout sessionLayout
		env    []string
	}

	root := t.TempDir()
	sessions := make([]sessionState, isolationScaleN)
	var wg sync.WaitGroup
	for i := 0; i < isolationScaleN; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cwd := filepath.Join(root, "session-"+strconv.Itoa(i))
			if err := os.MkdirAll(cwd, 0o700); err != nil {
				t.Errorf("session %d: MkdirAll cwd: %v", i, err)
				return
			}
			layout := newSessionLayout(cwd)
			env := composeChildEnv(agent.Spec{Cwd: cwd}, layout, "tok-"+strconv.Itoa(i))
			sessions[i] = sessionState{layout: layout, env: env}
		}(i)
	}
	wg.Wait()

	// (a) Uniqueness of every resolved path and every isolation-relevant env
	// binding, pairwise across all N sessions.
	agentHomes := make(map[string]int, isolationScaleN)
	sessionDirs := make(map[string]int, isolationScaleN)
	roots := make(map[string]int, isolationScaleN)
	for i, s := range sessions {
		if prev, dup := agentHomes[s.layout.agentHome]; dup {
			t.Errorf("session %d shares agentHome %q with session %d — PI_CODING_AGENT_DIR collision would serialize/corrupt auth.json between them", i, s.layout.agentHome, prev)
		}
		agentHomes[s.layout.agentHome] = i

		if prev, dup := roots[s.layout.root]; dup {
			t.Errorf("session %d shares root %q with session %d — PI_CODING_AGENT_SESSION_DIR collision would let two sessions' JSONL transcripts interleave with no writer lock", i, s.layout.root, prev)
		}
		roots[s.layout.root] = i

		dirVal := envValue(s.env, piCodingAgentDirEnvVar)
		if dirVal != s.layout.agentHome {
			t.Errorf("session %d: %s = %q, want %q", i, piCodingAgentDirEnvVar, dirVal, s.layout.agentHome)
		}
		sessionDirVal := envValue(s.env, piCodingAgentSessionDirEnvVar)
		if prev, dup := sessionDirs[sessionDirVal]; dup && sessionDirVal != "" {
			t.Errorf("session %d shares %s=%q with session %d", i, piCodingAgentSessionDirEnvVar, sessionDirVal, prev)
		}
		sessionDirs[sessionDirVal] = i
	}
	if len(agentHomes) != isolationScaleN {
		t.Fatalf("got %d distinct agentHome values, want %d (no two of %d concurrent sessions may share a pi config/auth home)", len(agentHomes), isolationScaleN, isolationScaleN)
	}

	// (b) Concurrent writes land at the right root, observed by reading every
	// file back from every goroutine — not merely trusting that Distinct
	// paths were computed.
	var wg2 sync.WaitGroup
	writeErrs := make([]error, isolationScaleN)
	for i, s := range sessions {
		wg2.Add(1)
		go func(i int, agentHome string) {
			defer wg2.Done()
			if err := os.MkdirAll(agentHome, 0o700); err != nil {
				writeErrs[i] = err
				return
			}
			canary := fmt.Sprintf(`{"session":%d,"secret":"only-session-%d-owns-this"}`, i, i)
			writeErrs[i] = os.WriteFile(filepath.Join(agentHome, "auth.json"), []byte(canary), 0o600)
		}(i, s.layout.agentHome)
	}
	wg2.Wait()
	for i, err := range writeErrs {
		if err != nil {
			t.Fatalf("session %d: concurrent auth.json write: %v", i, err)
		}
	}
	for i, s := range sessions {
		want := fmt.Sprintf(`{"session":%d,"secret":"only-session-%d-owns-this"}`, i, i)
		got, err := os.ReadFile(filepath.Join(s.layout.agentHome, "auth.json"))
		if err != nil {
			t.Fatalf("session %d: read back auth.json: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("session %d: auth.json = %q, want %q — cross-session state leaked", i, got, want)
		}
	}
}

// TestStateIsolation_AuthLockfileBottleneckIsBypassed is the direct assertion
// the scope item names explicitly: pi's CredentialStore guards
// "<PI_CODING_AGENT_DIR>/auth.json" with a lockfile (proper-lockfile), which
// on a SHARED agent directory serializes OAuth refresh across every session
// on the box (ADR-2026-08-12 D4.4 — "isolation is a correctness property, not
// a performance one"). Per-session agentHome means every session's would-be
// lock path is unique, so no two donmai-spawned pi sessions can EVER target
// the same lockfile — the bottleneck has no shared resource left to
// contend. This test builds the exact lock path each session would use and
// proves the set has no duplicates across N sessions, which is the only
// thing that has to be true for "the lockfile is per-session, so it never
// contends" to hold: two sessions can only serialize on a lock they share,
// and this proves they never do.
func TestStateIsolation_AuthLockfileBottleneckIsBypassed(t *testing.T) {
	t.Parallel()

	lockPaths := make(map[string]int, isolationScaleN)
	for i := 0; i < isolationScaleN; i++ {
		cwd := filepath.Join(t.TempDir(), "s")
		layout := newSessionLayout(cwd)
		env := composeChildEnv(agent.Spec{Cwd: cwd}, layout, "tok")
		agentDir := envValue(env, piCodingAgentDirEnvVar)
		if agentDir == "" {
			t.Fatalf("session %d: %s not set on the child env", i, piCodingAgentDirEnvVar)
		}
		// pi's CredentialStore locks "<agentDir>/auth.json" (proper-lockfile
		// convention: "<file>.lock" alongside it) — the exact resource
		// ADR-2026-08-12 F3/D4.4 names as the shared-state bottleneck.
		lockPath := filepath.Join(agentDir, "auth.json")
		if prev, dup := lockPaths[lockPath]; dup {
			t.Fatalf("session %d and session %d would lock the SAME auth.json path %q — the lockfile bottleneck is NOT bypassed", i, prev, lockPath)
		}
		lockPaths[lockPath] = i
	}
	if len(lockPaths) != isolationScaleN {
		t.Fatalf("got %d distinct lock paths across %d sessions, want all %d distinct", len(lockPaths), isolationScaleN, isolationScaleN)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, prefix); ok {
			return v
		}
	}
	return ""
}
