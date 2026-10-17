// Package redact removes credentials from text before it reaches a log, an
// error message or a diagnostic export.
//
// Quark signs download URLs with query parameters that encode the account, and
// the session cookie is itself a bearer credential. Either one in a log file is
// enough to take over the account, and logs are the artefact most likely to be
// pasted into a bug report. Redaction is therefore applied at the boundary
// rather than trusted to each call site remembering.
package redact

import (
	"net/url"
	"strings"
)

// Placeholder replaces a redacted value. It is deliberately recognisable so a
// reader can tell that something was removed rather than the field being empty.
const Placeholder = "[redacted]"

// QueryKeys are the query parameter names whose values are credentials.
//
// Quark's download URLs carry the signature and the account binding in the
// query string. The names are matched case-insensitively because a CDN is free
// to capitalise them.
var QueryKeys = []string{
	"sign", "signature", "token", "access_token", "auth", "auth_key",
	"expires", "expire", "ssig", "sid", "_",
}

// Headers are the header names whose values are credentials.
var Headers = []string{"cookie", "set-cookie", "authorization"}

// URL removes credential-bearing query parameters from a URL string.
//
// The scheme, host and path survive, so the log still says which object was
// involved. A URL that cannot be parsed is redacted wholesale: a malformed
// string is more likely to be an attempt to smuggle something past the filter
// than a legitimate address worth preserving.
func URL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Placeholder
	}
	if u.RawQuery == "" {
		return raw
	}
	q := u.Query()
	changed := false
	for _, key := range QueryKeys {
		for actual := range q {
			if strings.EqualFold(actual, key) {
				q.Set(actual, Placeholder)
				changed = true
			}
		}
	}
	if !changed {
		// Nothing matched, so the query may still be sensitive. Quark's
		// parameter names are not documented and have changed before, so the
		// safe default is to drop the whole query rather than pass it through.
		u.RawQuery = ""
		return u.String()
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Header returns a copy of a header map with credential values replaced.
func Header(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if isSensitiveHeader(k) {
			out[k] = Placeholder
			continue
		}
		out[k] = v
	}
	return out
}

// Cookie masks the value of every cookie in a Cookie header string, keeping
// the names so a log still shows which cookies were present.
func Cookie(raw string) string {
	if raw == "" {
		return raw
	}
	parts := strings.Split(raw, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		if i := strings.IndexByte(trimmed, '='); i >= 0 {
			out = append(out, strings.TrimSpace(trimmed[:i])+"="+Placeholder)
			continue
		}
		out = append(out, trimmed)
	}
	return strings.Join(out, "; ")
}

// isSensitiveHeader reports whether a header name carries a credential.
func isSensitiveHeader(name string) bool {
	for _, h := range Headers {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

// Path sanitises a request path, dropping any query string.
//
// Paths themselves are not secret, but a path carrying a query is how a signed
// link ends up in a log.
func Path(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i] + "?" + Placeholder
	}
	return p
}

// Error sanitises an error message.
//
// A provider error frequently quotes the request that failed, which means the
// signed URL and sometimes the cookie end up inside the message text. Scanning
// the rendered message is the only way to catch those, because the error was
// built before any of this ran.
func Error(err error) string {
	if err == nil {
		return ""
	}
	return Text(err.Error())
}

// Text sanitises arbitrary text, rewriting any URL and any Cookie header value
// it contains.
func Text(s string) string {
	if s == "" {
		return s
	}
	out := redactURLs(s)
	out = redactCookiePairs(out)
	return out
}

// redactURLs rewrites every http(s) URL in the text.
func redactURLs(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		rel := strings.Index(s[i:], "http://")
		if rel < 0 {
			rel = strings.Index(s[i:], "https://")
		}
		if rel < 0 {
			b.WriteString(s[i:])
			return b.String()
		}
		start := i + rel
		// A URL runs until whitespace or a delimiter that cannot appear in one.
		end := start
		for end < len(s) && !isURLTerminator(s[end]) {
			end++
		}
		b.WriteString(s[i:start])
		b.WriteString(URL(s[start:end]))
		i = end
	}
	return b.String()
}

// isURLTerminator reports whether a byte cannot appear inside a URL.
func isURLTerminator(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '"', '\'', '<', '>', ')', ']', '}', ',':
		return true
	}
	return false
}

// redactCookiePairs rewrites "name=value" pairs whose name is a known session
// cookie, which is how a Cookie header shows up inside an error message.
func redactCookiePairs(s string) string {
	for _, name := range []string{"__puus", "__pus", "cookie", "set-cookie"} {
		s = redactNamedPair(s, name)
	}
	return s
}

// redactNamedPair replaces the value of every occurrence of "name=value",
// matched case-insensitively.
func redactNamedPair(s, name string) string {
	lower := strings.ToLower(s)
	needle := strings.ToLower(name) + "="
	idx := 0
	for {
		rel := strings.Index(lower[idx:], needle)
		if rel < 0 {
			return s
		}
		at := idx + rel
		// The name must start at a boundary, otherwise "xcookie=" would match.
		if at > 0 {
			prev := s[at-1]
			if prev != ' ' && prev != ';' && prev != '"' && prev != '\'' && prev != ':' {
				idx = at + len(needle)
				continue
			}
		}
		valStart := at + len(needle)
		valEnd := valStart
		for valEnd < len(s) && s[valEnd] != ';' && s[valEnd] != ' ' &&
			s[valEnd] != '"' && s[valEnd] != '\'' && s[valEnd] != ',' &&
			s[valEnd] != '\n' && s[valEnd] != '\r' {
			valEnd++
		}
		if valEnd == valStart {
			idx = at + len(needle)
			continue
		}
		s = s[:valStart] + Placeholder + s[valEnd:]
		lower = strings.ToLower(s)
		idx = valStart + len(Placeholder)
	}
}
