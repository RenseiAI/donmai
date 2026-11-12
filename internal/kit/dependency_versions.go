// Package kit — dependency_versions.go: the semver range a dependency-store
// entry's `versions` key carries, and the arithmetic composition and
// selection need over it (parse, covers, overlaps).
//
// The grammar is the npm-style subset kit authors need to scope one entry to
// a package manager's major line:
//
//	range      = set { "||" set }
//	set        = comparator { space comparator } | "*"
//	comparator = [ ">=" | ">" | "<=" | "<" | "=" | "^" | "~" ] partial
//	partial    = N | N.N | N.N.N       (a missing or x/X/* part is a wildcard)
//
// Bounds carry no prerelease or build part. A version carrying a prerelease
// tag (for example 10.0.0-rc.1) is therefore covered by no ranged entry, and
// only an entry with no `versions` key covers it. That keeps ">=9 <10" from
// claiming 10.0.0-rc.1, which plain semver ordering would.
package kit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrVersionRangeInvalid marks a `versions` string this grammar rejects.
var ErrVersionRangeInvalid = errors.New("kit version range invalid")

// semVer is one release version. pre is the prerelease tag, set only on
// versions parsed from a pin, never on a range bound.
type semVer struct {
	major, minor, patch uint64
	pre                 string
}

func (a semVer) cmp(b semVer) int {
	for _, pair := range [][2]uint64{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// parseManagerVersion parses "N.N.N" with an optional "-prerelease" and
// "+build" part. It is strict: all three numbers are required.
func parseManagerVersion(s string) (semVer, error) {
	s = strings.TrimSpace(s)
	s, _, _ = strings.Cut(s, "+")
	s, pre, hasPre := strings.Cut(s, "-")
	if hasPre && pre == "" {
		return semVer{}, fmt.Errorf("%w: empty prerelease in %q", ErrVersionRangeInvalid, s)
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semVer{}, fmt.Errorf("%w: version %q is not major.minor.patch", ErrVersionRangeInvalid, s)
	}
	var nums [3]uint64
	for i, part := range parts {
		n, err := parseVersionNumber(part)
		if err != nil {
			return semVer{}, fmt.Errorf("%w: version %q: %v", ErrVersionRangeInvalid, s, err)
		}
		nums[i] = n
	}
	return semVer{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, nil
}

func parseVersionNumber(part string) (uint64, error) {
	if part == "" {
		return 0, errors.New("empty number")
	}
	for i := 0; i < len(part); i++ {
		if part[i] < '0' || part[i] > '9' {
			return 0, fmt.Errorf("%q is not a number", part)
		}
	}
	if len(part) > 1 && part[0] == '0' {
		return 0, fmt.Errorf("%q has a leading zero", part)
	}
	n, err := strconv.ParseUint(part, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is out of range", part)
	}
	return n, nil
}

// bound is one end of an interval; nil means unbounded.
type bound struct {
	v   semVer
	inc bool
}

// interval is a contiguous set of release versions.
type interval struct {
	lo, hi *bound
}

func (iv interval) empty() bool {
	if iv.lo == nil || iv.hi == nil {
		return false
	}
	c := iv.lo.v.cmp(iv.hi.v)
	return c > 0 || (c == 0 && !(iv.lo.inc && iv.hi.inc))
}

func (iv interval) contains(v semVer) bool {
	if iv.lo != nil {
		c := v.cmp(iv.lo.v)
		if c < 0 || (c == 0 && !iv.lo.inc) {
			return false
		}
	}
	if iv.hi != nil {
		c := v.cmp(iv.hi.v)
		if c > 0 || (c == 0 && !iv.hi.inc) {
			return false
		}
	}
	return true
}

func intersect(a, b interval) interval {
	out := interval{lo: a.lo, hi: a.hi}
	if b.lo != nil && (out.lo == nil || lowerTighter(*b.lo, *out.lo)) {
		out.lo = b.lo
	}
	if b.hi != nil && (out.hi == nil || upperTighter(*b.hi, *out.hi)) {
		out.hi = b.hi
	}
	return out
}

// lowerTighter reports whether lower bound x excludes more than y.
func lowerTighter(x, y bound) bool {
	c := x.v.cmp(y.v)
	return c > 0 || (c == 0 && !x.inc && y.inc)
}

// upperTighter reports whether upper bound x excludes more than y.
func upperTighter(x, y bound) bool {
	c := x.v.cmp(y.v)
	return c < 0 || (c == 0 && !x.inc && y.inc)
}

// VersionRange is a parsed `versions` value: a union of intervals. The zero
// value, and the result of parsing the empty string, cover every version.
type VersionRange struct {
	text      string
	any       bool
	intervals []interval
}

// ParseVersionRange parses a `versions` string. The empty string is the
// unbounded range, matching "a missing versions covers every version".
func ParseVersionRange(s string) (VersionRange, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return VersionRange{any: true}, nil
	}
	out := VersionRange{text: text}
	for _, set := range strings.Split(text, "||") {
		iv, err := parseComparatorSet(set)
		if err != nil {
			return VersionRange{}, fmt.Errorf("%w: %q: %v", ErrVersionRangeInvalid, text, err)
		}
		if iv.empty() {
			return VersionRange{}, fmt.Errorf("%w: %q: %q matches no version", ErrVersionRangeInvalid, text, strings.TrimSpace(set))
		}
		out.intervals = append(out.intervals, iv)
	}
	return out, nil
}

// String returns the range text, or "*" for the unbounded range.
func (r VersionRange) String() string {
	if r.any || r.text == "" {
		return "*"
	}
	return r.text
}

// Unbounded reports whether the range covers every version.
func (r VersionRange) Unbounded() bool { return r.any || r.text == "" }

// Covers reports whether the range covers a manager version such as
// "9.15.9" (a "+hash" suffix is ignored). A version with a prerelease tag is
// covered only by the unbounded range. A version that is not
// major.minor.patch is an error.
func (r VersionRange) Covers(version string) (bool, error) {
	v, err := parseManagerVersion(version)
	if err != nil {
		return false, err
	}
	if r.Unbounded() {
		return true, nil
	}
	if v.pre != "" {
		return false, nil
	}
	for _, iv := range r.intervals {
		if iv.contains(v) {
			return true, nil
		}
	}
	return false, nil
}

// Overlaps reports whether some version is covered by both ranges.
func (r VersionRange) Overlaps(other VersionRange) bool {
	if r.Unbounded() || other.Unbounded() {
		return true
	}
	for _, a := range r.intervals {
		for _, b := range other.intervals {
			if !intersect(a, b).empty() {
				return true
			}
		}
	}
	return false
}

func parseComparatorSet(set string) (interval, error) {
	tokens := strings.Fields(set)
	if len(tokens) == 0 {
		return interval{}, errors.New("empty comparator set")
	}
	// ">= 9" spelled with a space joins the operator to the next token.
	var comparators []string
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if isOperator(tok) {
			if i+1 >= len(tokens) {
				return interval{}, fmt.Errorf("operator %q has no version", tok)
			}
			i++
			tok += tokens[i]
		}
		comparators = append(comparators, tok)
	}
	all := interval{}
	for _, c := range comparators {
		iv, err := parseComparator(c)
		if err != nil {
			return interval{}, err
		}
		all = intersect(all, iv)
	}
	return all, nil
}

func isOperator(tok string) bool {
	switch tok {
	case ">=", "<=", ">", "<", "=", "^", "~":
		return true
	}
	return false
}

// partial is a version with 0 to 3 concrete parts.
type partial struct {
	parts int
	v     semVer
}

func parsePartial(s string) (partial, error) {
	if s == "" {
		return partial{}, errors.New("missing version")
	}
	if strings.ContainsAny(s, "-+") {
		return partial{}, fmt.Errorf("%q carries a prerelease or build part; bounds must be plain versions", s)
	}
	fields := strings.Split(s, ".")
	if len(fields) > 3 {
		return partial{}, fmt.Errorf("%q has more than three parts", s)
	}
	var p partial
	nums := [3]*uint64{&p.v.major, &p.v.minor, &p.v.patch}
	for i, f := range fields {
		if f == "x" || f == "X" || f == "*" {
			// A wildcard ends the concrete parts; nothing may follow it
			// except more wildcards.
			for _, rest := range fields[i:] {
				if rest != "x" && rest != "X" && rest != "*" {
					return partial{}, fmt.Errorf("%q has a number after a wildcard", s)
				}
			}
			return p, nil
		}
		n, err := parseVersionNumber(f)
		if err != nil {
			return partial{}, fmt.Errorf("%q: %v", s, err)
		}
		*nums[i] = n
		p.parts = i + 1
	}
	return p, nil
}

// next returns the first version above every version the partial covers:
// 9 -> 10.0.0, 9.15 -> 9.16.0.
func (p partial) next() semVer {
	switch p.parts {
	case 1:
		return semVer{major: p.v.major + 1}
	case 2:
		return semVer{major: p.v.major, minor: p.v.minor + 1}
	}
	return semVer{major: p.v.major, minor: p.v.minor, patch: p.v.patch + 1}
}

func parseComparator(tok string) (interval, error) {
	op := ""
	for _, candidate := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(tok, candidate) {
			op = candidate
			break
		}
	}
	p, err := parsePartial(strings.TrimPrefix(tok, op))
	if err != nil {
		return interval{}, err
	}
	if p.parts == 0 {
		// "*", "x", ">=*": every version. Other operators over a bare
		// wildcard would be meaningless.
		if op != "" && op != ">=" && op != "<=" && op != "=" {
			return interval{}, fmt.Errorf("operator %q over a wildcard", op)
		}
		return interval{}, nil
	}
	floor := bound{v: p.v, inc: true}
	ceil := bound{v: p.next()}
	switch op {
	case "", "=":
		if p.parts == 3 {
			return interval{lo: &floor, hi: &bound{v: p.v, inc: true}}, nil
		}
		return interval{lo: &floor, hi: &ceil}, nil
	case ">=":
		return interval{lo: &floor}, nil
	case ">":
		if p.parts == 3 {
			return interval{lo: &bound{v: p.v}}, nil
		}
		return interval{lo: &bound{v: ceil.v, inc: true}}, nil
	case "<":
		return interval{hi: &bound{v: p.v}}, nil
	case "<=":
		if p.parts == 3 {
			return interval{hi: &bound{v: p.v, inc: true}}, nil
		}
		return interval{hi: &ceil}, nil
	case "~":
		if p.parts == 1 {
			return interval{lo: &floor, hi: &ceil}, nil
		}
		return interval{lo: &floor, hi: &bound{v: semVer{major: p.v.major, minor: p.v.minor + 1}}}, nil
	case "^":
		upper := semVer{major: p.v.major + 1}
		switch {
		case p.v.major > 0:
		case p.parts == 1:
		case p.v.minor > 0 || p.parts == 2:
			upper = semVer{minor: p.v.minor + 1}
		default:
			upper = semVer{patch: p.v.patch + 1}
		}
		return interval{lo: &floor, hi: &bound{v: upper}}, nil
	}
	return interval{}, fmt.Errorf("unknown operator %q", op)
}
