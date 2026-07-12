//go:build race

package ptyhost

// raceEnabled is true when the test binary was built with -race. The firehose
// backpressure test (firehose_test.go) uses it to size the producer volume
// and the throughput floor for -race's real instrumentation overhead — the
// gate this lane runs under is `go test ... -race`, and the race detector's
// per-mutex/per-channel-op bookkeeping measurably slows the full ptyhost
// pipeline (PTY read -> VT feed -> framing -> ring/fan-out -> channel),
// throttling the producer via ordinary PTY buffer backpressure.
const raceEnabled = true
