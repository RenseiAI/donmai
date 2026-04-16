package inline

import (
	"os"
	"path/filepath"
	"testing"
)

// tempFileWriter creates a new file under t.TempDir() for use as a
// stand-in for DataWriter/ChromeWriter. The file is closed via
// t.Cleanup.
func tempFileWriter(t *testing.T, name string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// readFile reads the contents of a file by path.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test-scoped temp path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestData(t *testing.T) {
	tests := []struct {
		name   string
		format string
		args   []any
		want   string
	}{
		{"plain string", "hello", nil, "hello"},
		{"formatted int", "%d items", []any{3}, "3 items"},
		{"multiple args", "%s=%d", []any{"count", 42}, "count=42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tempFileWriter(t, "data.txt")
			orig := DataWriter
			DataWriter = f
			t.Cleanup(func() { DataWriter = orig })

			Data(tt.format, tt.args...)

			if err := f.Sync(); err != nil {
				t.Fatalf("sync: %v", err)
			}
			got := readFile(t, f.Name())
			if got != tt.want {
				t.Errorf("Data() wrote %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDataLn(t *testing.T) {
	tests := []struct {
		name   string
		format string
		args   []any
		want   string
	}{
		{"appends newline", "hello", nil, "hello\n"},
		{"formatted with newline", "%d items", []any{3}, "3 items\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tempFileWriter(t, "dataln.txt")
			orig := DataWriter
			DataWriter = f
			t.Cleanup(func() { DataWriter = orig })

			DataLn(tt.format, tt.args...)

			if err := f.Sync(); err != nil {
				t.Fatalf("sync: %v", err)
			}
			got := readFile(t, f.Name())
			if got != tt.want {
				t.Errorf("DataLn() wrote %q, want %q", got, tt.want)
			}
		})
	}
}

// TestChromeSuppressedWhenNotTTY verifies Chrome/ChromeLn write nothing
// when ChromeWriter is not a character device. A temp file is not a
// TTY, so isTerminal returns false and writes should be suppressed.
func TestChromeSuppressedWhenNotTTY(t *testing.T) {
	t.Run("Chrome suppressed", func(t *testing.T) {
		f := tempFileWriter(t, "chrome.txt")
		orig := ChromeWriter
		ChromeWriter = f
		t.Cleanup(func() { ChromeWriter = orig })

		Chrome("this should be suppressed: %d", 42)

		if err := f.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}
		got := readFile(t, f.Name())
		if got != "" {
			t.Errorf("Chrome() wrote %q when not a TTY, want empty", got)
		}
	})

	t.Run("ChromeLn suppressed", func(t *testing.T) {
		f := tempFileWriter(t, "chromeln.txt")
		orig := ChromeWriter
		ChromeWriter = f
		t.Cleanup(func() { ChromeWriter = orig })

		ChromeLn("this should also be suppressed: %s", "hi")

		if err := f.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}
		got := readFile(t, f.Name())
		if got != "" {
			t.Errorf("ChromeLn() wrote %q when not a TTY, want empty", got)
		}
	})
}

// TestIsTerminal exercises the tty helper on a non-TTY (temp file) —
// the only branch we can exercise without a pty.
func TestIsTerminalNonTTY(t *testing.T) {
	f := tempFileWriter(t, "tty.txt")
	if isTerminal(f) {
		t.Error("isTerminal(tempfile) = true, want false")
	}
}
