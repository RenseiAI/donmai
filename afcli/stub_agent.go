package afcli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/RenseiAI/donmai/provider/harness/stub/stubagent"
	"github.com/spf13/cobra"
)

// StubAgentExit carries a scripted exit status out of the hidden stub-agent
// command. It exists because "the scenario asked for status 1" and "the
// command failed" are different facts that a bare error cannot tell apart —
// and the stub harness exists precisely so a caller can assert on the
// difference.
type StubAgentExit struct {
	Code int
	Err  error
}

func (e *StubAgentExit) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("stub agent exited with status %d", e.Code)
}

func (e *StubAgentExit) Unwrap() error { return e.Err }

// StubAgentExitCode reports the process exit status an error asks for, and
// whether the error was a scripted stub-agent exit at all. A binary's main
// uses it so a scenario's exit code reaches the parent as an exit status
// instead of being flattened to 1.
func StubAgentExitCode(err error) (int, bool) {
	var exit *StubAgentExit
	if errors.As(err, &exit) {
		return exit.Code, true
	}
	return 0, false
}

// newStubAgentCmd constructs the hidden `stub-agent` command: the child half
// of the stub harness's interactive spawn mode
// (provider/harness/stub/interactive.go).
//
// It is hidden because no human runs it on purpose — it is what the harness
// re-invokes under a PTY. It ships INSIDE this binary rather than as a
// separate artifact so an integration environment cannot end up running a
// fake agent from one build against a daemon from another. It takes no flags:
// the scenario arrives through the environment, the one channel every spawn
// path in this repo already forwards end to end.
func newStubAgentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   stubagent.CommandName,
		Short: "Run the deterministic fake agent (test harness; configured through the environment)",
		Long: "Run the deterministic fake agent used by the stub harness's interactive spawn mode.\n\n" +
			"The scenario is read from " + stubagent.EnvScenario + " (inline JSON) or " +
			stubagent.EnvScenarioFile + " (a path to a JSON file). With neither set it runs the\n" +
			"default scenario: announce, idle briefly, exit 0.\n\n" +
			"When " + stubagent.EnvToolPolicy + " is set, the tool-permission policy it carries is\n" +
			"recorded as the first line of the transcript before the scenario runs.",
		Hidden: true,
		Args:   cobra.NoArgs,
		// A scripted non-zero exit is a correct outcome, not operator error:
		// printing cobra's usage block or its own "Error:" line at it would
		// bury the scenario's own transcript in noise.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return runStubAgent(cmd) },
	}
}

func runStubAgent(cmd *cobra.Command) error {
	scenario, err := stubagent.Load(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "stub agent:", err)
		return &StubAgentExit{Code: stubagent.ExitScenarioFailure, Err: err}
	}

	// Record the received tool policy FIRST, before a single scripted line, so
	// the transcript proves what this session was handed even when the
	// scenario's own first act is to exit. The stub harness declares the
	// native tool-policy channel satisfied by construction (it registers no
	// tools); this line is the evidence behind that claim, and its absence
	// means no policy arrived.
	//
	// A malformed value is refused rather than skipped: the variable exists to
	// make the claim auditable, so a child that discarded the record and ran
	// anyway would be asserting exactly the thing it had just lost.
	policy, err := stubagent.LoadToolPolicy(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "stub agent:", err)
		return &StubAgentExit{Code: stubagent.ExitScenarioFailure, Err: err}
	}
	if os.Getenv(stubagent.EnvToolPolicy) != "" {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), policy.Notice()); err != nil {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "stub agent: write tool policy notice:", err)
			return &StubAgentExit{Code: stubagent.ExitScenarioFailure, Err: err}
		}
	}

	// signal.Notify rather than the default disposition: SIGTERM must become
	// an OBSERVABLE event even when the scenario's answer is to carry on,
	// because "ignored the stop" and "never received the stop" are exactly the
	// two outcomes a cooperative-stop assertion has to tell apart.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	code, runErr := stubagent.Run(cmd.Context(), scenario, stubagent.Options{
		Stdout: cmd.OutOrStdout(),
		Stdin:  cmd.InOrStdin(),
		Stop:   signals,
	})
	if runErr != nil {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "stub agent:", runErr)
		return &StubAgentExit{Code: code, Err: runErr}
	}
	if code != 0 {
		return &StubAgentExit{Code: code}
	}
	return nil
}
