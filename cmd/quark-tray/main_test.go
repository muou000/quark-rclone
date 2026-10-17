package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPanelDiscoveryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	url := "http://127.0.0.1:17390/?t=abc"
	if err := writePanelDiscovery(dir, url); err != nil {
		t.Fatalf("writePanelDiscovery: %v", err)
	}
	if got := readPanelDiscovery(dir); got != url {
		t.Fatalf("readPanelDiscovery = %q, want %q", got, url)
	}
	removePanelDiscovery(dir)
	if got := readPanelDiscovery(dir); got != "" {
		t.Fatalf("after remove readPanelDiscovery = %q, want empty", got)
	}
}

func TestReadPanelDiscoveryRejectsNonLoopbackAndGarbage(t *testing.T) {
	dir := t.TempDir()

	// Not a loopback address.
	if err := writePanelDiscovery(dir, "http://10.0.0.5:80/?t=abc"); err != nil {
		t.Fatalf("writePanelDiscovery: %v", err)
	}
	if got := readPanelDiscovery(dir); got != "" {
		t.Fatalf("non-loopback URL accepted: %q", got)
	}

	// Malformed JSON.
	malformed := filepath.Join(dir, panelDiscoveryName)
	if err := os.WriteFile(malformed, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed: %v", err)
	}
	if got := readPanelDiscovery(dir); got != "" {
		t.Fatalf("malformed file accepted: %q", got)
	}

	// Missing file.
	if got := readPanelDiscovery(filepath.Join(dir, "nowhere")); got != "" {
		t.Fatalf("missing file accepted: %q", got)
	}
}

func TestProbePanelAcceptsAnyHTTPAnswer(t *testing.T) {
	// A 401 still proves a live panel: the token guard answers, the tray runs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing or invalid panel token", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	if !probePanel(srv.URL) {
		t.Error("probePanel = false for a live 401 panel, want true")
	}
}

func TestProbePanelRejectsDeadURL(t *testing.T) {
	// Reserve a port and shut the listener down: nothing answers there.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if probePanel(url) {
		t.Error("probePanel = true for a dead URL, want false")
	}
}
