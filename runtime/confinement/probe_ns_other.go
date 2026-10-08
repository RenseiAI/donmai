//go:build !linux

package confinement

import "errors"

var errLinuxProbe = errors.New("a Linux probe operation")

func allowAnyTracer() error { return nil }

func ptraceSeize(int) error { return errLinuxProbe }

func nestedRemount(string, string) error { return errLinuxProbe }

func runNestedRemountChild(string) error { return errLinuxProbe }
