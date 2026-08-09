package worker

// MergeCapabilities returns the operator-supplied capability tags with the
// required tags appended, preserving order and removing duplicates. Empty tags
// are dropped.
//
// Every registration path uses it so a worker ALWAYS advertises the capability
// tags for the lanes it actually runs, regardless of what an operator passed:
// the coordinator gates a claim on the advertised tag, so a lane that is wired
// but not advertised never receives work, and a tag that is advertised without
// the lane wired makes the worker claim work it cannot run.
func MergeCapabilities(operator []string, required ...string) []string {
	seen := make(map[string]struct{}, len(operator)+len(required))
	out := make([]string, 0, len(operator)+len(required))
	for _, c := range append(append([]string{}, operator...), required...) {
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}
