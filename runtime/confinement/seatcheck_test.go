package confinement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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

// seatOrphanEnv turns the test binary into the seat check's orphan: a
// descendant in a session of its own that dials the address in its value,
// holds the connection, and lives at most a minute ("sleep" only sleeps). A
// boundary that dies with its launcher closes that connection.
const seatOrphanEnv = "DONMAI_CONFINEMENT_TEST_SEAT_ORPHAN"

// seatCheckStep is one check: "dial" a TCP address, "get" a URL, "read" or
// "write" a file; "signal", "ptrace" or "proc_read" a pid outside;
// "dial_abstract" an abstract socket; "whoami" reports the seat's pid and
// parent pid; "foreground" checks the seat owns its terminal; "await_sigint"
// creates its target as a ready marker and waits for a Ctrl-C; "child_tree"
// starts, signals and reaps a child of its own; "orphan" starts the orphan
// (see seatOrphanEnv) dialing its target, then blocks until killed.
type seatCheckStep struct {
	ID     string `json:"id"`
	Op     string `json:"op"`
	Target string `json:"target"`
}

type seatCheckResult struct {
	ID     string `json:"id"`
	Err    string `json:"err,omitempty"`
	Status int    `json:"status,omitempty"`
	Output string `json:"output,omitempty"`
}

func runSeatCheckFromEnv() (bool, int) {
	if addr := os.Getenv(seatOrphanEnv); addr != "" {
		if addr == "sleep" {
			time.Sleep(time.Minute)
			return true, 0
		}
		// The address is the test's own loopback listener.
		if conn, err := net.DialTimeout("tcp", addr, 10*time.Second); err == nil { //nolint:gosec // G704: the test's own loopback listener address.
			time.Sleep(time.Minute)
			_ = conn.Close()
		}
		return true, 0
	}
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
	case "signal", "ptrace", "proc_read":
		var pid int
		if pid, err = strconv.Atoi(step.Target); err == nil {
			switch step.Op {
			case "signal":
				err = unix.Kill(pid, unix.SIGUSR1)
			case "ptrace":
				err = ptraceSeize(pid)
			default:
				_, err = os.ReadFile(filepath.Join("/proc", step.Target, "environ")) //nolint:gosec // G304: a fixed procfs file.
			}
		}
	case "dial_abstract":
		var conn net.Conn
		if conn, err = net.DialTimeout("unix", "@"+step.Target, 5*time.Second); err == nil {
			err = conn.Close()
		}
	case "whoami":
		result.Output = fmt.Sprintf("pid=%d ppid=%d", os.Getpid(), os.Getppid())
	case "foreground":
		var foreground int
		if foreground, err = unix.IoctlGetInt(0, unix.TIOCGPGRP); err == nil {
			result.Output = fmt.Sprintf("foreground=%d group=%d", foreground, unix.Getpgrp())
			if foreground != unix.Getpgrp() {
				err = errors.New("the seat is not its terminal's foreground group")
			} else {
				var tty *os.File
				if tty, err = os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
					err = tty.Close()
				}
			}
		}
	case "await_sigint":
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		if err = appendOrCreate(step.Target); err == nil {
			select {
			case <-interrupts:
				result.Output = "interrupted"
			case <-time.After(30 * time.Second):
				err = errors.New("no Ctrl-C arrived")
			}
		}
		signal.Stop(interrupts)
	case "child_tree":
		err = runChildTree()
	case "orphan":
		err = startOrphan(step.Target)
		if err == nil {
			time.Sleep(time.Minute)
		}
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

// runChildTree starts a child of the seat's own, signals it and reaps it:
// the seat's process tree is its own to manage.
func runChildTree() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "-test.run=^$") //nolint:gosec // G204: the test binary re-executed.
	cmd.Env = append(os.Environ(), seatOrphanEnv+"=sleep")
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	var exit *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exit) {
		return fmt.Errorf("the child was not reaped as killed: %v", err)
	}
	return nil
}

// startOrphan starts the orphan in a session of its own, dialing addr.
func startOrphan(addr string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "-test.run=^$") //nolint:gosec // G204: the test binary re-executed.
	cmd.Env = append(os.Environ(), seatOrphanEnv+"="+addr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
