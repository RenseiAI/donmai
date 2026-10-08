package hostwatch

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// updateGolden rewrites the golden renders instead of diffing against them.
// Run `go test ./internal/views/hostwatch/ -run TestGolden -update` after an
// intentional layout change and review the diff.
var updateGolden = flag.Bool("update", false, "rewrite testdata/*.golden render fixtures")

// TestGolden_Renders snapshots the whole dashboard frame over the
// real-shaped fixture at every acceptance width, at short heights, below the
// minimum card width, and with the detail pane open. Styled frames are
// compared with color stripped (frames, markers and wide characters stay);
// the raw styled frame is separately held to the terminal's width and
// height.
func TestGolden_Renders(t *testing.T) {
	type tc struct {
		width, height int
		plain, detail bool
		cursor        int
	}
	var cases []tc
	for _, w := range acceptanceWidths {
		cases = append(cases, tc{width: w, height: 40, cursor: 1}, tc{width: w, height: 40, plain: true, cursor: 1})
	}
	cases = append(cases,
		tc{width: 120, height: 10, cursor: 9},
		tc{width: 120, height: 10, plain: true, cursor: 9},
		tc{width: 80, height: 6, cursor: 4},
		tc{width: 30, height: 16, cursor: 2},
		tc{width: 120, height: 40, detail: true, cursor: 2},
		tc{width: 200, height: 40, detail: true, cursor: 2},
	)
	for _, c := range cases {
		mode := "styled"
		if c.plain {
			mode = "plain"
		}
		name := fmt.Sprintf("%dx%d-%s", c.width, c.height, mode)
		if c.detail {
			name += "-detail"
		}
		t.Run(name, func(t *testing.T) {
			m := newFixtureModel(t, c.width, c.height, c.plain, c.cursor)
			if c.detail {
				m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			}
			raw := m.render()
			lines := strings.Split(raw, "\n")
			if len(lines) != c.height {
				t.Fatalf("frame has %d rows, want %d", len(lines), c.height)
			}
			for i, line := range lines {
				if w := lipgloss.Width(line); w > c.width {
					t.Fatalf("row %d is %d cells wide (terminal %d): %q", i, w, c.width, ansi.Strip(line))
				}
			}
			got := ansi.Strip(raw) + "\n"
			if c.plain && got != raw+"\n" {
				t.Fatal("plain frame carries escapes")
			}
			path := filepath.Join("testdata", name+".golden")
			if *updateGolden {
				if err := os.MkdirAll("testdata", 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if got != string(want) {
				t.Errorf("render differs from %s (run with -update after an intentional change)\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
			}
		})
	}
}
