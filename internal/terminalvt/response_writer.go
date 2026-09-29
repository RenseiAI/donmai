package vt

import "io"

// SetResponseWriter routes engine-generated terminal output to a receiver-owned
// writer. Nil retains the default Read/InputPipe connection. Read-only mirrors
// should install io.Discard before restore or feeding input. This configuration
// is never exported or replaced by checkpoint restoration. Callers serialize
// this operation with all other emulator mutation.
func (e *Emulator) SetResponseWriter(w io.Writer) { e.responseWriter = w }

func (e *Emulator) responseOutput() io.Writer {
	if e.responseWriter != nil {
		return e.responseWriter
	}
	return e.pw
}
