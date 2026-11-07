package daemon

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testManagedCertPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "managed-test-root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	var buf strings.Builder
	if err := pem.Encode(&stringWriter{&buf}, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode pem: %v", err)
	}
	return buf.String()
}

type stringWriter struct {
	s *strings.Builder
}

func (w *stringWriter) Write(p []byte) (int, error) {
	return w.s.Write(p)
}

func testManagedPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	var buf strings.Builder
	if err := pem.Encode(&stringWriter{&buf}, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	return buf.String()
}

func buildManagedPolicyJSON(t *testing.T, signers []managedSignerWire, roots []string) []byte {
	t.Helper()
	wire := managedPolicyWire{Version: ManagedUpdatePolicyVersion, Signers: signers, PublicRootsPem: roots}
	if wire.PublicRootsPem == nil {
		wire.PublicRootsPem = []string{}
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	return raw
}

func validManagedSigners() []managedSignerWire {
	return []managedSignerWire{{Issuer: "https://issuer.example.com", Subject: "releases@example.com"}}
}

func TestManagedUpdatePolicyRoundTripSurvivesRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, managedUpdatePolicyFileName)
	store := NewManagedUpdatePolicyStore(path)
	cert := testManagedCertPEM(t)
	raw := buildManagedPolicyJSON(t, validManagedSigners(), []string{cert})
	digest := DigestManagedUpdatePolicy(raw)

	changed, err := store.Apply(raw, digest)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !changed {
		t.Fatal("first Apply should report stored")
	}
	got, gotDigest, ok := store.Snapshot()
	if !ok {
		t.Fatal("Snapshot should be present after Apply")
	}
	if gotDigest != digest || string(got) != string(raw) {
		t.Fatalf("Snapshot mismatch: digest %q want %q", gotDigest, digest)
	}

	// Restart: a fresh store over the same path reloads byte-for-byte.
	restarted := NewManagedUpdatePolicyStore(path)
	if err := restarted.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got2, digest2, ok := restarted.Snapshot()
	if !ok {
		t.Fatal("restarted Snapshot should be present")
	}
	if digest2 != digest || string(got2) != string(raw) {
		t.Fatal("restarted snapshot differs from applied bytes")
	}

	// Duplicate delivery performs no rewrite: mtime is unchanged and Apply
	// reports unchanged.
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	changed, err = restarted.Apply(raw, digest)
	if err != nil {
		t.Fatalf("duplicate Apply: %v", err)
	}
	if changed {
		t.Fatal("duplicate Apply should report unchanged")
	}
	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatal("duplicate Apply rewrote the durable record")
	}
}

func TestManagedUpdatePolicyRejectsBadInputs(t *testing.T) {
	t.Parallel()
	cert := testManagedCertPEM(t)
	priv := testManagedPrivateKeyPEM(t)

	cases := []struct {
		name    string
		mutate  func(t *testing.T) ([]byte, string)
		wantSub string
	}{
		{
			name: "wrong digest",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, validManagedSigners(), []string{})
				return raw, strings.Repeat("0", 64)
			},
			wantSub: "digest mismatch",
		},
		{
			name: "uppercase digest",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, validManagedSigners(), []string{})
				return raw, strings.ToUpper(DigestManagedUpdatePolicy(raw))
			},
			wantSub: "lowercase",
		},
		{
			name: "oversize policy",
			mutate: func(_ *testing.T) ([]byte, string) {
				big := strings.Repeat("a", ManagedUpdatePolicyMaxBytes)
				raw := []byte(`{"version":1,"signers":[{"issuer":"https://i.example","subject":"` + big + `"}],"publicRootsPem":[]}`)
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "exceeds",
		},
		{
			name: "unknown top-level key",
			mutate: func(_ *testing.T) ([]byte, string) {
				raw := []byte(`{"version":1,"signers":[{"issuer":"https://i.example","subject":"s"}],"publicRootsPem":[],"extra":1}`)
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "malformed policy",
		},
		{
			name: "unknown signer key",
			mutate: func(_ *testing.T) ([]byte, string) {
				raw := []byte(`{"version":1,"signers":[{"issuer":"https://i.example","subject":"s","sanRegex":".*"}],"publicRootsPem":[]}`)
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "malformed policy",
		},
		{
			name: "wrong version",
			mutate: func(_ *testing.T) ([]byte, string) {
				raw := []byte(`{"version":2,"signers":[{"issuer":"https://i.example","subject":"s"}],"publicRootsPem":[]}`)
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "unsupported version",
		},
		{
			name: "no signers",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, nil, []string{})
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "signers must hold",
		},
		{
			name: "empty issuer",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, []managedSignerWire{{Issuer: "", Subject: "s"}}, []string{})
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "empty issuer",
		},
		{
			name: "empty subject",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, []managedSignerWire{{Issuer: "https://i.example", Subject: "  "}}, []string{})
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "empty issuer or subject",
		},
		{
			name: "wildcard subject",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, []managedSignerWire{{Issuer: "https://i.example", Subject: "releases*"}}, []string{})
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "wildcards",
		},
		{
			name: "wildcard issuer",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, []managedSignerWire{{Issuer: "https://*.example", Subject: "s"}}, []string{})
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "wildcards",
		},
		{
			name: "non-https issuer",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, []managedSignerWire{{Issuer: "http://i.example", Subject: "s"}}, []string{})
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "invalid issuer",
		},
		{
			name: "too many signers",
			mutate: func(t *testing.T) ([]byte, string) {
				signers := make([]managedSignerWire, ManagedUpdatePolicyMaxSigners+1)
				for i := range signers {
					signers[i] = managedSignerWire{Issuer: "https://i.example", Subject: "s"}
				}
				raw := buildManagedPolicyJSON(t, signers, []string{})
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "signers must hold",
		},
		{
			name: "missing publicRootsPem",
			mutate: func(_ *testing.T) ([]byte, string) {
				raw := []byte(`{"version":1,"signers":[{"issuer":"https://i.example","subject":"s"}]}`)
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "publicRootsPem is required",
		},
		{
			name: "too many roots",
			mutate: func(t *testing.T) ([]byte, string) {
				roots := make([]string, ManagedUpdatePolicyMaxRoots+1)
				for i := range roots {
					roots[i] = cert
				}
				raw := buildManagedPolicyJSON(t, validManagedSigners(), roots)
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "publicRootsPem must hold",
		},
		{
			name: "non-pem root",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, validManagedSigners(), []string{"not-a-pem"})
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "invalid root",
		},
		{
			name: "private key root",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, validManagedSigners(), []string{priv})
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "private-key",
		},
		{
			name: "empty root entry",
			mutate: func(t *testing.T) ([]byte, string) {
				raw := buildManagedPolicyJSON(t, validManagedSigners(), []string{""})
				return raw, DigestManagedUpdatePolicy(raw)
			},
			wantSub: "invalid root",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, managedUpdatePolicyFileName)
			store := NewManagedUpdatePolicyStore(path)
			// Seed a valid snapshot first so the test proves the prior
			// record is retained on refusal.
			seed := buildManagedPolicyJSON(t, validManagedSigners(), []string{})
			seedDigest := DigestManagedUpdatePolicy(seed)
			if _, err := store.Apply(seed, seedDigest); err != nil {
				t.Fatalf("seed Apply: %v", err)
			}
			raw, digest := tc.mutate(t)
			changed, err := store.Apply(raw, digest)
			if err == nil {
				t.Fatalf("Apply should refuse %s", tc.name)
			}
			if !errors.Is(err, ErrManagedUpdatePolicyRefused) {
				t.Fatalf("err %v should wrap typed refusal", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err %q should contain %q", err.Error(), tc.wantSub)
			}
			if changed {
				t.Fatal("refused Apply must not report stored")
			}
			got, gotDigest, ok := store.Snapshot()
			if !ok || gotDigest != seedDigest || string(got) != string(seed) {
				t.Fatal("prior record was not retained after refusal")
			}
		})
	}
}

func TestManagedUpdatePolicyEmptyRootsMeansNoAdditionalRoots(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManagedUpdatePolicyStore(filepath.Join(dir, managedUpdatePolicyFileName))
	raw := buildManagedPolicyJSON(t, validManagedSigners(), []string{})
	digest := DigestManagedUpdatePolicy(raw)
	if _, err := store.Apply(raw, digest); err != nil {
		t.Fatalf("Apply with empty roots: %v", err)
	}
	parsed, err := ParseManagedUpdatePolicy(raw, digest)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.PublicRootsPem) != 0 {
		t.Fatalf("roots = %d, want 0", len(parsed.PublicRootsPem))
	}
}

func TestManagedUpdatePolicyPersistFailureRetainsPrior(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, managedUpdatePolicyFileName)
	store := NewManagedUpdatePolicyStore(path)
	seed := buildManagedPolicyJSON(t, validManagedSigners(), []string{})
	seedDigest := DigestManagedUpdatePolicy(seed)
	if _, err := store.Apply(seed, seedDigest); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	injected := errors.New("injected durability failure")
	store.persist = func(string, []byte) error { return injected }

	next := buildManagedPolicyJSON(t, []managedSignerWire{{Issuer: "https://other.example", Subject: "other@example.com"}}, []string{})
	nextDigest := DigestManagedUpdatePolicy(next)
	changed, err := store.Apply(next, nextDigest)
	if err == nil {
		t.Fatal("Apply should refuse on persist failure")
	}
	if !errors.Is(err, ErrManagedUpdatePolicyRefused) {
		t.Fatalf("err %v should wrap typed refusal", err)
	}
	if changed {
		t.Fatal("failed Apply must not report stored")
	}
	got, gotDigest, ok := store.Snapshot()
	if !ok || gotDigest != seedDigest || string(got) != string(seed) {
		t.Fatal("prior record was not retained after persist failure")
	}
	// The failed write must not have replaced the durable file either.
	restarted := NewManagedUpdatePolicyStore(path)
	if err := restarted.Load(); err != nil {
		t.Fatalf("Load after failed Apply: %v", err)
	}
	got2, digest2, ok := restarted.Snapshot()
	if !ok || digest2 != seedDigest || string(got2) != string(seed) {
		t.Fatal("durable record was disturbed by failed Apply")
	}
}

func TestManagedUpdatePolicyConcurrentReadsSeeCompleteSnapshots(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, managedUpdatePolicyFileName)
	store := NewManagedUpdatePolicyStore(path)
	oldRaw := buildManagedPolicyJSON(t, validManagedSigners(), []string{})
	oldDigest := DigestManagedUpdatePolicy(oldRaw)
	if _, err := store.Apply(oldRaw, oldDigest); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	cert := testManagedCertPEM(t)
	newRaw := buildManagedPolicyJSON(t, []managedSignerWire{{Issuer: "https://new.example", Subject: "new@example.com"}}, []string{cert})
	newDigest := DigestManagedUpdatePolicy(newRaw)

	// Slow the commit so readers race the replacement window.
	release := make(chan struct{})
	origPersist := writeManagedUpdatePolicyFile
	store.persist = func(p string, data []byte) error {
		<-release
		return origPersist(p, data)
	}

	var wg sync.WaitGroup
	observedOld := make(chan bool, 64)
	observedNew := make(chan bool, 64)
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, digest, ok := store.Snapshot()
				if !ok {
					t.Errorf("Snapshot should always be present during race")
					return
				}
				switch digest {
				case oldDigest:
					if string(got) != string(oldRaw) {
						t.Errorf("mixed pair: old digest with foreign bytes")
						return
					}
					select {
					case observedOld <- true:
					default:
					}
				case newDigest:
					if string(got) != string(newRaw) {
						t.Errorf("mixed pair: new digest with foreign bytes")
						return
					}
					select {
					case observedNew <- true:
					default:
					}
				default:
					t.Errorf("Snapshot observed unknown digest %q", digest)
					return
				}
			}
		}()
	}

	applyErr := make(chan error, 1)
	go func() {
		_, err := store.Apply(newRaw, newDigest)
		applyErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-applyErr; err != nil {
		t.Fatalf("Apply: %v", err)
	}
	close(stop)
	wg.Wait()
	if len(observedOld) == 0 {
		t.Error("expected some readers to observe the old snapshot")
	}
	if len(observedNew) == 0 {
		t.Error("expected some readers to observe the new snapshot")
	}

	// A restart after the race observes exactly one complete snapshot.
	restarted := NewManagedUpdatePolicyStore(path)
	if err := restarted.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, digest, ok := restarted.Snapshot()
	if !ok {
		t.Fatal("restarted snapshot missing")
	}
	if digest != newDigest || string(got) != string(newRaw) {
		t.Fatal("restarted snapshot is not the complete new pair")
	}
}

func TestManagedUpdatePolicyCorruptStateRefusesThenRecovers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	corruptions := []struct {
		name string
		data string
	}{
		{"truncated json", "{corrupt"},
		{"missing digest", `{"digest":"","policyBase64":"` + base64.StdEncoding.EncodeToString([]byte("{}")) + `"}`},
		{"bad base64", `{"digest":"` + strings.Repeat("0", 64) + `","policyBase64":"!!!"}`},
		{"digest mismatch", `{"digest":"` + strings.Repeat("0", 64) + `","policyBase64":"` + base64.StdEncoding.EncodeToString(buildManagedPolicyJSON(t, validManagedSigners(), []string{})) + `"}`},
	}
	for _, c := range corruptions {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sub := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-"), managedUpdatePolicyFileName)
			if err := os.MkdirAll(filepath.Dir(sub), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(sub, []byte(c.data), 0o600); err != nil {
				t.Fatalf("write corrupt: %v", err)
			}
			store := NewManagedUpdatePolicyStore(sub)
			if err := store.Load(); err == nil {
				t.Fatal("Load of corrupt state should refuse")
			} else if !errors.Is(err, ErrManagedUpdatePolicyRefused) {
				t.Fatalf("err %v should wrap typed refusal", err)
			}
			if _, _, ok := store.Snapshot(); ok {
				t.Fatal("corrupt state must refuse managed use (no snapshot)")
			}
			// A subsequent valid application recovers.
			raw := buildManagedPolicyJSON(t, validManagedSigners(), []string{})
			digest := DigestManagedUpdatePolicy(raw)
			changed, err := store.Apply(raw, digest)
			if err != nil {
				t.Fatalf("recovering Apply: %v", err)
			}
			if !changed {
				t.Fatal("recovering Apply should report stored")
			}
			got, gotDigest, ok := store.Snapshot()
			if !ok || gotDigest != digest || string(got) != string(raw) {
				t.Fatal("recovered snapshot mismatch")
			}
		})
	}

	// Absent file is not a refusal: Load succeeds with no snapshot.
	absent := NewManagedUpdatePolicyStore(filepath.Join(dir, "absent", managedUpdatePolicyFileName))
	if err := absent.Load(); err != nil {
		t.Fatalf("absent Load: %v", err)
	}
	if _, _, ok := absent.Snapshot(); ok {
		t.Fatal("absent store should have no snapshot")
	}
}

func TestManagedUpdatePolicyStatePathIsDaemonOwned(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "daemon.yaml")
	want := filepath.Join(filepath.Dir(cfgPath), managedUpdatePolicyFileName)
	if got := ManagedUpdatePolicyStatePath(cfgPath); got != want {
		t.Fatalf("StatePath = %q, want %q", got, want)
	}
	if got := ManagedUpdatePolicyStatePath(""); got == "" {
		t.Fatal("empty config path should fall back to brand state dir")
	}
	if got := DefaultManagedUpdatePolicyPath(); got == "" || !strings.HasSuffix(got, managedUpdatePolicyFileName) {
		t.Fatalf("default path = %q, want suffix %q", got, managedUpdatePolicyFileName)
	}
}
