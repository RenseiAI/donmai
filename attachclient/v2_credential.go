package attachclient

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// V2RetainedCredential holds one exact original attach-v2 bearer for consumed-
// adoption recovery. The bytes are deliberately private, non-JSON, defensively
// copied, and redacted under every fmt verb. Callers can only turn the retained
// bytes into the TokenSource expected by V2HostConfig.
type V2RetainedCredential struct {
	bearer []byte
}

// NewV2RetainedCredential freezes exact non-empty original bearer bytes.
func NewV2RetainedCredential(bearer []byte) (V2RetainedCredential, error) {
	if len(bearer) == 0 {
		return V2RetainedCredential{}, errors.New("attachclient: retained v2 credential is empty")
	}
	return V2RetainedCredential{bearer: append([]byte(nil), bearer...)}, nil
}

// TokenSource returns a source that replays the exact retained bytes. It never
// mints, refreshes, reconstructs, or exposes a mutable alias of the credential.
func (c V2RetainedCredential) TokenSource() TokenSource {
	bearer := append([]byte(nil), c.bearer...)
	return func(context.Context) (string, error) {
		if len(bearer) == 0 {
			return "", errors.New("attachclient: retained v2 credential is unavailable")
		}
		return string(append([]byte(nil), bearer...)), nil
	}
}

// IsZero reports whether no retained credential is available.
func (c V2RetainedCredential) IsZero() bool { return len(c.bearer) == 0 }

// Clone returns a defensive copy suitable for crossing another callback seam.
func (c V2RetainedCredential) Clone() V2RetainedCredential {
	return V2RetainedCredential{bearer: append([]byte(nil), c.bearer...)}
}

// Format prevents bearer bytes from reaching logs or errors through any fmt
// formatting verb, including detailed and Go-syntax forms.
func (V2RetainedCredential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<redacted-v2-credential>")
}
