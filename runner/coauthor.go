package runner

import (
	"log/slog"
	"net/mail"
	"strings"
	"unicode"
)

// ValidateCoAuthorPair preserves valid work-source attribution verbatim. Invalid
// or incomplete pairs are dropped together, with a warning that contains no values.
func ValidateCoAuthorPair(sessionID, name, email string) (string, string) {
	if name == "" && email == "" {
		return "", ""
	}
	reason := ""
	switch {
	case name == "" || email == "":
		reason = "incomplete pair"
	case strings.ContainsAny(name+email, "\r\n\x00<>"):
		reason = "forbidden character"
	case strings.Contains(strings.ToLower(name), "co-authored-by:") || strings.Contains(strings.ToLower(email), "co-authored-by:"):
		reason = "trailer injection"
	case strings.Count(email, "@") != 1 || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@") || strings.ContainsFunc(email, unicode.IsSpace):
		reason = "invalid single email address"
	}
	if reason == "" {
		address, err := mail.ParseAddress(email)
		if err != nil || address.Address != email {
			reason = "invalid single email address"
		}
	}
	if reason != "" {
		slog.Warn("dropping invalid commit co-author pair; using bot-only credit", "sessionId", sessionID, "reason", reason)
		return "", ""
	}
	return name, email
}
