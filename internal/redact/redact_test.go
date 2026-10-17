package redact

import (
	"errors"
	"strings"
	"testing"
)

func TestURLRedactsKnownQueryKeys(t *testing.T) {
	in := "https://cdn.example.com/file?sign=abc123&expires=999&x=1"
	got := URL(in)
	if strings.Contains(got, "abc123") || strings.Contains(got, "999") {
		t.Fatalf("URL() leaked a credential value: %q", got)
	}
	if !strings.Contains(got, "sign=") || !strings.Contains(got, "expires=") {
		t.Fatalf("URL() dropped a parameter name: %q", got)
	}
	if !strings.Contains(got, "x=1") {
		t.Fatalf("URL() redacted a non-credential parameter: %q", got)
	}
}

func TestURLDropsUnknownQueries(t *testing.T) {
	// An unrecognized query could be a new signed parameter, so the whole
	// query is dropped rather than passed through.
	got := URL("https://cdn.example.com/file?foo=bar")
	if strings.Contains(got, "foo=bar") {
		t.Fatalf("unknown query must be dropped, got %q", got)
	}
	if !strings.HasPrefix(got, "https://cdn.example.com/file") {
		t.Fatalf("scheme/host/path must survive, got %q", got)
	}
}

func TestURLKeepsQuerylessURL(t *testing.T) {
	in := "https://cdn.example.com/file"
	if got := URL(in); got != in {
		t.Fatalf("URL() = %q, want unchanged %q", got, in)
	}
}

func TestURLRedactsUnparseable(t *testing.T) {
	if got := URL("://not a url"); got != Placeholder {
		t.Fatalf("URL() = %q, want %q", got, Placeholder)
	}
}

func TestHeaderRedactsCredentials(t *testing.T) {
	in := map[string]string{
		"Cookie":     "__puus=secret",
		"User-Agent": "ua",
	}
	got := Header(in)
	if got["Cookie"] != Placeholder {
		t.Fatalf("Cookie = %q, want %q", got["Cookie"], Placeholder)
	}
	if got["User-Agent"] != "ua" {
		t.Fatalf("User-Agent must survive, got %q", got["User-Agent"])
	}
	if in["Cookie"] != "__puus=secret" {
		t.Fatal("input map must not be mutated")
	}
}

func TestCookieMasksValues(t *testing.T) {
	got := Cookie("__puus=abc; __pus=def; plain")
	want := "__puus=" + Placeholder + "; __pus=" + Placeholder + "; plain"
	if got != want {
		t.Fatalf("Cookie() = %q, want %q", got, want)
	}
}

func TestPathDropsQuery(t *testing.T) {
	got := Path("/file/download?fid=1&sign=x")
	if strings.Contains(got, "sign=x") {
		t.Fatalf("Path() = %q, query must be redacted", got)
	}
	if got != "/file/download?"+Placeholder {
		t.Fatalf("Path() = %q", got)
	}
}

func TestTextRedactsSessionCookiesAndURLs(t *testing.T) {
	in := "failed to fetch https://cdn.example.com/f?sign=SECRETSIGN with __puus=SECRETPUUS and __pus=SECRETPUS"
	got := Text(in)
	if strings.Contains(got, "SECRETSIGN") || strings.Contains(got, "SECRETPUUS") || strings.Contains(got, "SECRETPUS") {
		t.Fatalf("Text() leaked a credential: %q", got)
	}
	if !strings.Contains(got, "cdn.example.com") {
		t.Fatalf("Text() dropped the host: %q", got)
	}
}

func TestTextRedactsMultipleOccurrences(t *testing.T) {
	in := "__puus=one; other; __puus=two"
	got := Text(in)
	if strings.Contains(got, "one") || strings.Contains(got, "two") {
		t.Fatalf("Text() leaked a value: %q", got)
	}
	if got != "__puus="+Placeholder+"; other; __puus="+Placeholder {
		t.Fatalf("Text() = %q", got)
	}
}

func TestTextBoundaryDoesNotMatchPrefixedNames(t *testing.T) {
	got := Text("x__puus=value")
	if !strings.Contains(got, "value") {
		t.Fatalf("x__puus is not the session cookie and must survive: %q", got)
	}
}

func TestErrorRedactsMessage(t *testing.T) {
	err := errors.New("GET https://cdn.example.com/f?sign=abc failed with Cookie: __puus=xyz")
	got := Error(err)
	if strings.Contains(got, "abc") || strings.Contains(got, "xyz") {
		t.Fatalf("Error() leaked a credential: %q", got)
	}
}

func TestErrorNil(t *testing.T) {
	if got := Error(nil); got != "" {
		t.Fatalf("Error(nil) = %q, want empty", got)
	}
}
