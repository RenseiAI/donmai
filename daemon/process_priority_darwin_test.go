package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/installer/servicepriority"
)

func TestObserveDarwinProcessPriority(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		out          string
		err          error
		wantMode     servicepriority.Mode
		wantEvidence string
		wantWarning  bool
	}{
		{name: "background band", out: " 4\n", wantMode: servicepriority.Background, wantEvidence: "ps PRI=4"},
		{name: "launchd job without a process type", out: "20\n", wantMode: servicepriority.Default, wantEvidence: "ps PRI=20"},
		{name: "shell-launched process", out: "31\n", wantMode: servicepriority.Default, wantEvidence: "ps PRI=31"},
		{name: "just above the background band", out: "5\n", wantMode: servicepriority.Default, wantEvidence: "ps PRI=5"},
		{name: "ps fails", out: "ps: no such process", err: errors.New("exit status 1"), wantWarning: true},
		{name: "empty output", out: "\n", wantWarning: true},
		{name: "non-numeric output", out: "PRI\n", wantWarning: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotName string
			var gotArgs []string
			got := observeDarwinProcessPriority(4242, func(_ context.Context, name string, args ...string) ([]byte, error) {
				gotName, gotArgs = name, args
				return []byte(tc.out), tc.err
			})
			if gotName != "ps" || len(gotArgs) != 4 || gotArgs[3] != "4242" {
				t.Errorf("ran %s %v, want ps against pid 4242", gotName, gotArgs)
			}
			if tc.wantWarning {
				if got.mode != "" || got.warning == "" {
					t.Fatalf("observation = %+v, want a warning and no mode", got)
				}
				return
			}
			if got.warning != "" || got.mode != tc.wantMode || got.evidence != tc.wantEvidence {
				t.Fatalf("observation = %+v, want mode %q evidence %q", got, tc.wantMode, tc.wantEvidence)
			}
		})
	}
}
