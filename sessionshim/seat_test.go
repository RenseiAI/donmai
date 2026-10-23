package sessionshim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func testSeatLaunch(id Identity, epoch uint64) SeatLaunch {
	return NewSeatLaunch(id, epoch, "donmai-seat-0123456789abcdef0123456789abcdef-1.scope",
		SeatModeEnforced, 2, 512, 100, time.Unix(1_700_000_000, 0))
}

// TestSeatLaunchRoundTripsBesideTheRecord pins the launch record's storage: it
// persists through Put/Get unchanged at the registry's owner-only mode, under
// a name that is not a discovery record, so Scan — which an older daemon runs
// with a strict decoder — never enumerates it.
func TestSeatLaunchRoundTripsBesideTheRecord(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	if err := reg.Put(testRecord(t, id, reg)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	want := testSeatLaunch(id, 1)
	if err := reg.PutSeatLaunch(want); err != nil {
		t.Fatalf("PutSeatLaunch: %v", err)
	}
	got, found, err := reg.SeatLaunch(id, 1)
	if err != nil || !found || got != want {
		t.Fatalf("SeatLaunch = %+v, %v, %v; want %+v", got, found, err, want)
	}
	if _, found, err := reg.SeatLaunch(id, 2); err != nil || found {
		t.Fatalf("SeatLaunch at another epoch = found %v, err %v; want a separate, absent record", found, err)
	}
	entries, err := reg.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 1 || entries[0].Err != nil {
		t.Fatalf("Scan = %+v; want only the one discovery record (the launch record is not one)", entries)
	}
	info, err := os.Stat(filepath.Join(reg.Dir(), seatLaunchName(id, 1)))
	if err != nil {
		t.Fatalf("stat launch record: %v", err)
	}
	if perm := info.Mode().Perm(); perm != RecordFileMode {
		t.Errorf("launch record mode %#o, want %#o", perm, RecordFileMode)
	}
	if strings.HasSuffix(seatLaunchName(id, 1), recordSuffix) {
		t.Errorf("launch record name %q ends in the record suffix; Scan would read it as a discovery record", seatLaunchName(id, 1))
	}
}

// TestSeatLaunchRefusesMalformed pins the bounds: every malformed launch
// record fails closed on write and on read, so a corrupted file can never
// report limits, a posture or a scope the launch did not give the seat.
func TestSeatLaunchRefusesMalformed(t *testing.T) {
	t.Parallel()

	id := testIdentity()
	for name, mutate := range map[string]func(*SeatLaunch){
		"schema version":         func(s *SeatLaunch) { s.SchemaVersion = 2 },
		"identity":               func(s *SeatLaunch) { s.SessionID = "" },
		"unknown mode":           func(s *SeatLaunch) { s.Mode = "auto" },
		"negative cpus":          func(s *SeatLaunch) { s.CPUs = -1 },
		"negative memory":        func(s *SeatLaunch) { s.MemoryMB = -1 },
		"io weight too high":     func(s *SeatLaunch) { s.IOWeight = 10001 },
		"absurd cpus":            func(s *SeatLaunch) { s.CPUs = maxSeatCPUs + 1 },
		"scope too long":         func(s *SeatLaunch) { s.Scope = strings.Repeat("a", 250) + ".scope" },
		"scope alphabet":         func(s *SeatLaunch) { s.Scope = "donmai seat; reboot.scope" },
		"scope is not a scope":   func(s *SeatLaunch) { s.Scope = "sshd.service" },
		"bare suffix":            func(s *SeatLaunch) { s.Scope = ".scope" },
		"enforced with no scope": func(s *SeatLaunch) { s.Scope = "" },
		"missing createdAt":      func(s *SeatLaunch) { s.CreatedAtUnixNano = 0 },
	} {
		s := testSeatLaunch(id, 1)
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate = nil, want a refusal", name)
		}
		if err := newTestRegistry(t).PutSeatLaunch(s); err == nil {
			t.Errorf("%s: PutSeatLaunch = nil, want a refusal", name)
		}
	}
	// A best-effort or budgetless seat off Linux names no scope, and that is
	// well-formed.
	for _, mode := range []string{SeatModeBestEffort, SeatModeNone} {
		s := testSeatLaunch(id, 1)
		s.Mode, s.Scope = mode, ""
		if err := s.Validate(); err != nil {
			t.Errorf("unscoped %s seat: Validate = %v, want nil", mode, err)
		}
	}

	raw, err := json.Marshal(testSeatLaunch(id, 1))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for name, data := range map[string][]byte{
		"unknown field": []byte(strings.Replace(string(raw), `"mode"`, `"bearer":"x","mode"`, 1)),
		"trailing data": append(append([]byte(nil), raw...), []byte(` {}`)...),
		"oversize":      []byte(`{"pad":"` + strings.Repeat("x", maxSeatLaunchBytes) + `"}`),
	} {
		if _, err := decodeSeatLaunch(data); err == nil {
			t.Errorf("%s: decodeSeatLaunch = nil, want a refusal", name)
		}
	}
	reg := newTestRegistry(t)
	if err := reg.publish(seatLaunchName(id, 1), []byte(strings.Replace(string(raw), `"mode"`, `"secret":"x","mode"`, 1))); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, found, err := reg.SeatLaunch(id, 1); err == nil || found {
		t.Fatalf("SeatLaunch over a corrupted file = found %v, err %v; want an error, never empty facts", found, err)
	}
	other := testSeatLaunch(Identity{OrgID: "org-b", SessionID: "sess-b"}, 1)
	otherRaw, err := other.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := reg.publish(seatLaunchName(id, 2), otherRaw); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, found, err := reg.SeatLaunch(id, 2); err == nil || found {
		t.Fatalf("SeatLaunch over another launch's bytes = found %v, err %v; want a mismatch refusal", found, err)
	}
}

// TestSeatLaunchIsDisposedWithTheTerminalProof pins the record's lifetime: it
// survives the shim's own tombstone write (the seat still has to be reported
// until the daemon disposes the proof), and goes with the audited disposal of
// the tombstone or of the absence sidecar — and only for that launch.
func TestSeatLaunchIsDisposedWithTheTerminalProof(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	if err := reg.Put(testRecord(t, id, reg)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, epoch := range []uint64{1, 2} {
		if err := reg.PutSeatLaunch(testSeatLaunch(id, epoch)); err != nil {
			t.Fatalf("PutSeatLaunch(%d): %v", epoch, err)
		}
	}
	tomb := Tombstone{
		SchemaVersion: RecordSchemaVersion, OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-1", ProcessEpoch: 1, GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := reg.PutTombstone(tomb); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}
	if _, found, err := reg.SeatLaunch(id, 1); err != nil || !found {
		t.Fatalf("launch record after the shim's tombstone = found %v, err %v; want it kept until disposal", found, err)
	}
	if err := reg.RemoveTombstoneIncarnation(tomb); err != nil {
		t.Fatalf("RemoveTombstoneIncarnation: %v", err)
	}
	if _, found, err := reg.SeatLaunch(id, 1); err != nil || found {
		t.Fatalf("launch record after tombstone disposal = found %v, err %v; want it gone", found, err)
	}
	if _, found, err := reg.SeatLaunch(id, 2); err != nil || !found {
		t.Fatalf("sibling epoch's launch record = found %v, err %v; want it untouched", found, err)
	}
	if err := reg.DisposeWithdrawnAbsence(id, "shim-2", 2); err != nil {
		t.Fatalf("DisposeWithdrawnAbsence: %v", err)
	}
	if _, found, err := reg.SeatLaunch(id, 2); err != nil || found {
		t.Fatalf("launch record after absence disposal = found %v, err %v; want it gone", found, err)
	}
	if err := reg.RemoveSeatLaunch(id, 2); err != nil {
		t.Fatalf("RemoveSeatLaunch of a gone record = %v; want idempotent", err)
	}
}

// TestInteractiveRecordKeepsTheReleasedSchema pins the discovery record's JSON
// member set. Released daemons decode it with DisallowUnknownFields, so a new
// member — a seat fact, say — would make every older daemon quarantine every
// interactive shim launched after it. New per-launch state goes in a sidecar
// beside the record instead (see seat.go, flowcontrol.go, ack.go). The members
// added since are `workload`, which interactive records omit (absence means
// the PTY profile), and the Linux drift-free identity (`processStartTicks`,
// `bootId`), which records without the key omit so their bytes stay identical
// to what released readers decode; both omissions are pinned below. A Linux
// record with the key changes bytes and older daemons quarantine it fail
// closed (kill nothing) — the documented one-way break for the start-time
// conversion, which upgrades controller-first.
func TestInteractiveRecordKeepsTheReleasedSchema(t *testing.T) {
	t.Parallel()

	released := []string{
		"bootId", "createdAt", "orgId", "orphanDeadlineAt", "phase", "pid", "processEpoch",
		"processStartTicks", "processStartedAt", "protocolMax", "protocolMin", "resumeKey", "schemaVersion",
		"sessionId", "shimId", "socketDevice", "socketInode", "socketPath",
		"workareaPath", "workarea_root", "workload",
	}
	var members []string
	recordType := reflect.TypeOf(Record{})
	for i := 0; i < recordType.NumField(); i++ {
		tag := recordType.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			members = append(members, name)
		}
	}
	sort.Strings(members)
	if !reflect.DeepEqual(members, released) {
		t.Fatalf("Record JSON members = %v\nwant the released set %v\na new member makes older daemons quarantine every interactive shim; put new state in a sidecar", members, released)
	}
	reg := newTestRegistry(t)
	raw, err := json.Marshal(testRecord(t, testIdentity(), reg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"workload"`) {
		t.Fatalf("interactive record encodes a workload member: %s; released readers refuse it", raw)
	}
	if strings.Contains(string(raw), `"processStartTicks"`) || strings.Contains(string(raw), `"bootId"`) {
		t.Fatalf("interactive record without the drift-free key encodes it anyway: %s; released readers refuse unknown members", raw)
	}
}

// TestSeatEpochFloorSurvivesDisposal pins the monotonic launch number: the
// floor records the highest epoch launched, and raising it survives disposal
// of the launch's seat record — so a relaunch after terminal cleanup names a
// fresh scope instead of reusing one a stray process may still hold.
// Reverting nextShimProcessEpoch to records-and-tombstones-only (or dropping
// the floor write at launch) lets the next launch fall back to 1.
func TestSeatEpochFloorSurvivesDisposal(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := testIdentity()
	if _, found, err := reg.SeatEpochFloor(id); err != nil || found {
		t.Fatalf("SeatEpochFloor on a new identity = found %v, err %v; want absent", found, err)
	}
	if err := reg.RaiseSeatEpochFloor(id, 1); err != nil {
		t.Fatalf("RaiseSeatEpochFloor: %v", err)
	}
	if floor, found, err := reg.SeatEpochFloor(id); err != nil || !found || floor != 1 {
		t.Fatalf("SeatEpochFloor = %d, %v, %v; want 1, true, nil", floor, found, err)
	}
	// Monotonic: raising below the floor is a no-op, never a regression.
	if err := reg.RaiseSeatEpochFloor(id, 1); err != nil {
		t.Fatalf("RaiseSeatEpochFloor idempotent: %v", err)
	}
	if err := reg.RaiseSeatEpochFloor(id, 3); err != nil {
		t.Fatalf("RaiseSeatEpochFloor: %v", err)
	}
	if floor, _, _ := reg.SeatEpochFloor(id); floor != 3 {
		t.Fatalf("SeatEpochFloor = %d; want 3", floor)
	}
	// Disposal of the launch's seat record and tombstone leaves the floor:
	// the sidecar is per identity, not per incarnation.
	if err := reg.PutSeatLaunch(testSeatLaunch(id, 3)); err != nil {
		t.Fatalf("PutSeatLaunch: %v", err)
	}
	if err := reg.RemoveSeatLaunch(id, 3); err != nil {
		t.Fatalf("RemoveSeatLaunch: %v", err)
	}
	if floor, found, err := reg.SeatEpochFloor(id); err != nil || !found || floor != 3 {
		t.Fatalf("SeatEpochFloor after disposal = %d, %v, %v; want 3, true, nil", floor, found, err)
	}
	// The floor file is not a discovery record: Scan never enumerates it.
	entries, err := reg.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Scan = %d entries; want none (the floor is not a record)", len(entries))
	}
	if strings.HasSuffix(seatEpochFloorName(id), recordSuffix) {
		t.Errorf("floor name %q ends in the record suffix; Scan would read it as a discovery record", seatEpochFloorName(id))
	}
	// A sibling identity is unaffected.
	sibling := Identity{OrgID: id.OrgID, SessionID: "sess-other"}
	if _, found, _ := reg.SeatEpochFloor(sibling); found {
		t.Error("sibling identity has a floor; want none")
	}
}

// TestSeatEpochFloorRefusesMalformed pins the failure mode: a floor file
// that carries a scope, posture or limit — or names another identity —
// fails closed rather than reading as a floor a relaunch would trust.
func TestSeatEpochFloorRefusesMalformed(t *testing.T) {
	t.Parallel()

	id := testIdentity()
	reg := newTestRegistry(t)
	if err := reg.RaiseSeatEpochFloor(Identity{}, 1); err == nil {
		t.Error("RaiseSeatEpochFloor with an invalid identity succeeded; want a refusal")
	}
	// A floor file carrying a scope is not a floor.
	scoped := testSeatLaunch(id, 2)
	if err := reg.publish(seatEpochFloorName(id), mustEncodeSeatLaunch(t, scoped)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, _, err := reg.SeatEpochFloor(id); err == nil {
		t.Error("SeatEpochFloor over a scoped file succeeded; want a refusal")
	}
}

func mustEncodeSeatLaunch(t *testing.T, s SeatLaunch) []byte {
	t.Helper()
	raw, err := s.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}
