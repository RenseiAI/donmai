package attachwire

// §1 — Transport & version negotiation tokens (v1-frozen).
//
// Version negotiation and auth are orthogonal: the version token carries no
// auth material. These constants are the version-negotiation tokens for the two
// carriers; auth carriage lives on a separate channel (a header for native
// clients, a distinct subprotocol slot for browsers).
const (
	// ProtocolVersion is the wire protocol version identifier.
	ProtocolVersion = "interactive-attach-v1"

	// SubprotocolVersion is the WebSocket subprotocol a client offers and the
	// relay echoes to confirm the version on the WSS lane (§1). It equals
	// ProtocolVersion and carries no auth material.
	SubprotocolVersion = ProtocolVersion

	// VersionPathSegment is the degraded-lane version carrier embedded in the
	// attach URL path (§1, §14): HTTP/SSE has no subprotocol slot, so the "/v1/"
	// path segment negotiates the version. A relay that does not serve "/v1/"
	// degraded endpoints returns 404 and the client treats the lane as
	// unavailable.
	VersionPathSegment = "v1"

	// BearerSubprotocolPrefix is the browser-only auth subprotocol slot (§15):
	// the browser offers "bearer.<base64url(jwt)>" alongside SubprotocolVersion.
	// The relay reads the bearer slot, verifies it, and echoes back only the
	// version token — never the bearer slot. Kept distinct from version
	// negotiation so the two never collide.
	BearerSubprotocolPrefix = "bearer."
)
