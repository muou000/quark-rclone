// Package cookieutil operates on raw Cookie header strings.
//
// The credential the user imports is a browser-exported Cookie header value,
// and it is kept in exactly that shape: unknown cookies and original values are
// preserved verbatim (ADR 0003), because Quark parses the header as-is and any
// normalisation risks dropping a field the session needs.
//
// Both the Quark HTTP client and the session store manipulate this string, so
// the operations live in one place rather than being re-derived in each.
package cookieutil

import (
	"fmt"
	"strings"
)

// Set replaces a named cookie in a Cookie header value, appending it when
// absent. Cookie order is preserved.
func Set(header, name, value string) string {
	parts := strings.Split(header, ";")
	out := make([]string, 0, len(parts)+1)
	replaced := false
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		if strings.EqualFold(NameOf(trimmed), name) {
			out = append(out, name+"="+value)
			replaced = true
			continue
		}
		out = append(out, trimmed)
	}
	if !replaced {
		out = append(out, name+"="+value)
	}
	return strings.Join(out, "; ")
}

// Get returns the value of a named cookie, or "" when absent.
func Get(header, name string) string {
	for _, p := range strings.Split(header, ";") {
		trimmed := strings.TrimSpace(p)
		i := strings.IndexByte(trimmed, '=')
		if i < 0 || !strings.EqualFold(strings.TrimSpace(trimmed[:i]), name) {
			continue
		}
		return strings.TrimSpace(trimmed[i+1:])
	}
	return ""
}

// Del removes a named cookie from a Cookie header value.
func Del(header, name string) string {
	parts := strings.Split(header, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		if strings.EqualFold(NameOf(trimmed), name) {
			continue
		}
		out = append(out, trimmed)
	}
	return strings.Join(out, "; ")
}

// NameOf returns the cookie name from a "name=value" pair.
func NameOf(pair string) string {
	if i := strings.IndexByte(pair, '='); i >= 0 {
		return strings.TrimSpace(pair[:i])
	}
	return strings.TrimSpace(pair)
}

// Validate performs the basic shape check a pasted Cookie string must pass
// before it is worth sending to the provider (ADR 0003).
//
// It is deliberately not a filter: unknown cookies are preserved and only the
// "name=value" shape is enforced. Leading and trailing whitespace is trimmed
// here so the caller stores a canonicalised string.
func Validate(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("cookie is empty")
	}
	for _, p := range strings.Split(trimmed, ";") {
		part := strings.TrimSpace(p)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "=") {
			return "", fmt.Errorf("cookie segment %q is not name=value", part)
		}
		if NameOf(part) == "" {
			return "", fmt.Errorf("cookie segment %q has an empty name", part)
		}
	}
	return trimmed, nil
}
