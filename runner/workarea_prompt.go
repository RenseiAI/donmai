package runner

const workareaProtocolPartial = "# Repository workarea\nThe current directory is the selected repository inside a session-owned multi-repository workarea. Read ../.workarea/declaration.json for declared names, roles, authorities, and resolved refs. Modify only repositories whose authority is mutable. If .agent/turn-result.json uses its optional repositories member, include every mutable repository and no read-only repository."

func injectWorkareaProtocolPartial(existing string, enabled bool) string {
	if !enabled {
		return existing
	}
	if existing == "" {
		return workareaProtocolPartial
	}
	return existing + "\n\n" + workareaProtocolPartial
}
