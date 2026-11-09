package agent

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type headerCase struct {
	name    string
	in      string
	want    []ProviderHTTPHeader
	wantErr string
}

func TestParseProviderHTTPHeaders(t *testing.T) {
	lines := make([]string, MaxProviderHTTPHeaders+1)
	atCap := make([]ProviderHTTPHeader, MaxProviderHTTPHeaders)
	for i := range lines {
		lines[i] = fmt.Sprintf("x-h%d: v", i)
		if i < MaxProviderHTTPHeaders {
			atCap[i] = ProviderHTTPHeader{Name: fmt.Sprintf("x-h%d", i), Value: "v"}
		}
	}
	tests := []headerCase{
		{name: "absent", in: ""},
		{name: "blank lines only", in: " \n\n\t\n"},
		{
			name: "valid, trimmed, first colon splits, CRLF line endings",
			in:   "  http-referer: https://example.com  \r\n\nx-title:Example App\r\nx-trace: a:b",
			want: []ProviderHTTPHeader{
				{Name: "http-referer", Value: "https://example.com"},
				{Name: "x-title", Value: "Example App"},
				{Name: "x-trace", Value: "a:b"},
			},
		},
		{name: "cap admits exactly the maximum", in: strings.Join(lines[:MaxProviderHTTPHeaders], "\n"), want: atCap},
		{name: "cap refuses one more", in: strings.Join(lines, "\n"), wantErr: "more than 16 headers"},
		{name: "missing colon", in: "x-title Example", wantErr: "line 1: want"},
		{name: "name with space", in: "x title: Example", wantErr: "not an RFC 7230 token"},
		{name: "empty name", in: ": Example", wantErr: "not an RFC 7230 token"},
		{name: "empty value", in: "x-title:", wantErr: "non-empty printable ASCII"},
		{name: "CR injection inside a line", in: "x-title: a\rx-injected: b", wantErr: `"x-title" value`},
		{name: "NUL in value", in: "x-title: a\x00b", wantErr: `"x-title" value`},
		{name: "non-ASCII value", in: "x-title: café", wantErr: `"x-title" value`},
		{name: "duplicate case-insensitive", in: "X-Title: a\nx-title: b", wantErr: `line 2: header "x-title" is declared more than once`},
	}
	for _, reserved := range []string{
		"Authorization", "proxy-authorization", "Cookie", "host", "content-length", "Content-Type",
		"x-api-key", "anthropic-api-key", "x-goog-api-key", "X-Goog-Api-Client", "api-key",
		"transfer-encoding", "connection", "te", "upgrade",
	} {
		tests = append(tests, headerCase{
			name:    "reserved " + reserved,
			in:      "x-ok: 1\n" + reserved + ": v",
			wantErr: fmt.Sprintf("line 2: header %q is reserved", strings.ToLower(reserved)),
		})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProviderHTTPHeaders(tc.in)
			if tc.wantErr != "" {
				var typed *ProviderHTTPHeadersError
				if !errors.As(err, &typed) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want typed error containing %q", err, tc.wantErr)
				}
				if got != nil {
					t.Fatalf("refusal returned headers %v; want none", got)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestProviderHTTPHeadersErrorNeverEchoesValue(t *testing.T) {
	_, err := ParseProviderHTTPHeaders("x-title: secret-ish\x00value")
	if err == nil || strings.Contains(err.Error(), "secret-ish") {
		t.Fatalf("err = %v; want a refusal that does not echo the value", err)
	}
}
