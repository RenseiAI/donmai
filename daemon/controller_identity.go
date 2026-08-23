package daemon

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrControllerIdentityAlias reports credentials whose comparison-only
// correlation equals the immutable controller id. It carries no token bytes.
var ErrControllerIdentityAlias = errors.New("session shim: controller identity aliases runtime credentials")

// runtimeTokenJTI extracts only the string jti claim needed for correlation
// comparison. It does not verify a signature, authenticate a token, or grant
// authority. Malformed/missing/non-string jti reads as absent so existing
// credential validation semantics remain authoritative.
func runtimeTokenJTI(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	var claims map[string]json.RawMessage
	if err := dec.Decode(&claims); err != nil {
		return "", false
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", false
	}
	raw, ok := claims["jti"]
	if !ok {
		return "", false
	}
	var jti string
	if err := json.Unmarshal(raw, &jti); err != nil || jti == "" {
		return "", false
	}
	return jti, true
}

func (d *Daemon) validateControllerCredentials(workerID, runtimeToken string) error {
	if err := d.validateControllerAlias(workerID, "worker registration id"); err != nil {
		return fmt.Errorf("%w: worker registration id", ErrControllerIdentityAlias)
	}
	if jti, ok := runtimeTokenJTI(runtimeToken); ok && jti == d.controllerID() {
		return fmt.Errorf("%w: runtime token jti", ErrControllerIdentityAlias)
	}
	return nil
}
