package agycli

import "testing"

func TestExtractEnvelope_Present(t *testing.T) {
	t.Parallel()
	out := "narration line\n" +
		resultEnvelopeBegin + "\n" +
		`{"status":"passed","summary":"all good"}` + "\n" +
		resultEnvelopeEnd + "\n" +
		"WORK_RESULT:passed\n"
	env, raw, ok := extractEnvelope(out)
	if !ok {
		t.Fatalf("expected ok, raw=%q", raw)
	}
	if env.Status != "passed" || env.Summary != "all good" {
		t.Errorf("parsed wrong: %+v", env)
	}
}

func TestExtractEnvelope_Absent(t *testing.T) {
	t.Parallel()
	if _, _, ok := extractEnvelope("just some prose, no markers"); ok {
		t.Errorf("expected ok=false when markers absent")
	}
}

func TestExtractEnvelope_LastWins(t *testing.T) {
	t.Parallel()
	// The model echoed the instruction earlier; the genuine final block is last.
	out := resultEnvelopeBegin + "\n{\"status\":\"failed\",\"summary\":\"echo\"}\n" + resultEnvelopeEnd + "\n" +
		"... work happens ...\n" +
		resultEnvelopeBegin + "\n{\"status\":\"passed\",\"summary\":\"real\"}\n" + resultEnvelopeEnd + "\n"
	env, _, ok := extractEnvelope(out)
	if !ok {
		t.Fatal("expected ok")
	}
	if env.Summary != "real" || env.Status != "passed" {
		t.Errorf("did not pick the LAST envelope: %+v", env)
	}
}

func TestExtractEnvelope_CodeFenced(t *testing.T) {
	t.Parallel()
	out := resultEnvelopeBegin + "\n```json\n{\"status\":\"passed\",\"summary\":\"fenced\"}\n```\n" + resultEnvelopeEnd
	env, _, ok := extractEnvelope(out)
	if !ok {
		t.Fatalf("expected ok for code-fenced body")
	}
	if env.Summary != "fenced" {
		t.Errorf("fenced parse wrong: %+v", env)
	}
}

func TestExtractEnvelope_Malformed(t *testing.T) {
	t.Parallel()
	out := resultEnvelopeBegin + "\nnot json at all\n" + resultEnvelopeEnd
	_, raw, ok := extractEnvelope(out)
	if ok {
		t.Errorf("expected ok=false for malformed JSON")
	}
	if raw == "" {
		t.Errorf("expected raw body returned even on parse failure")
	}
}

func TestExtractEnvelope_EmptyBody(t *testing.T) {
	t.Parallel()
	out := resultEnvelopeBegin + "\n   \n" + resultEnvelopeEnd
	if _, _, ok := extractEnvelope(out); ok {
		t.Errorf("expected ok=false for empty envelope body")
	}
}

func TestSuccessFromEnvelope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status   string
		fallback bool
		want     bool
	}{
		{"passed", false, true},
		{"PASS", false, true},
		{"success", false, true},
		{"failed", true, false},
		{"error", true, false},
		{"", true, true},        // unknown → fallback
		{"weird", false, false}, // unknown → fallback
	}
	for _, c := range cases {
		got := successFromEnvelope(resultEnvelope{Status: c.status}, c.fallback)
		if got != c.want {
			t.Errorf("status=%q fallback=%v: got %v want %v", c.status, c.fallback, got, c.want)
		}
	}
}
