package preflight

import "strings"

// versionAtLeast compares dotted numeric versions: actual >= required. It is
// shared by the WinFsp gate on Windows and its tests everywhere else.
func versionAtLeast(actual, required string) bool {
	a, aok := parseVersion(actual)
	r, rok := parseVersion(required)
	if !aok || !rok {
		return false
	}
	for i := 0; i < 2; i++ {
		if a[i] != r[i] {
			return a[i] > r[i]
		}
	}
	return true
}

// parseVersion reads the first dotted numeric fields of a version string,
// e.g. "2.1.25312" or "v2.0".
func parseVersion(s string) ([2]int, bool) {
	var out [2]int
	s = strings.TrimPrefix(s, "v")
	fields := strings.SplitN(s, ".", 3)
	if len(fields) < 2 {
		return out, false
	}
	for i := 0; i < 2; i++ {
		n := 0
		for _, ch := range fields[i] {
			if ch < '0' || ch > '9' {
				return out, false
			}
			n = n*10 + int(ch-'0')
		}
		out[i] = n
	}
	return out, true
}
