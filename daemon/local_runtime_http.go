package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/localqueue"
)

func localBearer(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return ""
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if strings.TrimSpace(token) != token {
		return ""
	}
	return token
}

func readLocalBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		return nil, errors.New("local request exceeds its body bound")
	}
	if _, err = executioncell.NormalizeOperationalPayload(raw); err != nil {
		return nil, errors.New("local request must be a unique-key JSON object")
	}
	return raw, nil
}

func decodeLocalClosed(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("local request has invalid or unknown fields")
	}
	return nil
}

func (s *Server) handleLocalRuntime(w http.ResponseWriter, r *http.Request) {
	local := s.daemon.localRuntime.Load()
	if local == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, localRuntimePrefix+"/sessions/") {
		if local.auth.VerifyOperator(localBearer(r)) != nil {
			http.Error(w, "operator authentication required", http.StatusUnauthorized)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, localRuntimePrefix+"/sessions/")
		projection, err := local.store.Session(r.Context(), local.identity.ScopeID, id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		local.mu.Lock()
		reason := local.holdReasons[id]
		local.mu.Unlock()
		status := ""
		if projection.Terminal != nil {
			status = projection.Terminal.Status
		}
		writeJSON(w, http.StatusOK, map[string]any{"session": projection.Admission.Envelope.Session, "dispatchState": projection.Admission.DispatchState, "terminalStatus": status, "holdReason": reason, "publications": projection.Publications})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == localRuntimePrefix+"/intake" {
		if local.auth.VerifyOperator(localBearer(r)) != nil {
			http.Error(w, "operator authentication required", http.StatusUnauthorized)
			return
		}
		raw, err := readLocalBody(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var request LocalIntakeRequest
		if err = decodeLocalClosed(raw, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, err := local.intake(r.Context(), request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	const suffix = "/api/sessions/"
	if !strings.HasPrefix(r.URL.Path, localRuntimePrefix+suffix) {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, localRuntimePrefix+suffix), "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	_, err := local.authenticate(r.Context(), parts[0], localBearer(r))
	if err != nil {
		http.Error(w, "attempt authentication required", http.StatusUnauthorized)
		return
	}
	raw, err := readLocalBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var identity struct {
		WorkerID string `json:"workerId"`
	}
	if err = json.Unmarshal(raw, &identity); err != nil || identity.WorkerID != local.identity.WorkerID {
		http.Error(w, "request worker does not match authenticated attempt", http.StatusForbidden)
		return
	}
	local.ops.Lock()
	defer local.ops.Unlock()
	// Re-read after acquiring the transaction owner: a terminal result may have
	// landed while the request body was being read. Never refresh a stale phase.
	projection, err := local.store.Session(r.Context(), local.identity.ScopeID, parts[0])
	if err != nil {
		http.Error(w, "local session state unavailable", http.StatusServiceUnavailable)
		return
	}
	if err = local.receive(w, r, projection, parts[1], raw); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
	}
}

func (l *localRuntime) receive(w http.ResponseWriter, r *http.Request, projection localqueue.SessionProjection, route string, raw []byte) error {
	switch route {
	case "status":
		var state struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return err
		}
		if state.Status == "running" {
			if projection.Terminal != nil {
				return errors.New("terminal local session cannot return to running")
			}
			digest, err := l.persistObservation(projection, route, raw, false)
			if err != nil {
				return err
			}
			if err = l.recordLiveness(r.Context(), projection, digest); err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, map[string]any{"observed": true, "bodySha256": digest})
			return nil
		}
		receipt, err := l.store.CommitTerminal(r.Context(), *projection.Attempt, raw)
		if err != nil {
			return err
		}
		l.releaseTerminal(projection.Admission.Envelope.Session.SessionID)
		writeJSON(w, http.StatusOK, map[string]any{"committed": true, "bodySha256": receipt.BodySHA256, "revision": receipt.Revision})
		return nil
	case "lock-refresh":
		var request struct {
			WorkerID            string            `json:"workerId,omitempty"`
			IssueID             string            `json:"issueId,omitempty"`
			AckedInject         string            `json:"ackedInject,omitempty"`
			DeadLetteredInjects []json.RawMessage `json:"deadLetteredInjects,omitempty"`
			SessionClass        string            `json:"sessionClass,omitempty"`
		}
		if err := decodeLocalClosed(raw, &request); err != nil {
			return err
		}
		if request.SessionClass != "" || request.AckedInject != "" || len(request.DeadLetteredInjects) > 0 {
			return errors.New("local receiver does not grant interactive or injection authority")
		}
		stop := projection.Terminal != nil
		for _, observation := range projection.Observations {
			if observation.Kind == localqueue.ObservationStopRequest {
				stop = true
			}
		}
		if !stop {
			if err := l.recordLiveness(r.Context(), projection, localDigest(raw)); err != nil {
				return err
			}
		}
		writeJSON(w, http.StatusOK, map[string]bool{"refreshed": !stop, "stop": stop})
		return nil
	case "completion", "activity", "step-heartbeat":
		if err := validateLocalObservation(route, raw); err != nil {
			return err
		}
		digest, err := l.persistObservation(projection, route, raw, projection.Terminal != nil)
		if err != nil {
			return err
		}
		if route == "step-heartbeat" && projection.Terminal == nil {
			if err = l.recordLiveness(r.Context(), projection, digest); err != nil {
				return err
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"stored": true, "bodySha256": digest})
		return nil
	default:
		http.NotFound(w, r)
		return nil
	}
}

func validateLocalObservation(route string, raw []byte) error {
	switch route {
	case "completion":
		var body struct {
			WorkerID string `json:"workerId"`
			Summary  string `json:"summary"`
			Result   string `json:"result"`
		}
		if err := decodeLocalClosed(raw, &body); err != nil {
			return err
		}
		if body.Result != "success" && body.Result != "failure" {
			return errors.New("unsupported local completion result")
		}
	case "activity":
		var body struct {
			WorkerID string          `json:"workerId"`
			Activity json.RawMessage `json:"activity"`
		}
		if err := decodeLocalClosed(raw, &body); err != nil {
			return err
		}
		var activity struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(body.Activity, &activity); err != nil || activity.Type == "" {
			return errors.New("local activity requires typed content")
		}
	case "step-heartbeat":
		var body struct {
			WorkerID  string `json:"workerId"`
			EmittedAt string `json:"emittedAt"`
		}
		if err := decodeLocalClosed(raw, &body); err != nil {
			return err
		}
		if _, err := time.Parse(time.RFC3339Nano, body.EmittedAt); err != nil {
			return errors.New("local step heartbeat requires an emitted timestamp")
		}
	}
	return nil
}

func localDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func (l *localRuntime) recordLiveness(ctx context.Context, projection localqueue.SessionProjection, digest string) error {
	if projection.Attempt == nil {
		return errors.New("local liveness requires an owned attempt")
	}
	return l.store.RecordObservation(ctx, *projection.Attempt, localqueue.BoundedTypedObservation{Kind: localqueue.ObservationLiveness, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Evidence: digest})
}

func (l *localRuntime) persistObservation(projection localqueue.SessionProjection, route string, raw []byte, replayOnly bool) (string, error) {
	digest := localDigest(raw)
	name := localDigest([]byte(projection.Attempt.AttemptID+"\x00"+route+"\x00"+digest)) + ".json"
	path := l.options.QueueRoot + ".observations"
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !replayOnly {
		if err = os.Mkdir(path, 0o700); err != nil {
			return "", err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("local observation root is not private")
	}
	// A retry may follow an ambiguous directory publication. Reaffirm the
	// parent's link durability even when the observation directory already exists.
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	err = l.syncObservationDirectory(parent)
	_ = parent.Close()
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	opened, statErr := root.Stat(".")
	if statErr != nil || !opened.IsDir() || opened.Mode().Perm() != 0o700 || !os.SameFile(info, opened) {
		return "", errors.New("local observation root changed while opening")
	}
	if existing, readErr := readLocalObservation(root, name); readErr == nil {
		if !bytes.Equal(existing, raw) {
			return "", errors.New("local observation digest conflict")
		}
		if err = l.syncObservationDirectory(root); err != nil {
			return "", err
		}
		return digest, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", readErr
	}
	if replayOnly {
		return "", errors.New("terminal local attempt permits only exact observation replay")
	}
	temp, err := newLocalReference("observation_")
	if err != nil {
		return "", err
	}
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Remove(temp) }()
	written, writeErr := file.Write(raw)
	if writeErr == nil && written != len(raw) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err = errors.Join(writeErr, closeErr); err != nil {
		return "", err
	}
	if err = root.Link(temp, name); err != nil {
		return "", fmt.Errorf("publish local observation: %w", err)
	}
	if err = root.Remove(temp); err != nil {
		return "", err
	}
	if err = l.syncObservationDirectory(root); err != nil {
		return "", err
	}
	return digest, nil
}

func syncLocalDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (l *localRuntime) requestStop(ctx context.Context, sessionID string) error {
	l.ops.Lock()
	defer l.ops.Unlock()
	projection, err := l.store.Session(ctx, l.identity.ScopeID, sessionID)
	if err != nil {
		return err
	}
	if projection.Terminal != nil {
		return nil
	}
	if projection.Attempt == nil {
		return errors.New("local pending session has no running attempt")
	}
	return l.store.RecordObservation(ctx, *projection.Attempt, localqueue.BoundedTypedObservation{Kind: localqueue.ObservationStopRequest, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}

func (l *localRuntime) syncObservationDirectory(root *os.Root) error {
	if l.observationSync != nil {
		return l.observationSync(root)
	}
	return syncLocalDirectory(root)
}

func readLocalObservation(root *os.Root, name string) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	private := func(info os.FileInfo) bool {
		return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o600 && info.Size() <= 1<<20
	}
	if !private(before) {
		return nil, errors.New("local observation file is not private and bounded")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !private(opened) || !os.SameFile(before, opened) {
		return nil, errors.New("local observation file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	after, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !private(after) || !os.SameFile(opened, after) || len(raw) > 1<<20 || int64(len(raw)) != after.Size() {
		return nil, errors.New("local observation file changed while reading")
	}
	return raw, nil
}
