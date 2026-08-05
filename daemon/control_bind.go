package daemon

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// ResolveControlBind parses and normalizes the local control API listener
// address. The control API is intentionally unauthenticated, so it accepts
// only localhost or loopback IP literals and never resolves arbitrary names.
//
// rawHost normally comes from a host option, while port comes from a separate
// port option. rawHost may also include a port (for example, localhost:7734
// or [::1]:7734). When both specify a port, portExplicit determines whether a
// disagreement is rejected.
func ResolveControlBind(rawHost string, port int, portExplicit bool) (string, int, error) {
	host, embeddedPort, err := parseControlBindHost(rawHost)
	if err != nil {
		return "", 0, err
	}
	if port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("control bind port %d is outside 0-65535", port)
	}
	if embeddedPort != nil {
		if portExplicit && port != *embeddedPort {
			return "", 0, fmt.Errorf("control bind address port %d conflicts with explicit port %d", *embeddedPort, port)
		}
		port = *embeddedPort
	}
	return host, port, nil
}

func parseControlBindHost(rawHost string) (string, *int, error) {
	value := strings.TrimSpace(rawHost)
	if value == "" {
		return DefaultHTTPHost, nil, nil
	}

	host := value
	var portText string
	var hasPort bool
	switch {
	case strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]"):
		host = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	case strings.HasPrefix(value, "["):
		var err error
		host, portText, err = net.SplitHostPort(value)
		if err != nil {
			return "", nil, fmt.Errorf("invalid bracketed control bind address %q: %w", rawHost, err)
		}
		hasPort = true
	default:
		if _, err := netip.ParseAddr(value); err != nil && strings.Contains(value, ":") {
			var splitErr error
			host, portText, splitErr = net.SplitHostPort(value)
			if splitErr != nil {
				return "", nil, fmt.Errorf("invalid control bind address %q: %w", rawHost, splitErr)
			}
			hasPort = true
		}
	}

	var embeddedPort *int
	if hasPort {
		parsed, err := strconv.Atoi(portText)
		if err != nil || parsed < 0 || parsed > 65535 {
			return "", nil, fmt.Errorf("invalid control bind port in %q (want 0-65535)", rawHost)
		}
		embeddedPort = &parsed
	}

	if strings.EqualFold(host, "localhost") {
		return "localhost", embeddedPort, nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", nil, fmt.Errorf("control bind host %q is not localhost or a loopback IP literal", host)
	}
	if !ip.Unmap().IsLoopback() {
		return "", nil, fmt.Errorf("refusing non-loopback control bind %q", host)
	}
	return ip.Unmap().String(), embeddedPort, nil
}
