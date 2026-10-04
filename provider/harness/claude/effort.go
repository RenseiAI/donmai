package claude

import "github.com/RenseiAI/donmai/agent"

// effortEnvVar is the Claude Code environment variable that fixes a
// session's reasoning effort. Claude Code resolves effort as: this variable
// first, then --effort (or /effort), then the effortLevel / modelSettings
// keys of its settings files, then the model's own default. The variable
// therefore outranks the operator's ~/.claude/settings.json on a host seat,
// which --effort alone does not when no effort is configured: with no flag,
// the operator's saved level would apply.
const effortEnvVar = "CLAUDE_CODE_EFFORT_LEVEL"

// effortModelDefault is the effortEnvVar value that selects the model's own
// default effort, ignoring any level saved in a settings file.
const effortModelDefault = "auto"

// forceEffortEnv reports whether Spawn fixes the effort environment
// variable for this session. Only stamped headless sessions qualify: the
// stamp alone is not enough, because interactive sessions are stamped too,
// and interactive operators keep their own effort control (the --effort
// flag when a level is known, plus the in-session effort command and saved
// preferences). Unstamped standalone sessions inherit the environment
// untouched.
func forceEffortEnv(spec agent.Spec) bool {
	return spec.ExecutionSecurity != nil && spec.Interactive == nil
}

// withEffortEnv returns a copy of env with effortEnvVar fixed for the
// session: the configured effort when it is a level Claude recognises, and
// effortModelDefault when there is none or it is not a recognised level. It
// always sets the variable, so neither a level saved in the operator's
// settings nor a value inherited from the spawning environment can stand in
// for the session's configuration. A settings-file maxEffortLevel cap still
// applies on top, as Claude Code documents for every effort source.
//
// Callers gate on forceEffortEnv first: only stamped headless sessions fix
// the variable. Every other spawn mode leaves the operator's own effort
// control (flag, in-session command, saved preferences, inherited
// environment) in place.
func withEffortEnv(env map[string]string, effort agent.EffortLevel) map[string]string {
	out := make(map[string]string)
	for k, v := range env {
		out[k] = v
	}
	out[effortEnvVar] = effortEnvValue(effort)
	return out
}

// effortEnvValue is the effortEnvVar value for a configured effort. A
// non-empty value Claude does not recognise (e.g. another harness's naming
// such as "none" or "minimal") is treated the same as none configured:
// Claude silently ignores an unrecognised value and falls back to the
// operator's own saved effort, so passing it through would let the recorded
// and effective efforts silently diverge.
func effortEnvValue(effort agent.EffortLevel) string {
	if !effort.Known() {
		return effortModelDefault
	}
	return string(effort)
}
