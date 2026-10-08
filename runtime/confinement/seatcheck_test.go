package confinement

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"time"
)

// seatCheckEnv turns the test binary into a seat check: started inside a
// boundary, it runs the plan its value names and writes the results beside
// it, so a live test judges network reach and denials from inside a
// confined seat. The plan and the results sit in a writable root.
const seatCheckEnv = "DONMAI_CONFINEMENT_TEST_SEAT_CHECK"

type seatCheckPlan struct {
	ResultPath string          `json:"resultPath"`
	Steps      []seatCheckStep `json:"steps"`
}

// seatCheckStep is one check: "dial" a TCP address, "get" a URL, "read" or
// "write" a file.
type seatCheckStep struct {
	ID     string `json:"id"`
	Op     string `json:"op"`
	Target string `json:"target"`
}

type seatCheckResult struct {
	ID     string `json:"id"`
	Err    string `json:"err,omitempty"`
	Status int    `json:"status,omitempty"`
}

func runSeatCheckFromEnv() (bool, int) {
	planPath := os.Getenv(seatCheckEnv)
	if planPath == "" {
		return false, 0
	}
	raw, err := os.ReadFile(planPath) //nolint:gosec // G304: the check's own plan.
	if err != nil {
		return true, 3
	}
	var plan seatCheckPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return true, 3
	}
	results := make([]seatCheckResult, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		results = append(results, runSeatCheckStep(step))
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		return true, 3
	}
	if err := os.WriteFile(plan.ResultPath, encoded, 0o600); err != nil {
		return true, 3
	}
	return true, 0
}

func runSeatCheckStep(step seatCheckStep) seatCheckResult {
	result := seatCheckResult{ID: step.ID}
	var err error
	switch step.Op {
	case "dial":
		var conn net.Conn
		if conn, err = net.DialTimeout("tcp", step.Target, 10*time.Second); err == nil {
			err = conn.Close()
		}
	case "get":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var req *http.Request
		if req, err = http.NewRequestWithContext(ctx, http.MethodGet, step.Target, http.NoBody); err == nil {
			var resp *http.Response
			if resp, err = http.DefaultClient.Do(req); err == nil {
				result.Status = resp.StatusCode
				err = resp.Body.Close()
			}
		}
	case "read":
		_, err = os.ReadFile(step.Target)
	case "write":
		err = appendOrCreate(step.Target)
	default:
		err = errors.New("unknown seat check")
	}
	if err != nil {
		result.Err = err.Error()
	}
	return result
}

func appendOrCreate(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: the check's own target.
	if err != nil {
		return err
	}
	if _, err := f.WriteString("seat\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
