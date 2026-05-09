package daemon

import (
	"context"
	"log/slog"
)

// slogLineWriter is a [PrefixedWriter] that emits one slog record per
// child stdout/stderr line. The default daemon-level construction wires
// this up so spawn children's output is visible to operators by default
// (REN-1463 / v0.5.1). Callers that pass their own [PrefixedWriter] via
// [SpawnerOptions] override these defaults.
//
// The writer captures slog.Default() at construction time. Reading the
// global at every call (the prior shape) raced under -race with parallel
// tests in the same package that swap slog.Default() via captureSlog —
// the pump goroutines wrote into whichever handler was current when the
// race resolved, sometimes a sibling test's buffer, sometimes the
// already-restored production default. Snapshotting at construction
// keeps each writer pinned to the logger live when its owning daemon /
// test was set up.
type slogLineWriter struct {
	level  slog.Level
	stream string // "stdout" | "stderr" — folded into the prefix.
	logger *slog.Logger
}

// newStdoutSlogWriter returns a slogLineWriter configured to log at INFO.
func newStdoutSlogWriter() *slogLineWriter {
	return &slogLineWriter{level: slog.LevelInfo, stream: "stdout", logger: slog.Default()}
}

// newStderrSlogWriter returns a slogLineWriter configured to log at WARN.
func newStderrSlogWriter() *slogLineWriter {
	return &slogLineWriter{level: slog.LevelWarn, stream: "stderr", logger: slog.Default()}
}

// WriteWorkerLine implements [PrefixedWriter]. It emits one slog record
// per call, tagging the message with the stream + sessionID so log
// readers can filter on either field.
//
// Empty lines are still emitted — operators benefit from preserving the
// child's exact output rhythm when debugging "why did it hang?" cases.
func (w *slogLineWriter) WriteWorkerLine(workerID, line string) {
	msg := "[child " + w.stream + " sessionID=" + workerID + "] " + line
	switch w.level {
	case slog.LevelInfo:
		w.logger.Info(msg, "sessionID", workerID, "stream", w.stream)
	case slog.LevelWarn:
		w.logger.Warn(msg, "sessionID", workerID, "stream", w.stream)
	default:
		w.logger.Log(context.Background(), w.level, msg, "sessionID", workerID, "stream", w.stream)
	}
}
