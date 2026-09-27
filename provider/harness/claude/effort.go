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

// withEffortEnv returns a copy of env with effortEnvVar fixed for the
// session: the configured effort when there is one, and effortModelDefault
// when there is none. It always sets the variable, so neither a level saved
// in the operator's settings nor a value inherited from the spawning
// environment can stand in for the session's configuration. A settings-file
// maxEffortLevel cap still applies on top, as Claude Code documents for
// every effort source.
func withEffortEnv(env map[string]string, effort agent.EffortLevel) map[string]string {
	out := make(map[string]string)
	for k, v := range env {
		out[k] = v
	}
	out[effortEnvVar] = effortEnvValue(effort)
	return out
}

// effortEnvValue is the effortEnvVar value for a configured effort.
func effortEnvValue(effort agent.EffortLevel) string {
	if effort == "" {
		return effortModelDefault
	}
	return string(effort)
}
