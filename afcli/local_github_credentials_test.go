package afcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afcli/credentials"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/installer/launchd"
	"github.com/RenseiAI/donmai/installer/systemd"
)

type localGitHubRoundTrip func(*http.Request) (*http.Response, error)

func (fn localGitHubRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func localGitHubCredentialConfig(t *testing.T) (string, string) {
	t.Helper()
	queue := filepath.Join(t.TempDir(), "queue")
	cfg := daemon.DefaultConfig()
	cfg.APIVersion = daemon.LocalRuntimeConfigAPIVersion
	cfg.Orchestrator.URL = "file://" + queue
	cfg.Orchestrator.AuthToken = ""
	cfg.LocalRuntime = &daemon.LocalRuntimeConfig{
		Harness: "codex", Model: "gpt-6-sol", ModelAuthor: "openai",
		ExecutionSecurity: daemon.InitialLocalExecutionSecurity(),
		Repositories:      []daemon.LocalGitHubRepository{{RepositoryID: 42, OwnerRepo: "example/project", Label: "donmai", Ref: "main"}},
	}
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := daemon.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path, queue
}

func localGitHubFakeCLI(t *testing.T, home, body string) string {
	t.Helper()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\n" +
		"[ \"$#\" -eq 4 ] && [ \"$1\" = auth ] && [ \"$2\" = token ] && [ \"$3\" = --hostname ] && [ \"$4\" = github.com ] || exit 92\n" +
		"[ -z \"${GITHUB_TOKEN+x}\" ] && [ -z \"${GH_TOKEN+x}\" ] && [ -z \"${LD_PRELOAD+x}\" ] && [ -z \"${HTTP_PROXY+x}\" ] || exit 93\n" +
		"[ \"$PWD\" = / ] || exit 94\n" +
		"printf '%s\\n' \"$*\" >> \"$HOME/gh-args\"\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func localGitHubServicePath(t *testing.T, service string) string {
	t.Helper()
	switch service {
	case "launchd":
		plist, err := launchd.GeneratePlist("/usr/bin/donmai", "/tmp/donmai.log", "/tmp/donmai.err")
		if err != nil {
			t.Fatal(err)
		}
		home := regexp.MustCompile(`(?s)<key>HOME</key>\s*<string>([^<]+)</string>`).FindStringSubmatch(plist)
		if len(home) != 2 || html.UnescapeString(home[1]) != os.Getenv("HOME") {
			t.Fatal("generated launchd HOME differs from the service user")
		}
		match := regexp.MustCompile(`(?s)<key>PATH</key>\s*<string>([^<]+)</string>`).FindStringSubmatch(plist)
		if len(match) != 2 {
			t.Fatal("generated launchd service has no PATH")
		}
		if strings.Contains(plist, "GITHUB_TOKEN") || strings.Contains(plist, "GH_TOKEN") {
			t.Fatal("generated launchd service contains a GitHub bearer name")
		}
		return html.UnescapeString(match[1])
	case "systemd":
		unit, err := systemd.GenerateUnitFile(systemd.ScopeUser, "/usr/bin/donmai", systemd.InstallOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(unit, "GITHUB_TOKEN") || strings.Contains(unit, "GH_TOKEN") {
			t.Fatal("generated systemd service contains a GitHub bearer name")
		}
		for _, line := range strings.Split(unit, "\n") {
			if value, ok := strings.CutPrefix(line, "Environment=PATH="); ok {
				return value
			}
		}
		t.Fatal("generated systemd service has no PATH")
	default:
		t.Fatalf("unknown service %q", service)
	}
	return ""
}

// A real localgithub.Source uses its fixed https://api.github.com origin. This
// test owns only the standard-library transport, forwards that exact request
// to a verified local TLS fixture, and never contacts the real GitHub service.
func localGitHubExternalFixture(t *testing.T, bearer string) (*httptest.Server, *[]string) {
	t.Helper()
	paths := new([]string)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			t.Errorf("GitHub source did not use selected credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		pathsLocal := *paths
		pathsLocal = append(pathsLocal, r.Method+" "+r.URL.RequestURI())
		*paths = pathsLocal
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/example/project":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "full_name": "example/project"})
		case "/repos/example/project/branches/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]string{"sha": strings.Repeat("a", 40)}})
		case "/repos/example/project/issues":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{
				"number": 1, "title": "Fixture task", "body": "Do the fixture task", "state": "open",
				"html_url": "https://github.com/example/project/issues/1", "labels": []any{map[string]string{"name": "donmai"}},
			}})
		default:
			t.Errorf("unexpected GitHub fixture path %q", r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	fixtureURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	trustedTransport := server.Client().Transport
	previous := http.DefaultTransport
	http.DefaultTransport = localGitHubRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "api.github.com" || request.URL.User != nil || request.URL.RawQuery != "" && request.URL.Path != "/repos/example/project/issues" {
			return nil, fmt.Errorf("unexpected fixed GitHub origin or method")
		}
		forwarded := request.Clone(request.Context())
		urlCopy := *request.URL
		urlCopy.Scheme, urlCopy.Host = fixtureURL.Scheme, fixtureURL.Host
		forwarded.URL = &urlCopy
		forwarded.Host = fixtureURL.Host
		return trustedTransport.RoundTrip(forwarded)
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return server, paths
}

func TestLocalGitHubInstalledServiceUsesStoredLoginAtActualSource(t *testing.T) {
	for _, service := range []string{"launchd", "systemd"} {
		t.Run(service, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("GITHUB_TOKEN", "")
			t.Setenv("GH_TOKEN", "")
			t.Setenv("GH_CONFIG_DIR", "")
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("XDG_RUNTIME_DIR", "")
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
			t.Setenv("LD_PRELOAD", "unrelated-preload")
			t.Setenv("HTTP_PROXY", "http://unrelated-proxy.invalid")
			localGitHubFakeCLI(t, home, "printf 'fixture-native-token\\n'")
			servicePath := localGitHubServicePath(t, service)
			if !strings.HasPrefix(servicePath, filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)) {
				t.Fatal("generated service PATH does not select its user's CLI")
			}
			t.Setenv("PATH", servicePath)
			outside := t.TempDir()
			previousCWD, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Chdir(outside); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(previousCWD) })
			if root := resolveStandaloneGitRoot(); root != "" {
				t.Fatalf("service cwd unexpectedly yielded git root %q", root)
			}
			standalone, err := credentials.LoadLocalSource(resolveStandaloneGitRoot())
			if err != nil {
				t.Fatal(err)
			}
			baseEnv := standalone.MergeIntoBaseEnv(nil)
			configPath, _ := localGitHubCredentialConfig(t)
			_, paths := localGitHubExternalFixture(t, "fixture-native-token")
			options, _, err := newLocalRuntimeComposition(configPath, nil, baseEnv)
			if err != nil {
				t.Fatal(err)
			}
			if options == nil || options.NewSource == nil {
				t.Fatal("real local composition did not construct its GitHub source")
			}
			source, err := options.NewSource(options.Repositories)
			if err != nil {
				t.Fatal(err)
			}
			if closer, ok := source.(interface{ Close() error }); ok {
				t.Cleanup(func() { _ = closer.Close() })
			}
			issues, err := source.Poll(context.Background())
			if err != nil || len(issues) != 1 || issues[0].IssueNumber != 1 {
				t.Fatalf("real local GitHub Poll = %#v, %v", issues, err)
			}
			want := []string{
				"GET /repos/example/project",
				"GET /repos/example/project/branches/main",
				"GET /repos/example/project/issues?labels=donmai&page=1&per_page=100&state=open",
			}
			if strings.Join(*paths, "\n") != strings.Join(want, "\n") {
				t.Fatalf("fixed-origin GitHub requests = %q, want %q", *paths, want)
			}
			args, err := os.ReadFile(filepath.Join(home, "gh-args"))
			if err != nil || string(args) != "auth token --hostname github.com\n" {
				t.Fatalf("stored-login command args = %q, %v", args, err)
			}
			if baseEnv["GITHUB_TOKEN"] != "" || baseEnv["GH_TOKEN"] != "" {
				t.Fatal("stored-login credential was copied into worker BaseEnv")
			}
			config, err := os.ReadFile(configPath)
			if err != nil || strings.Contains(string(config), "fixture-native-token") {
				t.Fatal("stored-login credential appeared in daemon configuration")
			}
		})
	}
}

func TestLocalGitHubCredentialSelectorsRefuseConflictBeforeNativeLookup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	localGitHubFakeCLI(t, home, "printf 'native-token\\n'")
	t.Setenv("PATH", filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	configPath, _ := localGitHubCredentialConfig(t)
	for _, candidate := range []struct {
		name, github, gh, want string
		conflict               bool
	}{
		{"github-only", "fixture-github", "", "fixture-github", false},
		{"gh-only", "", "fixture-gh", "fixture-gh", false},
		{"same", "fixture-same", "fixture-same", "fixture-same", false},
		{"conflict", "fixture-github", "fixture-gh", "", true},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			base := map[string]string{"GITHUB_TOKEN": candidate.github, "GH_TOKEN": candidate.gh}
			got, err := localGitHubSourceToken(base)
			if candidate.conflict {
				if err == nil || !strings.Contains(err.Error(), "conflicting") || strings.Contains(err.Error(), candidate.github) || strings.Contains(err.Error(), candidate.gh) {
					t.Fatalf("conflict not refused without values: %q", err)
				}
				if _, _, compositionErr := newLocalRuntimeComposition(configPath, nil, base); compositionErr == nil {
					t.Fatal("actual composition accepted conflicting selectors")
				}
			} else if err != nil || got != candidate.want {
				t.Fatalf("selected credential %q, %v; want %q", got, err, candidate.want)
			}
			if _, statErr := os.Stat(filepath.Join(home, "gh-args")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("explicit env credential invoked native GitHub CLI")
			}
		})
	}
}

func TestLocalGitHubSoleGHTokenReachesActualSource(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "fixture-gh-only")
	t.Setenv("PATH", t.TempDir()) // no native gh may be selected
	configPath, _ := localGitHubCredentialConfig(t)
	_, paths := localGitHubExternalFixture(t, "fixture-gh-only")
	options, _, err := newLocalRuntimeComposition(configPath, nil, map[string]string{"GH_TOKEN": "fixture-gh-only"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := options.NewSource(options.Repositories)
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := source.(interface{ Close() error }); ok {
		t.Cleanup(func() { _ = closer.Close() })
	}
	issues, err := source.Poll(context.Background())
	if err != nil || len(issues) != 1 || len(*paths) != 3 {
		t.Fatalf("sole GH_TOKEN source poll = %#v, %v; requests=%q", issues, err, *paths)
	}
}

func TestLocalGitHubSourceKeepsProcessOverGitRootFilePrecedence(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env.local"), []byte("GITHUB_TOKEN=fixture-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "fixture-process")
	t.Setenv("GH_TOKEN", "")
	standalone, err := credentials.LoadLocalSource(root)
	if err != nil {
		t.Fatal(err)
	}
	token, err := localGitHubSourceToken(standalone.MergeIntoBaseEnv(nil))
	if err != nil || token != "fixture-process" {
		t.Fatalf("source precedence selected %q, %v", token, err)
	}
}

func TestStoredGitHubCLICredentialFailuresAreRedacted(t *testing.T) {
	for _, candidate := range []struct{ name, body string }{
		{"failed", "printf 'secret-failure-token\\n'; exit 91"},
		{"empty", "exit 0"},
		{"invalid", "printf 'secret\\nsecond-line\\n'"},
		{"oversize", "head -c 9000 /dev/zero | tr '\\000' x"},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("GITHUB_TOKEN", "")
			t.Setenv("GH_TOKEN", "")
			localGitHubFakeCLI(t, home, candidate.body)
			t.Setenv("PATH", filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
			_, err := localGitHubSourceToken(nil)
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "9000") {
				t.Fatalf("credential failure was accepted or disclosed: %v", err)
			}
		})
	}
}
