package usageping

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMintInstallationID_FormatAndUniqueness(t *testing.T) {
	t.Parallel()
	a, err := MintInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := MintInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidateInstallationID(a) {
		t.Fatalf("minted id %q fails validation", a)
	}
	if a == b {
		t.Fatal("two minted ids are identical")
	}
}

func TestValidateInstallationID_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id   string
		want bool
	}{
		{"", false},
		{"abc", false},
		{"0123456789abcdef0123456789ABCDEF", false}, // uppercase rejected
		{"0123456789abcdef0123456789abcde", false},  // 31 chars
		{"0123456789abcdef0123456789abcdef0", false}, // 33 chars
		{"0123456789abcdef0123456789abcdeg", false}, // non-hex
		{"0123456789abcdef0123456789abcdef", true},
	}
	for _, tc := range cases {
		if got := ValidateInstallationID(tc.id); got != tc.want {
			t.Errorf("ValidateInstallationID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestBuild_DisabledSendsNothing(t *testing.T) {
	t.Parallel()
	id, err := MintInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	for _, consent := range []Consent{
		{},                                        // everything off
		{Enabled: false, InstallationID: id},      // explicitly off
		{Enabled: true},                           // on but no id
		{Enabled: true, InstallationID: "bogus"},  // on but bad id
		{Enabled: true, InstallationID: id},       // on, id, but no endpoint
	} {
		if got := Build(consent, "v0.1.0"); got != nil {
			t.Fatalf("Build(%+v) = %+v, want nil (no request)", consent, got)
		}
	}
	if got := Build(Consent{Enabled: true, InstallationID: id, Endpoint: "https://example.invalid/ping"}, ""); got != nil {
		t.Fatalf("Build with empty version = %+v, want nil", got)
	}
}

func TestBuild_EnabledShape(t *testing.T) {
	t.Parallel()
	id, err := MintInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	got := Build(Consent{Enabled: true, InstallationID: id, Endpoint: "https://example.invalid/ping"}, "v0.1.0")
	if got == nil {
		t.Fatal("Build returned nil for opted-in consent")
	}
	if got.InstallationID != id || got.Version != "v0.1.0" || got.OS == "" {
		t.Fatalf("unexpected ping shape: %+v", got)
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	id, err := MintInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	orig := &Ping{InstallationID: id, Version: "v0.1.0", OS: "linux"}
	body, err := Encode(orig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *orig {
		t.Fatalf("round trip = %+v, want %+v", got, orig)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	id, err := MintInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	body := `{"installationId":` + strconv_quote(id) + `,"version":"v0.1.0","os":"linux","hostname":"evil"}`
	if _, err := Decode([]byte(body)); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestDecodeRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"installationId":"bogus","version":"v0.1.0","os":"linux"}`,
		`{"installationId":"0123456789abcdef0123456789abcdef","version":"","os":"linux"}`,
		`{"installationId":"0123456789abcdef0123456789abcdef","version":"v0.1.0","os":""}`,
		`{"installationId":"0123456789abcdef0123456789abcdef","version":"v0.1.0","os":"linux"} garbage`,
	} {
		if _, err := Decode([]byte(body)); err == nil {
			t.Fatalf("invalid body accepted: %s", body)
		}
	}
}

func TestDecodeRejectsOversize(t *testing.T) {
	t.Parallel()
	big := make([]byte, MaxPayloadBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if _, err := Decode(big); err == nil {
		t.Fatal("oversize body accepted")
	}
}

func TestSend_RequiresHTTPS(t *testing.T) {
	t.Parallel()
	id, err := MintInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	p := &Ping{InstallationID: id, Version: "v0.1.0", OS: "linux"}
	for _, endpoint := range []string{"", "http://example.invalid/ping"} {
		s := &Sender{Endpoint: endpoint}
		if err := s.Send(context.Background(), p); err == nil {
			t.Fatalf("endpoint %q accepted", endpoint)
		}
	}
}

func TestSend_SuccessAndFailure(t *testing.T) {
	t.Parallel()
	id, err := MintInstallationID()
	if err != nil {
		t.Fatal(err)
	}
	p := &Ping{InstallationID: id, Version: "v0.1.0", OS: "linux"}

	var gotBody []byte
	var gotUA string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		gotUA = r.Header.Get("User-Agent")
		buf := make([]byte, MaxPayloadBytes+1)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	if err := (&Sender{Endpoint: ok.URL, AllowHTTP: true}).Send(context.Background(), p); err != nil {
		t.Fatalf("Send: %v", err)
	}
	decoded, err := Decode(gotBody)
	if err != nil {
		t.Fatalf("server received undecodable body: %v", err)
	}
	if *decoded != *p {
		t.Fatalf("server received %+v, want %+v", decoded, p)
	}
	if !strings.HasPrefix(gotUA, "donmai-usage-ping/") {
		t.Fatalf("user-agent = %q", gotUA)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if err := (&Sender{Endpoint: bad.URL, AllowHTTP: true}).Send(context.Background(), p); err == nil {
		t.Fatal("5xx accepted as success")
	}
}

func TestDue_WeeklyMaximum(t *testing.T) {
	t.Parallel()
	now := time.Now()
	if !Due(time.Time{}, now) {
		t.Fatal("zero last should be due")
	}
	if !Due(now.Add(-PingInterval), now) {
		t.Fatal("exactly one interval ago should be due")
	}
	if Due(now.Add(-time.Hour), now) {
		t.Fatal("one hour ago should not be due")
	}
	if !Due(now.Add(-8*24*time.Hour), now) {
		t.Fatal("eight days ago should be due")
	}
	if got := NextEligible(now); !got.Equal(now.Add(PingInterval)) {
		t.Fatalf("NextEligible = %v", got)
	}
	if got := NextEligible(time.Time{}); !got.IsZero() {
		t.Fatalf("NextEligible(zero) = %v, want zero", got)
	}
}

func strconv_quote(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
}
