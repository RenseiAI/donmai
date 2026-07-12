//go:build !race

package ptyhost

// raceEnabled is false in a plain (non -race) test build. See
// racedetect_on_test.go for why the firehose backpressure test needs to know.
const raceEnabled = false
