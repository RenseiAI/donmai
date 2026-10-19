package claude

import (
	"crypto/rand"
	"fmt"
)

// generateOwnedSessionID mints the resume token this harness owns at
// launch. The id is passed to the CLI as `--session-id` on a fresh spawn
// (see buildArgs), so the token a later restore needs is known BEFORE the
// first turn — no InitEvent wait, no transcript read. The CLI echoes the
// same id on the init line, which the handle captures as confirmation.
//
// The minted shape is UUIDv4 (RFC 4122), the CLI's own session-id shape. A
// crypto/rand failure is a spawn failure, never a fallback to a weak id —
// a predictable resume token would let a sibling session on the same host
// claim this conversation's history.
func generateOwnedSessionID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("provider/claude: mint owned session id: %w", err)
	}
	// Set the version (4) and variant (RFC 4122) bits so the minted id is
	// shaped exactly like a CLI-native one.
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}
