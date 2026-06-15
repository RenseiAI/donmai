package vcs

import (
	"encoding/json"
	"fmt"
	"strings"
)

// graphqlEnvelope is the top-level shape of a `gh api graphql` JSON response.
// gh writes the raw GraphQL envelope ({ "data": ..., "errors": [...] }) to
// stdout.
type graphqlEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphqlError  `json:"errors"`
}

type graphqlError struct {
	Message string `json:"message"`
}

// mergeQueueEntry mirrors the enqueuePullRequest.mergeQueueEntry GraphQL object.
type mergeQueueEntry struct {
	State      string `json:"state"`
	Position   int    `json:"position"`
	EnqueuedAt string `json:"enqueuedAt"`
}

// decodeEnvelope parses the gh graphql envelope and returns the data payload,
// turning any GraphQL errors[] into a Go error (mirroring the TS graphql()
// helper that throws on response.errors).
func decodeEnvelope(stdout string) (json.RawMessage, error) {
	var env graphqlEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		return nil, fmt.Errorf("decode graphql response: %w", err)
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, len(env.Errors))
		for i, e := range env.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("graphql: %s", strings.Join(msgs, "; "))
	}
	return env.Data, nil
}

// parsePRNodeIDResponse extracts repository.pullRequest.id from a gh graphql
// response.
func parsePRNodeIDResponse(stdout string) (string, error) {
	data, err := decodeEnvelope(stdout)
	if err != nil {
		return "", err
	}
	var payload struct {
		Repository struct {
			PullRequest struct {
				ID string `json:"id"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("decode pr node id: %w", err)
	}
	if payload.Repository.PullRequest.ID == "" {
		return "", fmt.Errorf("pr node id missing in response")
	}
	return payload.Repository.PullRequest.ID, nil
}

// parseEnqueueResponse extracts enqueuePullRequest.mergeQueueEntry from a gh
// graphql response.
func parseEnqueueResponse(stdout string) (mergeQueueEntry, error) {
	data, err := decodeEnvelope(stdout)
	if err != nil {
		return mergeQueueEntry{}, err
	}
	var payload struct {
		EnqueuePullRequest struct {
			MergeQueueEntry mergeQueueEntry `json:"mergeQueueEntry"`
		} `json:"enqueuePullRequest"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return mergeQueueEntry{}, fmt.Errorf("decode enqueue response: %w", err)
	}
	return payload.EnqueuePullRequest.MergeQueueEntry, nil
}
