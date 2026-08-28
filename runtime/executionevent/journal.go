package executionevent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const (
	journalFileName    = "events.jsonl"
	ackFileName        = "ack.json"
	quarantineFileName = "quarantine.jsonl"
)

type ackState struct {
	StructuredSeq uint64 `json:"structuredSeq"`
}

type quarantineRecord struct {
	Record Record `json:"record"`
	Status int    `json:"status"`
	Reason string `json:"reason"`
}

// Journal is an append-only, fsync-backed source journal. The ack is advanced
// only after a successful platform response or an explicit durable
// quarantine disposition. A Journal is safe for concurrent append/flush use.
type Journal struct {
	mu      sync.Mutex
	dir     string
	lock    *os.File
	Records []Record
	Acked   uint64
}

// OpenJournal opens or creates one session's durable source journal.
func OpenJournal(dir, sessionID string) (*Journal, error) {
	if dir == "" {
		return nil, errors.New("executionevent: journal directory is required")
	}
	if sessionID == "" {
		return nil, errors.New("executionevent: session id is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("executionevent: create journal directory: %w", err)
	}
	j := &Journal{dir: dir}
	lock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}
	j.lock = lock
	if err := j.load(sessionID); err != nil {
		_ = j.Close()
		return nil, err
	}
	return j, nil
}

func acquireLock(dir string) (*os.File, error) {
	// Keep the inode in place and use advisory flock: unlike O_EXCL marker
	// files, the kernel releases this lock if the process crashes, allowing a
	// later runner to resume the durable journal.
	path := filepath.Join(dir, ".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is the validated per-session journal directory.
	if err != nil {
		return nil, fmt.Errorf("executionevent: acquire journal lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // Flock requires int; OS descriptors are int-sized.
		_ = f.Close()
		return nil, fmt.Errorf("executionevent: journal is already open: %w", err)
	}
	return f, nil
}

// Close releases the advisory process/crash-safe journal lock.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(j.lock.Fd()), syscall.LOCK_UN) //nolint:gosec // Flock requires int; OS descriptors are int-sized.
	err := j.lock.Close()
	j.lock = nil
	if err != nil {
		return fmt.Errorf("executionevent: close journal lock: %w", err)
	}
	return nil
}

func (j *Journal) load(sessionID string) error {
	f, err := os.Open(filepath.Join(j.dir, journalFileName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("executionevent: open journal: %w", err)
	}
	if err == nil {
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), MaxRecordBytes+4096)
		var previous uint64
		for scanner.Scan() {
			var record Record
			decoder := json.NewDecoder(bytesReader(scanner.Bytes()))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&record); err != nil {
				return fmt.Errorf("executionevent: decode journal record: %w", err)
			}
			if err := ValidateRecord(sessionID, record, previous); err != nil {
				return fmt.Errorf("executionevent: invalid journal record: %w", err)
			}
			j.Records = append(j.Records, record)
			previous = record.StructuredSeq
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("executionevent: read journal: %w", err)
		}
	}
	ackBytes, err := os.ReadFile(filepath.Join(j.dir, ackFileName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("executionevent: read ack: %w", err)
	}
	if err == nil {
		var ack ackState
		decoder := json.NewDecoder(bytesReader(ackBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&ack); err != nil {
			return fmt.Errorf("executionevent: decode ack: %w", err)
		}
		if ack.StructuredSeq > uint64(len(j.Records)) {
			return errors.New("executionevent: ack is ahead of journal")
		}
		j.Acked = ack.StructuredSeq
	}
	if err := j.loadQuarantine(sessionID); err != nil {
		return err
	}
	return nil
}

func (j *Journal) loadQuarantine(sessionID string) error {
	f, err := os.Open(filepath.Join(j.dir, quarantineFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("executionevent: open quarantine: %w", err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), MaxRecordBytes+4096)
	quarantined := make(map[uint64]bool)
	for scanner.Scan() {
		var entry quarantineRecord
		decoder := json.NewDecoder(bytesReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entry); err != nil {
			return fmt.Errorf("executionevent: decode quarantine record: %w", err)
		}
		if entry.Status != 400 && entry.Status != 404 && entry.Status != 409 && entry.Status != 413 {
			return fmt.Errorf("executionevent: invalid quarantine status %d", entry.Status)
		}
		if entry.Reason == "" || len(entry.Reason) > 256 {
			return errors.New("executionevent: invalid quarantine reason")
		}
		if err := validateRecordShape(sessionID, entry.Record); err != nil {
			return fmt.Errorf("executionevent: invalid quarantined record: %w", err)
		}
		if entry.Record.StructuredSeq > uint64(len(j.Records)) || j.Records[entry.Record.StructuredSeq-1].EventID != entry.Record.EventID {
			return errors.New("executionevent: quarantine record is not present in journal")
		}
		if quarantined[entry.Record.StructuredSeq] {
			return errors.New("executionevent: duplicate quarantined sequence")
		}
		quarantined[entry.Record.StructuredSeq] = true
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("executionevent: read quarantine: %w", err)
	}
	for j.Acked < uint64(len(j.Records)) && quarantined[j.Acked+1] {
		j.Acked++
	}
	return nil
}

// bytesReader avoids exposing a mutable bytes.Buffer to the decoder and keeps
// the journal's strict parsing helper small.
func bytesReader(b []byte) io.Reader { return &immutableReader{b: b} }

type immutableReader struct {
	b []byte
	i int
}

func (r *immutableReader) Read(p []byte) (int, error) {
	if r.i == len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// Append validates and fsyncs one source record before returning.
func (j *Journal) Append(sessionID string, record Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	var previous uint64
	if len(j.Records) > 0 {
		previous = j.Records[len(j.Records)-1].StructuredSeq
	}
	if err := ValidateRecord(sessionID, record, previous); err != nil {
		return err
	}
	b, err := MarshalCompact(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(j.dir, journalFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("executionevent: open journal append: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("executionevent: append journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("executionevent: sync journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("executionevent: close journal: %w", err)
	}
	j.Records = append(j.Records, record)
	return nil
}

// Pending returns a copy of records after the durable acknowledgement.
func (j *Journal) Pending() []Record {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Acked >= uint64(len(j.Records)) {
		return nil
	}
	return append([]Record(nil), j.Records[j.Acked:]...)
}

// NextSequence returns the next contiguous source sequence.
func (j *Journal) NextSequence() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.Records) == 0 {
		return 1
	}
	return j.Records[len(j.Records)-1].StructuredSeq + 1
}

// Ack durably advances the lowest-unacknowledged sequence.
func (j *Journal) Ack(seq uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if seq < j.Acked {
		return nil
	}
	if seq > uint64(len(j.Records)) {
		return errors.New("executionevent: ack exceeds journal")
	}
	if seq == j.Acked {
		return nil
	}
	return j.writeAckLocked(seq)
}

func (j *Journal) writeAckLocked(seq uint64) error {
	b, err := MarshalCompact(ackState{StructuredSeq: seq})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(j.dir, ".ack-*")
	if err != nil {
		return fmt.Errorf("executionevent: create ack temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("executionevent: chmod ack: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("executionevent: write ack: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("executionevent: sync ack: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("executionevent: close ack: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(j.dir, ackFileName)); err != nil {
		return fmt.Errorf("executionevent: install ack: %w", err)
	}
	if dir, err := os.Open(j.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	j.Acked = seq
	return nil
}

// Quarantine durably records a permanent response before acknowledging it.
func (j *Journal) Quarantine(records []Record, status int, reason string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(records) == 0 {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(j.dir, quarantineFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("executionevent: open quarantine: %w", err)
	}
	for _, record := range records {
		b, marshalErr := MarshalCompact(quarantineRecord{Record: record, Status: status, Reason: reason})
		if marshalErr != nil {
			_ = f.Close()
			return marshalErr
		}
		if _, writeErr := f.Write(append(b, '\n')); writeErr != nil {
			_ = f.Close()
			return fmt.Errorf("executionevent: append quarantine: %w", writeErr)
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("executionevent: sync quarantine: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("executionevent: close quarantine: %w", err)
	}
	return j.writeAckLocked(records[len(records)-1].StructuredSeq)
}

// Directory returns the journal directory for diagnostics and tests.
func (j *Journal) Directory() string { return j.dir }
