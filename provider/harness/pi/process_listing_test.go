package pi

import (
	"bytes"
	"strings"
)

// splitNULStrings splits a NUL-separated block into its non-empty strings.
func splitNULStrings(raw []byte) []string {
	var out []string
	for _, part := range bytes.Split(raw, []byte{0}) {
		if len(part) > 0 {
			out = append(out, string(part))
		}
	}
	return out
}

// sessionProcessListings returns the listing strings of every live
// same-user process whose exec-time environment names this session's state
// root — the pi child itself and, while one runs, any tool subprocess it
// started (callers sample between tool calls). Processes that exit or deny
// the read mid-scan are skipped.
func sessionProcessListings(stateRoot string) (map[int][]string, error) {
	pids, err := sameUserPIDs()
	if err != nil {
		return nil, err
	}
	marker := piCodingAgentSessionDirEnvVar + "=" + stateRoot
	out := map[int][]string{}
	for _, pid := range pids {
		strs, err := processListingStrings(pid)
		if err != nil {
			continue
		}
		for _, s := range strs {
			if s == marker || strings.HasPrefix(s, marker) {
				out[pid] = strs
				break
			}
		}
	}
	return out, nil
}
