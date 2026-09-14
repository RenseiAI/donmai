//go:build !windows

package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

const shutdownChildEnv = "DONMAI_CODEX_SHUTDOWN_TEST_CHILD"

// The child observes the real filesystem at SIGTERM and writes its final
// state before exiting. No sleeps or scheduling assumptions order the check.
func TestCodexShutdownChild(_ *testing.T) {
	role := os.Getenv(shutdownChildEnv)
	if role == "" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscallSIGTERM())
	if role == "launcher" {
		executable, err := os.Executable()
		if err != nil {
			os.Exit(5)
		}
		if err := os.Setenv(shutdownChildEnv, "1"); err != nil {
			os.Exit(5)
		}
		_, err = os.StartProcess(executable, []string{executable, "-test.run=^TestCodexShutdownChild$"}, &os.ProcAttr{
			Env:   os.Environ(),
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		})
		if err != nil {
			os.Exit(5)
		}
		<-signals
		// Deliberately exit before the native child, like a shell launcher
		// that handles TERM without joining its background process.
		os.Exit(0)
	}
	root, err := os.OpenRoot(os.Getenv("DONMAI_CODEX_SHUTDOWN_TEST_ROOT"))
	if err != nil {
		os.Exit(8)
	}
	identity, err := sessionshim.Self()
	if err != nil {
		os.Exit(6)
	}
	identityBytes, err := json.Marshal(identity)
	if err != nil {
		os.Exit(7)
	}
	if err := root.WriteFile("child-identity", identityBytes, 0o600); err != nil {
		os.Exit(8)
	}
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				continue
			}
			if request.Method == "test/closeStdout" {
				_ = os.Stdout.Close()
				return
			}
			if len(request.ID) > 0 {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"id": request.ID, "result": map[string]string{"codexHome": os.Getenv("CODEX_HOME")},
				})
			}
		}
	}()
	<-signals
	home := filepath.Base(os.Getenv("CODEX_HOME"))
	_, statErr := root.Stat(filepath.Join(home, codexConfigFileName))
	result := "config-present-at-SIGTERM"
	if statErr != nil {
		result = "config-removed-before-SIGTERM"
	}
	// A non-resumable late write must be removed too, after process exit.
	if err := root.MkdirAll(filepath.Join(home, ".tmp"), 0o700); err != nil {
		os.Exit(9)
	}
	if err := root.WriteFile(filepath.Join(home, ".tmp", "final-write"), []byte("temporary"), 0o600); err != nil {
		os.Exit(10)
	}
	if os.Getenv("DONMAI_CODEX_SHUTDOWN_TEST_ROLLOUT") == "1" {
		if err := root.MkdirAll(filepath.Join(home, codexSessionStateSubdir), 0o700); err != nil {
			os.Exit(2)
		}
		if err := root.WriteFile(filepath.Join(home, codexSessionStateSubdir, "rollout-final.jsonl"), []byte("final-session-state"), 0o600); err != nil {
			os.Exit(3)
		}
	}
	if err := root.WriteFile("child-result", []byte(result), 0o600); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestProvider_ShutdownWaitsForFinalWrites(t *testing.T) {
	for _, path := range []string{"shutdown", "EOF", "launcher"} {
		for _, rollout := range []bool{false, true} {
			name := "ephemeral"
			if rollout {
				name = "resumable"
			}
			t.Run(path+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				resultPath := filepath.Join(root, "child-result")
				identityPath := filepath.Join(root, "child-identity")
				env := map[string]string{
					shutdownChildEnv:                  "1",
					"DONMAI_CODEX_SHUTDOWN_TEST_ROOT": root,
				}
				if path == "launcher" {
					env[shutdownChildEnv] = "launcher"
				}
				if rollout {
					env["DONMAI_CODEX_SHUTDOWN_TEST_ROLLOUT"] = "1"
				}
				p, err := New(Options{
					CodexBin: os.Args[0], Args: []string{"-test.run=^TestCodexShutdownChild$"},
					Env: env, configTempDir: root,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
				startTestProvider(t, p)
				identityBytes, err := os.ReadFile(identityPath)
				if err != nil {
					t.Fatal(err)
				}
				var writer sessionshim.ProcessIdentity
				if err := json.Unmarshal(identityBytes, &writer); err != nil {
					t.Fatal(err)
				}
				// A failing regression must not strand the deliberately orphaned
				// fixture. This is one exact PID+start identity, never a host sweep.
				t.Cleanup(func() {
					if alive, err := writer.Alive(); err == nil && alive {
						process, err := os.FindProcess(writer.PID)
						if err == nil {
							_ = process.Kill()
						}
					}
				})
				identity, err := sessionshim.ProcessIdentityFor(p.cmd.Process.Pid)
				if err != nil {
					t.Fatal(err)
				}
				if path == "EOF" {
					closed := make(chan struct{})
					p.client.SetOnClose(func(cause error) { p.onClientClose(cause); close(closed) })
					if err := p.client.Notify("test/closeStdout", nil); err != nil {
						t.Fatal(err)
					}
					select {
					case <-closed:
					case <-time.After(p.opts.HandshakeTimeout):
						t.Fatal("EOF cleanup did not finish")
					}
				}
				if err := p.Shutdown(t.Context()); err != nil {
					t.Fatal(err)
				}
				alive, err := identity.Alive()
				if err != nil || alive {
					t.Fatalf("owned child after Shutdown: alive=%v err=%v", alive, err)
				}
				alive, err = writer.Alive()
				if err != nil || alive {
					t.Errorf("owned native writer after Shutdown: alive=%v err=%v", alive, err)
				}
				result, err := os.ReadFile(resultPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(result) != "config-present-at-SIGTERM" {
					t.Errorf("owned child observed %s", result)
				}
				if rollout {
					body, err := os.ReadFile(filepath.Join(p.config.home, codexSessionStateSubdir, "rollout-final.jsonl"))
					if err != nil || string(body) != "final-session-state" {
						t.Errorf("final rollout: body=%q err=%v", body, err)
					}
					if _, err := os.Stat(p.config.configPath); err != nil {
						t.Errorf("resume config lost: %v", err)
					}
				} else if _, err := os.Stat(p.config.home); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("ephemeral home survives: %v", err)
				}
			})
		}
	}
}

func TestProvider_ShutdownLeavesOtherProviderAlive(t *testing.T) {
	providers := make([]*Provider, 2)
	for i := range providers {
		p, err := New(Options{
			CodexBin: os.Args[0], Env: map[string]string{codexFakeAppServerEnv: "1"},
			configTempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
		startTestProvider(t, p)
		providers[i] = p
	}
	other := providers[1]
	identity, err := sessionshim.ProcessIdentityFor(other.cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := providers[0].Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	alive, err := identity.Alive()
	if err != nil || !alive {
		t.Fatalf("other provider identity: alive=%v err=%v", alive, err)
	}
	if err := other.config.validate(); err != nil {
		t.Fatalf("other provider home changed: %v", err)
	}
	if _, err := other.client.Request(t.Context(), "config/read", map[string]any{}, other.opts.RPCTimeout); err != nil {
		t.Fatalf("other provider stopped serving requests: %v", err)
	}
}
