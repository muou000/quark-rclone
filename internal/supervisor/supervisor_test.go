package supervisor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muou000/quark-rclone/internal/diag"
)

// listenLocal reserves an ephemeral loopback port.
func listenLocal() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

// freePort reserves an ephemeral port and releases it, approximating what a
// fixed-port configuration would name. The window is racy but harmless: the
// endpoint's own Listen failure reports it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := listenLocal()
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// startSession builds a session against a minimal fake provider.
func startSession(t *testing.T, mutate func(*Config)) *Session {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config":
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{}}`))
		case "/file/sort":
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{"list":[{"fid":"f1","file_name":"a.txt","file":true,"size":3}]},"metadata":{"_total":1}}`))
		case "/file/download":
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":[{"download_url":"http://cdn.example/obj"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.Close)

	cfg := Config{
		Port:       freePort(t),
		Cookie:     "__puus=session-seed",
		APIBaseURL: fake.URL,
		HTTPClient: fake.Client(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := Start(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)
	return s
}

// TestTempConfigHasOnlyLoopbackURLRootAndCookie pins the issue-18 contract
// for the temporary configuration: it contains the loopback URL (with the DAV
// root path), the vendor shape, and the Cookie header — and nothing else that
// could carry a second credential.
func TestTempConfigHasOnlyLoopbackURLRootAndCookie(t *testing.T) {
	s := startSession(t, nil)
	body, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"type = webdav",
		"vendor = other",
		"url = http://127.0.0.1:",
		"headers = Cookie,__puus=session-seed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "pass") || strings.Contains(text, "user ") {
		t.Fatalf("config carries a credential field it must not:\n%s", text)
	}
	// The file sits inside a dedicated temp directory, not the shared temp
	// root, so Stop's cleanup cannot sweep unrelated files.
	if filepath.Base(s.ConfigPath()) != "rclone.conf" {
		t.Fatalf("config name = %s", filepath.Base(s.ConfigPath()))
	}
}

// TestStopDeletesTempConfig checks the cleanup half of the contract: after
// the session ends the temporary file is gone, and Stop stays safe when the
// supervisor stops it twice.
func TestStopDeletesTempConfig(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{}}`))
	}))
	defer fake.Close()

	s, err := Start(t.Context(), Config{
		Port:       freePort(t),
		Cookie:     "__puus=x",
		APIBaseURL: fake.URL,
		HTTPClient: fake.Client(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	path := s.ConfigPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config missing before stop: %v", err)
	}
	s.Stop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config survived Stop: %v", err)
	}
	s.Stop() // must not panic or hang
}

// TestStartRejectsUnusableConfig checks the session refuses to half-start:
// a fixed port is required (ADR 0009 predictability), the cookie is required,
// and a cookie rclone's header format cannot carry fails loudly.
func TestStartRejectsUnusableConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"zero port", Config{Cookie: "__puus=x"}},
		{"empty cookie", Config{Port: freePort(t)}},
		{"comma cookie", Config{Port: freePort(t), Cookie: "__puus=a,b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Start(t.Context(), tc.cfg); err == nil {
				t.Fatal("Start accepted an unusable config")
			}
		})
	}
}

// TestSessionServesCapabilitySnapshot checks the recorder plumbs through to
// the endpoint (issue #22): /diagnostics sits behind the cookie gate and
// answers with the shared snapshot.
func TestSessionServesCapabilitySnapshot(t *testing.T) {
	rec := diag.NewRecorder()
	rec.RunProbe(t.Context(), diag.ProbeAuth, func(context.Context) error { return nil })
	rec.RecordWritebackFailure("/stuck.bin", errors.New("boom"))
	s := startSession(t, func(c *Config) { c.Recorder = rec })
	base := strings.TrimSuffix(s.URL(), "/dav")

	req, _ := http.NewRequest("GET", base+"/diagnostics", nil)
	req.Header.Set("Cookie", "__puus=session-seed")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("diagnostics = %d", resp.StatusCode)
	}
	for _, want := range []string{`"name": "auth"`, `stuck.bin`, `"copy": false`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("diagnostics missing %s: %s", want, body)
		}
	}
	if strings.Contains(string(body), "__puus=session-seed") {
		t.Fatal("diagnostics leaked the cookie")
	}

	// Without the cookie it is closed like the DAV surface.
	resp, err = http.Get(base + "/diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("diagnostics without cookie = %d, want 401", resp.StatusCode)
	}
}

// TestSessionServesDriveThroughLoopback checks the assembled session end to
// end: healthz is open, the DAV surface demands the pinned cookie, and a
// proper PROPFIND resolves the drive listing through the live store.
func TestSessionServesDriveThroughLoopback(t *testing.T) {
	s := startSession(t, nil)
	base := strings.TrimSuffix(s.URL(), "/dav") // healthz sits beside the DAV root
	addr := s.URL()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(got), `"ok"`) {
		t.Fatalf("healthz = %d %q", resp.StatusCode, got)
	}

	// Without the cookie the DAV surface is closed.
	resp, err = http.Get(addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dav without cookie = %d, want 401", resp.StatusCode)
	}

	// With it, the listing resolves through the fake provider.
	req, _ := http.NewRequest("PROPFIND", addr+"/", nil)
	req.Header.Set("Cookie", "__puus=session-seed")
	req.Header.Set("Depth", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus || !strings.Contains(string(body), "a.txt") {
		t.Fatalf("propfind = %d %q", resp.StatusCode, body)
	}
}
