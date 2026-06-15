package landing

// ConflictGraph is an undirected graph over proposals where an edge means two
// proposals modify at least one common file (or one of them has an empty file
// list, which is treated as a universal conflict so an unknown change set is
// never landed concurrently with anything).
//
// Greedy graph coloring (IndependentBatches) partitions proposals into batches
// of mutually non-conflicting proposals that may land in parallel.
//
// Ported from donmai-libraries merge-queue/conflict-graph.ts. This is a pure,
// deterministic data structure with no I/O.
type ConflictGraph struct {
	// proposalFiles maps a proposal number to its set of modified files.
	proposalFiles map[int]map[string]struct{}
	// order preserves insertion order of proposals so batching is deterministic
	// (Go map iteration order is randomized).
	order []int
	// edges is the adjacency list: proposal -> set of conflicting proposals.
	edges map[int]map[int]struct{}
}

// NewConflictGraph returns an empty conflict graph.
func NewConflictGraph() *ConflictGraph {
	return &ConflictGraph{
		proposalFiles: make(map[int]map[string]struct{}),
		edges:         make(map[int]map[int]struct{}),
	}
}

// BuildConflictGraph builds a conflict graph from a set of file manifests, in
// manifest order.
func BuildConflictGraph(manifests []FileManifest) *ConflictGraph {
	g := NewConflictGraph()
	for _, m := range manifests {
		g.AddProposal(m.Proposal, m.Files)
	}
	return g
}

// AddProposal records a proposal's file manifest and computes conflict edges
// against every previously added proposal. Re-adding an existing proposal
// overwrites its file set but does not recompute edges for already-present
// pairs; callers should add each proposal once.
func (g *ConflictGraph) AddProposal(proposal int, files []string) {
	fileSet := make(map[string]struct{}, len(files))
	for _, f := range files {
		fileSet[f] = struct{}{}
	}

	if _, seen := g.proposalFiles[proposal]; !seen {
		g.order = append(g.order, proposal)
	}
	g.proposalFiles[proposal] = fileSet
	if g.edges[proposal] == nil {
		g.edges[proposal] = make(map[int]struct{})
	}

	for _, other := range g.order {
		if other == proposal {
			continue
		}
		otherFiles := g.proposalFiles[other]
		if overlaps(fileSet, otherFiles) {
			g.edges[proposal][other] = struct{}{}
			if g.edges[other] == nil {
				g.edges[other] = make(map[int]struct{})
			}
			g.edges[other][proposal] = struct{}{}
		}
	}
}

// overlaps reports whether two file sets conflict. An empty set on either side
// is a universal conflict (an unknown/uncomputed change set must not land
// concurrently with anything).
func overlaps(a, b map[string]struct{}) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	// Iterate the smaller set for fewer lookups.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	for f := range small {
		if _, ok := large[f]; ok {
			return true
		}
	}
	return false
}

// Conflicts reports whether two proposals have conflicting file changes.
func (g *ConflictGraph) Conflicts(a, b int) bool {
	if adj, ok := g.edges[a]; ok {
		_, conflict := adj[b]
		return conflict
	}
	return false
}

// SharedFiles returns the files modified by both proposals, in no particular
// order. Returns nil if either proposal is unknown.
func (g *ConflictGraph) SharedFiles(a, b int) []string {
	fa, oka := g.proposalFiles[a]
	fb, okb := g.proposalFiles[b]
	if !oka || !okb {
		return nil
	}
	var shared []string
	for f := range fa {
		if _, ok := fb[f]; ok {
			shared = append(shared, f)
		}
	}
	return shared
}

// IndependentBatches partitions proposals into batches of mutually
// non-conflicting proposals via greedy coloring, preserving insertion order so
// the first batch holds the highest-priority proposals that can land together.
// maxBatchSize <= 0 means unlimited.
func (g *ConflictGraph) IndependentBatches(maxBatchSize int) [][]int {
	if len(g.order) == 0 {
		return nil
	}

	var batches [][]int
	assigned := make(map[int]struct{}, len(g.order))

	for _, p := range g.order {
		if _, done := assigned[p]; done {
			continue
		}
		placed := false
		for i := range batches {
			if maxBatchSize > 0 && len(batches[i]) >= maxBatchSize {
				continue
			}
			if !g.conflictsWithBatch(p, batches[i]) {
				batches[i] = append(batches[i], p)
				assigned[p] = struct{}{}
				placed = true
				break
			}
		}
		if !placed {
			batches = append(batches, []int{p})
			assigned[p] = struct{}{}
		}
	}
	return batches
}

func (g *ConflictGraph) conflictsWithBatch(p int, batch []int) bool {
	adj := g.edges[p]
	for _, member := range batch {
		if _, conflict := adj[member]; conflict {
			return true
		}
	}
	return false
}

// Size returns the number of proposals in the graph.
func (g *ConflictGraph) Size() int {
	return len(g.proposalFiles)
}
