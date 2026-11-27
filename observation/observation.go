// Package observation encodes the host-observation report a daemon sends to
// its control plane. Encoding is closed: a kind is a typed attribute struct
// registered in code with unexported methods, so an undeclared field cannot
// be serialized, and Encode refuses bad enums or key shapes rather than emit
// a partial item. Raw ids, tokens and paths have no field: an account id
// reaches the wire only as a SHA-256 itemKey, a config home only as a slot
// hash. A stdlib-only leaf; design: donmai-architecture
// ADR-2026-11-19-host-observation-contract.md, Parts G and S.
package observation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Disclosure is an attribute's disclosure class.
type Disclosure string

// Identity says whether an itemKey names a host-independent account.
type Identity string

// ReadinessState is an item's readiness verdict.
type ReadinessState string

// Wire constants: the report version, the default slot, disclosure
// classes, item identities and readiness states.
const (
	ReportVersion = 1
	DefaultSlot   = "default"

	DisclosurePublic   Disclosure = "public"
	DisclosureIdentity Disclosure = "identity"
	DisclosureHashed   Disclosure = "hashed"
	DisclosureBoolean  Disclosure = "boolean"
	DisclosureEnum     Disclosure = "enum"

	IdentityAccount    Identity = "account"
	IdentityHostScoped Identity = "host-scoped"

	ReadinessReady       ReadinessState = "ready"
	ReadinessDegraded    ReadinessState = "degraded"
	ReadinessUnavailable ReadinessState = "unavailable"
	ReadinessUnknown     ReadinessState = "unknown"
)

// Attributes is one kind's closed attribute schema; the unexported methods
// keep the kind set closed. normalize validates and returns what to encode.
type Attributes interface {
	kindID() string
	normalize() (Attributes, error)
}

// registry is the closed kind registry, added to by ADR, never by config.
// Each attribute struct tags every field with its `disclosure` class.
var registry = map[string]Attributes{
	KindSubscriptionLogin: SubscriptionLogin{},
}

// Readiness is an item's latest readiness check.
type Readiness struct {
	State     ReadinessState `json:"state"`
	Reasons   []string       `json:"reasons"`
	CheckedAt string         `json:"checkedAt"`
}

// Header is the kind-independent part of an item.
type Header struct {
	ItemKey    string    `json:"itemKey"`
	Slot       string    `json:"slot"`
	Identity   Identity  `json:"identity"`
	Definition string    `json:"definition,omitempty"`
	Scopes     []string  `json:"scopes,omitempty"`
	Readiness  Readiness `json:"readiness"`
}

var (
	hex64Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex16Pattern      = regexp.MustCompile(`^[0-9a-f]{16}$`)
	slugPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	reasonPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	scopePattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,127}$`)
	definitionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}:[a-z0-9][a-z0-9._-]{0,63}$`)
)

func validTime(s string) bool {
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

func oneOf[T ~string](v T, allowed ...T) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func allMatch(re *regexp.Regexp, values []string) bool {
	for _, v := range values {
		if !re.MatchString(v) {
			return false
		}
	}
	return true
}

func (h Header) normalize() (Header, error) {
	switch {
	case !hex64Pattern.MatchString(h.ItemKey):
		return h, errors.New("itemKey is not 64 lowercase hex")
	case h.Slot != DefaultSlot && !hex16Pattern.MatchString(h.Slot):
		return h, errors.New(`slot is neither "default" nor 16 lowercase hex`)
	case !oneOf(h.Identity, IdentityAccount, IdentityHostScoped):
		return h, fmt.Errorf("identity %q is not allowed", h.Identity)
	case h.Definition != "" && !definitionPattern.MatchString(h.Definition):
		return h, errors.New(`definition is not "<provider>:<plan>"`)
	case !oneOf(h.Readiness.State, ReadinessReady, ReadinessDegraded, ReadinessUnavailable, ReadinessUnknown):
		return h, fmt.Errorf("readiness state %q is not allowed", h.Readiness.State)
	case !validTime(h.Readiness.CheckedAt):
		return h, errors.New("readiness checkedAt is not RFC3339")
	}
	if !allMatch(scopePattern, h.Scopes) || !allMatch(reasonPattern, h.Readiness.Reasons) {
		return h, errors.New("a scope or readiness reason is not a lowercase code")
	}
	h.Readiness.Reasons = append([]string{}, h.Readiness.Reasons...)
	return h, nil
}

// Item is one encoded observed item. Only Encode produces one; the zero
// Item refuses to marshal.
type Item struct {
	ref Ref
	raw json.RawMessage
}

// Ref names one item: its kind, itemKey and slot.
type Ref struct {
	Kind    string `json:"kind"`
	ItemKey string `json:"itemKey"`
	Slot    string `json:"slot"`
}

// Ref returns the item's reference.
func (i Item) Ref() Ref { return i.ref }

// MarshalJSON emits the bytes Encode validated.
func (i Item) MarshalJSON() ([]byte, error) {
	if len(i.raw) == 0 {
		return nil, errors.New("observation: item was not produced by Encode")
	}
	return i.raw, nil
}

type wireItem struct {
	Kind string `json:"kind"`
	Header
	Attributes Attributes `json:"attributes"`
}

// Encode validates one item and encodes it through its kind's closed
// schema. Invalid input returns an error and no item.
func Encode(h Header, attrs Attributes) (Item, error) {
	if attrs == nil {
		return Item{}, errors.New("observation: nil attributes")
	}
	kind := attrs.kindID()
	if _, ok := registry[kind]; !ok {
		return Item{}, fmt.Errorf("observation: unknown kind %q", kind)
	}
	header, err := h.normalize()
	if err != nil {
		return Item{}, fmt.Errorf("observation: %s item: %w", kind, err)
	}
	normalized, err := attrs.normalize()
	if err != nil {
		return Item{}, fmt.Errorf("observation: %s attributes: %w", kind, err)
	}
	raw, err := json.Marshal(wireItem{Kind: kind, Header: header, Attributes: normalized})
	if err != nil {
		return Item{}, fmt.Errorf("observation: encode %s item: %w", kind, err)
	}
	return Item{ref: Ref{Kind: kind, ItemKey: header.ItemKey, Slot: header.Slot}, raw: raw}, nil
}

// Report is one host-observation report. Full means the report is a
// complete scan of Kinds: an item of those kinds it omits is gone.
type Report struct {
	Version   int      `json:"version"`
	ScannedAt string   `json:"scannedAt"`
	Full      bool     `json:"full"`
	Kinds     []string `json:"kinds"`
	Items     []Item   `json:"items"`
}

// NewReport builds a report over encoded items. Every kind must be
// registered and every item must belong to one of kinds.
func NewReport(scannedAt time.Time, full bool, kinds []string, items []Item) (*Report, error) {
	if scannedAt.IsZero() || len(kinds) == 0 {
		return nil, errors.New("observation: report needs a scan time and at least one kind")
	}
	covered := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		if _, ok := registry[k]; !ok || covered[k] {
			return nil, fmt.Errorf("observation: report kind %q is unknown or repeated", k)
		}
		covered[k] = true
	}
	for _, it := range items {
		if len(it.raw) == 0 || !covered[it.ref.Kind] {
			return nil, fmt.Errorf("observation: item of kind %q is not covered by the report", it.ref.Kind)
		}
	}
	return &Report{
		Version: ReportVersion, ScannedAt: scannedAt.UTC().Format(time.RFC3339), Full: full,
		Kinds: append([]string{}, kinds...), Items: append([]Item{}, items...),
	}, nil
}

// hashHex is lowercase hex SHA-256 of parts joined by NUL.
func hashHex(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// SubscriptionLoginItemKey derives a subscription login's host-independent
// itemKey from the provider's stable account id, plus the user id while
// the provider meters per seat.
func SubscriptionLoginItemKey(provider, accountID, userID string) string {
	parts := []string{"donmai-subscription-login-v1", provider, accountID}
	if userID != "" {
		parts = append(parts, userID)
	}
	return hashHex(parts...)
}

// HostScopedItemKey derives the itemKey of an item with no stable account
// id. It is unique within one host only and never aggregates across hosts.
func HostScopedItemKey(kind, harness, slot string) string {
	return hashHex("donmai-"+kind+"-host-scoped-v1", harness, slot)
}

// SlotFor names the slot of the config home a harness reads its login
// from: DefaultSlot for the default home (""), else a 16-hex hash.
func SlotFor(harness, configHome string) string {
	if configHome == "" {
		return DefaultSlot
	}
	return hashHex("donmai-slot-v1", harness, configHome)[:16]
}
