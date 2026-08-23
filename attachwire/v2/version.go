package attachwirev2

const (
	// ProtocolVersion is the exact v2 WebSocket subprotocol token.
	ProtocolVersion = "interactive-attach-v2"
	// SubprotocolVersion is offered and must be echoed on the host WSS leg.
	SubprotocolVersion = ProtocolVersion
	// VersionPathSegment is the required route segment.
	VersionPathSegment = "v2"
)
