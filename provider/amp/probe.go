package amp

import "os"

// defaultGetenv wraps os.Getenv so tests can inject a fake without
// touching the global process environment.
func defaultGetenv(key string) string { return os.Getenv(key) }
