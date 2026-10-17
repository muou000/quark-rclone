package trayui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func panelTestServer(t *testing.T) (*Server, *Manager, *managerHarness) {
	t.Helper()
	h := newManagerHarness(t, MountOptions{Letter: "Q:", RootFID: "r1", RootPath: "/docs"})
	if err := h.mgr.Start(context.Background(), "seed-cookie"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := NewServer(h.mgr, "panel-token-123")
	return s, h.mgr, h
}

func postJSON(t *testing.T, h http.Handler, path, token, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Panel-Token", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result()
}

func TestPanelPageCarriesTokenAndChinese(t *testing.T) {
	s, _, _ := panelTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("panel = %d", resp.StatusCode)
	}
	page := string(body)
	for _, want := range []string{"夸克网盘挂载", "恢复时间线", "回写失败", "重新登录", `"panel-token-123"`} {
		if !strings.Contains(page, want) {
			t.Errorf("panel page missing %q", want)
		}
	}
	if strings.Contains(page, "__PANEL_TOKEN__") {
		t.Error("an unreplaced token placeholder leaked into the page")
	}
	// Deep paths are not routes.
	resp2, err := http.Get(srv.URL + "/elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("GET /elsewhere = %d, want 404", resp2.StatusCode)
	}
}

func TestTokenGuardsMutations(t *testing.T) {
	s, mgr, h := panelTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	for _, token := range []string{"", "wrong-token"} {
		resp := postJSON(t, s.Handler(), "/api/reauth", token, `{"cookie":"good"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("POST with token %q = %d, want 401", token, resp.StatusCode)
		}
	}
	if len(h.persisted) != 0 {
		t.Errorf("persisted = %v, want nothing without the token", h.persisted)
	}
	if mgr.runnerSnapshot() == nil {
		t.Error("the mount must be untouched without the token")
	}
}

func TestValidateFlowAnswersWithoutEchoingCookie(t *testing.T) {
	s, _, _ := panelTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp := postJSON(t, s.Handler(), "/api/validate", "panel-token-123", `{"cookie":"good"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		OK        bool      `json:"ok"`
		Error     string    `json:"error"`
		Dirs      []RootDir `json:"dirs"`
		ListError string    `json:"list_error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("validate response: %v (%s)", err, body)
	}
	if !out.OK || len(out.Dirs) != 1 || out.Dirs[0].FID != "d1" {
		t.Fatalf("validate = %s", body)
	}
	if strings.Contains(string(body), "effective-cookie") {
		t.Error("the effective cookie must never be echoed back to the page")
	}

	// A rejected candidate is a business answer, not a 500.
	resp = postJSON(t, s.Handler(), "/api/validate", "panel-token-123", `{"cookie":"bad"}`)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":false`) || !strings.Contains(string(body), "31001") {
		t.Errorf("rejected validate = %s", body)
	}
}

func TestReauthKeepsOptionsAndPersistsOnlyOnSuccess(t *testing.T) {
	s, _, h := panelTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// The failing reauth: nothing persisted, mount untouched.
	resp := postJSON(t, s.Handler(), "/api/reauth", "panel-token-123", `{"cookie":"bad"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":false`) {
		t.Errorf("failed reauth = %s", body)
	}
	if len(h.persisted) != 0 {
		t.Errorf("persisted = %v, want nothing on failure (AC 3)", h.persisted)
	}

	// The successful reauth: the effective cookie lands, options unchanged.
	resp = postJSON(t, s.Handler(), "/api/reauth", "panel-token-123", `{"cookie":"good"}`)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("reauth = %s", body)
	}
	if len(h.persisted) != 1 || h.persisted[0] != "effective-cookie" {
		t.Errorf("persisted = %v", h.persisted)
	}
	if got := h.optsSeen[len(h.optsSeen)-1]; got.Letter != "Q:" || got.RootFID != "r1" {
		t.Errorf("restart options = %+v, want the untouched profile choices (AC 3)", got)
	}
}

func TestApplyPersistsConfigAndRestarts(t *testing.T) {
	s, _, h := panelTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp := postJSON(t, s.Handler(), "/api/apply", "panel-token-123",
		`{"cookie":"good","letter":"x","root_fid":"r9","root_path":"/r9"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("apply = %s", body)
	}
	if got := h.mgr.optsSnapshot(); got.Letter != "X:" || got.RootFID != "r9" {
		t.Errorf("applied opts = %+v, want X:/r9", got)
	}
	if len(h.savedCfg) == 0 || h.savedCfg[len(h.savedCfg)-1].RootFID != "r9" {
		t.Errorf("savedCfg = %+v", h.savedCfg)
	}
}

func TestLogExportIsRedactedAndDownloadable(t *testing.T) {
	s, mgr, _ := panelTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	mgr.log.Add("keepalive observed %s", "__puus=secret-value")

	resp, err := http.Get(srv.URL + "/api/log")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Disposition") == "" {
		t.Error("the export should download as a file")
	}
	text := string(body)
	if strings.Contains(text, "secret-value") {
		t.Fatalf("the export leaked credential material: %q", text)
	}
	if !strings.Contains(text, "[redacted]") {
		t.Errorf("export should carry the placeholder: %q", text)
	}
}

func TestRecheckIsGuardedAndRestarts(t *testing.T) {
	s, _, h := panelTestServer(t)

	// Guarded like every mutation.
	resp := postJSON(t, s.Handler(), "/api/recheck", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized recheck = %d, want 401", resp.StatusCode)
	}

	// With the token the mount restarts from the stored credential.
	startsBefore := len(h.started)
	resp = postJSON(t, s.Handler(), "/api/recheck", "panel-token-123", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("recheck = %s", body)
	}
	if len(h.started) != startsBefore+1 {
		t.Errorf("starts = %d, want the recheck restart", len(h.started))
	}
}

func TestPanelDataCarriesTheDocument(t *testing.T) {
	s, _, _ := panelTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/panel")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var data PanelData
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatalf("panel data: %v", err)
	}
	if data.Status.Label == "" || data.Caps.Static.List != true || data.Caps.Static.Copy != false {
		t.Fatalf("panel data = %s", body)
	}
	if strings.Contains(string(body), "seed-cookie") || strings.Contains(string(body), "effective-cookie") {
		t.Error("panel data must never carry a credential")
	}
}
