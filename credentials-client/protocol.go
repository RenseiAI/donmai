package credentials

import "time"

// The wire-protocol message types below mirror the contract documented in
// doc.go. They are unexported because the loader is the only encoder /
// decoder for these frames; downstream agents never marshal them
// directly.

// helloMessage is the first frame an agent sends after dial.
type helloMessage struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
}

// initialMessage is the server's reply to a HELLO. Env carries the
// initial credential snapshot. The server is expected to have already
// applied its own blocklist; the client re-applies the local blocklist
// as defence-in-depth.
type initialMessage struct {
	Type string            `json:"type"`
	Env  map[string]string `json:"env"`
}

// updateMessage is pushed for every rotation delta after the INITIAL
// reply. Delta is the sparse map of keys that changed (absent keys
// retain their prior value).
type updateMessage struct {
	Type      string            `json:"type"`
	Delta     map[string]string `json:"delta"`
	RotatedAt time.Time         `json:"rotatedAt"`
}

// byeMessage signals a graceful close. Either side may send it.
type byeMessage struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}
