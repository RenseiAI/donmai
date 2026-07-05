package codeintel

// pagerank.go — iterative PageRank over the file import graph. Faithful port of
// the TS PageRank class in
// donmai-libraries/packages/code-intelligence/src/repo-map/pagerank.ts
// (damping 0.85, up to 100 iterations, convergence tolerance 1e-6, in-link /
// out-degree bookkeeping).

import (
	"math"
	"sort"
)

// PageRank computes structural-importance scores over a directed graph.
type PageRank struct {
	damping    float64
	iterations int
	tolerance  float64
}

// NewPageRank returns a PageRank with the TS-reference defaults.
func NewPageRank() *PageRank {
	return &PageRank{damping: 0.85, iterations: 100, tolerance: 1e-6}
}

// Compute returns node -> PageRank score for the given adjacency (node -> set of
// nodes it links to, i.e. imports). Nodes are processed in sorted order so the
// result is deterministic regardless of Go map iteration order.
func (p *PageRank) Compute(adjacency map[string]map[string]struct{}) map[string]float64 {
	nodes := make([]string, 0, len(adjacency))
	for n := range adjacency {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	n := len(nodes)
	if n == 0 {
		return map[string]float64{}
	}
	nf := float64(n)

	inLinks := make(map[string][]string, n)
	outDegree := make(map[string]int, n)
	for _, node := range nodes {
		inLinks[node] = nil
		outDegree[node] = len(adjacency[node])
	}
	for _, from := range nodes {
		for to := range adjacency[from] {
			if _, ok := inLinks[to]; ok {
				inLinks[to] = append(inLinks[to], from)
			}
		}
	}

	scores := make(map[string]float64, n)
	initial := 1.0 / nf
	for _, node := range nodes {
		scores[node] = initial
	}

	for iter := 0; iter < p.iterations; iter++ {
		newScores := make(map[string]float64, n)
		maxDelta := 0.0
		for _, node := range nodes {
			sum := 0.0
			for _, linker := range inLinks[node] {
				if od := outDegree[linker]; od > 0 {
					sum += scores[linker] / float64(od)
				}
			}
			ns := (1-p.damping)/nf + p.damping*sum
			newScores[node] = ns
			if d := math.Abs(ns - scores[node]); d > maxDelta {
				maxDelta = d
			}
		}
		scores = newScores
		if maxDelta < p.tolerance {
			break
		}
	}
	return scores
}
