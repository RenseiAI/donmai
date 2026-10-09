package daemon

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/installer/servicepriority"
)

// Linux scheduling policy numbers (sched.h) as they appear in /proc/<pid>/stat.
const (
	schedOther    = 0
	schedFIFO     = 1
	schedRR       = 2
	schedBatch    = 3
	schedIdle     = 5
	schedDeadline = 6
)

// procStatNiceField and procStatPolicyField are the 1-based field numbers of
// nice and policy in /proc/<pid>/stat (proc(5)).
const (
	procStatNiceField   = 19
	procStatPolicyField = 41
)

// observeProcStatPriority classifies the process from its /proc stat line. The
// systemd background mode sets CPUSchedulingPolicy=idle (SCHED_IDLE), which is
// what this keys on; every other policy is the default mode.
func observeProcStatPriority(read func() ([]byte, error)) processPriorityObservation {
	data, err := read()
	if err != nil {
		return processPriorityObservation{warning: fmt.Sprintf("could not read the live scheduling policy: %v", err)}
	}
	nice, policy, err := parseProcStat(string(data))
	if err != nil {
		return processPriorityObservation{warning: fmt.Sprintf("could not read the live scheduling policy: %v", err)}
	}
	mode := servicepriority.Default
	if policy == schedIdle {
		mode = servicepriority.Background
	}
	return processPriorityObservation{
		mode:     mode,
		evidence: fmt.Sprintf("%s, nice %d", schedPolicyName(policy), nice),
	}
}

// parseProcStat extracts nice and the scheduling policy from a
// /proc/<pid>/stat line. The command name (field 2) is parenthesised and may
// itself contain spaces and parentheses, so fields are counted from the last
// closing parenthesis.
func parseProcStat(stat string) (nice, policy int, err error) {
	end := strings.LastIndex(stat, ")")
	if end < 0 {
		return 0, 0, errors.New("malformed stat line: no command field")
	}
	// After the command, fields start at 3 (state).
	fields := strings.Fields(stat[end+1:])
	const firstField = 3
	if len(fields) < procStatPolicyField-firstField+1 {
		return 0, 0, fmt.Errorf("malformed stat line: %d fields after the command, want at least %d", len(fields), procStatPolicyField-firstField+1)
	}
	nice, err = strconv.Atoi(fields[procStatNiceField-firstField])
	if err != nil {
		return 0, 0, fmt.Errorf("parse nice: %w", err)
	}
	policy, err = strconv.Atoi(fields[procStatPolicyField-firstField])
	if err != nil {
		return 0, 0, fmt.Errorf("parse scheduling policy: %w", err)
	}
	return nice, policy, nil
}

func schedPolicyName(policy int) string {
	switch policy {
	case schedOther:
		return "SCHED_OTHER"
	case schedFIFO:
		return "SCHED_FIFO"
	case schedRR:
		return "SCHED_RR"
	case schedBatch:
		return "SCHED_BATCH"
	case schedIdle:
		return "SCHED_IDLE"
	case schedDeadline:
		return "SCHED_DEADLINE"
	default:
		return "policy " + strconv.Itoa(policy)
	}
}
