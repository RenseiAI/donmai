package afcli

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

func TestProjectModeShowsCurrentMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *afclient.DaemonYAML
		want string
	}{
		{
			name: "no opinion reads as enumerated",
			cfg:  &afclient.DaemonYAML{},
			want: afclient.ProjectAdmissionModeEnumerated,
		},
		{
			name: "explicit all-routed",
			cfg:  &afclient.DaemonYAML{ProjectAdmissionMode: afclient.ProjectAdmissionModeAllRouted},
			want: afclient.ProjectAdmissionModeAllRouted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := &mockConfigRW{cfg: tc.cfg}
			out, err := newTestProjectCmd(rw, "", []string{"mode"})
			if err != nil {
				t.Fatalf("project mode: %v", err)
			}
			if strings.TrimSpace(out.String()) != tc.want {
				t.Fatalf("project mode printed %q, want %q", out.String(), tc.want)
			}
			if rw.written != nil {
				t.Fatal("a read-only `project mode` wrote the config")
			}
		})
	}
}

func TestProjectModeSetsAllRouted(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{cfg: &afclient.DaemonYAML{}}
	out, err := newTestProjectCmd(rw, "", []string{"mode", "all-routed"})
	if err != nil {
		t.Fatalf("project mode all-routed: %v", err)
	}
	if rw.written == nil {
		t.Fatal("project mode all-routed did not write the config")
	}
	if !rw.written.AdmitsAnyRoutedProject() {
		t.Fatalf("written config mode = %q, want all-routed", rw.written.EffectiveProjectAdmissionMode())
	}
	// Setting the mode must also make enabledProjectIds authoritative — the
	// mode is meaningless while the file is still on legacy admission.
	if rw.written.ProjectAdmissionVersion != afclient.ProjectAdmissionVersionV2 {
		t.Fatalf("written admission version = %d, want %d",
			rw.written.ProjectAdmissionVersion, afclient.ProjectAdmissionVersionV2)
	}
	if !strings.Contains(out.String(), "all-routed") {
		t.Fatalf("output did not confirm the new mode: %q", out.String())
	}
}

func TestProjectModeSetsEnumerated(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{cfg: &afclient.DaemonYAML{
		ProjectAdmissionVersion: afclient.ProjectAdmissionVersionV2,
		ProjectAdmissionMode:    afclient.ProjectAdmissionModeAllRouted,
		EnabledProjectIDs:       []string{"proj_a"},
	}}
	if _, err := newTestProjectCmd(rw, "", []string{"mode", "enumerated"}); err != nil {
		t.Fatalf("project mode enumerated: %v", err)
	}
	if rw.written == nil {
		t.Fatal("project mode enumerated did not write the config")
	}
	if rw.written.AdmitsAnyRoutedProject() {
		t.Fatal("withdrawing consent left the machine on all-routed")
	}
}

func TestProjectModeRejectsUnknownMode(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{cfg: &afclient.DaemonYAML{}}
	_, err := newTestProjectCmd(rw, "", []string{"mode", "all_routed"})
	if err == nil {
		t.Fatal("project mode all_routed succeeded; a typo must not silently narrow the machine")
	}
	if !strings.Contains(err.Error(), "all_routed") {
		t.Fatalf("error %q should quote the rejected value", err)
	}
	if rw.written != nil {
		t.Fatal("a rejected mode still wrote the config")
	}
}
