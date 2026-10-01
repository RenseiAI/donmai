//go:build linux

package daemon

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

func TestShimPTYFailureDiagnosticOnlyPrintsBoundedNumericFields(t *testing.T) {
	const hidden = "synthetic-private-value"
	status := []byte("Name:\t" + hidden + "\nSeccomp:\t2\nNoNewPrivs:\t1\nThreads:\t7\nEnv:\t" + hidden + "\n")
	facts := shimPTYFacts{
		errno: 1, pid: 12, pgid: 13, sid: 14,
		nproc: [2]uint64{15, 16}, nprocOK: true,
		nofile: [2]uint64{17, 18}, nofileOK: true,
		status: parseShimPTYStatus(status),
		pids:   [2]string{parseShimPTYPids([]byte("19\n")), parseShimPTYPids([]byte("max\n"))},
	}
	line := facts.line()
	if len(line) > 512 || strings.Contains(line, hidden) || facts.status != [3]string{"2", "1", "7"} {
		t.Fatal("diagnostic leaked or lost the bounded status fields")
	}
	allowed := map[string]bool{
		"errno": true, "pid": true, "pgid": true, "sid": true,
		"nproc_cur": true, "nproc_max": true, "nofile_cur": true, "nofile_max": true,
		"seccomp": true, "no_new_privs": true, "threads": true,
		"mount_pids_current": true, "mount_pids_max": true,
	}
	fields := strings.Fields(line)
	if len(fields) != len(allowed)+1 || fields[0] != "shim-pty-testdiag" {
		t.Fatal("diagnostic emitted an unexpected field count or prefix")
	}
	for _, field := range fields[1:] {
		name, value, ok := strings.Cut(field, "=")
		if !ok || !allowed[name] {
			t.Fatal("diagnostic emitted a non-allowlisted field")
		}
		delete(allowed, name)
		if name == "mount_pids_max" && value == "max" {
			continue
		}
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			t.Fatal("diagnostic emitted a nonnumeric value")
		}
	}
	if len(allowed) != 0 {
		t.Fatal("diagnostic omitted an expected numeric field")
	}
	if values := parseShimPTYStatus(bytes.Repeat([]byte("x"), shimPTYStatusLimit+1)); values != [3]string{} {
		t.Fatal("oversized status was parsed")
	}
	if values := parseShimPTYStatus([]byte("Seccomp: 2 " + hidden + "\nThreads: 8\n")); values != [3]string{"", "", "8"} {
		t.Fatal("status parser accepted text appended to a numeric field")
	}
	if parseShimPTYPids([]byte("19 "+hidden)) != "" || parseShimPTYPids(bytes.Repeat([]byte("9"), 65)) != "" {
		t.Fatal("cgroup parser accepted nonnumeric or oversized content")
	}
}
