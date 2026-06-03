package kgextract

import (
	"encoding/json"
	"errors"
	"strings"
)

// rawGraph is the loosely-typed shape the model emits. Node `type` is read as a
// string first so an out-of-set value can be dropped (rather than failing the
// whole decode) during validation.
type rawGraph struct {
	Nodes []rawNode `json:"nodes"`
	Edges []rawEdge `json:"edges"`
}

type rawNode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

type rawEdge struct {
	SourceNodeID     string `json:"sourceNodeId"`
	TargetNodeID     string `json:"targetNodeId"`
	RelationshipName string `json:"relationshipName"`
}

// parseGraph parses a model emit into a validated ExtractedGraph.
//
// It is tolerant of the common ways a constrained LLM wraps JSON:
//   - a ```json … ``` (or bare ``` … ```) markdown fence,
//   - leading/trailing prose around a single top-level object.
//
// It then VALIDATES each node/edge against the ExtractedNode/ExtractedEdge shape
// (defense-in-depth; the platform re-validates and is the source of truth):
//   - a node is kept only when id+name are non-empty AND type is one of the
//     closed NodeType set; invalid nodes are dropped.
//   - an edge is kept only when sourceNodeId+targetNodeId+relationshipName are
//     non-empty; invalid edges are dropped.
//
// Returns an error only when NO top-level JSON object can be located/decoded at
// all (an unparseable emit). A successfully-decoded-but-empty graph is NOT an
// error: it returns an empty (non-nil) graph so a model that legitimately finds
// no triples reports an empty graph rather than a failure.
func parseGraph(raw string) (ExtractedGraph, error) {
	out := ExtractedGraph{Nodes: []ExtractedNode{}, Edges: []ExtractedEdge{}}

	jsonText, ok := extractJSONObject(raw)
	if !ok {
		return out, errors.New("kgextract: no JSON object found in emit")
	}

	var g rawGraph
	if err := json.Unmarshal([]byte(jsonText), &g); err != nil {
		return out, err
	}

	for _, n := range g.Nodes {
		nt := NodeType(n.Type)
		if n.ID == "" || n.Name == "" || !isValidNodeType(nt) {
			continue // drop invalid node (defense-in-depth)
		}
		out.Nodes = append(out.Nodes, ExtractedNode{
			ID:          n.ID,
			Name:        n.Name,
			Type:        nt,
			Description: n.Description,
		})
	}
	for _, e := range g.Edges {
		if e.SourceNodeID == "" || e.TargetNodeID == "" || e.RelationshipName == "" {
			continue // drop invalid edge (defense-in-depth)
		}
		out.Edges = append(out.Edges, ExtractedEdge{
			SourceNodeID:     e.SourceNodeID,
			TargetNodeID:     e.TargetNodeID,
			RelationshipName: e.RelationshipName,
		})
	}
	return out, nil
}

// extractJSONObject locates the JSON object inside a model emit. It strips an
// optional markdown code fence, then falls back to the substring between the
// first '{' and the last '}'. Returns ("", false) when no object delimiter pair
// is present.
func extractJSONObject(raw string) (string, bool) {
	s := strings.TrimSpace(raw)

	// Strip a leading ```json / ``` fence and its trailing ```.
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		}
		if end := strings.LastIndex(s, "```"); end >= 0 {
			s = s[:end]
		}
		s = strings.TrimSpace(s)
	}

	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		return s, true
	}

	// Fall back to the outermost { … } span (handles surrounding prose).
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < 0 || end < start {
		return "", false
	}
	return s[start : end+1], true
}
