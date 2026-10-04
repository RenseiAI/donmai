//go:build !darwin

package confinement

// DefaultBackend returns the confinement backend for the running OS. No
// backend exists here yet, so every Prepare refuses with backend_absent.
func DefaultBackend() Backend { return nil }

// ResolverSockets returns the OS resolver sockets a harness that needs name
// resolution declares in Spec.Sockets. None are needed here.
func ResolverSockets() []string { return nil }

// sharedLocations are the shared temporary locations a self-test probes.
func sharedLocations() ([]string, error) {
	return []string{"/tmp", "/var/tmp"}, nil
}
