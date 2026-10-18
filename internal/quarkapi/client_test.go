package quarkapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// fakeSource is a CredentialSource double. It applies observed refresh fields
// onto its current value — the same merge semantics the production session
// store uses — and records every merge so a test can assert what the client
// forwarded.
type fakeSource struct {
	mu     sync.Mutex
	cookie string
	merges []map[string]string
}

func (f *fakeSource) CookieSnapshot() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cookie
}

func (f *fakeSource) MergeRefresh(fields map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges = append(f.merges, fields)
	f.cookie = applyRefresh(f.cookie, fields)
}

// snapshot returns the current cookie and the merge count under one lock.
func (f *fakeSource) snapshot() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cookie, len(f.merges)
}

// newFakeAPI builds a client pointed at a fake Quark control plane.
func newFakeAPI(t *testing.T, handler http.HandlerFunc) (*Client, *fakeSource) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	src := &fakeSource{cookie: "__puus=abc; __pus=def"}
	c := New(src, Config{BaseURL: srv.URL}, srv.Client())
	return c, src
}

// writeJSON writes a JSON body with the content type set.
func writeJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// TestClassify pins the provider-to-category mapping. Every entry is a shape
// the real provider has been observed to produce, and the categories are what
// the rest of the program switches on.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		in   *APIError
		want quarkerr.Category
	}{
		{"http 401", &APIError{HTTPStatus: 401}, quarkerr.CatAuthRequired},
		{"http 403", &APIError{HTTPStatus: 403}, quarkerr.CatPermissionDenied},
		{"http 404", &APIError{HTTPStatus: 404}, quarkerr.CatObjectNotFound},
		{"http 409", &APIError{HTTPStatus: 409}, quarkerr.CatTargetConflict},
		{"http 423", &APIError{HTTPStatus: 423}, quarkerr.CatLocked},
		{"http 429", &APIError{HTTPStatus: 429}, quarkerr.CatRateLimited},
		{"http 507", &APIError{HTTPStatus: 507}, quarkerr.CatQuotaExceeded},
		{"code 31001 login expired", &APIError{HTTPStatus: 200, Status: 401, Code: 31001, Message: "登录已过期"}, quarkerr.CatAuthRequired},
		{"code 31002 need login", &APIError{HTTPStatus: 200, Status: 401, Code: 31002}, quarkerr.CatAuthRequired},
		{"envelope 401", &APIError{HTTPStatus: 200, Status: 401}, quarkerr.CatAuthRequired},
		{"envelope 403", &APIError{HTTPStatus: 200, Status: 403}, quarkerr.CatPermissionDenied},
		{"envelope 404", &APIError{HTTPStatus: 200, Status: 404}, quarkerr.CatObjectNotFound},
		{"envelope 429", &APIError{HTTPStatus: 200, Status: 429}, quarkerr.CatRateLimited},
		{"envelope 500", &APIError{HTTPStatus: 200, Status: 500}, quarkerr.CatUpstreamTransient},
		{"envelope 400", &APIError{HTTPStatus: 200, Status: 400}, quarkerr.CatInvalidRequest},
		{"unrecognised", &APIError{HTTPStatus: 200, Status: 200, Code: 4}, quarkerr.CatUnknown},
		{"nil", nil, quarkerr.CatUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.in); got != tc.want {
				t.Fatalf("Classify(%+v) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestCategoriseOmitsProviderMessage pins that the rendered error carries the
// structured status triple but never the provider's free-form message, which
// can echo request material such as a signed URL fragment.
func TestCategoriseOmitsProviderMessage(t *testing.T) {
	apiErr := &APIError{
		HTTPStatus: 200,
		Status:     500,
		Code:       500,
		Message:    "internal error; requestId=abc; https://oss.example.com/obj?Signature=LEAK",
	}
	err := categorise("list", apiErr)
	got := err.Error()
	if strings.Contains(got, apiErr.Message) {
		t.Fatalf("categorise leaked provider message: %s", got)
	}
	if !strings.Contains(got, "http=200 status=500 code=500") {
		t.Fatalf("categorise lost the structured detail: %s", got)
	}
	// The raw rejection stays reachable through the cause for callers that
	// need the provider's numbers.
	var unwrapped *APIError
	if !errors.As(err, &unwrapped) || unwrapped != apiErr {
		t.Fatalf("categorise dropped the APIError cause: %v", err)
	}
}

// TestNonEnvelopeErrorOmitsBody pins the same rule for a body that is not the
// provider envelope at all — the shape an intermediary such as a captive
// portal produces, and the most likely place for echoed request material.
func TestNonEnvelopeErrorOmitsBody(t *testing.T) {
	body := "<html>gateway error; cookie __puus=LEAK</html>"
	err := nonEnvelopeError("list", http.StatusBadGateway, []byte(body))
	got := err.Error()
	if strings.Contains(got, body) || strings.Contains(got, "__puus") {
		t.Fatalf("nonEnvelopeError leaked body text: %s", got)
	}
	if !strings.Contains(got, "http=502") {
		t.Fatalf("nonEnvelopeError lost the status detail: %s", got)
	}
	if quarkerr.CategoryOf(err) != quarkerr.CatUpstreamTransient {
		t.Fatalf("category = %s, want upstream_transient", quarkerr.CategoryOf(err))
	}
}

// TestValidateCandidateProbesConfigThenRoot checks validation is exactly two
// probes: /config first, then the first page of the root listing. The root FID
// is a protocol constant, so no resolving call is needed before it.
func TestValidateCandidateProbesConfigThenRoot(t *testing.T) {
	var paths []string
	var sortQuery map[string][]string
	c, src := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/config":
			writeJSON(t, w, map[string]any{"status": 200, "code": 0, "data": map[string]any{}})
		case "/file/sort":
			sortQuery = r.URL.Query()
			writeJSON(t, w, map[string]any{
				"status": 200, "code": 0,
				"data":     map[string]any{"list": []any{}},
				"metadata": map[string]any{"_total": 0},
			})
		default:
			http.NotFound(w, r)
		}
	})

	effective, err := c.ValidateCandidate(t.Context(), "__puus=candidate")
	if err != nil {
		t.Fatalf("ValidateCandidate: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/config" || paths[1] != "/file/sort" {
		t.Fatalf("probe order = %v, want [/config /file/sort]", paths)
	}
	if got := sortQuery["pdir_fid"]; len(got) != 1 || got[0] != RootFID {
		t.Fatalf("root listing pdir_fid = %v, want %s", got, RootFID)
	}
	if effective != "__puus=candidate" {
		t.Fatalf("effective = %q, want the input unchanged when no refresh is observed", effective)
	}
	if cookie, merges := src.snapshot(); merges != 0 || cookie != "__puus=abc; __pus=def" {
		t.Fatalf("active credential was disturbed: cookie=%q merges=%d", cookie, merges)
	}
}

func TestValidateCandidateRejectsEmptyCookie(t *testing.T) {
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may be made for an empty cookie")
	})
	if _, err := c.ValidateCandidate(t.Context(), "  "); err == nil {
		t.Fatal("ValidateCandidate should reject an empty cookie")
	}
}

// TestValidateCandidateAppliesRefreshObservedDuringProbes checks refresh
// fields seen while validating are layered into the effective cookie that the
// caller then activates, so the credential starts from the freshest session
// the provider issued during validation.
func TestValidateCandidateAppliesRefreshObservedDuringProbes(t *testing.T) {
	c, src := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config":
			http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "fresh-puus"})
			writeJSON(t, w, map[string]any{"status": 200, "code": 0, "data": map[string]any{}})
		case "/file/sort":
			http.SetCookie(w, &http.Cookie{Name: "__pus", Value: "fresh-pus"})
			writeJSON(t, w, map[string]any{
				"status": 200, "code": 0,
				"data":     map[string]any{"list": []any{}},
				"metadata": map[string]any{"_total": 0},
			})
		default:
			http.NotFound(w, r)
		}
	})

	effective, err := c.ValidateCandidate(t.Context(), "__puus=candidate; keep=1")
	if err != nil {
		t.Fatalf("ValidateCandidate: %v", err)
	}
	for _, want := range []string{"__puus=fresh-puus", "__pus=fresh-pus", "keep=1"} {
		if !strings.Contains(effective, want) {
			t.Errorf("effective cookie %q missing %q", effective, want)
		}
	}
	if _, merges := src.snapshot(); merges != 0 {
		t.Fatalf("merges = %d, want 0: probes must not touch the active credential", merges)
	}
}

// TestValidateCandidateFailureLeavesActiveCredentialAlone is the ADR 0003
// contract: a rejected candidate preserves the current credential untouched.
func TestValidateCandidateFailureLeavesActiveCredentialAlone(t *testing.T) {
	c, src := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"status": 401, "code": 31001, "message": "登录已过期"})
	})

	if _, err := c.ValidateCandidate(t.Context(), "__puus=bad"); err == nil {
		t.Fatal("ValidateCandidate should fail")
	}
	if cookie, merges := src.snapshot(); merges != 0 || cookie != "__puus=abc; __pus=def" {
		t.Fatalf("active credential was disturbed: cookie=%q merges=%d", cookie, merges)
	}
}

func TestValidateCandidateMapsAuthRejection(t *testing.T) {
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"status": 401, "code": 31001, "message": "登录已过期"})
	})
	_, err := c.ValidateCandidate(t.Context(), "__puus=stale")
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("err = %v, want ErrNotAuthenticated", err)
	}
}

// TestValidateCandidateRejectsWhenRootListingRefused covers the cookie that
// passes /config but cannot list the drive: it must not be activated, and the
// failure is a permission refusal rather than an auth failure, so the
// supervisor does not tear down the session over it.
func TestValidateCandidateRejectsWhenRootListingRefused(t *testing.T) {
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config":
			writeJSON(t, w, map[string]any{"status": 200, "code": 0, "data": map[string]any{}})
		case "/file/sort":
			writeJSON(t, w, map[string]any{"status": 403, "code": 40001, "message": "无权限"})
		default:
			http.NotFound(w, r)
		}
	})
	_, err := c.ValidateCandidate(t.Context(), "__puus=scoped")
	if !quarkerr.IsCategory(err, quarkerr.CatPermissionDenied) {
		t.Fatalf("category = %s, want permission_denied", quarkerr.CategoryOf(err))
	}
	if errors.Is(err, ErrNotAuthenticated) {
		t.Fatal("a permission refusal is not an authentication failure")
	}
}

// TestRefreshSessionCookieStripsPuusButKeepsTheRest pins the reissue trick:
// the server only reissues __puus when the request omits it, so the probe is
// sent without that one field while every other cookie rides along.
func TestRefreshSessionCookieStripsPuusButKeepsTheRest(t *testing.T) {
	var probeCookie string
	c, src := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		probeCookie = r.Header.Get("Cookie")
		http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "reissued"})
		writeJSON(t, w, map[string]any{"status": 200, "code": 0, "data": map[string]any{}})
	})

	if err := c.RefreshSessionCookie(t.Context()); err != nil {
		t.Fatalf("RefreshSessionCookie: %v", err)
	}
	if strings.Contains(probeCookie, "__puus=") {
		t.Errorf("probe cookie %q must not carry __puus", probeCookie)
	}
	if !strings.Contains(probeCookie, "__pus=def") {
		t.Errorf("probe cookie %q must keep every other field", probeCookie)
	}
	cookie, merges := src.snapshot()
	if merges != 1 || !strings.Contains(cookie, "__puus=reissued") {
		t.Fatalf("reissue was not merged: cookie=%q merges=%d", cookie, merges)
	}
	// The rest of the credential survives the merge.
	if !strings.Contains(cookie, "__pus=def") {
		t.Errorf("merge lost unrelated fields: %q", cookie)
	}
}

// TestRefreshSessionCookieSucceedsWithoutReissue covers a server that answers
// but does not rotate: the session is still proven alive.
func TestRefreshSessionCookieSucceedsWithoutReissue(t *testing.T) {
	c, src := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"status": 200, "code": 0, "data": map[string]any{}})
	})
	if err := c.RefreshSessionCookie(t.Context()); err != nil {
		t.Fatalf("RefreshSessionCookie: %v", err)
	}
	if _, merges := src.snapshot(); merges != 0 {
		t.Fatalf("merges = %d, want 0", merges)
	}
}

func TestRefreshSessionCookieFailureLeavesStoreUntouched(t *testing.T) {
	c, src := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"status": 401, "code": 31001, "message": "登录已过期"})
	})
	if err := c.RefreshSessionCookie(t.Context()); err == nil {
		t.Fatal("RefreshSessionCookie should fail on a rejection")
	}
	if cookie, merges := src.snapshot(); merges != 0 || cookie != "__puus=abc; __pus=def" {
		t.Fatalf("failed refresh disturbed the store: cookie=%q merges=%d", cookie, merges)
	}
}

// TestRequestsCarryProtocolIdentity checks every request carries the query
// pair and headers the provider expects, because a missing one is answered
// with a generic rejection that looks like an auth failure.
func TestRequestsCarryProtocolIdentity(t *testing.T) {
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("pr") != "ucpro" || q.Get("fr") != "pc" {
			t.Errorf("query pr/fr = %q/%q, want ucpro/pc", q.Get("pr"), q.Get("fr"))
		}
		for _, h := range []string{"Cookie", "Referer", "User-Agent", "Origin", "Accept"} {
			if r.Header.Get(h) == "" {
				t.Errorf("header %s is missing", h)
			}
		}
		writeJSON(t, w, map[string]any{"status": 200, "code": 0, "data": map[string]any{}})
	})
	if err := c.doQuery(t.Context(), EndpointConfig, http.MethodGet, nil, nil, nil); err != nil {
		t.Fatalf("doQuery: %v", err)
	}
}

// TestResponseRefreshMergesIntoSource checks an ordinary call forwards
// observed refresh fields to the credential source, which is how a passive
// rotation reaches the store without any caller knowing about it.
func TestResponseRefreshMergesIntoSource(t *testing.T) {
	c, src := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "rotated"})
		writeJSON(t, w, map[string]any{"status": 200, "code": 0, "data": map[string]any{}})
	})
	if err := c.do(t.Context(), EndpointConfig, http.MethodGet, nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	cookie, merges := src.snapshot()
	if merges != 1 {
		t.Fatalf("merges = %d, want 1", merges)
	}
	if !strings.Contains(cookie, "__puus=rotated") || !strings.Contains(cookie, "__pus=def") {
		t.Fatalf("merge result = %q", cookie)
	}
}

// TestNilSourcePanics pins the wiring contract: a client without a credential
// source is a programming error, not a runtime condition.
func TestNilSourcePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with a nil source should panic")
		}
	}()
	New(nil, Config{}, nil)
}

// TestDownloadURLSnapshotsCookieForSignature checks the URL is paired with
// the cookie it was minted against, even when the same response rotates the
// session: fetching it later with the rotated cookie would be a 403.
func TestDownloadURLSnapshotsCookieForSignature(t *testing.T) {
	var downloadCookie string
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file/download":
			downloadCookie = r.Header.Get("Cookie")
			http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "rotated"})
			writeJSON(t, w, map[string]any{
				"status": 200, "code": 0,
				"data": []map[string]string{{"download_url": "https://cdn.example.com/abc"}},
			})
		default:
			http.NotFound(w, r)
		}
	})

	res, err := c.DownloadURL(t.Context(), "fid-1")
	if err != nil {
		t.Fatalf("DownloadURL: %v", err)
	}
	if downloadCookie != "__puus=abc; __pus=def" {
		t.Fatalf("download request cookie = %q, want the pre-rotation snapshot", downloadCookie)
	}
	if got := res.Header["Cookie"]; got != downloadCookie {
		t.Fatalf("result header cookie = %q, want %q (the signed snapshot)", got, downloadCookie)
	}
	if res.URL != "https://cdn.example.com/abc" {
		t.Fatalf("url = %q", res.URL)
	}
}

func TestDownloadURLErrorsWhenNoURL(t *testing.T) {
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"status": 200, "code": 0, "data": []map[string]string{{"download_url": ""}}})
	})
	if _, err := c.DownloadURL(t.Context(), "fid-1"); !errors.Is(err, ErrNoDownloadURL) {
		t.Fatalf("DownloadURL error = %v, want ErrNoDownloadURL", err)
	}
}

func TestListFollowsPages(t *testing.T) {
	calls := 0
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file/sort" {
			http.NotFound(w, r)
			return
		}
		calls++
		if r.URL.Query().Get("_page") == "1" {
			list := make([]map[string]any, PageSize)
			for i := range list {
				list[i] = map[string]any{"fid": fmt.Sprintf("fid-%03d", i), "file_name": fmt.Sprintf("n%03d", i), "file": true, "size": 1}
			}
			writeJSON(t, w, map[string]any{
				"status": 200, "code": 0,
				"data":     map[string]any{"list": list},
				"metadata": map[string]any{"_total": PageSize + 1},
			})
			return
		}
		writeJSON(t, w, map[string]any{
			"status": 200, "code": 0,
			"data":     map[string]any{"list": []map[string]any{{"fid": "f2", "file_name": "last", "file": true, "size": 2}}},
			"metadata": map[string]any{"_total": PageSize + 1},
		})
	})

	files, err := c.List(t.Context(), "root")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != PageSize+1 {
		t.Fatalf("got %d files, want %d", len(files), PageSize+1)
	}
	if calls != 2 {
		t.Fatalf("page calls = %d, want 2", calls)
	}
}

// TestListFansOutAndKeepsPageOrder pins the parallel-pagination contract: the
// first page lands alone (it decides whether more pages exist), the remaining
// pages are fetched concurrently, and the merged listing stays in page order
// so the result is deterministic regardless of completion order.
func TestListFansOutAndKeepsPageOrder(t *testing.T) {
	const total = 3*PageSize + 17 // four pages; the last one is short
	var mu sync.Mutex
	inFlight, peak := 0, 0
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file/sort" {
			http.NotFound(w, r)
			return
		}
		page, err := strconv.Atoi(r.URL.Query().Get("_page"))
		if err != nil {
			t.Errorf("bad _page: %v", err)
			return
		}
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()
		// Hold every page long enough for the fan-out to overlap, so the
		// peak-concurrency assertion below observes the real shape rather
		// than a lucky scheduling order.
		time.Sleep(20 * time.Millisecond)

		count := PageSize
		if n := total - (page-1)*PageSize; n < count {
			count = n
		}
		list := make([]map[string]any, count)
		for i := range list {
			idx := (page-1)*PageSize + i
			list[i] = map[string]any{
				"fid": fmt.Sprintf("fid-%04d", idx), "file_name": fmt.Sprintf("f%04d", idx), "file": true,
			}
		}
		writeJSON(t, w, map[string]any{
			"status": 200, "code": 0,
			"data":     map[string]any{"list": list},
			"metadata": map[string]any{"_total": total},
		})
	})

	files, err := c.List(t.Context(), "root")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != total {
		t.Fatalf("got %d files, want %d", len(files), total)
	}
	for i, f := range files {
		if want := fmt.Sprintf("f%04d", i); f.Name != want {
			t.Fatalf("files[%d].Name = %q, want %q: page order not preserved", i, f.Name, want)
		}
	}
	if peak < 2 {
		t.Fatalf("peak concurrent page fetches = %d, want the pages beyond the first to overlap", peak)
	}
}

// TestListDeduplicatesAcrossPages covers the failure mode concurrency makes
// reachable: if the provider's ordering shifts between page requests, the same
// FID can land on two pages while another entry is skipped. The FID is the
// stable identity, so a duplicate is folded to its first occurrence rather
// than handed to the WebDAV layer twice, where the name-keyed directory map
// would silently drop one of them.
func TestListDeduplicatesAcrossPages(t *testing.T) {
	const total = 2 * PageSize
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file/sort" {
			http.NotFound(w, r)
			return
		}
		page, err := strconv.Atoi(r.URL.Query().Get("_page"))
		if err != nil {
			t.Errorf("bad _page: %v", err)
			return
		}
		var list []map[string]any
		switch page {
		case 1:
			for i := range PageSize {
				list = append(list, map[string]any{
					"fid": fmt.Sprintf("fid-%04d", i), "file_name": fmt.Sprintf("f%04d", i), "file": true,
				})
			}
		case 2:
			// Page 2 re-serves the last entry of page 1, as an unstable
			// sort would, and fills the rest with new entries.
			list = append(list, map[string]any{
				"fid": fmt.Sprintf("fid-%04d", PageSize-1), "file_name": fmt.Sprintf("f%04d", PageSize-1), "file": true,
			})
			for i := PageSize; i < total; i++ {
				list = append(list, map[string]any{
					"fid": fmt.Sprintf("fid-%04d", i), "file_name": fmt.Sprintf("f%04d", i), "file": true,
				})
			}
		default:
			t.Errorf("unexpected page %d", page)
		}
		writeJSON(t, w, map[string]any{
			"status": 200, "code": 0,
			"data":     map[string]any{"list": list},
			"metadata": map[string]any{"_total": total},
		})
	})

	files, err := c.List(t.Context(), "root")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != total {
		t.Fatalf("got %d files, want %d: the re-served FID must be folded, not duplicated", len(files), total)
	}
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		if seen[f.FID] {
			t.Fatalf("fid %s appeared twice in the listing", f.FID)
		}
		seen[f.FID] = true
	}
}

// TestListKeepsGoingWhenTotalUnderReports covers a provider whose `_total` is
// smaller than the pages it actually serves. Fanning out from the reported
// total would then truncate the listing at exactly the wrong place — and a
// truncated listing is indistinguishable from a folder that lost files. When
// the last planned page still comes back full, one more page is fetched to
// prove the listing really ended.
func TestListKeepsGoingWhenTotalUnderReports(t *testing.T) {
	const realTotal = 2 * PageSize // two full pages exist
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file/sort" {
			http.NotFound(w, r)
			return
		}
		page, err := strconv.Atoi(r.URL.Query().Get("_page"))
		if err != nil {
			t.Errorf("bad _page: %v", err)
			return
		}
		var list []map[string]any
		if page <= 2 {
			for i := range PageSize {
				idx := (page-1)*PageSize + i
				list = append(list, map[string]any{
					"fid": fmt.Sprintf("fid-%04d", idx), "file_name": fmt.Sprintf("f%04d", idx), "file": true,
				})
			}
		}
		writeJSON(t, w, map[string]any{
			"status": 200, "code": 0,
			"data": map[string]any{"list": list},
			// The provider claims one page while serving two.
			"metadata": map[string]any{"_total": PageSize},
		})
	})

	files, err := c.List(t.Context(), "root")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != realTotal {
		t.Fatalf("got %d files, want %d: an under-reported total must not truncate", len(files), realTotal)
	}
}

// TestListStopsFanningOutOnCancellation checks a cancelled context aborts the
// fan-out instead of letting the remaining page goroutines keep hammering the
// provider after the mount gave up on the listing.
func TestListStopsFanningOutOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file/sort" {
			http.NotFound(w, r)
			return
		}
		// The first page reports a large total, then cancels: every later
		// page must observe the cancelled context rather than call out.
		cancel()
		writeJSON(t, w, map[string]any{
			"status": 200, "code": 0,
			"data":     map[string]any{"list": []map[string]any{{"fid": "f", "file_name": "n", "file": true}}},
			"metadata": map[string]any{"_total": 1000},
		})
	})

	if _, err := c.List(ctx, "root"); !errors.Is(err, context.Canceled) {
		t.Fatalf("List error = %v, want context.Canceled", err)
	}
}

// shrinkBackoffs collapses every retry pause so a retried stage costs
// microseconds instead of the production backoff. Restored on cleanup.
func shrinkBackoffs(t *testing.T) {
	t.Helper()
	origUpload, origUploadMax := uploadInitialBackoff, uploadMaxBackoff
	origVis, origVisMax := uploadVisibilityInitialBackoff, uploadVisibilityMaxBackoff
	origDel, origDelMax := deleteInitialBackoff, deleteMaxBackoff
	uploadInitialBackoff, uploadMaxBackoff = time.Millisecond, 2*time.Millisecond
	uploadVisibilityInitialBackoff, uploadVisibilityMaxBackoff = time.Millisecond, 2*time.Millisecond
	deleteInitialBackoff, deleteMaxBackoff = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() {
		uploadInitialBackoff, uploadMaxBackoff = origUpload, origUploadMax
		uploadVisibilityInitialBackoff, uploadVisibilityMaxBackoff = origVis, origVisMax
		deleteInitialBackoff, deleteMaxBackoff = origDel, origDelMax
	})
}
