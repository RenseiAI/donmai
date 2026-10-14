package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/linear"
)

func TestLinearExitProcess(_ *testing.T) {
	if os.Getenv("DONMAI_TEST_LINEAR_MAIN") != "1" {
		return
	}
	os.Args = []string{"donmai", "linear", "list-teams"}
	main()
}

func TestLinearProxyExitStatus(t *testing.T) {
	proxy, err := linear.NewProxiedClient("https://example.invalid", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	expectedPath := strings.TrimPrefix(proxy.BaseURL, "https://example.invalid")
	for _, tc := range []struct {
		status, code int
		retry        string
	}{
		{429, 75, "0"}, {429, 75, ""}, {503, 75, "0"}, {500, 1, ""},
	} {
		t.Run(fmt.Sprintf("%d/header=%s", tc.status, tc.retry), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != expectedPath {
					t.Errorf("path=%s", r.URL.Path)
				}
				w.Header().Set("Retry-After", tc.retry)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"errors":[{"message":"budget exhausted","extensions":{"code":"BUDGET_EXHAUSTED"}}]}`)
			}))
			t.Cleanup(srv.Close)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			capture, err := os.CreateTemp(directory, "output")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = capture.Close() })
			process, err := os.StartProcess(executable, []string{executable, "-test.run=^TestLinearExitProcess$"}, &os.ProcAttr{
				Dir: directory,
				Env: []string{
					"DONMAI_TEST_LINEAR_MAIN=1", "LINEAR_API_KEY=", "LINEAR_ACCESS_TOKEN=",
					"WORKER_AUTH_TOKEN=fixture", "DONMAI_API_URL=" + srv.URL,
					"HOME=" + directory, "DONMAI_STATE_HOME=" + directory,
				},
				Files: []*os.File{nil, capture, capture},
			})
			if err != nil {
				t.Fatal(err)
			}
			state, err := process.Wait()
			if err != nil {
				t.Fatal(err)
			}
			output, err := os.ReadFile(capture.Name())
			if err != nil {
				t.Fatal(err)
			}
			if state.ExitCode() != tc.code {
				t.Fatalf("exit=%d want %d output=%s", state.ExitCode(), tc.code, output)
			}
			for _, want := range []string{"budget exhausted", "BUDGET_EXHAUSTED"} {
				if !strings.Contains(string(output), want) {
					t.Errorf("output=%s missing %s", output, want)
				}
			}
			if tc.retry != "" && !strings.Contains(string(output), "retry after 0s") {
				t.Errorf("output=%s missing retry guidance", output)
			}
		})
	}
}
