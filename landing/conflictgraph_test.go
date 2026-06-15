package landing

import (
	"reflect"
	"sort"
	"testing"
)

func TestConflictGraph_Conflicts(t *testing.T) {
	tests := []struct {
		name      string
		proposals map[int][]string // added in numeric order
		a, b      int
		want      bool
	}{
		{
			name:      "disjoint files do not conflict",
			proposals: map[int][]string{1: {"src/a.ts", "src/b.ts"}, 2: {"src/c.ts", "src/d.ts"}},
			a:         1, b: 2, want: false,
		},
		{
			name:      "overlapping files conflict",
			proposals: map[int][]string{1: {"src/a.ts", "src/b.ts"}, 2: {"src/b.ts", "src/c.ts"}},
			a:         1, b: 2, want: true,
		},
		{
			name:      "empty file list is a universal conflict",
			proposals: map[int][]string{1: {"src/a.ts"}, 2: {}},
			a:         1, b: 2, want: true,
		},
		{
			name:      "unknown proposal does not conflict",
			proposals: map[int][]string{1: {"src/a.ts"}},
			a:         1, b: 99, want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewConflictGraph()
			addInOrder(g, tt.proposals)
			if got := g.Conflicts(tt.a, tt.b); got != tt.want {
				t.Errorf("Conflicts(%d,%d) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			// Conflicts is symmetric.
			if got := g.Conflicts(tt.b, tt.a); got != tt.want {
				t.Errorf("Conflicts(%d,%d) (reversed) = %v, want %v", tt.b, tt.a, got, tt.want)
			}
		})
	}
}

func TestConflictGraph_SharedFiles(t *testing.T) {
	tests := []struct {
		name      string
		proposals map[int][]string
		a, b      int
		want      []string
	}{
		{
			name:      "no shared files",
			proposals: map[int][]string{1: {"src/a.ts", "src/b.ts"}, 2: {"src/c.ts", "src/d.ts"}},
			a:         1, b: 2, want: nil,
		},
		{
			name:      "one shared file",
			proposals: map[int][]string{1: {"src/a.ts", "src/b.ts"}, 2: {"src/b.ts", "src/c.ts"}},
			a:         1, b: 2, want: []string{"src/b.ts"},
		},
		{
			name:      "unknown proposal yields nil",
			proposals: map[int][]string{1: {"src/a.ts"}},
			a:         1, b: 2, want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewConflictGraph()
			addInOrder(g, tt.proposals)
			got := g.SharedFiles(tt.a, tt.b)
			sort.Strings(got)
			want := append([]string(nil), tt.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
				t.Errorf("SharedFiles(%d,%d) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestConflictGraph_IndependentBatches(t *testing.T) {
	tests := []struct {
		name string
		// adds is an ordered list of proposals so insertion order is deterministic.
		adds         []proposalFiles
		maxBatchSize int
		want         [][]int
	}{
		{
			name:         "three independent proposals share one batch",
			adds:         []proposalFiles{{1, []string{"src/a.ts"}}, {2, []string{"src/b.ts"}}, {3, []string{"src/c.ts"}}},
			maxBatchSize: 0,
			want:         [][]int{{1, 2, 3}},
		},
		{
			name:         "conflicting proposals split into batches",
			adds:         []proposalFiles{{1, []string{"src/a.ts"}}, {2, []string{"src/a.ts"}}, {3, []string{"src/b.ts"}}},
			maxBatchSize: 0,
			want:         [][]int{{1, 3}, {2}},
		},
		{
			name:         "maxBatchSize caps batch width",
			adds:         []proposalFiles{{1, []string{"src/a.ts"}}, {2, []string{"src/b.ts"}}, {3, []string{"src/c.ts"}}},
			maxBatchSize: 2,
			want:         [][]int{{1, 2}, {3}},
		},
		{
			name:         "conflict chain isolates the bridge proposal",
			adds:         []proposalFiles{{1, []string{"src/a.ts"}}, {2, []string{"src/a.ts", "src/b.ts"}}, {3, []string{"src/b.ts"}}},
			maxBatchSize: 0,
			want:         [][]int{{1, 3}, {2}},
		},
		{
			name:         "empty graph yields no batches",
			adds:         nil,
			maxBatchSize: 0,
			want:         nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewConflictGraph()
			for _, a := range tt.adds {
				g.AddProposal(a.proposal, a.files)
			}
			got := g.IndependentBatches(tt.maxBatchSize)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("IndependentBatches(%d) = %v, want %v", tt.maxBatchSize, got, tt.want)
			}
		})
	}
}

func TestConflictGraph_Size(t *testing.T) {
	g := NewConflictGraph()
	if got := g.Size(); got != 0 {
		t.Errorf("empty Size() = %d, want 0", got)
	}
	g.AddProposal(1, []string{"a"})
	g.AddProposal(2, []string{"b"})
	if got := g.Size(); got != 2 {
		t.Errorf("Size() = %d, want 2", got)
	}
}

func TestBuildConflictGraph(t *testing.T) {
	manifests := []FileManifest{
		{Proposal: 1, SourceBranch: "feat-1", Files: []string{"src/a.ts"}},
		{Proposal: 2, SourceBranch: "feat-2", Files: []string{"src/b.ts"}},
		{Proposal: 3, SourceBranch: "feat-3", Files: []string{"src/a.ts", "src/c.ts"}},
	}
	g := BuildConflictGraph(manifests)

	if got := g.Size(); got != 3 {
		t.Fatalf("Size() = %d, want 3", got)
	}
	if g.Conflicts(1, 2) {
		t.Error("Conflicts(1,2) = true, want false")
	}
	if !g.Conflicts(1, 3) {
		t.Error("Conflicts(1,3) = false, want true (both touch src/a.ts)")
	}
	if g.Conflicts(2, 3) {
		t.Error("Conflicts(2,3) = true, want false")
	}
}

// proposalFiles pairs a proposal number with its file set for ordered adds.
type proposalFiles struct {
	proposal int
	files    []string
}

// addInOrder adds proposals to g sorted by proposal number, so batching order is
// deterministic regardless of map iteration order.
func addInOrder(g *ConflictGraph, proposals map[int][]string) {
	nums := make([]int, 0, len(proposals))
	for n := range proposals {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		g.AddProposal(n, proposals[n])
	}
}
