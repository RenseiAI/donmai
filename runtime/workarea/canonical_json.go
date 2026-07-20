package workarea

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type canonicalJSONKind uint8

const (
	canonicalJSONString canonicalJSONKind = iota + 1
	canonicalJSONInteger
	canonicalJSONBool
	canonicalJSONNull
)

type canonicalJSONValue struct {
	kind    canonicalJSONKind
	text    string
	integer int64
	boolean bool
}

func parseCanonicalJSONObject(data []byte) (map[string]canonicalJSONValue, error) {
	if len(data) >= 3 && bytes.Equal(data[:3], []byte{0xef, 0xbb, 0xbf}) {
		return nil, errors.New("runtime/workarea: JSON BOM is forbidden")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("runtime/workarea: JSON must be valid UTF-8")
	}
	p := canonicalJSONParser{data: data}
	p.skipSpace()
	if !p.consume('{') {
		return nil, errors.New("runtime/workarea: JSON root must be one object")
	}
	values := make(map[string]canonicalJSONValue)
	p.skipSpace()
	if p.consume('}') {
		p.skipSpace()
		if p.pos != len(p.data) {
			return nil, errors.New("runtime/workarea: trailing JSON value")
		}
		return values, nil
	}
	for {
		p.skipSpace()
		name, err := p.parseString()
		if err != nil {
			return nil, fmt.Errorf("runtime/workarea: decode JSON member name: %w", err)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("runtime/workarea: duplicate JSON member %q", name)
		}
		p.skipSpace()
		if !p.consume(':') {
			return nil, fmt.Errorf("runtime/workarea: JSON member %q missing colon", name)
		}
		p.skipSpace()
		value, err := p.parseScalar()
		if err != nil {
			return nil, fmt.Errorf("runtime/workarea: decode JSON member %q: %w", name, err)
		}
		values[name] = value
		p.skipSpace()
		switch {
		case p.consume(','):
			continue
		case p.consume('}'):
			p.skipSpace()
			if p.pos != len(p.data) {
				return nil, errors.New("runtime/workarea: trailing JSON value")
			}
			return values, nil
		default:
			return nil, errors.New("runtime/workarea: JSON object is not terminated")
		}
	}
}

type canonicalJSONParser struct {
	data []byte
	pos  int
}

func (p *canonicalJSONParser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		default:
			return
		}
	}
}

func (p *canonicalJSONParser) consume(want byte) bool {
	if p.pos >= len(p.data) || p.data[p.pos] != want {
		return false
	}
	p.pos++
	return true
}

func (p *canonicalJSONParser) parseScalar() (canonicalJSONValue, error) {
	if p.pos >= len(p.data) {
		return canonicalJSONValue{}, errors.New("missing JSON value")
	}
	switch p.data[p.pos] {
	case '"':
		value, err := p.parseString()
		return canonicalJSONValue{kind: canonicalJSONString, text: value}, err
	case 't':
		if p.consumeLiteral("true") {
			return canonicalJSONValue{kind: canonicalJSONBool, boolean: true}, nil
		}
	case 'f':
		if p.consumeLiteral("false") {
			return canonicalJSONValue{kind: canonicalJSONBool, boolean: false}, nil
		}
	case 'n':
		if p.consumeLiteral("null") {
			return canonicalJSONValue{kind: canonicalJSONNull}, nil
		}
	default:
		return p.parseInteger()
	}
	return canonicalJSONValue{}, errors.New("invalid JSON literal")
}

func (p *canonicalJSONParser) consumeLiteral(literal string) bool {
	if !bytes.HasPrefix(p.data[p.pos:], []byte(literal)) {
		return false
	}
	p.pos += len(literal)
	return true
}

func (p *canonicalJSONParser) parseInteger() (canonicalJSONValue, error) {
	start := p.pos
	if p.pos >= len(p.data) || p.data[p.pos] < '0' || p.data[p.pos] > '9' {
		return canonicalJSONValue{}, errors.New("value must be a non-negative JSON integer")
	}
	if p.data[p.pos] == '0' {
		p.pos++
		if p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			return canonicalJSONValue{}, errors.New("JSON integer has a leading zero")
		}
	} else {
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == '.' || p.data[p.pos] == 'e' || p.data[p.pos] == 'E' || p.data[p.pos] == '+') {
		return canonicalJSONValue{}, errors.New("value must use integer spelling without exponent or fraction")
	}
	value, err := strconv.ParseInt(string(p.data[start:p.pos]), 10, 64)
	if err != nil {
		return canonicalJSONValue{}, errors.New("JSON integer is outside signed 64-bit range")
	}
	return canonicalJSONValue{kind: canonicalJSONInteger, integer: value}, nil
}

func (p *canonicalJSONParser) parseString() (string, error) {
	if !p.consume('"') {
		return "", errors.New("expected JSON string")
	}
	var out strings.Builder
	for p.pos < len(p.data) {
		if p.data[p.pos] == '"' {
			p.pos++
			return out.String(), nil
		}
		if p.data[p.pos] == '\\' {
			p.pos++
			if p.pos >= len(p.data) {
				return "", errors.New("unterminated JSON escape")
			}
			escape := p.data[p.pos]
			p.pos++
			switch escape {
			case '"', '\\', '/':
				out.WriteByte(escape)
			case 'b':
				out.WriteByte('\b')
			case 'f':
				out.WriteByte('\f')
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			case 'u':
				r, err := p.parseUnicodeEscape()
				if err != nil {
					return "", err
				}
				out.WriteRune(r)
			default:
				return "", fmt.Errorf("invalid JSON escape \\%c", escape)
			}
			continue
		}
		r, size := utf8.DecodeRune(p.data[p.pos:])
		if r == utf8.RuneError && size == 1 {
			return "", errors.New("malformed UTF-8 in JSON string")
		}
		if r < 0x20 {
			return "", errors.New("unescaped control character in JSON string")
		}
		if r >= 0xd800 && r <= 0xdfff {
			return "", errors.New("literal surrogate code point in JSON string")
		}
		out.WriteRune(r)
		p.pos += size
	}
	return "", errors.New("unterminated JSON string")
}

func (p *canonicalJSONParser) parseUnicodeEscape() (rune, error) {
	first, err := p.parseHex16()
	if err != nil {
		return 0, err
	}
	if first >= 0xdc00 && first <= 0xdfff {
		return 0, errors.New("isolated low surrogate escape")
	}
	if first < 0xd800 || first > 0xdbff {
		return rune(first), nil
	}
	if p.pos+2 > len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
		return 0, errors.New("high surrogate is not followed by a low surrogate")
	}
	p.pos += 2
	second, err := p.parseHex16()
	if err != nil {
		return 0, err
	}
	if second < 0xdc00 || second > 0xdfff {
		return 0, errors.New("high surrogate is not followed by a low surrogate")
	}
	decoded := utf16.DecodeRune(rune(first), rune(second))
	if decoded == utf8.RuneError {
		return 0, errors.New("invalid surrogate pair")
	}
	return decoded, nil
}

func (p *canonicalJSONParser) parseHex16() (uint16, error) {
	if p.pos+4 > len(p.data) {
		return 0, errors.New("short Unicode escape")
	}
	var value uint16
	for i := 0; i < 4; i++ {
		value <<= 4
		switch c := p.data[p.pos+i]; {
		case c >= '0' && c <= '9':
			value |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			value |= uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			value |= uint16(c-'A') + 10
		default:
			return 0, errors.New("invalid Unicode escape")
		}
	}
	p.pos += 4
	return value, nil
}

func canonicalJSONStringBytes(value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, errors.New("runtime/workarea: string must be valid UTF-8")
	}
	for _, r := range value {
		if r >= 0xd800 && r <= 0xdfff {
			return nil, errors.New("runtime/workarea: string contains a surrogate code point")
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

func appendCanonicalStringField(dst []byte, name, value string, first bool) ([]byte, error) {
	if !first {
		dst = append(dst, ',')
	}
	dst = append(dst, '"')
	dst = append(dst, name...)
	dst = append(dst, '"', ':')
	encoded, err := canonicalJSONStringBytes(value)
	if err != nil {
		return nil, err
	}
	return append(dst, encoded...), nil
}

func appendCanonicalIntegerField(dst []byte, name string, value int64, first bool) ([]byte, error) {
	if value < 0 {
		return nil, fmt.Errorf("runtime/workarea: %s must not be negative", name)
	}
	if !first {
		dst = append(dst, ',')
	}
	dst = append(dst, '"')
	dst = append(dst, name...)
	dst = append(dst, '"', ':')
	return strconv.AppendInt(dst, value, 10), nil
}

func appendCanonicalBoolField(dst []byte, name string, value bool, first bool) []byte {
	if !first {
		dst = append(dst, ',')
	}
	dst = append(dst, '"')
	dst = append(dst, name...)
	dst = append(dst, '"', ':')
	return strconv.AppendBool(dst, value)
}

func appendCanonicalNullableStringField(dst []byte, name string, value *string, first bool) ([]byte, error) {
	if !first {
		dst = append(dst, ',')
	}
	dst = append(dst, '"')
	dst = append(dst, name...)
	dst = append(dst, '"', ':')
	if value == nil {
		return append(dst, "null"...), nil
	}
	encoded, err := canonicalJSONStringBytes(*value)
	if err != nil {
		return nil, err
	}
	return append(dst, encoded...), nil
}

func requireClosedFields(values map[string]canonicalJSONValue, fields ...string) error {
	if len(values) != len(fields) {
		return fmt.Errorf("runtime/workarea: JSON object has %d fields; want %d", len(values), len(fields))
	}
	for _, field := range fields {
		if _, ok := values[field]; !ok {
			return fmt.Errorf("runtime/workarea: required JSON member %q is missing", field)
		}
	}
	return nil
}

func requireString(values map[string]canonicalJSONValue, field string) (string, error) {
	value, ok := values[field]
	if !ok || value.kind != canonicalJSONString {
		return "", fmt.Errorf("runtime/workarea: JSON member %q must be a string", field)
	}
	return value.text, nil
}

func requireInteger(values map[string]canonicalJSONValue, field string) (int64, error) {
	value, ok := values[field]
	if !ok || value.kind != canonicalJSONInteger {
		return 0, fmt.Errorf("runtime/workarea: JSON member %q must be an integer", field)
	}
	return value.integer, nil
}

func requireBool(values map[string]canonicalJSONValue, field string) (bool, error) {
	value, ok := values[field]
	if !ok || value.kind != canonicalJSONBool {
		return false, fmt.Errorf("runtime/workarea: JSON member %q must be a boolean", field)
	}
	return value.boolean, nil
}

func requireNullableString(values map[string]canonicalJSONValue, field string) (*string, error) {
	value, ok := values[field]
	if !ok {
		return nil, fmt.Errorf("runtime/workarea: required JSON member %q is missing", field)
	}
	if value.kind == canonicalJSONNull {
		return nil, nil
	}
	if value.kind != canonicalJSONString {
		return nil, fmt.Errorf("runtime/workarea: JSON member %q must be a string or null", field)
	}
	out := value.text
	return &out, nil
}
