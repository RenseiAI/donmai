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
	"time"
)

const (
	journalFileName    = "events.jsonl"
	ackFileName        = "ack.json"
	quarantineFileName = "quarantine.jsonl"
)

type ackState struct {
	StructuredSeq uint64     `json:"structuredSeq"`
	AckedAt       *time.Time `json:"ackedAt,omitempty"`
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
	mu        sync.Mutex
	dir       string
	sessionID string
	lock      *os.File
	Records   []Record
	Acked     uint64
	ackedAt   *time.Time
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
	j.sessionID = sessionID
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
		if ack.AckedAt != nil && !ack.AckedAt.IsZero() {
			utc := ack.AckedAt.UTC()
			j.ackedAt = &utc
		}
	}
	ackSeqBeforeQuarantine := j.Acked
	if err := j.loadQuarantine(sessionID); err != nil {
		return err
	}
	if j.Acked != ackSeqBeforeQuarantine {
		j.ackedAt = nil
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
	now := time.Now().UTC()
	b, err := MarshalCompact(ackState{StructuredSeq: seq, AckedAt: &now})
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
	j.ackedAt = &now
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

// JournalSnapshotStatus is the bounded availability status for inspection.
type JournalSnapshotStatus string

const (
	// JournalSnapshotAvailable means all durable files reread and validated.
	JournalSnapshotAvailable JournalSnapshotStatus = "available"
	// JournalSnapshotUnavailable means inspection could not validate the files.
	JournalSnapshotUnavailable JournalSnapshotStatus = "unavailable"
)

// Bounded unavailable reasons; no paths, records, or raw errors cross.
const (
	JournalSnapshotReadError   = "read_error"
	JournalSnapshotInvalidData = "invalid_data"
)

// JournalSnapshot reports backlog and acknowledgement health without exposing
// record content, paths, reasons, tokens, or credentials. Available
// pointer-to-zero is measured healthy zero; nil is unknown/not applicable.
type JournalSnapshot struct {
	Status                  JournalSnapshotStatus `json:"status"`
	UnavailableReason       string                `json:"unavailableReason,omitempty"`
	PendingRecords          *uint64               `json:"pendingRecords,omitempty"`
	PendingBytes            *uint64               `json:"pendingBytes,omitempty"`
	OldestUnacknowledgedAge *time.Duration        `json:"oldestUnacknowledgedAge,omitempty"`
	AckedSequence           *uint64               `json:"ackedSequence,omitempty"`
	AckedAt                 *time.Time            `json:"ackedAt,omitempty"`
	QuarantineTotal         *uint64               `json:"quarantineTotal,omitempty"`
	QuarantineConflicts     *uint64               `json:"quarantineConflicts,omitempty"`
}

func unavailableSnapshot(reason string) JournalSnapshot {
	return JournalSnapshot{Status: JournalSnapshotUnavailable, UnavailableReason: reason}
}

func uint64SnapshotValue(v uint64) *uint64 { out := v; return &out }

// Snapshot holds j.mu, rereads and validates durable files without writes,
// applies quarantine recovery, sums pending JSONL bytes with newlines, and
// clamps negative oldest age to zero. Failures return unavailable with nil
// metrics and a bounded reason.
func (j *Journal) Snapshot(now time.Time) JournalSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.sessionID == "" {
		return unavailableSnapshot(JournalSnapshotInvalidData)
	}
	records, sizes, bad := snapshotEventsLocked(j.dir, j.sessionID)
	if bad != nil {
		return *bad
	}
	ackSeq, ackAt, bad := snapshotAckLocked(j.dir, uint64(len(records)))
	if bad != nil {
		return *bad
	}
	total, conflicts, quarantined, bad := snapshotQuarantineLocked(j.dir, j.sessionID, records)
	if bad != nil {
		return *bad
	}
	effective := ackSeq
	for effective < uint64(len(records)) && quarantined[effective+1] {
		effective++
	}
	effectiveAt := ackAt
	if effective != ackSeq || effective == 0 {
		effectiveAt = nil
	}
	var pendingCount, pendingBytes uint64
	if effective < uint64(len(records)) {
		pendingCount = uint64(len(records)) - effective
		for i := effective; i < uint64(len(records)); i++ {
			pendingBytes += sizes[i]
		}
	}
	var age *time.Duration
	if pendingCount > 0 {
		observed, err := time.Parse(time.RFC3339Nano, records[effective].ObservedAt)
		if err != nil {
			return unavailableSnapshot(JournalSnapshotInvalidData)
		}
		d := now.Sub(observed)
		if d < 0 {
			d = 0
		}
		out := d
		age = &out
	}
	snap := JournalSnapshot{Status: JournalSnapshotAvailable}
	snap.PendingRecords = uint64SnapshotValue(pendingCount)
	snap.PendingBytes = uint64SnapshotValue(pendingBytes)
	snap.OldestUnacknowledgedAge = age
	if effective > 0 {
		snap.AckedSequence = uint64SnapshotValue(effective)
	}
	snap.AckedAt = effectiveAt
	snap.QuarantineTotal = uint64SnapshotValue(total)
	snap.QuarantineConflicts = uint64SnapshotValue(conflicts)
	return snap
}

func snapshotEventsLocked(dir, sessionID string) ([]Record, []uint64, *JournalSnapshot) {
	f, err := snapshotOpenFile(dir, journalFileName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		failed := unavailableSnapshot(JournalSnapshotReadError)
		return nil, nil, &failed
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), MaxRecordBytes+4096)
	var records []Record
	var sizes []uint64
	var previous uint64
	for scanner.Scan() {
		raw := scanner.Bytes()
		size := uint64(len(raw) + 1)
		var record Record
		decoder := json.NewDecoder(bytesReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil || ValidateRecord(sessionID, record, previous) != nil {
			failed := unavailableSnapshot(JournalSnapshotInvalidData)
			return nil, nil, &failed
		}
		records = append(records, record)
		sizes = append(sizes, size)
		previous = record.StructuredSeq
	}
	if err := scanner.Err(); err != nil {
		failed := unavailableSnapshot(JournalSnapshotInvalidData)
		return nil, nil, &failed
	}
	return records, sizes, nil
}

func snapshotAckLocked(dir string, recordCount uint64) (uint64, *time.Time, *JournalSnapshot) {
	ackBytes, err := os.ReadFile(filepath.Join(dir, ackFileName)) //nolint:gosec // dir is the validated per-session journal directory.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil, nil
		}
		failed := unavailableSnapshot(JournalSnapshotReadError)
		return 0, nil, &failed
	}
	var ack ackState
	decoder := json.NewDecoder(bytesReader(ackBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ack); err != nil || ack.StructuredSeq > recordCount {
		failed := unavailableSnapshot(JournalSnapshotInvalidData)
		return 0, nil, &failed
	}
	if ack.AckedAt != nil && !ack.AckedAt.IsZero() {
		utc := ack.AckedAt.UTC()
		return ack.StructuredSeq, &utc, nil
	}
	return ack.StructuredSeq, nil, nil
}

func snapshotQuarantineLocked(dir, sessionID string, records []Record) (uint64, uint64, map[uint64]bool, *JournalSnapshot) {
	quarantined := make(map[uint64]bool)
	f, err := snapshotOpenFile(dir, quarantineFileName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, quarantined, nil
		}
		failed := unavailableSnapshot(JournalSnapshotReadError)
		return 0, 0, nil, &failed
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), MaxRecordBytes+4096)
	var total, conflicts uint64
	for scanner.Scan() {
		var entry quarantineRecord
		decoder := json.NewDecoder(bytesReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entry); err != nil {
			failed := unavailableSnapshot(JournalSnapshotInvalidData)
			return 0, 0, nil, &failed
		}
		if entry.Status != 400 && entry.Status != 404 && entry.Status != 409 && entry.Status != 413 || entry.Reason == "" || len(entry.Reason) > 256 || validateRecordShape(sessionID, entry.Record) != nil || entry.Record.StructuredSeq > uint64(len(records)) || records[entry.Record.StructuredSeq-1].EventID != entry.Record.EventID || quarantined[entry.Record.StructuredSeq] {
			failed := unavailableSnapshot(JournalSnapshotInvalidData)
			return 0, 0, nil, &failed
		}
		quarantined[entry.Record.StructuredSeq] = true
		total++
		if entry.Status == 409 {
			conflicts++
		}
	}
	if err := scanner.Err(); err != nil {
		failed := unavailableSnapshot(JournalSnapshotInvalidData)
		return 0, 0, nil, &failed
	}
	return total, conflicts, quarantined, nil
}

func snapshotOpenFile(dir, name string) (*os.File, error) {
	f, err := os.Open(filepath.Join(dir, name)) //nolint:gosec // dir is the validated per-session journal directory.
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || st.IsDir() {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("executionevent: not a file")
	}
	return f, nil
}
