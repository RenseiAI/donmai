// Package semverrange parses semantic versions and the small range grammar a
// kit dependency-store entry uses for its `versions` field. It is the only
// range implementation: manifest validation and composition (internal/kit)
// and entry selection (runtime/depprep) both use it, so a range a kit
// validates is the range a preparer selects by.
//
// The grammar is a subset of the npm range syntax:
//
//	range      = set *( "||" set )
//	set        = comparator *( whitespace comparator )
//	comparator = [ ">=" | "<=" | ">" | "<" | "=" | "^" | "~" ] [ whitespace ] bound
//	bound      = part [ "." part [ "." part ] ]
//	part       = number | "x" | "X" | "*"
//
// A missing or wildcard part is a wildcard; nothing but wildcards may follow
// one ("9.x" and "9.x.x" are "9"; "9.x.1" is refused). A bare "*" (or "x")
// is every version, and only "", "=", ">=" and "<=" may precede a bare
// wildcard. Bounds are plain release versions: a "v" prefix, a pre-release
// or build part, a hyphen range and "&&" are refused rather than guessed
// at. A set that no version can satisfy (">=10 <9") is refused.
//
// A bare or "=" partial covers every version it names a prefix of ("9" is
// ">=9.0.0-0 <10.0.0-0"). "^" and "~" follow npm. A version satisfies a
// range when it satisfies every comparator of at least one set. A partial
// bound names a whole release line, its pre-releases included, so
// 10.0.0-rc.1 satisfies ">=10 <11" and not ">=9 <10": a manager's
// pre-release selects the entry of the line it belongs to. The npm rule
// that hides pre-releases from ranges without one is not applied, because
// a manager's pre-release still has to select an entry.
//
// The empty string is not a range. A caller for which a missing range means
// "every version" uses All.
package semverrange

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalid marks a version or range that does not parse.
var ErrInvalid = errors.New("semver: invalid")

// Version is a parsed semantic version. Build metadata is dropped.
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string
}

// String renders the version in canonical form.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

// ParseVersion parses a complete version: "1.2.3", "v1.2.3", "1.2.3-rc.1",
// "1.2.3+build". Surrounding whitespace is trimmed.
func ParseVersion(s string) (Version, error) {
	v, parts, err := parsePartial(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if err != nil {
		return Version{}, err
	}
	if parts != 3 {
		return Version{}, fmt.Errorf("%w: version %q is not major.minor.patch", ErrInvalid, s)
	}
	return v, nil
}

// Compare orders a before b: -1, 0 or 1.
func Compare(a, b Version) int {
	for _, pair := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	return comparePre(a.Pre, b.Pre)
}

func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		ai, aNum := numericIdent(a[i])
		bi, bNum := numericIdent(b[i])
		switch {
		case aNum && bNum:
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
		case aNum:
			return -1
		case bNum:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func numericIdent(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

type op int

const (
	opGE op = iota
	opGT
	opLE
	opLT
)

type comparator struct {
	op op
	v  Version
}

func (c comparator) match(v Version) bool {
	cmp := Compare(v, c.v)
	switch c.op {
	case opGE:
		return cmp >= 0
	case opGT:
		return cmp > 0
	case opLE:
		return cmp <= 0
	default:
		return cmp < 0
	}
}

// Range is a parsed range.
type Range struct {
	raw  string
	sets [][]comparator
}

// All is the range every version satisfies, pre-releases included. It is
// what a missing `versions` key means.
func All() Range { return Range{sets: [][]comparator{nil}} }

// String returns the range as written, or "*" for All.
func (r Range) String() string {
	if strings.TrimSpace(r.raw) == "" {
		return "*"
	}
	return r.raw
}

// Contains reports whether v satisfies the range.
func (r Range) Contains(v Version) bool {
	for _, set := range r.sets {
		ok := true
		for _, c := range set {
			if !c.match(v) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Overlaps reports whether some version satisfies both ranges. It is exact
// for the interval sets this grammar produces.
func (r Range) Overlaps(o Range) bool {
	for _, a := range r.sets {
		for _, b := range o.sets {
			if intervalsMeet(interval(a), interval(b)) {
				return true
			}
		}
	}
	return false
}

// bound is one end of an interval; nil v means unbounded.
type bound struct {
	v         *Version
	inclusive bool
}

func interval(set []comparator) [2]bound {
	var lo, hi bound
	for _, c := range set {
		v := c.v
		switch c.op {
		case opGE, opGT:
			incl := c.op == opGE
			if lo.v == nil || Compare(v, *lo.v) > 0 || (Compare(v, *lo.v) == 0 && !incl) {
				lo = bound{v: &v, inclusive: incl}
			}
		default:
			incl := c.op == opLE
			if hi.v == nil || Compare(v, *hi.v) < 0 || (Compare(v, *hi.v) == 0 && !incl) {
				hi = bound{v: &v, inclusive: incl}
			}
		}
	}
	return [2]bound{lo, hi}
}

// nonEmpty reports whether some version lies between the bounds.
func nonEmpty(lo, hi bound) bool {
	if lo.v == nil || hi.v == nil {
		return true
	}
	c := Compare(*lo.v, *hi.v)
	return c < 0 || (c == 0 && lo.inclusive && hi.inclusive)
}

func intervalsMeet(a [2]bound, b [2]bound) bool {
	return nonEmpty(maxLow(a[0], b[0]), minHigh(a[1], b[1]))
}

func maxLow(a, b bound) bound {
	switch {
	case a.v == nil:
		return b
	case b.v == nil:
		return a
	}
	c := Compare(*a.v, *b.v)
	if c > 0 || (c == 0 && !a.inclusive) {
		return a
	}
	return b
}

func minHigh(a, b bound) bound {
	switch {
	case a.v == nil:
		return b
	case b.v == nil:
		return a
	}
	c := Compare(*a.v, *b.v)
	if c < 0 || (c == 0 && !a.inclusive) {
		return a
	}
	return b
}

// operators, longest first so ">=" is not read as ">".
var operators = []string{">=", "<=", ">", "<", "=", "^", "~"}

func isOperator(tok string) bool {
	for _, o := range operators {
		if tok == o {
			return true
		}
	}
	return false
}

// ParseRange parses a range in the grammar above.
func ParseRange(s string) (Range, error) {
	raw := s
	s = strings.TrimSpace(s)
	if s == "" {
		return Range{}, fmt.Errorf("%w: empty range", ErrInvalid)
	}
	r := Range{raw: raw}
	for _, setText := range strings.Split(s, "||") {
		fields := strings.Fields(setText)
		if len(fields) == 0 {
			return Range{}, fmt.Errorf("%w: range %q has an empty alternative", ErrInvalid, raw)
		}
		set := []comparator{}
		for i := 0; i < len(fields); i++ {
			field := fields[i]
			// ">= 9" spelled with a space joins the operator to its bound.
			if isOperator(field) {
				if i+1 >= len(fields) {
					return Range{}, fmt.Errorf("%w: comparator %q has no version (in range %q)", ErrInvalid, field, raw)
				}
				i++
				field += fields[i]
			}
			cs, err := parseComparator(field)
			if err != nil {
				return Range{}, fmt.Errorf("%w (in range %q)", err, raw)
			}
			set = append(set, cs...)
		}
		iv := interval(set)
		if !nonEmpty(iv[0], iv[1]) {
			return Range{}, fmt.Errorf("%w: %q matches no version (in range %q)", ErrInvalid, strings.TrimSpace(setText), raw)
		}
		r.sets = append(r.sets, set)
	}
	return r, nil
}

func parseComparator(field string) ([]comparator, error) {
	var prefix string
	for _, p := range operators {
		if strings.HasPrefix(field, p) {
			prefix = p
			break
		}
	}
	text := strings.TrimPrefix(field, prefix)
	if text == "" {
		return nil, fmt.Errorf("%w: comparator %q has no version", ErrInvalid, field)
	}
	v, parts, err := parseBound(text)
	if err != nil {
		return nil, err
	}
	if parts == 0 {
		// "*", "x", ">=*": every version. Any other operator over a bare
		// wildcard means nothing.
		switch prefix {
		case "", "=", ">=", "<=":
			return nil, nil
		}
		return nil, fmt.Errorf("%w: operator %q over a wildcard", ErrInvalid, prefix)
	}
	if parts < 3 {
		// A partial names a whole release line, pre-releases included:
		// "10" starts at 10.0.0-0, so 10.0.0-rc.1 is a 10.
		v = lowest(v)
	}
	switch prefix {
	case ">=":
		return []comparator{{opGE, v}}, nil
	case "<":
		return []comparator{{opLT, v}}, nil
	case ">":
		if parts < 3 {
			// ">9" means above every 9.x: ">=10.0.0".
			return []comparator{{opGE, bumpAt(v, parts)}}, nil
		}
		return []comparator{{opGT, v}}, nil
	case "<=":
		if parts < 3 {
			return []comparator{{opLT, bumpAt(v, parts)}}, nil
		}
		return []comparator{{opLE, v}}, nil
	case "^":
		upper := Version{Major: v.Major + 1}
		switch {
		case v.Major == 0 && parts >= 2 && v.Minor == 0 && parts == 3:
			upper = Version{Patch: v.Patch + 1}
		case v.Major == 0 && parts >= 2:
			upper = Version{Minor: v.Minor + 1}
		}
		return []comparator{{opGE, v}, {opLT, lowest(upper)}}, nil
	case "~":
		if parts == 1 {
			return []comparator{{opGE, v}, {opLT, lowest(Version{Major: v.Major + 1})}}, nil
		}
		return []comparator{{opGE, v}, {opLT, lowest(Version{Major: v.Major, Minor: v.Minor + 1})}}, nil
	default: // "" or "="
		if parts == 3 {
			return []comparator{{opGE, v}, {opLE, v}}, nil
		}
		return []comparator{{opGE, v}, {opLT, bumpAt(v, parts)}}, nil
	}
}

// parseBound parses a range bound: up to three dot-separated parts, each a
// number or a wildcard, with only wildcards after the first wildcard. It
// reports how many numeric parts lead the bound (0 for a bare wildcard).
func parseBound(s string) (Version, int, error) {
	if strings.ContainsAny(s, "-+") {
		return Version{}, 0, fmt.Errorf("%w: bound %q carries a prerelease or build part; bounds are plain versions", ErrInvalid, s)
	}
	fields := strings.Split(s, ".")
	if len(fields) > 3 {
		return Version{}, 0, fmt.Errorf("%w: bound %q has more than three parts", ErrInvalid, s)
	}
	var vals [3]uint64
	parts := 0
	for i, f := range fields {
		if f == "x" || f == "X" || f == "*" {
			for _, rest := range fields[i:] {
				if rest != "x" && rest != "X" && rest != "*" {
					return Version{}, 0, fmt.Errorf("%w: bound %q has a number after a wildcard", ErrInvalid, s)
				}
			}
			break
		}
		n, err := parseNumber(f)
		if err != nil {
			return Version{}, 0, err
		}
		vals[i] = n
		parts = i + 1
	}
	return Version{Major: vals[0], Minor: vals[1], Patch: vals[2]}, parts, nil
}

// parseNumber parses one numeric version part: digits, no leading zero.
func parseNumber(n string) (uint64, error) {
	if n == "" || (len(n) > 1 && n[0] == '0') {
		return 0, fmt.Errorf("%w: version part %q", ErrInvalid, n)
	}
	for j := 0; j < len(n); j++ {
		if n[j] < '0' || n[j] > '9' {
			return 0, fmt.Errorf("%w: version part %q", ErrInvalid, n)
		}
	}
	val, err := strconv.ParseUint(n, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: version part %q", ErrInvalid, n)
	}
	return val, nil
}

// bumpAt returns the first version after every version the partial names:
// "9" -> 10.0.0, "9.1" -> 9.2.0.
func bumpAt(v Version, parts int) Version {
	if parts == 1 {
		return lowest(Version{Major: v.Major + 1})
	}
	return lowest(Version{Major: v.Major, Minor: v.Minor + 1})
}

// lowest returns the first version of v's release, below every
// pre-release of it: 10.0.0 -> 10.0.0-0.
func lowest(v Version) Version {
	v.Pre = []string{"0"}
	return v
}

// parsePartial parses major[.minor[.patch[-pre]]][+build] and reports how
// many numeric parts were present. It parses versions, not range bounds.
func parsePartial(s string) (Version, int, error) {
	if plus := strings.IndexByte(s, '+'); plus >= 0 {
		if plus == len(s)-1 {
			return Version{}, 0, fmt.Errorf("%w: %q has empty build metadata", ErrInvalid, s)
		}
		s = s[:plus]
	}
	var pre []string
	if dash := strings.IndexByte(s, '-'); dash >= 0 {
		preText := s[dash+1:]
		s = s[:dash]
		if preText == "" {
			return Version{}, 0, fmt.Errorf("%w: %q has an empty pre-release", ErrInvalid, s)
		}
		for _, id := range strings.Split(preText, ".") {
			if id == "" || !identChars(id) {
				return Version{}, 0, fmt.Errorf("%w: pre-release %q", ErrInvalid, preText)
			}
			pre = append(pre, id)
		}
	}
	nums := strings.Split(s, ".")
	if len(nums) == 0 || len(nums) > 3 {
		return Version{}, 0, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	if pre != nil && len(nums) != 3 {
		return Version{}, 0, fmt.Errorf("%w: pre-release on a partial version %q", ErrInvalid, s)
	}
	var vals [3]uint64
	for i, n := range nums {
		val, err := parseNumber(n)
		if err != nil {
			return Version{}, 0, err
		}
		vals[i] = val
	}
	return Version{Major: vals[0], Minor: vals[1], Patch: vals[2], Pre: pre}, len(nums), nil
}

func identChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' {
			return false
		}
	}
	return true
}
