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

func TestShellHooksDriveAutostartAndUpgrade(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	if err := h.mgr.Start(context.Background(), "seed-cookie"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var autostartCalls []bool
	var upgradeCalls int
	s := NewServer(h.mgr, "panel-token-123").WithHooks(Hooks{
		AutostartEnabled: func() bool { return len(autostartCalls) > 0 && autostartCalls[len(autostartCalls)-1] },
		Autostart: func(enabled bool) error {
			autostartCalls = append(autostartCalls, enabled)
			return nil
		},
		StagedVersion: func() string { return "v9.9" },
		Upgrade: func(context.Context) error {
			upgradeCalls++
			return nil
		},
	})

	// The panel document reflects the hooks.
	resp, err := http.Get(httptest.NewServer(s.Handler()).URL + "/api/panel")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"upgrade_version":"v9.9"`) {
		t.Errorf("panel data missing the staged version: %s", body)
	}

	// Autostart toggle goes through the hook, token-guarded.
	resp = postJSON(t, s.Handler(), "/api/autostart", "", `{"enabled":true}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized autostart = %d, want 401", resp.StatusCode)
	}
	resp = postJSON(t, s.Handler(), "/api/autostart", "panel-token-123", `{"enabled":true}`)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("autostart = %s", body)
	}
	if len(autostartCalls) != 1 || !autostartCalls[0] {
		t.Errorf("autostart hook calls = %v", autostartCalls)
	}

	// Upgrade goes through the hook, and the panel document then carries the
	// enabled autostart state.
	resp = postJSON(t, s.Handler(), "/api/upgrade", "panel-token-123", "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("upgrade = %s", body)
	}
	if upgradeCalls != 1 {
		t.Errorf("upgrade calls = %d", upgradeCalls)
	}
	resp, err = http.Get(httptest.NewServer(s.Handler()).URL + "/api/panel")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"autostart":true`) {
		t.Errorf("panel data missing autostart state: %s", body)
	}
}

func TestNilHooksAnswerHonestly(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	s := NewServer(h.mgr, "panel-token-123")

	for _, path := range []string{"/api/autostart", "/api/upgrade"} {
		resp := postJSON(t, s.Handler(), path, "panel-token-123", "{}")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), `"ok":false`) {
			t.Errorf("%s without hooks = %s, want an honest refusal", path, body)
		}
	}
}

func TestPreflightEndpointCarriesHookReport(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	calls := 0
	s := NewServer(h.mgr, "panel-token-123").WithHooks(Hooks{
		Preflight: func(ctx context.Context) PreflightView {
			calls++
			return PreflightView{
				OK:     false,
				Status: "winfsp_required",
				Findings: []PreflightFindingView{
					{Check: "platform", OK: true, Detail: "平台 windows/amd64 在首版验证范围内。"},
					{Check: "winfsp", OK: false, Detail: "未检测到 WinFsp：请安装 WinFsp x64 2.0 或更高版本。"},
				},
			}
		},
	})

	// A read-only GET like /api/panel: no token guard, loopback only.
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/preflight")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var got PreflightView
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("preflight response: %v (%s)", err, body)
	}
	if got.OK || got.Status != "winfsp_required" || len(got.Findings) != 2 || got.Findings[1].OK {
		t.Fatalf("preflight = %s", body)
	}
	if calls != 1 {
		t.Errorf("preflight hook calls = %d, want 1", calls)
	}

	// An all-green report is what unlocks the wizard on the page.
	s2 := NewServer(h.mgr, "panel-token-123").WithHooks(Hooks{
		Preflight: func(ctx context.Context) PreflightView {
			return PreflightView{OK: true, Status: "ok", Findings: []PreflightFindingView{
				{Check: "platform", OK: true, Detail: "通过"},
			}}
		},
	})
	srv2 := httptest.NewServer(s2.Handler())
	t.Cleanup(srv2.Close)
	resp, err = http.Get(srv2.URL + "/api/preflight")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("green preflight = %s", body)
	}
}

func TestPreflightWithoutHookAnswersHonestly(t *testing.T) {
	h := newManagerHarness(t, MountOptions{})
	s := NewServer(h.mgr, "panel-token-123")

	resp, err := http.Get(httptest.NewServer(s.Handler()).URL + "/api/preflight")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":false`) || !strings.Contains(string(body), "unavailable") {
		t.Errorf("preflight without hook = %s, want an honest refusal", body)
	}
}
