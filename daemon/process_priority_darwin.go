package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/installer/launchd"
)

var launchctlPIDRegexp = regexp.MustCompile(`"PID"\s*=\s*(\d+)`)

type processPriorityProbe struct {
	pid           int
	run           func(context.Context, string, ...string) ([]byte, error)
	readFile      func(string) ([]byte, error)
	plistForLabel func(string) (string, error)
}

func daemonProcessPriorityStatus() afclient.DaemonProcessPriorityStatus {
	probe := processPriorityProbe{
		pid:           os.Getpid(),
		run:           runProcessPriorityCommand,
		readFile:      os.ReadFile,
		plistForLabel: launchd.PlistPathForLabel,
	}
	return probe.inspect()
}

func runProcessPriorityCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // fixed system tools with local arguments only
}

func (p processPriorityProbe) inspect() afclient.DaemonProcessPriorityStatus {
	status := afclient.DaemonProcessPriorityStatus{Supported: true}
	pri, err := p.psPriority()
	if err != nil {
		status.Mode = string(launchd.ProcessPriorityDefault)
		status.Warning = "could not read live process priority: " + err.Error()
		status.Evidence = "macOS self-check fell back before classification completed"
		return status
	}
	status.ProcessPriority = pri

	launchdMode, serviceManaged, err := p.launchdMode()
	if err != nil {
		status.Warning = "could not read launchd service definition: " + err.Error()
	}
	if serviceManaged {
		status.LaunchdMode = string(launchdMode)
	}

	switch {
	case pri == 4 && (!serviceManaged || launchdMode == launchd.ProcessPriorityBackground):
		status.Mode = string(launchd.ProcessPriorityBackground)
		status.QoSClass = string(launchd.ProcessPriorityBackground)
		status.IOPolicy = string(launchd.ProcessPriorityBackground)
		if serviceManaged {
			status.Evidence = "active launchd service wraps the daemon with taskpolicy -b and the live process reports ps PRI=4"
		} else {
			status.Evidence = "the live process reports ps PRI=4, which is the Darwin background priority"
		}
		return status
	case serviceManaged && launchdMode == launchd.ProcessPriorityUtility:
		status.Mode = string(launchd.ProcessPriorityUtility)
		status.QoSClass = string(launchd.ProcessPriorityUtility)
		status.IOPolicy = "throttled"
		status.Evidence = "active launchd service wraps the daemon with taskpolicy -c utility; utility-clamped processes keep ps PRI=20, so the live service definition is the runtime proof"
		if pri != 20 {
			status.Warning = fmt.Sprintf("utility service definition is active but ps PRI=%d (utility-clamped processes normally report 20)", pri)
		}
		return status
	case serviceManaged && launchdMode == launchd.ProcessPriorityDefault:
		status.Mode = string(launchd.ProcessPriorityDefault)
		status.QoSClass = string(launchd.ProcessPriorityDefault)
		status.IOPolicy = string(launchd.ProcessPriorityDefault)
		status.Evidence = fmt.Sprintf("active launchd service has no taskpolicy wrapper and the live process reports ps PRI=%d", pri)
		if pri != 20 {
			status.Warning = fmt.Sprintf("default launchd service definition is active but ps PRI=%d (default daemon processes normally report 20)", pri)
		}
		return status
	case serviceManaged && launchdMode == launchd.ProcessPriorityBackground && pri != 4:
		status.Mode = "unknown"
		status.QoSClass = string(launchd.ProcessPriorityBackground)
		status.IOPolicy = string(launchd.ProcessPriorityBackground)
		status.Evidence = "active launchd service wraps the daemon with taskpolicy -b"
		status.Warning = fmt.Sprintf("background service definition is active but ps PRI=%d (background processes should report 4)", pri)
		return status
	default:
		status.Mode = string(launchd.ProcessPriorityDefault)
		status.QoSClass = string(launchd.ProcessPriorityDefault)
		status.IOPolicy = string(launchd.ProcessPriorityDefault)
		status.Evidence = fmt.Sprintf("launchd service is not the current daemon process; the live process reports ps PRI=%d", pri)
		return status
	}
}

func (p processPriorityProbe) psPriority() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := p.run(ctx, "ps", "-o", "pri=", "-p", strconv.Itoa(p.pid))
	if err != nil {
		return 0, fmt.Errorf("ps: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return 0, fmt.Errorf("ps returned an empty PRI value")
	}
	pri, err := strconv.Atoi(strings.Fields(value)[0])
	if err != nil {
		return 0, fmt.Errorf("parse ps PRI %q: %w", value, err)
	}
	return pri, nil
}

func (p processPriorityProbe) launchdMode() (launchd.ProcessPriority, bool, error) {
	for _, label := range []string{launchd.LaunchdLabel, launchd.RenseiDaemonLabel} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, err := p.run(ctx, "launchctl", "list", label)
		cancel()
		if err != nil {
			continue
		}
		match := launchctlPIDRegexp.FindStringSubmatch(string(out))
		if len(match) != 2 {
			continue
		}
		servicePID, err := strconv.Atoi(match[1])
		if err != nil {
			return launchd.ProcessPriorityDefault, false, fmt.Errorf("parse launchctl PID %q: %w", match[1], err)
		}
		if servicePID != p.pid {
			continue
		}
		plistPath, err := p.plistForLabel(label)
		if err != nil {
			return launchd.ProcessPriorityDefault, true, fmt.Errorf("plist path: %w", err)
		}
		plist, err := p.readFile(plistPath)
		if err != nil {
			return launchd.ProcessPriorityDefault, true, fmt.Errorf("read plist: %w", err)
		}
		return launchd.DetectProcessPriorityFromPlist(string(plist)), true, nil
	}
	return launchd.ProcessPriorityDefault, false, nil
}
