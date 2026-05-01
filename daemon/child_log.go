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
// The writer holds an *slog.Logger reference (not a level + handler) so
// the test harness can swap the global default and still observe records
// through a captured handler.
type slogLineWriter struct {
	level  slog.Level
	stream string // "stdout" | "stderr" — folded into the prefix.
}

// newStdoutSlogWriter returns a slogLineWriter configured to log at INFO.
func newStdoutSlogWriter() *slogLineWriter {
	return &slogLineWriter{level: slog.LevelInfo, stream: "stdout"}
}

// newStderrSlogWriter returns a slogLineWriter configured to log at WARN.
func newStderrSlogWriter() *slogLineWriter {
	return &slogLineWriter{level: slog.LevelWarn, stream: "stderr"}
}

// WriteWorkerLine implements [PrefixedWriter]. It emits one slog record
// per call, tagging the message with the stream + sessionID so log
// readers can filter on either field.
//
// Empty lines are still emitted — operators benefit from preserving the
// child's exact output rhythm when debugging "why did it hang?" cases.
func (w *slogLineWriter) WriteWorkerLine(workerID, line string) {
	// Use slog.Default() at call time so tests that swap the default
	// handler observe records emitted from the spawn goroutine.
	logger := slog.Default()
	msg := "[child " + w.stream + " sessionID=" + workerID + "] " + line
	switch w.level {
	case slog.LevelInfo:
		logger.Info(msg, "sessionID", workerID, "stream", w.stream)
	case slog.LevelWarn:
		logger.Warn(msg, "sessionID", workerID, "stream", w.stream)
	default:
		logger.Log(context.Background(), w.level, msg, "sessionID", workerID, "stream", w.stream)
	}
}
