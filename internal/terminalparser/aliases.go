// Package terminalparser owns the bounded streaming parser used by the terminal mirror.
package terminalparser

import "github.com/charmbracelet/x/ansi"

// Handler retains the upstream callback contract.
type (
	Handler = ansi.Handler
	// Params retains the upstream parameter-list identity.
	Params = ansi.Params
	// Param retains the upstream packed-parameter identity.
	Param = ansi.Param
	// Cmd retains the upstream command identity.
	Cmd = ansi.Cmd
)

// ESC is the ASCII escape control byte.
const ESC = ansi.ESC

// SetHandler installs receiver-owned callbacks.
func (p *Parser) SetHandler(h Handler) { p.handler = h }
