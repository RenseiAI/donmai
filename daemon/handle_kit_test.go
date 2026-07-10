package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

// fakeKitRegistry is a programmable kitRegistryDoer used by handler tests.
type fakeKitRegistry struct {
	kits     []afclient.Kit
	manifest map[string]afclient.KitManifest
	sources  []afclient.KitRegistrySource

	enableErr        error
	disableErr       error
	getErr           error
	verifyErr        error
	installErr       error
	enableSourceErr  error
	disableSourceErr error
}

func (f *fakeKitRegistry) List() []afclient.Kit { return f.kits }

func (f *fakeKitRegistry) Get(id string) (afclient.KitManifest, error) {
	if f.getErr != nil {
		return afclient.KitManifest{}, f.getErr
	}
	if m, ok := f.manifest[id]; ok {
		return m, nil
	}
	return afclient.KitManifest{}, fmt.Errorf("%s: %w", id, ErrKitNotFound)
}

func (f *fakeKitRegistry) Enable(id string) (afclient.Kit, error) {
	if f.enableErr != nil {
		return afclient.Kit{}, f.enableErr
	}
	for _, k := range f.kits {
		if k.ID == id {
			k.Status = afclient.KitStatusActive
			return k, nil
		}
	}
	return afclient.Kit{}, fmt.Errorf("%s: %w", id, ErrKitNotFound)
}

func (f *fakeKitRegistry) Disable(id string) (afclient.Kit, error) {
	if f.disableErr != nil {
		return afclient.Kit{}, f.disableErr
	}
	for _, k := range f.kits {
		if k.ID == id {
			k.Status = afclient.KitStatusDisabled
			return k, nil
		}
	}
	return afclient.Kit{}, fmt.Errorf("%s: %w", id, ErrKitNotFound)
}

func (f *fakeKitRegistry) VerifySignature(id string) (afclient.KitSignatureResult, error) {
	if f.verifyErr != nil {
		return afclient.KitSignatureResult{}, f.verifyErr
	}
	for _, k := range f.kits {
		if k.ID == id {
			return afclient.KitSignatureResult{KitID: id, Trust: afclient.KitTrustUnsigned, OK: true}, nil
		}
	}
	return afclient.KitSignatureResult{}, fmt.Errorf("%s: %w", id, ErrKitNotFound)
}

func (f *fakeKitRegistry) Install(id string, _ afclient.KitInstallRequest) (afclient.KitInstallResult, error) {
	if f.installErr != nil {
		return afclient.KitInstallResult{}, f.installErr
	}
	return afclient.KitInstallResult{Kit: afclient.Kit{ID: id}, Message: "installed"}, nil
}

func (f *fakeKitRegistry) ListSources() []afclient.KitRegistrySource { return f.sources }

func (f *fakeKitRegistry) EnableSource(name string) (afclient.KitRegistrySource, error) {
	if f.enableSourceErr != nil {
		return afclient.KitRegistrySource{}, f.enableSourceErr
	}
	for _, s := range f.sources {
		if s.Name == name {
			s.Enabled = true
			return s, nil
		}
	}
	return afclient.KitRegistrySource{}, fmt.Errorf("%s: %w", name, ErrKitSourceNotFound)
}

func (f *fakeKitRegistry) DisableSource(name string) (afclient.KitRegistrySource, error) {
	if f.disableSourceErr != nil {
		return afclient.KitRegistrySource{}, f.disableSourceErr
	}
	for _, s := range f.sources {
		if s.Name == name {
			s.Enabled = false
			return s, nil
		}
	}
	return afclient.KitRegistrySource{}, fmt.Errorf("%s: %w", name, ErrKitSourceNotFound)
}

// kitTestServer wires a Server with a fake KitRegistry into an httptest server.
func kitTestServer(t *testing.T, fake *fakeKitRegistry) *httptest.Server {
	t.Helper()
	s := &Server{kitReg: fake}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/kits", s.handleKitsCollection)
	mux.HandleFunc(kitRoutePrefix, s.handleKitDetail)
	mux.HandleFunc("/api/daemon/kit-sources", s.handleKitSourcesCollection)
	mux.HandleFunc(kitSourceRoutePrefix, s.handleKitSourceDetail)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHandleKits_ListEmpty(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	resp, err := http.Get(srv.URL + "/api/daemon/kits")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	var got afclient.ListKitsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kits == nil {
		t.Error("Kits: want non-nil empty slice (not null)")
	}
	if len(got.Kits) != 0 {
		t.Errorf("Kits: want empty, got %d", len(got.Kits))
	}
}

func TestHandleKits_ListPopulated(t *testing.T) {
	fake := &fakeKitRegistry{
		kits: []afclient.Kit{
			{ID: "spring/java", Name: "Spring", Version: "1.0", Status: afclient.KitStatusActive, Source: afclient.KitSourceLocal},
			{ID: "next-js", Name: "Next.js", Version: "2.0", Status: afclient.KitStatusDisabled, Source: afclient.KitSourceBundled},
		},
	}
	srv := kitTestServer(t, fake)
	resp, _ := http.Get(srv.URL + "/api/daemon/kits")
	defer func() { _ = resp.Body.Close() }()

	var got afclient.ListKitsResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Kits) != 2 {
		t.Fatalf("Kits: want 2, got %d", len(got.Kits))
	}
	if got.Kits[0].ID != "spring/java" {
		t.Errorf("Kits[0].ID: want spring/java, got %q", got.Kits[0].ID)
	}
}

func TestHandleKits_MethodNotAllowed(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/daemon/kits", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: want 405, got %d", resp.StatusCode)
	}
}

func TestHandleKits_GetKnown(t *testing.T) {
	fake := &fakeKitRegistry{
		manifest: map[string]afclient.KitManifest{
			"spring/java": {
				Kit:           afclient.Kit{ID: "spring/java", Name: "Spring", Version: "1.0"},
				SupportedOS:   []string{"linux", "macos"},
				SupportedArch: []string{"x86_64"},
			},
		},
	}
	srv := kitTestServer(t, fake)

	resp, _ := http.Get(srv.URL + "/api/daemon/kits/spring/java")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	var got afclient.KitManifestEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Kit.ID != "spring/java" {
		t.Errorf("ID: want spring/java, got %q", got.Kit.ID)
	}
	if len(got.Kit.SupportedOS) != 2 {
		t.Errorf("SupportedOS: want 2, got %d", len(got.Kit.SupportedOS))
	}
}

func TestHandleKits_GetUnknown(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	resp, _ := http.Get(srv.URL + "/api/daemon/kits/does/not/exist")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
}

func TestHandleKits_VerifySignature(t *testing.T) {
	fake := &fakeKitRegistry{kits: []afclient.Kit{{ID: "spring/java"}}}
	srv := kitTestServer(t, fake)
	resp, _ := http.Get(srv.URL + "/api/daemon/kits/spring/java/verify-signature")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	var got afclient.KitSignatureResult
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.KitID != "spring/java" {
		t.Errorf("KitID: want spring/java, got %q", got.KitID)
	}
	if got.Trust != afclient.KitTrustUnsigned {
		t.Errorf("Trust: want unsigned, got %q", got.Trust)
	}
}

func TestHandleKits_VerifySignatureUnknown(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	resp, _ := http.Get(srv.URL + "/api/daemon/kits/nope/verify-signature")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
}

func TestHandleKits_InstallStub501(t *testing.T) {
	fake := &fakeKitRegistry{installErr: ErrKitInstallUnimplemented}
	srv := kitTestServer(t, fake)
	resp, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/install", "application/json", strings.NewReader(`{"version":"1.0"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status: want 501, got %d", resp.StatusCode)
	}
}

func TestHandleKits_InstallFederationUnimplemented501(t *testing.T) {
	fake := &fakeKitRegistry{installErr: ErrKitSourceFederationUnimplemented}
	srv := kitTestServer(t, fake)
	resp, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/install", "application/json",
		strings.NewReader(`{"source":{"kind":"tessl","url":"https://registry.tessl.io/spring"}}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status: want 501, got %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["kitId"] != "spring/java" {
		t.Errorf("body.kitId: want spring/java, got %q", body["kitId"])
	}
	if body["kind"] != "tessl" {
		t.Errorf("body.kind: want tessl, got %q", body["kind"])
	}
	if body["details"] == "" {
		t.Errorf("body.details: want non-empty, got empty")
	}
}

func TestHandleKits_InstallSourceFetchFailed502(t *testing.T) {
	fake := &fakeKitRegistry{installErr: ErrKitInstallSourceFetchFailed}
	srv := kitTestServer(t, fake)
	resp, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/install", "application/json",
		strings.NewReader(`{"source":{"kind":"git","url":"file:///nope"}}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: want 502, got %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["kitId"] != "spring/java" {
		t.Errorf("body.kitId: want spring/java, got %q", body["kitId"])
	}
	if body["source"] != "file:///nope" {
		t.Errorf("body.source: want file:///nope, got %q", body["source"])
	}
}

func TestHandleKits_InstallManifestNotFound422(t *testing.T) {
	fake := &fakeKitRegistry{installErr: ErrKitInstallManifestNotFound}
	srv := kitTestServer(t, fake)
	resp, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/install", "application/json",
		strings.NewReader(`{"source":{"kind":"git","url":"file:///empty"}}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: want 422, got %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["kitId"] != "spring/java" {
		t.Errorf("body.kitId: want spring/java, got %q", body["kitId"])
	}
}

func TestHandleKits_InstallTrustGateRejected403(t *testing.T) {
	fake := &fakeKitRegistry{installErr: ErrKitTrustGateRejected}
	srv := kitTestServer(t, fake)
	resp, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/install", "application/json", strings.NewReader(`{}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: want 403, got %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["kitId"] != "spring/java" {
		t.Errorf("body.kitId: want spring/java, got %q", body["kitId"])
	}
	if body["trust"] != string(afclient.KitTrustSignedUnverified) {
		t.Errorf("body.trust: want %q, got %q", afclient.KitTrustSignedUnverified, body["trust"])
	}
	if body["error"] == "" {
		t.Errorf("body.error: want non-empty, got empty")
	}
}

func TestHandleKits_InstallPackageTrustGatePreservesPackageState(t *testing.T) {
	installErr := trustGateRejectionError("default/go", afclient.KitSignatureResult{
		KitID: "default/go",
		Trust: afclient.KitTrustPackageSignedUnverified,
	}, TrustModeSignedByAllowlist)
	fake := &fakeKitRegistry{installErr: installErr}
	srv := kitTestServer(t, fake)
	resp, err := http.Post(srv.URL+"/api/daemon/kits/default/go/install", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden || body["trust"] != string(afclient.KitTrustPackageSignedUnverified) {
		t.Fatalf("status/body = %d %+v", resp.StatusCode, body)
	}
}

func TestHandleKits_InstallPackageErrorsMapToContractStatuses(t *testing.T) {
	tests := []struct {
		err    error
		status int
	}{
		{ErrKitPackageInvalid, http.StatusUnprocessableEntity},
		{ErrKitPackageEquivocation, http.StatusConflict},
		{ErrKitPackageConflict, http.StatusConflict},
	}
	for _, test := range tests {
		fake := &fakeKitRegistry{installErr: test.err}
		srv := kitTestServer(t, fake)
		resp, err := http.Post(srv.URL+"/api/daemon/kits/default/go/install", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		srv.Close()
		if resp.StatusCode != test.status {
			t.Errorf("%v status = %d, want %d", test.err, resp.StatusCode, test.status)
		}
	}
}

func TestHandleKits_InstallEmptyBody(t *testing.T) {
	fake := &fakeKitRegistry{}
	srv := kitTestServer(t, fake)
	resp, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/install", "application/json", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestHandleKits_InstallBadBody(t *testing.T) {
	fake := &fakeKitRegistry{}
	srv := kitTestServer(t, fake)
	resp, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/install", "application/json", strings.NewReader(`not json`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
}

func TestHandleKits_EnableDisable(t *testing.T) {
	fake := &fakeKitRegistry{kits: []afclient.Kit{{ID: "spring/java"}}}
	srv := kitTestServer(t, fake)

	resp, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/enable", "application/json", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable status: want 200, got %d", resp.StatusCode)
	}
	var k afclient.Kit
	_ = json.NewDecoder(resp.Body).Decode(&k)
	if k.Status != afclient.KitStatusActive {
		t.Errorf("enable status: want active, got %q", k.Status)
	}

	resp2, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/disable", "application/json", nil)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("disable status: want 200, got %d", resp2.StatusCode)
	}
	_ = json.NewDecoder(resp2.Body).Decode(&k)
	if k.Status != afclient.KitStatusDisabled {
		t.Errorf("disable status: want disabled, got %q", k.Status)
	}
}

func TestHandleKits_EnableDisableUnknown(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	resp, _ := http.Post(srv.URL+"/api/daemon/kits/nope/enable", "application/json", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("enable unknown: want 404, got %d", resp.StatusCode)
	}
	resp2, _ := http.Post(srv.URL+"/api/daemon/kits/nope/disable", "application/json", nil)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("disable unknown: want 404, got %d", resp2.StatusCode)
	}
}

func TestHandleKits_EnableDisableInternalError(t *testing.T) {
	fake := &fakeKitRegistry{enableErr: errors.New("disk full")}
	srv := kitTestServer(t, fake)
	resp, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/enable", "application/json", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status: want 500, got %d", resp.StatusCode)
	}
}

func TestHandleKits_DetailMethodMismatch(t *testing.T) {
	fake := &fakeKitRegistry{kits: []afclient.Kit{{ID: "spring/java"}}, manifest: map[string]afclient.KitManifest{"spring/java": {Kit: afclient.Kit{ID: "spring/java"}}}}
	srv := kitTestServer(t, fake)

	cases := []struct {
		name, method, path string
	}{
		{"get-with-post", http.MethodPost, "/api/daemon/kits/spring/java"},
		{"verify-with-post", http.MethodPost, "/api/daemon/kits/spring/java/verify-signature"},
		{"install-with-get", http.MethodGet, "/api/daemon/kits/spring/java/install"},
		{"enable-with-get", http.MethodGet, "/api/daemon/kits/spring/java/enable"},
		{"disable-with-get", http.MethodGet, "/api/daemon/kits/spring/java/disable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
			resp, _ := http.DefaultClient.Do(req)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status: want 405, got %d", resp.StatusCode)
			}
		})
	}
}

func TestHandleKits_DetailUnknownAction(t *testing.T) {
	fake := &fakeKitRegistry{
		manifest: map[string]afclient.KitManifest{"spring/java/foo": {Kit: afclient.Kit{ID: "spring/java/foo"}}},
	}
	srv := kitTestServer(t, fake)
	// "foo" isn't a known action; fall through to kit-by-id GET.
	resp, _ := http.Get(srv.URL + "/api/daemon/kits/spring/java/foo")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200 (unknown suffix → id lookup), got %d", resp.StatusCode)
	}
}

func TestHandleKits_DetailEmptyPath(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	resp, _ := http.Get(srv.URL + "/api/daemon/kits/")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
}

// ── kit-sources ──────────────────────────────────────────────────────────

func TestHandleKitSources_ListEmpty(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	resp, _ := http.Get(srv.URL + "/api/daemon/kit-sources")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	var got afclient.ListKitSourcesResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Sources == nil {
		t.Error("Sources: want non-nil")
	}
}

func TestHandleKitSources_List(t *testing.T) {
	fake := &fakeKitRegistry{
		sources: []afclient.KitRegistrySource{
			{Name: "local", Enabled: true, Priority: 1, Kind: "local"},
			{Name: "tessl", Enabled: false, Priority: 4, Kind: "tessl"},
		},
	}
	srv := kitTestServer(t, fake)
	resp, _ := http.Get(srv.URL + "/api/daemon/kit-sources")
	defer func() { _ = resp.Body.Close() }()
	var got afclient.ListKitSourcesResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Sources) != 2 {
		t.Errorf("Sources: want 2, got %d", len(got.Sources))
	}
}

func TestHandleKitSources_MethodNotAllowed(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/daemon/kit-sources", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: want 405, got %d", resp.StatusCode)
	}
}

func TestHandleKitSources_EnableDisable(t *testing.T) {
	fake := &fakeKitRegistry{
		sources: []afclient.KitRegistrySource{{Name: "tessl", Kind: "tessl"}},
	}
	srv := kitTestServer(t, fake)
	resp, _ := http.Post(srv.URL+"/api/daemon/kit-sources/tessl/enable", "application/json", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable status: want 200, got %d", resp.StatusCode)
	}
	var got afclient.KitSourceToggleResult
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if !got.Source.Enabled {
		t.Error("Enabled: want true")
	}

	resp2, _ := http.Post(srv.URL+"/api/daemon/kit-sources/tessl/disable", "application/json", nil)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("disable status: want 200, got %d", resp2.StatusCode)
	}
}

func TestHandleKitSources_UnknownName(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	resp, _ := http.Post(srv.URL+"/api/daemon/kit-sources/nope/enable", "application/json", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
}

func TestHandleKitSources_DetailMethodMismatch(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	resp, _ := http.Get(srv.URL + "/api/daemon/kit-sources/tessl/enable")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: want 405, got %d", resp.StatusCode)
	}
}

func TestHandleKitSources_DetailEmptyOrUnknownAction(t *testing.T) {
	srv := kitTestServer(t, &fakeKitRegistry{})
	resp, _ := http.Post(srv.URL+"/api/daemon/kit-sources/tessl", "application/json", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
	resp2, _ := http.Post(srv.URL+"/api/daemon/kit-sources/tessl/foo", "application/json", nil)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404 for unknown action, got %d", resp2.StatusCode)
	}
}

func TestHandleKitSources_InternalError(t *testing.T) {
	fake := &fakeKitRegistry{enableSourceErr: errors.New("disk full")}
	srv := kitTestServer(t, fake)
	resp, _ := http.Post(srv.URL+"/api/daemon/kit-sources/tessl/enable", "application/json", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status: want 500, got %d", resp.StatusCode)
	}
}

// TestSplitKitPath pins the path-suffix dispatcher's expected behaviour
// for slash-bearing kit ids like spring/java.
func TestSplitKitPath(t *testing.T) {
	cases := []struct {
		in, wantID, wantAction string
	}{
		{"spring/java", "spring/java", ""},
		{"spring/java/enable", "spring/java", "enable"},
		{"spring/java/disable", "spring/java", "disable"},
		{"spring/java/install", "spring/java", "install"},
		{"spring/java/verify-signature", "spring/java", "verify-signature"},
		{"a/b/c/install", "a/b/c", "install"},
		{"x", "x", ""},
		{"", "", ""},
		{"x/foo", "x/foo", ""}, // unknown verb falls back to id
	}
	for _, c := range cases {
		gotID, gotAction := splitKitPath(c.in)
		if gotID != c.wantID || gotAction != c.wantAction {
			t.Errorf("splitKitPath(%q) = (%q, %q); want (%q, %q)",
				c.in, gotID, gotAction, c.wantID, c.wantAction)
		}
	}
}

// TestKitRegistryOrEmpty_LazyConstruct verifies the lazy registry
// fallback produces a usable registry when no fake is injected.
func TestKitRegistryOrEmpty_LazyConstruct(t *testing.T) {
	s := &Server{}
	reg := s.kitRegistryOrEmpty()
	if reg == nil {
		t.Fatal("registry: want non-nil after lazy construction")
	}
	// Calling again returns the same registry (cache hit).
	if s.kitRegistryOrEmpty() != reg {
		t.Error("registry: want cached instance on second call")
	}
}

// TestKitRegistryOrEmpty_HonorsConfigScanPaths is the Wave 11 / S4
// wire-up regression: a daemon constructed with daemon.yaml's
// `kit.scanPaths` populated must surface those paths through the
// lazy-constructed KitRegistry. Before this wave, kitRegistryOrEmpty
// hardcoded [DefaultKitScanPath()] regardless of operator config.
func TestKitRegistryOrEmpty_HonorsConfigScanPaths(t *testing.T) {
	// Pin permissive mode so the lazy construction yields a real
	// *KitRegistry — this test is about scan-path plumbing, not the
	// trust gate (covered by TestKitRegistryOrEmpty_DefaultTrust*).
	t.Setenv(envKitTrustMode, string(TrustModePermissive))
	tmp := t.TempDir()
	kitsA := filepath.Join(tmp, "kits-a")
	kitsB := filepath.Join(tmp, "kits-b")

	cfg := DefaultConfig()
	cfg.Machine.ID = "test-machine"
	cfg.Capacity.MaxConcurrentSessions = 1
	cfg.Orchestrator.URL = "file:///tmp/queue"
	cfg.Kit = KitConfig{ScanPaths: []string{kitsA, kitsB}}
	cfgPath := filepath.Join(tmp, "daemon.yaml")
	if err := WriteConfig(cfgPath, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	d := New(Options{
		ConfigPath: cfgPath,
		JWTPath:    filepath.Join(tmp, "daemon.jwt"),
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0,
		SkipWizard: true,
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.Stop(ctx)
	})

	s := &Server{daemon: d}
	reg := s.kitRegistryOrEmpty()
	concrete, ok := reg.(*KitRegistry)
	if !ok {
		t.Fatalf("registry type = %T, want *KitRegistry", reg)
	}
	got := concrete.ScanPaths()
	want := []string{kitsA, kitsB}
	if len(got) != len(want) {
		t.Fatalf("ScanPaths len = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i, p := range want {
		if got[i] != p {
			t.Errorf("ScanPaths[%d] = %q, want %q", i, got[i], p)
		}
	}
}

// TestKitRegistryOrEmpty_FallsBackWhenConfigEmpty pins the back-compat
// fallback: when the daemon's loaded config has no kit.scanPaths set
// AND has not been through applyDefaults (e.g., set directly on a test
// daemon), kitRegistryOrEmpty still produces a usable registry pointed
// at DefaultKitScanPath().
func TestKitRegistryOrEmpty_FallsBackWhenConfigEmpty(t *testing.T) {
	// Pin permissive mode so the lazy construction yields a real
	// *KitRegistry — this test is about scan-path plumbing, not the
	// trust gate (covered by TestKitRegistryOrEmpty_DefaultTrust*).
	t.Setenv(envKitTrustMode, string(TrustModePermissive))
	// Build a daemon whose Config() returns a Config with empty Kit.
	d := &Daemon{}
	d.config = &Config{Kit: KitConfig{ScanPaths: nil}}
	s := &Server{daemon: d}

	reg := s.kitRegistryOrEmpty()
	concrete, ok := reg.(*KitRegistry)
	if !ok {
		t.Fatalf("registry type = %T, want *KitRegistry", reg)
	}
	got := concrete.ScanPaths()
	if len(got) != 1 || got[0] != DefaultKitScanPath() {
		t.Errorf("ScanPaths = %v, want [%s]", got, DefaultKitScanPath())
	}
}

// TestKitRegistryOrEmpty_DefaultTrustBlocksInstall pins the secure
// out-of-box behaviour: with no daemon.yaml trust block and no env
// override, the default trust mode is signed-by-allowlist AND the issuer
// set is seeded with the official vendor signer (defaultVendorIssuerSet).
// That is a VALID config (not a misconfiguration), so kitRegistryOrEmpty
// returns a real *KitRegistry — official signed kits install, while an
// unsigned kit is still rejected fail-closed by the trust gate with an
// actionable reason.
func TestKitRegistryOrEmpty_DefaultTrustBlocksInstall(t *testing.T) {
	t.Setenv(envKitTrustMode, "")
	s := &Server{}
	reg := s.kitRegistryOrEmpty()
	kitReg, ok := reg.(*KitRegistry)
	if !ok {
		t.Fatalf("registry type = %T, want *KitRegistry (default issuerSet is seeded, so config is valid)", reg)
	}
	// The seeded default issuer set must be the official vendor identity.
	if got := kitReg.TrustConfig().IssuerSet; len(got) != 1 || got[0] != vendorSignerSAN {
		t.Fatalf("default issuerSet = %v, want [%q]", got, vendorSignerSAN)
	}
	if got := kitReg.TrustConfig().Mode; got != TrustModeSignedByAllowlist {
		t.Errorf("default trust mode = %q, want %q", got, TrustModeSignedByAllowlist)
	}
	// An UNSIGNED kit is still rejected fail-closed at the gate.
	repoURL := newLocalGitFixture(t, fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML})
	_, err := kitReg.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if !errors.Is(err, ErrKitTrustGateRejected) {
		t.Fatalf("Install of unsigned kit: want ErrKitTrustGateRejected, got %v", err)
	}
	for _, want := range []string{"trust.issuerSet", envKitTrustMode} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install error: want substring %q, got: %s", want, err.Error())
		}
	}
}

// TestKitRegistryOrEmpty_EnvOptOutPermissive pins the operator opt-out:
// DONMAI_KIT_TRUST_MODE=permissive restores a fully-working permissive
// registry on an unconfigured daemon.
func TestKitRegistryOrEmpty_EnvOptOutPermissive(t *testing.T) {
	t.Setenv(envKitTrustMode, string(TrustModePermissive))
	s := &Server{}
	if _, ok := s.kitRegistryOrEmpty().(*KitRegistry); !ok {
		t.Fatalf("registry type = %T, want *KitRegistry under env opt-out", s.kitReg)
	}
}

// TestMisconfiguredKitRegistry_TrustOverrideStillWorks pins the explicit
// per-install escape hatch through the misconfigured-policy stub: a
// trustOverride: "allowed-this-once" request delegates to the real
// registry and installs (audit-logged by the gate), matching the
// behaviour of a healthy signed-by-allowlist gate.
func TestMisconfiguredKitRegistry_TrustOverrideStillWorks(t *testing.T) {
	repoURL := newLocalGitFixture(t, fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML})
	wrapped := NewKitRegistryWithTrust([]string{t.TempDir()}, TrustConfig{Mode: TrustModeSignedByAllowlist})
	stub := &misconfiguredKitRegistry{real: wrapped, reason: errors.New("misconfigured for test")}

	src := &afclient.KitInstallSource{Kind: "git", URL: repoURL}

	// Without the override: blocked with the misconfiguration reason.
	_, err := stub.Install("rensei/example", afclient.KitInstallRequest{Source: src})
	if !errors.Is(err, ErrKitTrustGateRejected) {
		t.Fatalf("Install without override: want ErrKitTrustGateRejected, got %v", err)
	}

	// With the override: delegates to the real registry and installs.
	res, err := stub.Install("rensei/example", afclient.KitInstallRequest{
		Source:        src,
		TrustOverride: afclient.TrustOverrideAllowedThisOnce,
	})
	if err != nil {
		t.Fatalf("Install with override: want success, got %v", err)
	}
	if res.Kit.Trust != afclient.KitTrustUnsigned {
		t.Errorf("Kit.Trust: want unsigned (override bypasses gate, not the verifier), got %q", res.Kit.Trust)
	}
}

// TestHandleKits_BodyDecode pins the request-body shape used for install.
func TestHandleKits_InstallBodyDecoded(t *testing.T) {
	var captured afclient.KitInstallRequest
	fake := &capturingFakeRegistry{capture: &captured}
	srv := kitTestServer(t, &fakeKitRegistry{})
	// Replace registry with capturing one.
	mux := http.NewServeMux()
	s := &Server{kitReg: fake}
	mux.HandleFunc(kitRoutePrefix, s.handleKitDetail)
	srv.Config.Handler = mux

	body := bytes.NewReader([]byte(`{"version":"2.0.0"}`))
	resp, _ := http.Post(srv.URL+"/api/daemon/kits/spring/java/install", "application/json", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if captured.Version != "2.0.0" {
		t.Errorf("captured Version: want 2.0.0, got %q", captured.Version)
	}
}

// capturingFakeRegistry records the install request's body for assertion.
type capturingFakeRegistry struct {
	fakeKitRegistry
	capture *afclient.KitInstallRequest
}

func (c *capturingFakeRegistry) Install(id string, req afclient.KitInstallRequest) (afclient.KitInstallResult, error) {
	*c.capture = req
	return afclient.KitInstallResult{Kit: afclient.Kit{ID: id}, Message: "ok"}, nil
}
